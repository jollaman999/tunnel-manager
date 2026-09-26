package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/alert"
	"github.com/jollaman999/tunnel-manager/internal/models"
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

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	// The local forwards are there because a save that changes the port reads
	// them.
	err = db.AutoMigrate(&settings.Settings{}, &models.Host{}, &models.LocalForward{})
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

	// The set the handler is told this process started on is what is stored at
	// the time it is built, which is what a startup hands it. A test that wants
	// the two apart stores something else afterwards.
	startup, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	logger := zap.New(core)
	cipher := newTestCipher(t)

	return NewSettingsHandler(db, logger, level, gormLevel, *startup, t.TempDir(), cipher,
		alert.NewSender(logger, cipher)), level, gormLevel, logs
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

	err := settings.Save(db, &stored, nil)
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

// TestTheUpdateSettingsTakeHoldWithoutARestart saves each of the three on its
// own. The update loop reads them on every pass, so a save that called any of
// them waiting would leave the Update screen saying the next start is owed for
// a change that is already in place.
func TestTheUpdateSettingsTakeHoldWithoutARestart(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"update.check_enabled", `{"update_check_enabled":false}`},
		{"update.check_interval_hours", `{"update_check_interval_hours":6}`},
		{"update.auto_install", `{"update_auto_install":true}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newSettingsDB(t)
			h, _, _, _ := newSettingsHandler(t, db)

			rec := settingsRequest(t, h, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			saved := decodeSaved(t, rec)
			if len(saved.Changes) != 1 || saved.Changes[0].Name != tc.name ||
				saved.Changes[0].Applied != appliedNow {
				t.Fatalf("changes = %+v, want %s applied %s", saved.Changes, tc.name, appliedNow)
			}
			if saved.RestartRequired {
				t.Fatalf("a save of %s asks for a restart", tc.name)
			}

			pending, _ := decodePending(t, settingsRequest(t, h, ""))
			if len(pending) != 0 {
				t.Fatalf("pending = %+v, want none for %s", pending, tc.name)
			}
		})
	}
}

// TestSaveKeepsWhatTheBodyDoesNotName is what lets the screen send the fields
// it has. A setting the body leaves out keeps its stored value rather than
// being stored as a zero.
func TestSaveKeepsWhatTheBodyDoesNotName(t *testing.T) {
	db := newSettingsDB(t)

	stored := settings.Defaults()
	stored.SecurityKeyFile = "secrets/tunnel-manager.key"
	stored.LoggingFileMaxBackups = 9

	err := settings.Save(db, &stored, nil)
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

// decodePending reads what a read says is stored but not being run on, along
// with the answer it arrived in, so that a test can look at both.
func decodePending(t *testing.T, rec *httptest.ResponseRecorder) ([]pendingSetting, map[string]interface{}) {
	t.Helper()

	var resp struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
		Error   string                 `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if !resp.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	var view struct {
		Data struct {
			PendingRestart []pendingSetting `json:"pending_restart"`
		} `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &view)
	if err != nil {
		t.Fatalf("failed to read what waits for a restart: %v, body: %s", err, rec.Body.String())
	}

	return view.Data.PendingRestart, resp.Data
}

// TestReadReportsNothingPendingAtStartup is the state a process that was just
// started is in: it is running on what is stored, so there is nothing to say.
// An empty list is answered rather than none at all, because the screen tells
// the two apart by what is in the list.
func TestReadReportsNothingPendingAtStartup(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	pending, data := decodePending(t, rec)
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none, body: %s", pending, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), `"pending_restart":[]`) {
		t.Errorf("the empty list is not in the answer as one: %s", rec.Body.String())
	}

	// The settings themselves are still where a client reads them. They sit
	// beside what waits for a restart rather than inside a field of their own.
	if data["api_port"] != float64(settings.Defaults().APIPort) {
		t.Errorf("api_port = %v, want %d, body: %s", data["api_port"],
			settings.Defaults().APIPort, rec.Body.String())
	}
}

// TestReadReportsWhatWaitsForARestart is what the screen is drawn from after a
// save that stored a setting this process cannot take on. It is answered by
// every read, so a session opened later and a second browser are told it too.
func TestReadReportsWhatWaitsForARestart(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":9100}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	pending, _ := decodePending(t, settingsRequest(t, h, ""))

	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the port alone", pending)
	}
	if pending[0].Name != "api.port" {
		t.Fatalf("pending = %+v, want api.port", pending[0])
	}
	if pending[0].Running != "8888" || pending[0].Stored != "9100" {
		t.Fatalf("pending = %+v, want the process on 8888 and 9100 stored", pending[0])
	}

	// A second read answers the same. Nothing about it is spent by being read,
	// which is what the answer of the save was.
	again, _ := decodePending(t, settingsRequest(t, h, ""))
	if len(again) != 1 || again[0] != pending[0] {
		t.Fatalf("the second read says %+v, want %+v", again, pending)
	}
}

// TestRestartClearsWhatWaitedForOne is the half that empties the list. A
// handler built again over the same database is what a restart leaves behind:
// the process comes back on the stored settings, so there is nothing left that
// differs and nothing to clear by hand.
func TestRestartClearsWhatWaitedForOne(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":9100,"monitoring_interval_sec":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	pending, _ := decodePending(t, settingsRequest(t, h, ""))
	if len(pending) != 2 {
		t.Fatalf("pending = %+v, want the port and the monitoring period", pending)
	}

	restarted, _, _, _ := newSettingsHandler(t, db)

	pending, _ = decodePending(t, settingsRequest(t, restarted, ""))
	if len(pending) != 0 {
		t.Fatalf("pending = %+v after a restart, want none", pending)
	}
}

// TestSettingsInPlaceNowDoNotWaitForARestart covers the one setting this
// process takes on as it is stored. The set this process started with still
// holds the level it started at, so a plain comparison would report a level
// that is already being written at and send the operator to restart for it.
func TestSettingsInPlaceNowDoNotWaitForARestart(t *testing.T) {
	db := newSettingsDB(t)
	h, level, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"logging_level":"debug"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if level.Level() != zapcore.DebugLevel {
		t.Fatalf("the running level is %s after the save, want debug", level.Level())
	}

	pending, _ := decodePending(t, settingsRequest(t, h, ""))
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none for a level that is in place", pending)
	}
}

// TestEveryLanguageTheUIOffersIsSavedAndReadBack walks the languages the server
// takes and puts each one through the two calls the screen makes. A code the
// list holds and the save refuses is a language an operator can pick and cannot
// keep.
func TestEveryLanguageTheUIOffersIsSavedAndReadBack(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	for _, code := range settings.Languages() {
		rec := settingsRequest(t, h, `{"ui_default_language":"`+code+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("saving %s: status = %d, want %d, body: %s",
				code, rec.Code, http.StatusOK, rec.Body.String())
		}

		saved := decodeSaved(t, rec)
		if saved.Settings.UIDefaultLanguage != code {
			t.Errorf("saving %s answered with %q", code, saved.Settings.UIDefaultLanguage)
		}
		if saved.RestartRequired {
			t.Errorf("saving %s asks for a restart", code)
		}

		_, data := decodePending(t, settingsRequest(t, h, ""))
		if data["ui_default_language"] != code {
			t.Errorf("the read after saving %s answers with %v", code, data["ui_default_language"])
		}
	}
}

