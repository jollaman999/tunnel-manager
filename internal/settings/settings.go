package settings

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// settingsID is the primary key of the single row this package keeps. The
// settings describe the one process that reads them, and this setup takes one
// process per database file, so a second row would be a set of settings nobody
// reads. A fixed key makes every read and every write name the same row instead
// of depending on what happens to be stored.
const settingsID = 1

// Settings holds what used to be the api, monitoring, reconcile, security and
// logging sections of the configuration file.
//
// It is one row with a column per setting rather than a table of key and value
// pairs. A column keeps the type the setting has, so a port stays an int and
// compress stays a bool instead of being parsed back out of text on every read,
// Validate sees the whole set at once, and a save is a single UPDATE, so the
// process never reads half of one save and half of the next. Pairs would make
// adding a setting cheaper, which is the smaller job here: the settings are
// read once at startup and written from one screen.
type Settings struct {
	ID uint `gorm:"primaryKey" json:"-"`

	APIPort int `json:"api_port"`

	// APIHTTPSEnabled is what decides whether the port above is served over
	// TLS. It is stored with a column default of true so that a database
	// written by a version that had no such column reads back as HTTPS on:
	// SQLite fills the rows that are already there with the default as the
	// column is added, and an installation that is upgraded gets the same
	// answer a fresh one does.
	//
	// The column is named here because the name gorm works out from the field
	// is api_http_s_enabled: it knows HTTP as one word and leaves the S of
	// HTTPS on its own.
	APIHTTPSEnabled bool `gorm:"column:api_https_enabled;default:true" json:"api_https_enabled"`

	MonitoringIntervalSec int `json:"monitoring_interval_sec"`
	ReconcileIntervalSec  int `json:"reconcile_interval_sec"`

	SecurityKeyFile string `json:"security_key_file"`

	LoggingLevel  string `json:"logging_level"`
	LoggingFormat string `json:"logging_format"`

	LoggingFilePath       string `json:"logging_file_path"`
	LoggingFileMaxSize    int    `json:"logging_file_max_size"`
	LoggingFileMaxBackups int    `json:"logging_file_max_backups"`
	LoggingFileMaxAge     int    `json:"logging_file_max_age"`
	LoggingFileCompress   bool   `json:"logging_file_compress"`

	UpdatedAt time.Time `json:"updated_at"`
}

// Defaults returns what a first startup stores.
//
// Every value is the one the configuration file fell back to while the settings
// lived there, so a deployment that drops its settings sections runs on as it
// did. api.port and monitoring.interval_sec are the exception: they had no
// fallback at all, since a file that left them out was refused, so the values
// the configuration file carried before it was removed are used. logging.file.compress had no
// fallback either and a file that left it out ran with it off, which is what
// stands here.
func Defaults() Settings {
	return Settings{
		ID:                    settingsID,
		APIPort:               8888,
		APIHTTPSEnabled:       true,
		MonitoringIntervalSec: 5,
		ReconcileIntervalSec:  5,
		SecurityKeyFile:       "keys/tunnel-manager.key",
		LoggingLevel:          "info",
		LoggingFormat:         "json",
		LoggingFilePath:       "logs/tunnel-manager.log",
		LoggingFileMaxSize:    100,
		LoggingFileMaxBackups: 5,
		LoggingFileMaxAge:     30,
		LoggingFileCompress:   false,
	}
}

var validLevels = map[string]bool{
	"debug":  true,
	"info":   true,
	"warn":   true,
	"error":  true,
	"dpanic": true,
	"panic":  true,
	"fatal":  true,
}

var validFormats = map[string]bool{
	"json":    true,
	"console": true,
}

// Validate holds the rules the configuration file was checked against before
// the settings moved into the database. They matter more here than they did
// there: a file that refuses to start can be edited, while a stored setting
// that refuses to start leaves nothing to edit but the database itself.
func (s *Settings) Validate() error {
	if s.APIPort < 1 || s.APIPort > 65535 {
		return fmt.Errorf("invalid API port: %d", s.APIPort)
	}

	// api.https_enabled carries no rule of its own. It is a bool, and both of
	// its values start a server: one that speaks TLS and one that speaks in the
	// clear. What could go wrong with it, a certificate that cannot be built or
	// cannot be read back, is not a property of this set and is reported where
	// the certificate is read.

	if s.MonitoringIntervalSec <= 0 {
		return fmt.Errorf("invalid monitoring interval: %d", s.MonitoringIntervalSec)
	}

	if s.ReconcileIntervalSec <= 0 {
		return fmt.Errorf("invalid reconcile interval: %d", s.ReconcileIntervalSec)
	}

	// The key file is the one rule that was not in the configuration file. An
	// empty path stops the startup where the key is loaded, and a setting that
	// stops the startup can no longer be corrected by editing a file.
	if s.SecurityKeyFile == "" {
		return fmt.Errorf("the encryption key file is required")
	}

	if !validLevels[s.LoggingLevel] {
		return fmt.Errorf("invalid log level: %s", s.LoggingLevel)
	}

	if !validFormats[s.LoggingFormat] {
		return fmt.Errorf("invalid log format: %s", s.LoggingFormat)
	}

	if s.LoggingFileMaxSize < 0 {
		return fmt.Errorf("invalid log max size: %d", s.LoggingFileMaxSize)
	}
	if s.LoggingFileMaxBackups < 0 {
		return fmt.Errorf("invalid log max backups: %d", s.LoggingFileMaxBackups)
	}
	if s.LoggingFileMaxAge < 0 {
		return fmt.Errorf("invalid log max age: %d", s.LoggingFileMaxAge)
	}

	return nil
}

