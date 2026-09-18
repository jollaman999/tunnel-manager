package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/config"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

// newLoggingConfig returns a configuration whose logging section is filled in
// and whose log file is the given path. Everything else is left at zero,
// because the startup steps exercised here read nothing else.
func newLoggingConfig(path string) *config.Config {
	cfg := &config.Config{}
	cfg.Logging.Level = "info"
	cfg.Logging.Format = "json"
	cfg.Logging.File.Path = path
	cfg.Logging.File.MaxSize = 1
	cfg.Logging.File.MaxBackups = 1
	cfg.Logging.File.MaxAge = 1

	return cfg
}

// openPathsOfThisProcess returns the file every descriptor of this process
// points at, so a descriptor that was left behind can be told from one that
// was closed.
func openPathsOfThisProcess(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("the open descriptors of this process cannot be read: %v", err)
	}

	var paths []string

	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			// A descriptor closed between the listing and the readlink, which
			// says nothing about the one under test.
			continue
		}
		paths = append(paths, target)
	}

	return paths
}

func TestPrepareLogFileCreatesTheDirectoryAndTheFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	err := prepareLogFile(newLoggingConfig(logFile))
	if err != nil {
		t.Fatalf("failed to prepare a log file under a directory that can be created: %v", err)
	}

	info, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("the log file was not created: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("the log path was created as a directory")
	}
}

// TestPrepareLogFileLeavesNoDescriptorOpenOnTheLogFile records what the
// function does today: the descriptor it opens to see that the file can be
// written to is closed again before it returns. The logs themselves go through
// lumberjack, which opens the file on its own, so a descriptor kept here would
// be held for the life of the process for nothing.
func TestPrepareLogFileLeavesNoDescriptorOpenOnTheLogFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	before := len(openPathsOfThisProcess(t))

	err := prepareLogFile(newLoggingConfig(logFile))
	if err != nil {
		t.Fatalf("failed to prepare the log file: %v", err)
	}

	for _, path := range openPathsOfThisProcess(t) {
		if path == logFile {
			t.Fatalf("a descriptor on %s was left open after the log file was prepared", logFile)
		}
	}

	after := len(openPathsOfThisProcess(t))
	if after > before {
		t.Fatalf("%d descriptors were open before the log file was prepared and %d after", before, after)
	}
}

func TestPrepareLogFileReportsADirectoryThatCannotBeCreated(t *testing.T) {
	// The parent of the log directory is a regular file, so the directory
	// cannot be created whatever the privileges of the process are.
	blocking := filepath.Join(t.TempDir(), "not-a-directory")

	err := os.WriteFile(blocking, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("failed to write the blocking file: %v", err)
	}

	err = prepareLogFile(newLoggingConfig(filepath.Join(blocking, "logs", "tunnel-manager.log")))
	if err == nil {
		t.Fatal("a log directory that cannot be created was reported as prepared")
	}
	if !strings.Contains(err.Error(), "failed to create log directory") {
		t.Fatalf("the failure does not name the log directory: %v", err)
	}
}

func TestPrepareLogFileReportsALogFileThatCannotBeOpened(t *testing.T) {
	// The log path itself is a directory, so opening it for writing fails
	// whatever the privileges of the process are.
	logFile := filepath.Join(t.TempDir(), "tunnel-manager.log")

	err := os.Mkdir(logFile, 0755)
	if err != nil {
		t.Fatalf("failed to create the directory that takes the place of the log file: %v", err)
	}

	err = prepareLogFile(newLoggingConfig(logFile))
	if err == nil {
		t.Fatal("a log file that cannot be opened was reported as prepared")
	}
	if !strings.Contains(err.Error(), "failed to create log file") {
		t.Fatalf("the failure does not name the log file: %v", err)
	}
}

// withStdoutCaptured runs the function with os.Stdout replaced by a pipe and
// returns what was written to it. The logger takes hold of os.Stdout when it is
// built, so the replacement has to be in place before initLogger is called.
func withStdoutCaptured(t *testing.T, run func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create the pipe that stands in for stdout: %v", err)
	}

	saved := os.Stdout
	os.Stdout = write

	collected := make(chan string, 1)

	go func() {
		body, _ := io.ReadAll(read)
		collected <- string(body)
	}()

	func() {
		defer func() {
			os.Stdout = saved
			_ = write.Close()
		}()
		run()
	}()

	output := <-collected
	_ = read.Close()

	return output
}

