package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
)

type Config struct {
	// Database is a file now, so a path is all it takes. The settings the
	// MySQL setup carried are gone, and a file that still holds them is
	// refused rather than loaded with them ignored.
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	API struct {
		Port int `yaml:"port"`
	} `yaml:"api"`

	Monitoring struct {
		IntervalSec int `yaml:"interval_sec"`
	} `yaml:"monitoring"`

	// Reconcile is how often the tunnels that should be up are compared with
	// the ones that are up. Monitoring.IntervalSec checks whether a tunnel
	// that is already up is still alive, which is a different job, so the two
	// stay separate even while they share a default.
	Reconcile struct {
		IntervalSec int `yaml:"interval_sec"`
	} `yaml:"reconcile"`

	Security struct {
		KeyFile string `yaml:"key_file"`
	} `yaml:"security"`

	Logging struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
		File   struct {
			Path       string `yaml:"path"`
			MaxSize    int    `yaml:"max_size"`
			MaxBackups int    `yaml:"max_backups"`
			MaxAge     int    `yaml:"max_age"`
			Compress   bool   `yaml:"compress"`
		} `yaml:"file"`
	} `yaml:"logging"`
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

func (c *Config) Validate() error {
	if c.Database.Path == "" {
		return fmt.Errorf("database path is required")
	}

	if c.API.Port < 1 || c.API.Port > 65535 {
		return fmt.Errorf("invalid API port: %d", c.API.Port)
	}

	if c.Monitoring.IntervalSec <= 0 {
		return fmt.Errorf("invalid monitoring interval: %d", c.Monitoring.IntervalSec)
	}

	if c.Reconcile.IntervalSec <= 0 {
		return fmt.Errorf("invalid reconcile interval: %d", c.Reconcile.IntervalSec)
	}

	validLevels := map[string]bool{
		"debug":  true,
		"info":   true,
		"warn":   true,
		"error":  true,
		"dpanic": true,
		"panic":  true,
		"fatal":  true,
	}
	if !validLevels[c.Logging.Level] {
		return fmt.Errorf("invalid log level: %s", c.Logging.Level)
	}

	validFormats := map[string]bool{
		"json":    true,
		"console": true,
	}
	if !validFormats[c.Logging.Format] {
		return fmt.Errorf("invalid log format: %s", c.Logging.Format)
	}

	if c.Logging.File.MaxSize < 0 {
		return fmt.Errorf("invalid log max size: %d", c.Logging.File.MaxSize)
	}
	if c.Logging.File.MaxBackups < 0 {
		return fmt.Errorf("invalid log max backups: %d", c.Logging.File.MaxBackups)
	}
	if c.Logging.File.MaxAge < 0 {
		return fmt.Errorf("invalid log max age: %d", c.Logging.File.MaxAge)
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
	if c.Reconcile.IntervalSec == 0 {
		c.Reconcile.IntervalSec = 5
	}
	if c.Security.KeyFile == "" {
		c.Security.KeyFile = "keys/tunnel-manager.key"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Logging.File.Path == "" {
		c.Logging.File.Path = "logs/tunnel-manager.log"
	}
	if c.Logging.File.MaxSize <= 0 {
		c.Logging.File.MaxSize = 100
	}
	if c.Logging.File.MaxBackups <= 0 {
		c.Logging.File.MaxBackups = 5
	}
	if c.Logging.File.MaxAge <= 0 {
		c.Logging.File.MaxAge = 30
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

	if err := config.setDefaults(); err != nil {
		return nil, err
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}