// read returns the stored row, or nil when there is none. It does not validate,
// because the callers that reach for it are the ones that have to look at a row
// which does not pass.
func read(db *gorm.DB) (*Settings, error) {
	var s Settings

	err := db.First(&s, settingsID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the settings: %w", err)
	}

	return &s, nil
}

// Load returns the stored settings, storing the defaults first when there is no
// row yet, which is what a first startup finds.
//
// A stored set that does not pass Validate stops the startup rather than being
// repaired here. Repairing it would put the process on values nobody chose
// while the screen keeps showing the stored ones, so the failure names the flag
// that puts the defaults back.
func Load(db *gorm.DB) (*Settings, error) {
	stored, err := read(db)
	if err != nil {
		return nil, err
	}

	if stored == nil {
		created := Defaults()

		err = db.Create(&created).Error
		if err != nil {
			return nil, fmt.Errorf("failed to store the default settings: %w", err)
		}

		return &created, nil
	}

	err = stored.Validate()
	if err != nil {
		return nil, fmt.Errorf("the stored settings are refused: %w. Start with -reset-settings "+
			"to put every setting back to its default", err)
	}

	return stored, nil
}

// Save validates before it writes, so a set that would keep the process from
// starting again never reaches the database. The whole row is written in one
// statement: a save that landed field by field could leave a set that no screen
// ever asked for if the process ended in the middle of it.
func Save(db *gorm.DB, s *Settings) error {
	err := s.Validate()
	if err != nil {
		return err
	}

	stored := *s
	stored.ID = settingsID

	err = db.Save(&stored).Error
	if err != nil {
		return fmt.Errorf("failed to store the settings: %w", err)
	}

	*s = stored

	return nil
}

// Reset puts every setting back to its default and reports what was stored
// before, so the caller can say what it changed. It is the way out of a stored
// set that keeps the process from starting, which is why it reads the row
// without validating it.
func Reset(db *gorm.DB) (before *Settings, after *Settings, err error) {
	before, err = read(db)
	if err != nil {
		return nil, nil, err
	}

	defaults := Defaults()

	err = Save(db, &defaults)
	if err != nil {
		return nil, nil, err
	}

	return before, &defaults, nil
}

// Change is one setting that differs between two sets, named as the
// configuration file used to spell it.
type Change struct {
	Name string
	From string
	To   string
}

// value is one setting as a name and the text of its value. Everything that
// walks the settings one by one goes through this list, so a setting that is
// added is named in one place.
type value struct {
	name string
	text string
}

func values(s *Settings) []value {
	return []value{
		{"api.port", strconv.Itoa(s.APIPort)},
		{"api.https_enabled", strconv.FormatBool(s.APIHTTPSEnabled)},
		{"monitoring.interval_sec", strconv.Itoa(s.MonitoringIntervalSec)},
		{"reconcile.interval_sec", strconv.Itoa(s.ReconcileIntervalSec)},
		{"security.key_file", s.SecurityKeyFile},
		{"logging.level", s.LoggingLevel},
		{"logging.format", s.LoggingFormat},
		{"logging.file.path", s.LoggingFilePath},
		{"logging.file.max_size", strconv.Itoa(s.LoggingFileMaxSize)},
		{"logging.file.max_backups", strconv.Itoa(s.LoggingFileMaxBackups)},
		{"logging.file.max_age", strconv.Itoa(s.LoggingFileMaxAge)},
		{"logging.file.compress", strconv.FormatBool(s.LoggingFileCompress)},
	}
}

// Diff returns the settings that differ between the two sets. A nil before
// stands for a database that held no settings at all, and every setting is
// reported as new.
func Diff(before *Settings, after *Settings) []Change {
	newValues := values(after)

	if before == nil {
		changes := make([]Change, 0, len(newValues))
		for _, v := range newValues {
			changes = append(changes, Change{Name: v.name, From: "", To: v.text})
		}
		return changes
	}

	oldValues := values(before)

	var changes []Change

	for i, v := range newValues {
		if oldValues[i].text == v.text {
			continue
		}
		changes = append(changes, Change{Name: v.name, From: oldValues[i].text, To: v.text})
	}

	return changes
}
