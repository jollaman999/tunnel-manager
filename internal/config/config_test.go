package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseConfig = `database:
  path: /var/lib/tunnel-manager/tunnel-manager.db

api:
  port: 8888

monitoring:
  interval_sec: 5
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func validConfig() *Config {
	var c Config
	c.Database.Path = "/var/lib/tunnel-manager/tunnel-manager.db"
	c.API.Port = 8888
	c.Monitoring.IntervalSec = 5
	c.Reconcile.IntervalSec = 5
	c.Logging.Level = "info"
	c.Logging.Format = "json"
	return &c
}

func TestLoadConfigDefaultsReconcileInterval(t *testing.T) {
	path := writeConfig(t, baseConfig)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Reconcile.IntervalSec != 5 {
		t.Errorf("reconcile interval = %d, want 5", cfg.Reconcile.IntervalSec)
	}
}

func TestLoadConfigKeepsReconcileInterval(t *testing.T) {
	path := writeConfig(t, baseConfig+`
reconcile:
  interval_sec: 30
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Reconcile.IntervalSec != 30 {
		t.Errorf("reconcile interval = %d, want 30", cfg.Reconcile.IntervalSec)
	}
}

func TestLoadConfigKeepsReconcileAndMonitoringApart(t *testing.T) {
	path := writeConfig(t, `database:
  path: /var/lib/tunnel-manager/tunnel-manager.db

api:
  port: 8888

monitoring:
  interval_sec: 7

reconcile:
  interval_sec: 30
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Monitoring.IntervalSec != 7 {
		t.Errorf("monitoring interval = %d, want 7", cfg.Monitoring.IntervalSec)
	}
	if cfg.Reconcile.IntervalSec != 30 {
		t.Errorf("reconcile interval = %d, want 30", cfg.Reconcile.IntervalSec)
	}
}

func TestLoadConfigDefaultsOtherFields(t *testing.T) {
	path := writeConfig(t, baseConfig)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Monitoring.IntervalSec != 5 {
		t.Errorf("monitoring interval = %d, want 5", cfg.Monitoring.IntervalSec)
	}
	if cfg.Security.KeyFile != "keys/tunnel-manager.key" {
		t.Errorf("key file = %q, want keys/tunnel-manager.key", cfg.Security.KeyFile)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("log level = %q, want info", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("log format = %q, want json", cfg.Logging.Format)
	}
	if cfg.Logging.File.Path != "logs/tunnel-manager.log" {
		t.Errorf("log path = %q, want logs/tunnel-manager.log", cfg.Logging.File.Path)
	}
	if cfg.Logging.File.MaxSize != 100 {
		t.Errorf("log max size = %d, want 100", cfg.Logging.File.MaxSize)
	}
	if cfg.Logging.File.MaxBackups != 5 {
		t.Errorf("log max backups = %d, want 5", cfg.Logging.File.MaxBackups)
	}
	if cfg.Logging.File.MaxAge != 30 {
		t.Errorf("log max age = %d, want 30", cfg.Logging.File.MaxAge)
	}
}

func TestLoadConfigRejectsNegativeReconcileInterval(t *testing.T) {
	path := writeConfig(t, baseConfig+`
reconcile:
  interval_sec: -1
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted a negative reconcile interval")
	}
	if !strings.Contains(err.Error(), "reconcile interval") {
		t.Errorf("error = %v, want it to name the reconcile interval", err)
	}
}

func TestValidateReconcileInterval(t *testing.T) {
	cases := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{name: "zero", value: 0, wantErr: true},
		{name: "negative", value: -1, wantErr: true},
		{name: "positive", value: 30, wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Reconcile.IntervalSec = tc.value

			err := c.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate accepted reconcile interval %d", tc.value)
				}
				if !strings.Contains(err.Error(), "reconcile interval") {
					t.Errorf("error = %v, want it to name the reconcile interval", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if c.Reconcile.IntervalSec != tc.value {
				t.Errorf("reconcile interval = %d, want %d", c.Reconcile.IntervalSec, tc.value)
			}
		})
	}
}

func TestValidateExistingFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "empty database path", mutate: func(c *Config) { c.Database.Path = "" }, want: "database path"},
		{name: "api port out of range", mutate: func(c *Config) { c.API.Port = 0 }, want: "API port"},
		{name: "zero monitoring interval", mutate: func(c *Config) { c.Monitoring.IntervalSec = 0 }, want: "monitoring interval"},
		{name: "negative monitoring interval", mutate: func(c *Config) { c.Monitoring.IntervalSec = -1 }, want: "monitoring interval"},
		{name: "unknown log level", mutate: func(c *Config) { c.Logging.Level = "verbose" }, want: "log level"},
		{name: "unknown log format", mutate: func(c *Config) { c.Logging.Format = "text" }, want: "log format"},
		{name: "negative log max size", mutate: func(c *Config) { c.Logging.File.MaxSize = -1 }, want: "log max size"},
		{name: "negative log max backups", mutate: func(c *Config) { c.Logging.File.MaxBackups = -1 }, want: "log max backups"},
		{name: "negative log max age", mutate: func(c *Config) { c.Logging.File.MaxAge = -1 }, want: "log max age"},
	}

	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate rejected a valid config: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "no-such-config.yaml"))
	if err == nil {
		t.Fatal("LoadConfig accepted a missing file")
	}
}

// TestLoadConfigKeepsTheDatabasePath holds that the configured path is the one
// that comes out, since it is the single setting the database section carries.
func TestLoadConfigKeepsTheDatabasePath(t *testing.T) {
	path := writeConfig(t, baseConfig)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Database.Path != "/var/lib/tunnel-manager/tunnel-manager.db" {
		t.Errorf("database path = %q, want /var/lib/tunnel-manager/tunnel-manager.db", cfg.Database.Path)
	}
}

// TestLoadConfigRefusesTheMySQLSettings is the reason the second read of the
// file exists. yaml.v2 drops a key that matches no field without a word, so a
// configuration left over from the MySQL setup would load, fall back to the
// default path and build an empty database while the operator goes on believing
// the MySQL server is being read. Every key that is still there has to be named,
// because naming one would send the operator back for the next one.
func TestLoadConfigRefusesTheMySQLSettings(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "host alone",
			body: "database:\n  host: 127.0.0.1\n  path: /tmp/x.db\n\napi:\n  port: 8888\n\nmonitoring:\n  interval_sec: 5\n",
			want: []string{"database.host"},
		},
		{
			name: "the whole MySQL section",
			body: "database:\n  host: 127.0.0.1\n  port: 3307\n  user: tunnel-manager\n  password: placeholder-value\n" +
				"  name: tunnel-manager\n  timeout_sec: 30\n\napi:\n  port: 8888\n\nmonitoring:\n  interval_sec: 5\n",
			want: []string{"database.host", "database.port", "database.user", "database.password",
				"database.name", "database.timeout_sec"},
		},
		{
			name: "timeout alone",
			body: "database:\n  path: /tmp/x.db\n  timeout_sec: 30\n\napi:\n  port: 8888\n\nmonitoring:\n  interval_sec: 5\n",
			want: []string{"database.timeout_sec"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("LoadConfig accepted a MySQL configuration: %+v", cfg)
			}
			for _, key := range tc.want {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error = %v, want it to name %s", err, key)
				}
			}
			if !strings.Contains(err.Error(), "database.path") {
				t.Errorf("error = %v, want it to say what to write instead", err)
			}
		})
	}
}

// TestLoadConfigDefaultsTheDatabasePathToTheUserConfigDir covers the case the
// binary is downloaded and run with no path configured: the file lands where
// the platform keeps user data, not in whatever directory the process was
// started from.
func TestLoadConfigDefaultsTheDatabasePathToTheUserConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path := writeConfig(t, "api:\n  port: 8888\n\nmonitoring:\n  interval_sec: 5\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}

	want := filepath.Join(dir, "tunnel-manager", "tunnel-manager.db")
	if cfg.Database.Path != want {
		t.Errorf("database path = %q, want %q", cfg.Database.Path, want)
	}
}

// TestLoadConfigRefusesToGuessWithoutAHome is the other half of the default.
// With neither XDG_CONFIG_HOME nor HOME there is no place the platform calls
// its own, and inventing one would let one startup build a database in one
// directory and the next one build another somewhere else, so the Hosts that
// were registered would look gone.
func TestLoadConfigRefusesToGuessWithoutAHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	path := writeConfig(t, "api:\n  port: 8888\n\nmonitoring:\n  interval_sec: 5\n")

	cfg, err := LoadConfig(path)
	if err == nil {
		t.Fatalf("LoadConfig made up a database path: %+v", cfg)
	}
	if !strings.Contains(err.Error(), "database.path") {
		t.Errorf("error = %v, want it to ask for database.path", err)
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("error = %v, want it to ask for an absolute path", err)
	}
}
