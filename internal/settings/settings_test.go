package settings

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newDB returns a handle on an empty database file that holds the settings
// table and nothing else. A real file is used rather than a fake driver because
// what is under test is what comes back out of the database after a write.
func newDB(t *testing.T) *gorm.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tunnel-manager.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&Settings{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	return db
}

// TestDefaultsAreTheValuesTheConfigurationFileRanOn holds every default against
// the value the configuration file fell back to before the settings moved into
// the database. A default that drifted here would change what a deployment runs
// on without anybody asking for it.
func TestDefaultsAreTheValuesTheConfigurationFileRanOn(t *testing.T) {
	d := Defaults()

	cases := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"api.port", d.APIPort, 8888},
		{"monitoring.interval_sec", d.MonitoringIntervalSec, 5},
		{"reconcile.interval_sec", d.ReconcileIntervalSec, 5},
		{"security.key_file", d.SecurityKeyFile, "keys/tunnel-manager.key"},
		{"logging.level", d.LoggingLevel, "info"},
		{"logging.format", d.LoggingFormat, "json"},
		{"logging.file.path", d.LoggingFilePath, "logs/tunnel-manager.log"},
		{"logging.file.max_size", d.LoggingFileMaxSize, 100},
		{"logging.file.max_backups", d.LoggingFileMaxBackups, 5},
		{"logging.file.max_age", d.LoggingFileMaxAge, 30},
		{"logging.file.compress", d.LoggingFileCompress, false},
	}

	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	if err := d.Validate(); err != nil {
		t.Fatalf("the defaults do not pass the validation: %v", err)
	}
}

// TestLoadStoresTheDefaultsOnAFirstStartup covers the empty database a first
// startup finds. The row is written there and then, so the screen that shows
// the settings has something to read and the next startup reads the same values.
func TestLoadStoresTheDefaultsOnAFirstStartup(t *testing.T) {
	db := newDB(t)

	loaded, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	defaults := Defaults()
	if *loaded != withUpdatedAtOf(defaults, *loaded) {
		t.Fatalf("the settings that came back are %+v, want the defaults %+v", *loaded, defaults)
	}

	var count int64

	err = db.Model(&Settings{}).Count(&count).Error
	if err != nil {
		t.Fatalf("counting the stored settings: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d settings rows were stored, want 1", count)
	}
}

// withUpdatedAtOf returns s with the timestamp of other, so that two sets can
// be compared over the settings alone.
func withUpdatedAtOf(s Settings, other Settings) Settings {
	s.UpdatedAt = other.UpdatedAt
	return s
}

// TestLoadReturnsWhatWasStored is the second startup: the stored row is read
// and the defaults are not written over it.
func TestLoadReturnsWhatWasStored(t *testing.T) {
	db := newDB(t)

	stored, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	stored.APIPort = 9999
	stored.LoggingLevel = "debug"

	err = Save(db, stored)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if again.APIPort != 9999 {
		t.Errorf("api.port = %d, want 9999", again.APIPort)
	}
	if again.LoggingLevel != "debug" {
		t.Errorf("logging.level = %q, want debug", again.LoggingLevel)
	}
}