// TestInitLoggerFallsBackToStdoutWhenTheLogFileCannotBeOpened pins the fallback
// that keeps the logs somewhere when the log file is unusable. Without it the
// startup either has no logger at all or writes nowhere, and an operator
// looking into why the process misbehaves finds nothing.
func TestInitLoggerFallsBackToStdoutWhenTheLogFileCannotBeOpened(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "tunnel-manager.log")

	err := os.Mkdir(logFile, 0755)
	if err != nil {
		t.Fatalf("failed to create the directory that takes the place of the log file: %v", err)
	}

	cfg := newLoggingConfig(logFile)

	var logger *zap.Logger

	output := withStdoutCaptured(t, func() {
		logger, err = initLogger(cfg)
		if err != nil || logger == nil {
			return
		}
		logger.Info("a line that has nowhere else to go")
	})

	if err != nil {
		t.Fatalf("a log file that cannot be opened ended the startup: %v", err)
	}
	if logger == nil {
		t.Fatal("no logger was returned although no failure was reported")
	}

	if !strings.Contains(output, "a line that has nowhere else to go") {
		t.Fatalf("the entry did not reach stdout while the log file was unusable, stdout: %q", output)
	}
	if !strings.Contains(output, "logging to file is disabled") {
		t.Fatalf("stdout does not say that logging to file is disabled, stdout: %q", output)
	}
}

func TestInitLoggerWritesToTheLogFileWhenItCanBeOpened(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	logger, err := initLogger(newLoggingConfig(logFile))
	if err != nil {
		t.Fatalf("failed to build the logger: %v", err)
	}

	logger.Info("a line that belongs in the log file")

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}
	if !strings.Contains(string(body), "a line that belongs in the log file") {
		t.Fatalf("the entry did not reach the log file, its content is %q", string(body))
	}

	// The format is the configured one, so the file can be read by whatever
	// collects it.
	var entry map[string]interface{}

	firstLine := strings.SplitN(strings.TrimSpace(string(body)), "\n", 2)[0]
	err = json.Unmarshal([]byte(firstLine), &entry)
	if err != nil {
		t.Fatalf("the log file is not written in the configured json format: %v, line: %q", err, firstLine)
	}
}

func TestInitLoggerWritesNothingBelowTheConfiguredLevel(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	cfg := newLoggingConfig(logFile)
	cfg.Logging.Level = "warn"

	logger, err := initLogger(cfg)
	if err != nil {
		t.Fatalf("failed to build the logger: %v", err)
	}

	logger.Info("an entry below the configured level")
	logger.Warn("an entry at the configured level")

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}

	if strings.Contains(string(body), "an entry below the configured level") {
		t.Fatalf("an entry below the configured level was written, content: %q", string(body))
	}
	if !strings.Contains(string(body), "an entry at the configured level") {
		t.Fatalf("an entry at the configured level was not written, content: %q", string(body))
	}
}

func TestInitLoggerRefusesALevelItCannotRead(t *testing.T) {
	cfg := newLoggingConfig(filepath.Join(t.TempDir(), "logs", "tunnel-manager.log"))
	cfg.Logging.Level = "chatty"

	logger, err := initLogger(cfg)
	if err == nil {
		t.Fatal("a log level that cannot be read was accepted")
	}
	if logger != nil {
		t.Fatal("a logger was returned for a log level that cannot be read")
	}
	if !strings.Contains(err.Error(), "failed to parse log level") {
		t.Fatalf("the failure does not name the log level: %v", err)
	}
}

// The encryption key check reads the Hosts through gorm, which is served here
// by a fake driver that answers every query with the stored passwords. Only the
// id and the password are handed back, because that is all the check reads.

type hostRows struct {
	hosts []models.Host
	next  int
}

func (r *hostRows) Columns() []string { return []string{"id", "password"} }

func (r *hostRows) Close() error { return nil }

func (r *hostRows) Next(dest []driver.Value) error {
	if r.next >= len(r.hosts) {
		return io.EOF
	}

	host := r.hosts[r.next]
	r.next++

	dest[0] = int64(host.ID)
	dest[1] = host.Password

	return nil
}

type hostConn struct {
	hosts   []models.Host
	readErr error
}

