package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// newAccountDB returns a gorm handle over the fake driver, whose account table
// holds rowCount rows, and the recorder that holds the statements the account
// setup made.
func newAccountDB(t *testing.T, rowCount int64) (*gorm.DB, *statementRecorder) {
	t.Helper()

	sqlDB, recorder := newRecordingDB(t, rowCount)

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("failed to open the database handle: %v", err)
	}

	return db, recorder
}

// readInitialPassword returns what was written to the initial password file.
func readInitialPassword(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the initial password file: %v", err)
	}

	password := strings.TrimRight(string(body), "\n")
	if password == "" {
		t.Fatalf("the initial password file is empty")
	}

	return password
}

// TestEnsureUserKeepsTheInitialPasswordOutOfTheLog reads back everything that
// was logged, fields and all, and looks for what was written to the file. The
// log goes to the console as well as to the log file, so the value must not
// reach it, and the path must.
func TestEnsureUserKeepsTheInitialPasswordOutOfTheLog(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)

	db, _ := newAccountDB(t, 0)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	passwordFile := filepath.Join(filepath.Dir(configFile), "initial-password")

	ensureUser(db, zap.New(core), configFile)

	password := readInitialPassword(t, passwordFile)

	entries := logs.All()
	if len(entries) == 0 {
		t.Fatalf("nothing was logged")
	}

	pathWasLogged := false

	for _, entry := range entries {
		line := entry.Message
		for key, value := range entry.ContextMap() {
			line += " " + key + "=" + toText(value)
			if toText(value) == passwordFile {
				pathWasLogged = true
			}
		}

		if strings.Contains(line, password) {
			t.Fatalf("a log entry carries the initial password, message: %q", entry.Message)
		}
	}

	if !pathWasLogged {
		t.Fatalf("the path of the initial password file was not logged")
	}
}

// toText spells a logged field value.
func toText(value interface{}) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

// TestEnsureUserStoresAHashAndNotThePassword reads the insert the account setup
// made and checks that the initial password is not among its values.
func TestEnsureUserStoresAHashAndNotThePassword(t *testing.T) {
	db, recorder := newAccountDB(t, 0)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	passwordFile := filepath.Join(filepath.Dir(configFile), "initial-password")

	ensureUser(db, zap.NewNop(), configFile)

	password := readInitialPassword(t, passwordFile)

	var insert *recordedStatement

	for i, statement := range recorder.all() {
		if strings.HasPrefix(strings.ToUpper(statement.query), "INSERT") {
			insert = &recorder.all()[i]
		}
	}

	if insert == nil {
		t.Fatalf("no row was inserted")
	}

	if !strings.Contains(insert.query, "`user`") {
		t.Fatalf("the insert does not name the table `user`: %s", insert.query)
	}

	var hash string
	setupRequired := false

	for _, arg := range insert.args {
		switch value := arg.Value.(type) {
		case string:
			if value == password {
				t.Fatalf("the initial password was sent to the database as it is")
			}
			if strings.HasPrefix(value, "$2a$") {
				hash = value
			}
		case bool:
			setupRequired = value
		case int64:
			setupRequired = value == 1
		}
	}

	if hash == "" {
		t.Fatalf("the insert carries no bcrypt hash")
	}

	if !auth.CheckPassword(hash, password) {
		t.Fatalf("the stored hash does not verify against the initial password")
	}

	if !setupRequired {
		t.Fatalf("the account was not created with setup_required set")
	}
}

// TestEnsureUserLeavesAnAccountThatIsAlreadyThere pins that a second startup
// neither writes a row nor a password file, so the account that is in use is
// not replaced by one whose password the operator never saw.
func TestEnsureUserLeavesAnAccountThatIsAlreadyThere(t *testing.T) {
	db, recorder := newAccountDB(t, 1)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	passwordFile := filepath.Join(filepath.Dir(configFile), "initial-password")

	ensureUser(db, zap.NewNop(), configFile)

	for _, statement := range recorder.all() {
		if strings.HasPrefix(strings.ToUpper(statement.query), "INSERT") {
			t.Fatalf("a row was inserted while an account was already there: %s", statement.query)
		}
		if strings.HasPrefix(strings.ToUpper(statement.query), "UPDATE") {
			t.Fatalf("a row was updated while an account was already there: %s", statement.query)
		}
	}

	_, err := os.Stat(passwordFile)
	if err == nil {
		t.Fatalf("an initial password file was written while an account was already there")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("failed to check the initial password file: %v", err)
	}
}