// TestSaveRefusesASetThatWouldNotStart is why the validation moved here with
// the settings. A stored api.port of 0 keeps the process from serving the very
// screen the setting is changed on, so the save is refused and what is stored
// is left as it was.
func TestSaveRefusesASetThatWouldNotStart(t *testing.T) {
	db := newDB(t)

	stored, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	broken := *stored
	broken.APIPort = 0

	err = Save(db, &broken)
	if err == nil {
		t.Fatal("Save accepted an API port of 0")
	}
	if !strings.Contains(err.Error(), "API port") {
		t.Errorf("error = %v, want it to name the API port", err)
	}

	again, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if again.APIPort != stored.APIPort {
		t.Fatalf("api.port was stored as %d although the save was refused, want %d",
			again.APIPort, stored.APIPort)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Settings)
		want   string
	}{
		{"api port of zero", func(s *Settings) { s.APIPort = 0 }, "API port"},
		{"api port out of range", func(s *Settings) { s.APIPort = 70000 }, "API port"},
		{"zero monitoring interval", func(s *Settings) { s.MonitoringIntervalSec = 0 }, "monitoring interval"},
		{"negative monitoring interval", func(s *Settings) { s.MonitoringIntervalSec = -1 }, "monitoring interval"},
		{"zero reconcile interval", func(s *Settings) { s.ReconcileIntervalSec = 0 }, "reconcile interval"},
		{"negative reconcile interval", func(s *Settings) { s.ReconcileIntervalSec = -1 }, "reconcile interval"},
		{"empty key file", func(s *Settings) { s.SecurityKeyFile = "" }, "encryption key file"},
		{"unknown log level", func(s *Settings) { s.LoggingLevel = "verbose" }, "log level"},
		{"unknown log format", func(s *Settings) { s.LoggingFormat = "text" }, "log format"},
		{"negative log max size", func(s *Settings) { s.LoggingFileMaxSize = -1 }, "log max size"},
		{"negative log max backups", func(s *Settings) { s.LoggingFileMaxBackups = -1 }, "log max backups"},
		{"negative log max age", func(s *Settings) { s.LoggingFileMaxAge = -1 }, "log max age"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Defaults()
			tc.mutate(&s)

			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestLoadRefusesAStoredSetThatDoesNotPass covers a row that was written around
// Save, by hand or by an older version. Starting on it would leave the process
// on values nobody chose, so the read stops and names the way out.
func TestLoadRefusesAStoredSetThatDoesNotPass(t *testing.T) {
	db := newDB(t)

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = db.Model(&Settings{}).Where("id = ?", settingsID).Update("api_port", 0).Error
	if err != nil {
		t.Fatalf("storing an API port of 0 by hand: %v", err)
	}

	loaded, err := Load(db)
	if err == nil {
		t.Fatalf("Load accepted a stored API port of 0: %+v", loaded)
	}
	if !strings.Contains(err.Error(), "-reset-settings") {
		t.Errorf("error = %v, want it to name the flag that puts the defaults back", err)
	}
}

// TestResetPutsTheDefaultsBackAndSaysWhatChanged is the flag that reaches a
// stored set which keeps the process from starting. It has to work on exactly
// the row a read refuses, which is why it reads without validating.
func TestResetPutsTheDefaultsBackAndSaysWhatChanged(t *testing.T) {
	db := newDB(t)

	stored, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	stored.LoggingLevel = "debug"

	err = Save(db, stored)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A value that Save would refuse, written by hand, is the case the flag
	// exists for.
	err = db.Model(&Settings{}).Where("id = ?", settingsID).Update("api_port", 0).Error
	if err != nil {
		t.Fatalf("storing an API port of 0 by hand: %v", err)
	}

	before, after, err := Reset(db)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}

	defaults := Defaults()
	if *after != withUpdatedAtOf(defaults, *after) {
		t.Fatalf("Reset stored %+v, want the defaults %+v", *after, defaults)
	}

	changes := Diff(before, after)

	got := map[string]Change{}
	for _, change := range changes {
		got[change.Name] = change
	}

	if len(got) != 2 {
		t.Fatalf("Reset reported %d changes, want 2: %+v", len(got), changes)
	}
	if change := got["api.port"]; change.From != "0" || change.To != "8888" {
		t.Errorf("api.port was reported as %q -> %q, want 0 -> 8888", change.From, change.To)
	}
	if change := got["logging.level"]; change.From != "debug" || change.To != "info" {
		t.Errorf("logging.level was reported as %q -> %q, want debug -> info", change.From, change.To)
	}

	reloaded, err := Load(db)
	if err != nil {
		t.Fatalf("Load after Reset: %v", err)
	}
	if reloaded.APIPort != 8888 || reloaded.LoggingLevel != "info" {
		t.Fatalf("what was stored after the reset is %+v, want the defaults", *reloaded)
	}
}

// TestResetReportsEverythingWhenNothingWasStored covers the database that holds
// no settings row at all, since the flag may be reached before a first startup.
func TestResetReportsEverythingWhenNothingWasStored(t *testing.T) {
	db := newDB(t)

	before, after, err := Reset(db)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if before != nil {
		t.Fatalf("Reset reported stored settings in an empty database: %+v", *before)
	}

	changes := Diff(before, after)
	if len(changes) != len(values(after)) {
		t.Fatalf("Reset reported %d changes, want every setting (%d)", len(changes), len(values(after)))
	}
	for _, change := range changes {
		if change.From != "" {
			t.Errorf("%s was reported as coming from %q, want nothing", change.Name, change.From)
		}
	}
}

func TestDiffReportsNothingWhenTheSettingsAreTheSame(t *testing.T) {
	a := Defaults()
	b := Defaults()

	if changes := Diff(&a, &b); len(changes) != 0 {
		t.Fatalf("two identical sets were reported as differing: %+v", changes)
	}
}