// TestSavingNoLanguageIsAllowed is the value that clears the setting. It is not
// a language that was got wrong but the installation naming none, and a browser
// that has picked nothing then reads the screen in the language it asks for.
func TestSavingNoLanguageIsAllowed(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	if rec := settingsRequest(t, h, `{"ui_default_language":"ko"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec := settingsRequest(t, h, `{"ui_default_language":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	stored, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}
	if stored.UIDefaultLanguage != "" {
		t.Fatalf("ui.default_language = %q after it was cleared, want nothing", stored.UIDefaultLanguage)
	}
}

// TestSaveRefusesALanguageWithNoCatalog is the rule that keeps the screens
// readable. A stored code nothing is translated into would draw the page in the
// names of its keys for every browser that has picked nothing.
//
// The refusal carries a code of its own and the list of languages beside it, so
// that a screen can say no in the language it is already running in and show
// what it would take instead.
func TestSaveRefusesALanguageWithNoCatalog(t *testing.T) {
	for _, code := range []string{"xx", "EN", "ko-KR", "en-US", "klingon"} {
		t.Run(code, func(t *testing.T) {
			db := newSettingsDB(t)
			h, _, _, _ := newSettingsHandler(t, db)

			rec := settingsRequest(t, h, `{"ui_default_language":"`+code+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			var body struct {
				Error string            `json:"error"`
				Code  string            `json:"error_code"`
				Args  map[string]string `json:"error_args"`
			}

			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
			}

			if body.Code != string(errSettingsLanguageUnsupported) {
				t.Errorf("error_code = %q, want %q", body.Code, errSettingsLanguageUnsupported)
			}
			if body.Args["language"] != code {
				t.Errorf("error_args.language = %q, want %q", body.Args["language"], code)
			}
			if !strings.Contains(body.Args["languages"], "pt-BR") {
				t.Errorf("error_args.languages = %q, want the languages that would be taken",
					body.Args["languages"])
			}
			if !strings.Contains(body.Error, code) {
				t.Errorf("the sentence does not name what was refused: %q", body.Error)
			}

			stored, err := settings.Load(db)
			if err != nil {
				t.Fatalf("failed to read the settings: %v", err)
			}
			if stored.UIDefaultLanguage != "" {
				t.Fatalf("ui.default_language = %q after a refused save", stored.UIDefaultLanguage)
			}
		})
	}
}

// TestTheLanguageDoesNotWaitForARestart is the answer to whether this setting
// is one the process has to come back for. It is not: nothing in this process
// reads it, and the browser reads it out of the same answer every read gives.
// Reported as waiting, the screen would carry a notice that a restart would not
// clear anything, because there is nothing to clear.
func TestTheLanguageDoesNotWaitForARestart(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"ui_default_language":"ja"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	saved := decodeSaved(t, rec)
	if saved.RestartRequired {
		t.Fatalf("the save asks for a restart: %+v", saved.Changes)
	}

	if len(saved.Changes) != 1 {
		t.Fatalf("the save reported %d changes, want one: %+v", len(saved.Changes), saved.Changes)
	}
	if saved.Changes[0].Name != "ui.default_language" {
		t.Fatalf("the save reported %q, want ui.default_language", saved.Changes[0].Name)
	}
	if saved.Changes[0].Applied != appliedNow {
		t.Fatalf("the language was reported as %q, want %q", saved.Changes[0].Applied, appliedNow)
	}

	pending, _ := decodePending(t, settingsRequest(t, h, ""))
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none for a language that is in place", pending)
	}
}

