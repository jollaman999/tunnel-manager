package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newSettingsDB returns a handle on an empty database file holding the settings
// table. A real file is used rather than one of the stubs above, because what
// is under test is what comes back out of the database after a save.
func newSettingsDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&settings.Settings{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	return db
}

// recordedGormLevel stands in for the handle the level of the database logger
// is changed by. The real one lives in internal/database; what matters here is
// that the save reaches it, because the lines gorm writes go through a level
// check of their own.
type recordedGormLevel struct {
	levels []string
}

func (r *recordedGormLevel) Set(level string) {
	r.levels = append(r.levels, level)
}

// last is the level the handle was left at, or "" when nothing was put on it.
func (r *recordedGormLevel) last() string {
	if len(r.levels) == 0 {
		return ""
	}

	return r.levels[len(r.levels)-1]
}

// newSettingsHandler returns the handler together with the two level handles it
// was given and the log it writes through. The levels are what the save under
// test has to reach, and the log is read to see that it reached them.
func newSettingsHandler(t *testing.T, db *gorm.DB) (*SettingsHandler, zap.AtomicLevel,
	*recordedGormLevel, *observer.ObservedLogs) {
	t.Helper()

	level := zap.NewAtomicLevelAt(zapcore.InfoLevel)

	// The core is built around the same handle the handler is handed, which is
	// how the running process is put together: the level is not baked into the
	// core, so setting it decides what the core writes from then on.
	core, logs := observer.New(level)
	gormLevel := &recordedGormLevel{}

	return NewSettingsHandler(db, zap.New(core), level, gormLevel), level, gormLevel, logs
}

// settingsRequest runs one call against the handler and hands back what it
// wrote. A body of "" is a GET, anything else is a PUT carrying it.
func settingsRequest(t *testing.T, h *SettingsHandler, body string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()

	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	} else {
		req = httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	var err error
	if body == "" {
		err = h.GetSettings(c)
	} else {
		err = h.UpdateSettings(c)
	}
	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec
}

