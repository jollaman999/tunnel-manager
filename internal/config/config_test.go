package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseConfig is a whole configuration file. The path of the database file is
// the only setting the file carries: everything else is stored in that database
// and changed on the Settings screen.
const baseConfig = `database:
  path: /var/lib/tunnel-manager/tunnel-manager.db
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestValidateRequiresTheDatabasePath(t *testing.T) {
	var c Config

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a configuration with no database path")
	} else if !strings.Contains(err.Error(), "database path") {
		t.Errorf("error = %v, want it to name the database path", err)
	}

	c.Database.Path = "/var/lib/tunnel-manager/tunnel-manager.db"

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid config: %v", err)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "no-such-config.yaml"))
	if err == nil {
		t.Fatal("LoadConfig accepted a missing file")
	}
}

// TestLoadConfigKeepsTheDatabasePath holds that the configured path is the one
// that comes out, since it is the single setting the file carries.
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
			body: "database:\n  host: 127.0.0.1\n  path: /tmp/x.db\n",
			want: []string{"database.host"},
		},
		{
			name: "the whole MySQL section",
			body: "database:\n  host: 127.0.0.1\n  port: 3307\n  user: tunnel-manager\n  password: placeholder-value\n" +
				"  name: tunnel-manager\n  timeout_sec: 30\n",
			want: []string{"database.host", "database.port", "database.user", "database.password",
				"database.name", "database.timeout_sec"},
		},
		{
			name: "timeout alone",
			body: "database:\n  path: /tmp/x.db\n  timeout_sec: 30\n",
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

// TestLoadConfigRefusesTheSettingsThatMovedToTheDatabase is the same guard for
// the sections that became settings rows. Ignoring them would leave the
// operator editing a file nothing reads while the process runs on the stored
// values, so the startup stops and names every section that is still there.
func TestLoadConfigRefusesTheSettingsThatMovedToTheDatabase(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "api alone",
			body: baseConfig + "\napi:\n  port: 8888\n",
			want: []string{"api"},
		},
		{
			name: "logging alone",
			body: baseConfig + "\nlogging:\n  level: info\n  format: json\n",
			want: []string{"logging"},
		},
		{
			name: "every section that moved",
			body: baseConfig + "\napi:\n  port: 8888\n\nmonitoring:\n  interval_sec: 5\n\n" +
				"reconcile:\n  interval_sec: 5\n\nsecurity:\n  key_file: keys/tunnel-manager.key\n\n" +
				"logging:\n  level: info\n",
			want: []string{"api", "monitoring", "reconcile", "security", "logging"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("LoadConfig accepted a file that still carries the settings: %+v", cfg)
			}
			for _, section := range tc.want {
				if !strings.Contains(err.Error(), section) {
					t.Errorf("error = %v, want it to name %s", err, section)
				}
			}
			if !strings.Contains(err.Error(), "Settings screen") {
				t.Errorf("error = %v, want it to say where the settings are changed now", err)
			}
			if !strings.Contains(err.Error(), "-reset-settings") {
				t.Errorf("error = %v, want it to name the flag that puts the defaults back", err)
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

	path := writeConfig(t, "# the database path is left out on purpose\n")

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

	path := writeConfig(t, "# the database path is left out on purpose\n")

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