// jsonName is the name a field arrives under in a request body, or "" when it
// arrives under none. It is worked out the way encoding/json does: the tag
// decides, a tag of "-" keeps the field out of JSON altogether, and a field
// with no tag is named after itself.
func jsonName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

	if name == "-" {
		return ""
	}
	if name == "" {
		return field.Name
	}

	return name
}

// writeOnlyFields are the fields of updateSettingsRequest requestFields leaves
// out, for the reason given there.
var writeOnlyFields = map[string]bool{
	"smtp_password":           true,
	"smtp_password_clear":     true,
	"alert_webhook_url":       true,
	"alert_webhook_url_clear": true,
}

// requestFields is every field a save may name, read off updateSettingsRequest
// itself. The tests below are written against this rather than against a list
// of their own, so that a field added to the request is covered by them the
// moment it is added and a tag that was typed wrong shows up as a field that
// never arrives.
//
// The password of the mail server, the webhook address and the flags that
// clear them are left out. The password is sealed before it is stored, the
// address is kept out of the JSON of the stored settings the way the password
// is, and the flags are no setting at all, so a comparison of what was sent
// with what was stored says nothing about them. The tests of the password and
// of the webhook address are what covers them.
func requestFields() map[string]bool {
	names := map[string]bool{}

	typ := reflect.TypeOf(updateSettingsRequest{})
	for i := 0; i < typ.NumField(); i++ {
		name := jsonName(typ.Field(i))
		if name != "" && !writeOnlyFields[name] {
			names[name] = true
		}
	}

	return names
}

// settingsFields maps every field of a set of settings to the name it is
// reached by in a request body. A field that is reached by none is in the map
// under its own name, because the body of a test names it that way to show
// that naming it changes nothing.
func settingsFields(s *settings.Settings) map[string]reflect.Value {
	fields := map[string]reflect.Value{}

	typ := reflect.TypeOf(*s)
	val := reflect.ValueOf(*s)

	for i := 0; i < typ.NumField(); i++ {
		name := jsonName(typ.Field(i))
		if name == "" {
			name = typ.Field(i).Name
		}

		fields[name] = val.Field(i)
	}

	return fields
}

// anotherValue returns a value of the same type that differs from the one it is
// given, so that a field can be asked for in a body and then told apart from
// what was stored. It is used for the fields no save may name, where what the
// value is does not matter as long as it is not the one already there.
func anotherValue(t *testing.T, name string, v reflect.Value) interface{} {
	t.Helper()

	if v.Type() == reflect.TypeOf(time.Time{}) {
		return time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	}

	switch v.Kind() {
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() + 1
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() + 1
	case reflect.String:
		return v.String() + "-from-the-body"
	}

	t.Fatalf("no other value is known for %s, which is a %s: this helper has to learn the type",
		name, v.Kind())

	return nil
}