func (c *hostConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("the fake driver does not prepare statements")
}

func (c *hostConn) Close() error { return nil }

func (c *hostConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

func (c *hostConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return fakeResult{}, nil
}

func (c *hostConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query == versionQuery {
		return &fakeRows{columns: []string{"version"}, values: []driver.Value{sqliteVersion}}, nil
	}
	if c.readErr != nil {
		return nil, c.readErr
	}
	return &hostRows{hosts: c.hosts}, nil
}

type hostConnector struct {
	hosts   []models.Host
	readErr error
}

func (c *hostConnector) Connect(context.Context) (driver.Conn, error) {
	return &hostConn{hosts: c.hosts, readErr: c.readErr}, nil
}

func (c *hostConnector) Driver() driver.Driver { return fakeDriver{} }

// newHostDB returns a gorm handle whose Host table holds the given rows, or
// fails to be read when readErr is set.
func newHostDB(t *testing.T, hosts []models.Host, readErr error) *gorm.DB {
	t.Helper()

	sqlDB := sql.OpenDB(&hostConnector{hosts: hosts, readErr: readErr})

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	db, err := gorm.Open(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("failed to open the database handle: %v", err)
	}

	return db
}

// runCheckEncryptionKey runs the check and reports whether it stopped the
// startup, along with everything it logged. logger.Fatal ends the process, so
// the logger is built with a hook that panics instead, which is what the
// recover below catches.
func runCheckEncryptionKey(t *testing.T, db *gorm.DB, cipher *crypto.Cipher, keyFile string) (bool, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.WithFatalHook(zapcore.WriteThenPanic))

	stopped := false

	func() {
		defer func() {
			if recover() != nil {
				stopped = true
			}
		}()
		checkEncryptionKey(db, cipher, logger, keyFile)
	}()

	return stopped, logs
}

// loggedAtLeast reports whether an entry of that level carries the text.
func loggedAtLeast(logs *observer.ObservedLogs, level zapcore.Level, text string) bool {
	for _, entry := range logs.All() {
		if entry.Level == level && strings.Contains(entry.Message, text) {
			return true
		}
	}
	return false
}

// TestCheckEncryptionKeyStopsTheStartupWhenAMarkedPasswordDoesNotOpen pins the
// case the check exists for. A process holds one key, so a key that opens none
// of the sealed passwords builds no tunnel at all, and serving an API that
// looks healthy over it hides the reason.
func TestCheckEncryptionKeyStopsTheStartupWhenAMarkedPasswordDoesNotOpen(t *testing.T) {
	stored := newTestCipher(t)
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: encryptedWith(t, stored, "test-password")},
		{ID: 2, Password: encryptedWith(t, stored, "test-password-2")},
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if !stopped {
		t.Fatal("the startup went on although the key opens none of the sealed passwords")
	}
	if !loggedAtLeast(logs, zapcore.FatalLevel, "opens none of the stored passwords") {
		t.Fatalf("the reason the startup stopped was not logged, logged: %v", logs.AllUntimed())
	}
}

// TestAPlaintextPasswordThatLooksEncodedDoesNotBlockTheStartup guards the
// regression eaafaf9 fixed. A password stored before passwords were encrypted
// carries no marker, and one that happens to be spelled in base64 characters
// and is long enough is reported as one that does not open. Treating that as
// proof of a wrong key kept a deployment that holds nothing but plaintext from
// starting, with no API left to set the passwords again.
func TestAPlaintextPasswordThatLooksEncodedDoesNotBlockTheStartup(t *testing.T) {
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: "test-password"},                            // hook:allow
		{ID: 2, Password: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0"}, // hook:allow
		{ID: 3, Password: "test-password-3"},                          // hook:allow
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped although no stored password carries the marker, logged: %v",
			logs.AllUntimed())
	}

	// The row is still reported, because no tunnel is built for it until its
	// password is set again.
	if !loggedAtLeast(logs, zapcore.WarnLevel, "does not open with the encryption key in use") {
		t.Fatalf("the Host whose password does not open was not reported, logged: %v", logs.AllUntimed())
	}
}

