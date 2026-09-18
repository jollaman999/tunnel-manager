package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
)

// Config is the whole of the configuration file. Everything but the path of
// the database file is stored in that database and changed on the Settings
// screen, so this is the one setting the file still carries: the process has to
// know where the database is before it can read anything out of it, and that
// is the only thing it cannot look up there.
type Config struct {
	// Database is a file now, so a path is all it takes. The settings the
	// MySQL setup carried are gone, and a file that still holds them is
	// refused rather than loaded with them ignored.
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
}

// removedDatabaseKeys are the settings the MySQL setup carried. They are kept
// here so that a configuration file left over from it can be named key by key.
var removedDatabaseKeys = []string{"host", "port", "user", "password", "name", "timeout_sec"}

// databaseFileName is where the database goes under the directory the platform
// keeps user data in.
const databaseFileName = "tunnel-manager.db"

// defaultDatabasePath returns the file to use when the configuration does not
// name one. It is worked out from os.UserConfigDir rather than from the working
// directory, which differs between running from the repository, from the
// container and from systemd, and would put a fresh empty database wherever the
// process happened to be started.
//
// A missing HOME is reported rather than worked around. Inventing a location
// would let one startup build a database in one place and the next one build
// another somewhere else, and the Hosts that were registered would look gone.
func defaultDatabasePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no default location for the database file is available: %w. "+
			"Set database.path in the configuration file to an absolute path", err)
	}

	return filepath.Join(dir, "tunnel-manager", databaseFileName), nil
}

// checkRemovedDatabaseKeys refuses a configuration file that still carries the
// MySQL settings. yaml.v2 drops a key that matches no field without a word, so
// such a file would otherwise load, fall back to the default path and build an
// empty database while the operator goes on believing the MySQL server is being
// read. The file is read a second time into a MapSlice because that keeps the
// keys that are written down apart from the ones that merely came out zero.
func checkRemovedDatabaseKeys(data []byte) error {
	var raw struct {
		Database yaml.MapSlice `yaml:"database"`
	}

	err := yaml.Unmarshal(data, &raw)
	if err != nil {
		// The load already reported the parse failure against the real struct.
		return nil
	}

	var found []string

	for _, item := range raw.Database {
		name := fmt.Sprint(item.Key)
		for _, removed := range removedDatabaseKeys {
			if name == removed {
				found = append(found, "database."+name)
			}
		}
	}

	if len(found) == 0 {
		return nil
	}

	return fmt.Errorf("the configuration file still carries the MySQL settings %s. "+
		"tunnel-manager stores its data in a SQLite file now: remove them and write "+
		"database.path with the path of that file, or leave database out to use the default",
		strings.Join(found, ", "))
}

// movedSections are the configuration sections that became rows of the settings
// table. They are kept here so that a file left over from the time they lived
// in it can be named section by section.
var movedSections = []string{"api", "monitoring", "reconcile", "security", "logging"}

// checkMovedSections refuses a configuration file that still carries the
// settings which moved into the database. It is the same read as
// checkRemovedDatabaseKeys and exists for the same reason: yaml.v2 drops a key
// that matches no field without a word, so such a file would load and the
// process would run on the stored settings while the operator edits a file that
// nothing reads and wonders why the port never changes.
func checkMovedSections(data []byte) error {
	var raw yaml.MapSlice

	err := yaml.Unmarshal(data, &raw)
	if err != nil {
		// The load already reported the parse failure against the real struct.
		return nil
	}

	var found []string

	for _, item := range raw {
		name := fmt.Sprint(item.Key)
		for _, moved := range movedSections {
			if name == moved {
				found = append(found, name)
			}
		}
	}

	if len(found) == 0 {
		return nil
	}

	return fmt.Errorf("the configuration file still carries the settings sections %s. "+
		"tunnel-manager keeps those settings in the database now and they are changed on the "+
		"Settings screen of the web UI: remove these sections, which leaves database.path as the "+
		"only setting the file carries. Start the binary with -reset-settings to put every stored "+
		"setting back to its default",
		strings.Join(found, ", "))
}

func (c *Config) Validate() error {
	if c.Database.Path == "" {
		return fmt.Errorf("database path is required")
	}

	return nil
}

func (c *Config) setDefaults() error {
	if c.Database.Path == "" {
		path, err := defaultDatabasePath()
		if err != nil {
			return err
		}
		c.Database.Path = path
	}

	return nil
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %w", err)
	}

	if err := checkRemovedDatabaseKeys(data); err != nil {
		return nil, err
	}

	if err := checkMovedSections(data); err != nil {
		return nil, err
	}

	if err := config.setDefaults(); err != nil {
		return nil, err
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}