// isTheAskedValue says whether what is stored is what the body asked for. The
// two arrive as different types where JSON and the database each have their own
// idea of a number, so they are held against each other as they read.
func isTheAskedValue(asked interface{}, stored reflect.Value) bool {
	if at, ok := asked.(time.Time); ok {
		st, ok := stored.Interface().(time.Time)

		return ok && st.Equal(at)
	}

	return fmt.Sprintf("%v", asked) == fmt.Sprintf("%v", stored.Interface())
}

// anotherSet is a valid set of settings with every field differing from the
// defaults, which is what the save of a screen carries.
func anotherSet() settings.Settings {
	return settings.Settings{
		APIPort:                  9443,
		APIHTTPSEnabled:          false,
		MonitoringIntervalSec:    30,
		ReconnectMaxIntervalSec:  120,
		ReconcileIntervalSec:     45,
		SecurityKeyFile:          "secrets/another.key",
		LoggingLevel:             "warn",
		LoggingFormat:            "console",
		LoggingFilePath:          "logs/another.log",
		LoggingFileMaxSize:       7,
		LoggingFileMaxBackups:    3,
		LoggingFileMaxAge:        14,
		LoggingFileCompress:      false,
		UIDefaultLanguage:        "ja",
		UpdateCheckEnabled:       false,
		UpdateCheckIntervalHours: 6,
		UpdateAutoInstall:        true,
		AlertAfterSec:            600,
		AlertWebhookURL:          "https://hooks.example.com/alert",
		SMTPHost:                 "mail.example.com",
		SMTPPort:                 587,
		SMTPSecurity:             settings.SMTPSecurityStartTLS,
		SMTPAuth:                 settings.SMTPAuthPlain,
		SMTPUsername:             "alerts",
		SMTPFrom:                 "tunnel-manager@example.com",
		SMTPTo:                   "ops@example.com, oncall@example.net",
		SMTPSkipVerify:           true,
	}
}

// TestSaveTakesEveryFieldTheRequestNames walks updateSettingsRequest and
// requires each of its fields to reach the database. The request is what stands
// between a body and the stored settings, so a field it names and does not copy
// over is a box on the screen that is typed in, answered as saved and never
// stored, which nothing else here would notice.
func TestSaveTakesEveryFieldTheRequestNames(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	defaults := settings.Defaults()
	other := anotherSet()

	asked := settingsFields(&other)
	before := settingsFields(&defaults)

	body := map[string]interface{}{}

	for name := range requestFields() {
		field, known := asked[name]
		if !known {
			t.Fatalf("the request names %q, which is no field of the settings: the tag is wrong "+
				"and nothing a body puts under that name is stored", name)
		}

		// A field that is being stored as what it already holds would pass the
		// check below without having been written at all.
		if isTheAskedValue(field.Interface(), before[name]) {
			t.Fatalf("the set this test saves holds the default for %q, so storing it proves nothing", name)
		}

		body[name] = field.Interface()
	}

	sent, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to build the body: %v", err)
	}

	rec := settingsRequest(t, h, string(sent))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	after, err := settings.LoadOpened(db, h.cipher)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored := settingsFields(after)

	for name := range requestFields() {
		if !isTheAskedValue(asked[name].Interface(), stored[name]) {
			t.Errorf("%s = %v after a save that asked for %v", name,
				stored[name].Interface(), asked[name].Interface())
		}
	}
}

// TestSaveWritesNothingTheRequestDoesNotName is the other half, and it is what
// the request exists for. Every field of the stored settings that
// updateSettingsRequest leaves out is named in the body anyway, under the name
// a body would reach it by, and has to come back holding something else.
//
// The settings this end decides are kept out by being absent from the request
// rather than by a tag on the field, so a field added to settings.Settings is
// not writable until somebody writes it into the request on purpose. This test
// is what says so: it reads both structs rather than a list, so the field added
// next is covered by it without anybody remembering to come back here.
func TestSaveWritesNothingTheRequestDoesNotName(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	stored, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	named := requestFields()
	fields := settingsFields(stored)

	// The body carries a real change as well as the fields it must not be able
	// to make. A body nothing at all was taken from would pass every check
	// below while reaching none of the settings.
	body := map[string]interface{}{"monitoring_interval_sec": 30}

	asked := map[string]interface{}{}

	for name, field := range fields {
		if named[name] {
			continue
		}

		asked[name] = anotherValue(t, name, field)
		body[name] = asked[name]
	}

	if len(asked) == 0 {
		t.Fatalf("the request names every field of the settings, so this test asks for nothing")
	}

	sent, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to build the body: %v", err)
	}

	rec := settingsRequest(t, h, string(sent))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	after, err := settings.LoadOpened(db, h.cipher)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.MonitoringIntervalSec != 30 {
		t.Fatalf("the body reached none of the settings: monitoring.interval_sec is %d, want 30",
			after.MonitoringIntervalSec)
	}

	written := settingsFields(after)

	for name, value := range asked {
		if isTheAskedValue(value, written[name]) {
			t.Errorf("the body named %s and it was stored as %v, which is what the body asked for",
				name, written[name].Interface())
		}
	}
}