// decodeSaved reads the answer of a save.
func decodeSaved(t *testing.T, rec *httptest.ResponseRecorder) settingsSaved {
	t.Helper()

	var resp struct {
		Success bool          `json:"success"`
		Data    settingsSaved `json:"data"`
		Error   string        `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if !resp.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	return resp.Data
}

// TestGetSettingsAnswersWhatIsStored covers the read the screen is filled from.
func TestGetSettingsAnswersWhatIsStored(t *testing.T) {
	db := newSettingsDB(t)

	stored := settings.Defaults()
	stored.APIPort = 9001
	stored.LoggingLevel = "warn"

	err := settings.Save(db, &stored)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	h, _, _, _ := newSettingsHandler(t, db)
	rec := settingsRequest(t, h, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if !success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	if data["api_port"] != float64(9001) {
		t.Errorf("api_port = %v, want 9001, body: %s", data["api_port"], rec.Body.String())
	}
	if data["logging_level"] != "warn" {
		t.Errorf("logging_level = %v, want warn, body: %s", data["logging_level"], rec.Body.String())
	}
}

// TestSaveRefusesAPortOutsideTheRange is what keeps a set that would stop the
// next startup out of the database. There is no file left to repair one in.
func TestSaveRefusesAPortOutsideTheRange(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":0}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	// The answer says which value was refused, because the screen shows it next
	// to the box it was typed in.
	if !strings.Contains(rec.Body.String(), "API port") {
		t.Errorf("the answer does not say what was refused: %s", rec.Body.String())
	}

	stored, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}
	if stored.APIPort != settings.Defaults().APIPort {
		t.Fatalf("api.port = %d after a refused save, want %d", stored.APIPort, settings.Defaults().APIPort)
	}
}

// TestSaveRefusesALevelTheLoggerDoesNotKnow covers the other half of the rules:
// a level outside the list is refused rather than reaching the logger.
func TestSaveRefusesALevelTheLoggerDoesNotKnow(t *testing.T) {
	db := newSettingsDB(t)
	h, level, gormLevel, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"logging_level":"chatty"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	if level.Level() != zapcore.InfoLevel {
		t.Fatalf("the running level is %s after a refused save, want info", level.Level())
	}

	if put := gormLevel.last(); put != "" {
		t.Fatalf("the database logger was put at %q by a refused save", put)
	}
}

// TestSaveSetsTheRunningLogLevel is the one setting this process takes on as it
// is stored. The core was built around the handle the handler was given, so a
// debug entry written after the save is one the running logger let through.
func TestSaveSetsTheRunningLogLevel(t *testing.T) {
	db := newSettingsDB(t)
	h, level, gormLevel, logs := newSettingsHandler(t, db)

	// The logger of the handler is the one built around the handle, so writing
	// through it is writing through what the running process writes through.
	writer := h.logger

	writer.Debug("before the save")
	if logs.FilterMessage("before the save").Len() != 0 {
		t.Fatalf("a debug entry was written while the level was info")
	}

	rec := settingsRequest(t, h, `{"logging_level":"debug"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if level.Level() != zapcore.DebugLevel {
		t.Fatalf("the running level is %s after the save, want debug", level.Level())
	}

	// The lines gorm writes go through a level check of their own, so a save
	// that reached zap alone would leave the statements it reports at the level
	// the process started on while the screen says the level took hold.
	if put := gormLevel.last(); put != "debug" {
		t.Fatalf("the database logger was put at %q, want debug", put)
	}

	writer.Debug("after the save")
	if logs.FilterMessage("after the save").Len() != 1 {
		t.Fatalf("no debug entry was written after the level was stored as debug")
	}

	saved := decodeSaved(t, rec)
	if len(saved.Changes) != 1 {
		t.Fatalf("changes = %v, want the level alone", saved.Changes)
	}
	if saved.Changes[0].Name != "logging.level" || saved.Changes[0].Applied != appliedNow {
		t.Fatalf("change = %+v, want logging.level applied %s", saved.Changes[0], appliedNow)
	}
	if saved.RestartRequired {
		t.Fatalf("the answer asks for a restart for a level that is already in place")
	}
}

// TestSaveReportsWhatWaitsForARestart covers the settings that are stored and
// nothing more. The screen says so from this answer, so a change reported as
// taken on would leave the operator believing the process moved.
func TestSaveReportsWhatWaitsForARestart(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h,
		`{"api_port":9100,"reconcile_interval_sec":11,"logging_level":"debug"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	saved := decodeSaved(t, rec)

	want := map[string]string{
		"api.port":               appliedRestart,
		"reconcile.interval_sec": appliedRestart,
		"logging.level":          appliedNow,
	}

	got := map[string]string{}
	for _, change := range saved.Changes {
		got[change.Name] = change.Applied
	}

	for name, applied := range want {
		if got[name] != applied {
			t.Errorf("%s is reported as %q, want %q, body: %s", name, got[name], applied, rec.Body.String())
		}
	}

	if len(saved.Changes) != len(want) {
		t.Errorf("changes = %+v, want %d of them", saved.Changes, len(want))
	}

	if !saved.RestartRequired {
		t.Fatalf("the answer does not ask for a restart, body: %s", rec.Body.String())
	}
}

// TestSaveKeepsWhatTheBodyDoesNotName is what lets the screen send the fields
// it has. A setting the body leaves out keeps its stored value rather than
// being stored as a zero.
func TestSaveKeepsWhatTheBodyDoesNotName(t *testing.T) {
	db := newSettingsDB(t)

	stored := settings.Defaults()
	stored.SecurityKeyFile = "/var/lib/tunnel-manager/tunnel-manager.key"
	stored.LoggingFileMaxBackups = 9

	err := settings.Save(db, &stored)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"monitoring_interval_sec":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	after, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.MonitoringIntervalSec != 30 {
		t.Errorf("monitoring.interval_sec = %d, want 30", after.MonitoringIntervalSec)
	}
	if after.SecurityKeyFile != stored.SecurityKeyFile {
		t.Errorf("security.key_file = %q, want %q", after.SecurityKeyFile, stored.SecurityKeyFile)
	}
	if after.LoggingFileMaxBackups != 9 {
		t.Errorf("logging.file.max_backups = %d, want 9", after.LoggingFileMaxBackups)
	}

	saved := decodeSaved(t, rec)
	if len(saved.Changes) != 1 || saved.Changes[0].Name != "monitoring.interval_sec" {
		t.Fatalf("changes = %+v, want the monitoring period alone", saved.Changes)
	}
	if saved.Changes[0].From != "5" || saved.Changes[0].To != "30" {
		t.Fatalf("change = %+v, want 5 to 30", saved.Changes[0])
	}
}

// TestSaveThatChangesNothingReportsNoChange keeps the screen from announcing a
// restart for a press that stored what was already there.
func TestSaveThatChangesNothingReportsNoChange(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	stored := settingsRequest(t, h, "")
	if stored.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", stored.Code, http.StatusOK, stored.Body.String())
	}

	rec := settingsRequest(t, h, `{"api_port":8888}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	saved := decodeSaved(t, rec)
	if len(saved.Changes) != 0 {
		t.Fatalf("changes = %+v, want none", saved.Changes)
	}
	if saved.RestartRequired {
		t.Fatalf("the answer asks for a restart though nothing changed")
	}
}