// TestCheckEncryptionKeyGoesOnWhenOneStoredPasswordOpens pins that a single row
// that does not open is a fault of that row. The other Hosts are served, and
// the one that is left behind is named so that its password can be set again.
func TestCheckEncryptionKeyGoesOnWhenOneStoredPasswordOpens(t *testing.T) {
	inUse := newTestCipher(t)
	other := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: encryptedWith(t, inUse, "test-password")},
		{ID: 2, Password: encryptedWith(t, other, "test-password-2")},
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped although a stored password opens, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.WarnLevel, "does not open with the encryption key in use") {
		t.Fatalf("the Host whose password does not open was not reported, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.InfoLevel, "the encryption key opens the stored passwords") {
		t.Fatalf("the key that opens a stored password was not reported as such, logged: %v", logs.AllUntimed())
	}

	for _, entry := range logs.All() {
		for key, value := range entry.ContextMap() {
			if key == "host_ids_that_do_not_open" && fmt.Sprintf("%v", value) != "[2]" {
				t.Fatalf("the Hosts that do not open were logged as %v, want [2]", value)
			}
		}
	}
}

func TestCheckEncryptionKeyHasNothingToCheckWhenNoPasswordIsEncrypted(t *testing.T) {
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: "test-password"},   // hook:allow
		{ID: 2, Password: "test-password-2"}, // hook:allow
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped although no stored password is encrypted, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.InfoLevel, "no stored password is encrypted") {
		t.Fatalf("a deployment that holds no encrypted password was not reported as such, logged: %v",
			logs.AllUntimed())
	}
}

// TestCheckEncryptionKeyGoesOnWhenTheHostsCannotBeRead pins that a read which
// fails is not taken as a wrong key. The read says nothing about the key, and
// stopping here would turn a database hiccup into a startup that never comes up.
func TestCheckEncryptionKeyGoesOnWhenTheHostsCannotBeRead(t *testing.T) {
	inUse := newTestCipher(t)
	db := newHostDB(t, nil, fmt.Errorf("the Host table cannot be read"))

	stopped, logs := runCheckEncryptionKey(t, db, inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped over a read that failed, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.WarnLevel, "failed to read the Hosts") {
		t.Fatalf("the read that failed was not reported, logged: %v", logs.AllUntimed())
	}
}

func TestTheValidatorAcceptsARequestThatIsFilledIn(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	err := cv.Validate(&models.CreateHostRequest{
		IP:       "192.0.2.10",
		Port:     22,
		User:     "tester",
		Password: "test-password", // hook:allow
	})
	if err != nil {
		t.Fatalf("a request that is filled in was refused: %v", err)
	}
}

// TestTheValidatorNamesTheFieldAsTheApiSpellsIt pins both halves of the
// validator: a request that breaks the rules is refused, and the field is named
// by its json name, which is the only name the caller ever sent.
func TestTheValidatorNamesTheFieldAsTheApiSpellsIt(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	err := cv.Validate(&models.CreateServicePortRequest{
		ServiceIP:   "not-an-address",
		ServicePort: 22,
		LocalPort:   70000,
	})
	if err == nil {
		t.Fatal("a request with an address that is not one and a port out of range was accepted")
	}

	if !strings.Contains(err.Error(), "service_ip") {
		t.Fatalf("the field is not named as the api spells it: %v", err)
	}
	if strings.Contains(err.Error(), "ServiceIP") {
		t.Fatalf("the field is named by its Go name, which no caller ever sent: %v", err)
	}
	if !strings.Contains(err.Error(), "local_port") {
		t.Fatalf("the port out of range was not reported: %v", err)
	}
}

// TestTheValidatorFallsBackToTheGoNameWhenThereIsNoJsonName pins that a field
// which is kept out of the json body is still named in the failure. A field
// reported as an empty name would leave the caller with a rule broken and no
// field to look at.
func TestTheValidatorFallsBackToTheGoNameWhenThereIsNoJsonName(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	request := struct {
		Hidden  string `json:"-" validate:"required"`
		Unnamed string `validate:"required"`
	}{}

	err := cv.Validate(&request)
	if err == nil {
		t.Fatal("a request with two empty required fields was accepted")
	}
	if !strings.Contains(err.Error(), "Hidden") {
		t.Fatalf("the field that carries no json name was not named: %v", err)
	}
	if !strings.Contains(err.Error(), "Unnamed") {
		t.Fatalf("the field that carries no json tag at all was not named: %v", err)
	}
}