// apiPortTakenAnswer is a refusal of a new api_port, with the data it carries.
type apiPortTakenAnswer struct {
	Success bool         `json:"success"`
	Data    apiPortTaken `json:"data"`
	Code    string       `json:"error_code"`
	Args    errorArgs    `json:"error_args"`
}

func readAPIPortTaken(t *testing.T, rec *httptest.ResponseRecorder) apiPortTakenAnswer {
	t.Helper()

	var answer apiPortTakenAnswer

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	return answer
}

// TestSaveRefusesAnAPIPortALocalForwardOpens pins the refusal of a port a
// local forward opens: a 409 that names the forward and a free port, and a
// save that stored nothing, the other setting of the body included. The pool
// holds one connection, so a read through h.db inside the transaction would
// hang here.
func TestSaveRefusesAnAPIPortALocalForwardOpens(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, []models.LocalForward{
		storedLocalForward(1, 1, 15432),
		storedLocalForward(2, 1, 15433),
		storedLocalForward(3, 1, 15434),
	})
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":15432,"monitoring_interval_sec":31}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Success || answer.Code != string(errSettingsAPIPortLocalForward) {
		t.Errorf("success = %v, error_code = %q, want false and %q", answer.Success, answer.Code,
			errSettingsAPIPortLocalForward)
	}

	want := apiPortHolder{Number: 1, HostID: 1, HostAddress: "192.0.2.1", LocalPort: 15432,
		TargetAddress: "127.0.0.1", TargetPort: 5432}
	if answer.Data.LocalForward != want {
		t.Errorf("local_forward = %+v, want %+v", answer.Data.LocalForward, want)
	}

	// 15433 and 15434 are opened by the other two forwards.
	if answer.Data.SuggestedPort != 15435 {
		t.Errorf("suggested_port = %d, want 15435", answer.Data.SuggestedPort)
	}

	if answer.Args["api_port"] != "15432" || answer.Args["host"] != "192.0.2.1" ||
		answer.Args["target"] != "127.0.0.1:5432" {
		t.Errorf("error_args = %v", answer.Args)
	}

	after, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.APIPort != localForwardAPIPort || after.MonitoringIntervalSec == 31 {
		t.Errorf("the refused save was stored: api_port %d, monitoring.interval_sec %d",
			after.APIPort, after.MonitoringIntervalSec)
	}
}

// TestSaveTakesAnAPIPortNoLocalForwardOpens is the other side: with forwards
// stored, a port none of them opens is saved as it always was.
func TestSaveTakesAnAPIPortNoLocalForwardOpens(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, []models.LocalForward{
		storedLocalForward(1, 1, 15432),
	})
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":15500}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	after, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.APIPort != 15500 {
		t.Errorf("api_port = %d, want 15500", after.APIPort)
	}
}

// TestSaveSuggestsAPortClearOfTheRunningAPIPort is the refusal of a port a
// local forward opens while this process listens on a port other than the
// stored one: the port suggested steps over the running port as it does over
// the stored one.
func TestSaveSuggestsAPortClearOfTheRunningAPIPort(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, []models.LocalForward{
		storedLocalForward(1, 1, 15432),
	})
	h, _, _, _ := newSettingsHandler(t, db)
	h.startup.APIPort = 15433

	rec := settingsRequest(t, h, `{"api_port":15432}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Code != string(errSettingsAPIPortLocalForward) {
		t.Errorf("error_code = %q, want %q", answer.Code, errSettingsAPIPortLocalForward)
	}

	if answer.Data.SuggestedPort != 15434 {
		t.Errorf("suggested_port = %d, want 15434, past the running port 15433", answer.Data.SuggestedPort)
	}
}
