package settings

import (
	"errors"
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
//
// Two have been changed on purpose since, and are held to the new value rather
// than the old one: api.https_enabled, because the screens carry a password,
// and logging.file.compress, because what it compresses is a rotated log that
// nothing reads again. A stored setting is not touched either way; this is what
// an installation that has none starts on.
//
// ui.default_language was never in the configuration file, and it is held to
// the empty string: an installation that has said nothing about a language
// leaves every browser reading the screen in the language that browser asks
// for, which is what they all did before the setting existed.
func TestDefaultsAreTheValuesTheConfigurationFileRanOn(t *testing.T) {
	d := Defaults()

	cases := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"api.port", d.APIPort, 8888},
		{"api.https_enabled", d.APIHTTPSEnabled, true},
		{"monitoring.interval_sec", d.MonitoringIntervalSec, 5},
		{"reconcile.interval_sec", d.ReconcileIntervalSec, 5},
		{"security.key_file", d.SecurityKeyFile, "keys/tunnel-manager.key"},
		{"logging.level", d.LoggingLevel, "info"},
		{"logging.format", d.LoggingFormat, "json"},
		{"logging.file.path", d.LoggingFilePath, "logs/tunnel-manager.log"},
		{"logging.file.max_size", d.LoggingFileMaxSize, 100},
		{"logging.file.max_backups", d.LoggingFileMaxBackups, 5},
		{"logging.file.max_age", d.LoggingFileMaxAge, 30},
		{"logging.file.compress", d.LoggingFileCompress, true},
		{"ui.default_language", d.UIDefaultLanguage, ""},
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

// TestSaveStoresHTTPSTurnedOff is the half of the column default that is easy
// to lose. The column carries DEFAULT true so that a row written before the
// setting existed reads as HTTPS on, and gorm leaves a field at its zero value
// out of an INSERT when it carries such a default. A save that turned HTTPS off
// and came back on would leave the operator with no way to reach a server whose
// certificate their client refuses.
func TestSaveStoresHTTPSTurnedOff(t *testing.T) {
	db := newDB(t)

	stored, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !stored.APIHTTPSEnabled {
		t.Fatalf("a first startup stored api.https_enabled as off, want on")
	}

	stored.APIHTTPSEnabled = false

	err = Save(db, stored)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if again.APIHTTPSEnabled {
		t.Fatalf("api.https_enabled came back on after it was saved off")
	}
}

// TestARowWrittenBeforeTheHTTPSSettingReadsAsHTTPSOn covers the installation
// that is upgraded. Its settings row was written by a version whose table had
// no column for HTTPS, and a column added without a default would read back as
// false and quietly leave that installation serving in the clear.
//
// The row that version wrote is built by dropping the column again, which is
// what the table looked like before the migration added it.
func TestARowWrittenBeforeTheHTTPSSettingReadsAsHTTPSOn(t *testing.T) {
	db := newDB(t)

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = db.Exec("ALTER TABLE settings DROP COLUMN api_https_enabled").Error
	if err != nil {
		t.Fatalf("dropping the column to build a row from before it existed: %v", err)
	}

	err = db.AutoMigrate(&Settings{})
	if err != nil {
		t.Fatalf("migrating the column back: %v", err)
	}

	upgraded, err := Load(db)
	if err != nil {
		t.Fatalf("Load after the migration: %v", err)
	}
	if !upgraded.APIHTTPSEnabled {
		t.Fatalf("a row written before the setting existed reads as HTTPS off, want on")
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
		{"unknown language", func(s *Settings) { s.UIDefaultLanguage = "xx" }, "UI language"},
		// The two near misses. A code is stored as it is written, so the one
		// that is spelled in capitals and the one that carries a region the UI
		// has no catalog for are refused rather than quietly turned into
		// something else.
		{"language in capitals", func(s *Settings) { s.UIDefaultLanguage = "EN" }, "UI language"},
		{"language with a region", func(s *Settings) { s.UIDefaultLanguage = "ko-KR" }, "UI language"},
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

// TestEveryLanguageTheUIOffersCanBeStored walks the list this package holds and
// stores each of them. A code that is offered by the UI and refused here is a
// language an operator can pick on the screen and cannot save.
func TestEveryLanguageTheUIOffersCanBeStored(t *testing.T) {
	db := newDB(t)

	for _, code := range Languages() {
		stored, err := Load(db)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		stored.UIDefaultLanguage = code

		err = Save(db, stored)
		if err != nil {
			t.Fatalf("Save of the %s language: %v", code, err)
		}

		again, err := Load(db)
		if err != nil {
			t.Fatalf("Load after saving %s: %v", code, err)
		}
		if again.UIDefaultLanguage != code {
			t.Errorf("ui.default_language came back as %q after %q was saved", again.UIDefaultLanguage, code)
		}
	}
}

// TestNoLanguageIsAValueOfItsOwn is the half of the setting that is easy to
// lose. The empty string is this installation naming no language rather than a
// setting that was got wrong, so it has to pass the rules and it has to survive
// a save: refused or turned into "en" on the way through, every browser that
// has picked nothing would be shown English instead of what it asks for.
func TestNoLanguageIsAValueOfItsOwn(t *testing.T) {
	db := newDB(t)

	stored, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.UIDefaultLanguage != "" {
		t.Fatalf("a first startup stored ui.default_language as %q, want no language at all",
			stored.UIDefaultLanguage)
	}

	stored.UIDefaultLanguage = "ko"

	err = Save(db, stored)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	stored.UIDefaultLanguage = ""

	err = Save(db, stored)
	if err != nil {
		t.Fatalf("Save of no language at all: %v", err)
	}

	again, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if again.UIDefaultLanguage != "" {
		t.Fatalf("ui.default_language came back as %q after it was cleared", again.UIDefaultLanguage)
	}
}

// TestAnUnsupportedLanguageIsRefusedUnderItsOwnError is what lets the API
// answer this one refusal with a code of its own and the list of languages in
// it. Wrapped rather than reported as a sentence, so the handler does not have
// to read the English text to know which rule said no.
func TestAnUnsupportedLanguageIsRefusedUnderItsOwnError(t *testing.T) {
	s := Defaults()
	s.UIDefaultLanguage = "xx"

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted a language no catalog is shipped for")
	}
	if !errors.Is(err, ErrLanguageUnsupported) {
		t.Fatalf("error = %v, want it to wrap ErrLanguageUnsupported", err)
	}
	if !strings.Contains(err.Error(), "xx") {
		t.Errorf("error = %v, want it to name the language that was refused", err)
	}

	// Every other refusal has to stay tellable from this one, or the handler
	// would answer a bad port with the sentence about languages.
	broken := Defaults()
	broken.APIPort = 0

	if err := broken.Validate(); errors.Is(err, ErrLanguageUnsupported) {
		t.Errorf("a refused API port reads as an unsupported language: %v", err)
	}
}

// TestARowWrittenBeforeTheLanguageSettingReadsAsNoLanguage covers the
// installation that is upgraded, the way the HTTPS test above does. The column
// is added by the migration and the rows that are already there are filled with
// NULL, which has to read back as the empty string and not stop the startup.
//
// The name of the column is written out here on purpose: gorm works it out from
// the field, and a name that is not what this says would be a column added
// beside the stored one, leaving the setting behind at every upgrade.
func TestARowWrittenBeforeTheLanguageSettingReadsAsNoLanguage(t *testing.T) {
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

	err = db.Exec("ALTER TABLE settings DROP COLUMN ui_default_language").Error
	if err != nil {
		t.Fatalf("dropping the column to build a row from before it existed: %v", err)
	}

	err = db.AutoMigrate(&Settings{})
	if err != nil {
		t.Fatalf("migrating the column back: %v", err)
	}

	upgraded, err := Load(db)
	if err != nil {
		t.Fatalf("Load after the migration: %v", err)
	}
	if upgraded.UIDefaultLanguage != "" {
		t.Fatalf("a row written before the setting existed reads as %q, want no language",
			upgraded.UIDefaultLanguage)
	}

	// The settings that were stored before the column existed are still there.
	if upgraded.LoggingLevel != "debug" {
		t.Fatalf("logging.level is %q after the migration, want the stored debug", upgraded.LoggingLevel)
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
