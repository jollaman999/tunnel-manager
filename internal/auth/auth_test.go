package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestInitialPasswordFileSitsNextToTheDatabaseFile(t *testing.T) {
	cases := []struct {
		databaseFile string
		want         string
	}{
		{"data/tunnel-manager.db", filepath.Join("data", "initial-password")},
		{"/var/lib/tunnel-manager/tunnel-manager.db", filepath.Join("/var/lib/tunnel-manager", "initial-password")},
		{"tunnel-manager.db", "initial-password"},
	}

	for _, c := range cases {
		got := InitialPasswordFile(c.databaseFile)
		if got != c.want {
			t.Fatalf("the initial password file of the database file %q is %q, want %q", c.databaseFile, got, c.want)
		}
	}
}

func TestGenerateInitialPasswordDoesNotRepeatItself(t *testing.T) {
	const runs = 100

	seen := make(map[string]bool, runs)

	for i := 0; i < runs; i++ {
		generated, err := GenerateInitialPassword()
		if err != nil {
			t.Fatalf("failed to generate: %v", err)
		}

		// base32 spells every 5 bytes in 8 characters and the padding is off.
		wantLen := initialPasswordBytes * 8 / 5
		if initialPasswordBytes%5 != 0 {
			wantLen++
		}
		if len(generated) != wantLen {
			t.Fatalf("the generated password is %d characters long, want %d", len(generated), wantLen)
		}

		if strings.Trim(generated, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
			t.Fatalf("the generated password holds a character outside the base32 alphabet")
		}

		if seen[generated] {
			t.Fatalf("the same password was generated twice within %d runs", runs)
		}
		seen[generated] = true
	}
}

func TestHashPasswordVerifiesTheOriginalOnly(t *testing.T) {
	const password = "test-password"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash: %v", err)
	}

	if hash == password {
		t.Fatalf("the password was stored as it is")
	}

	if !CheckPassword(hash, password) {
		t.Fatalf("the password does not verify against its own hash")
	}

	for _, other := range []string{"test-password-2", "test-passwor", "", "TEST-PASSWORD"} {
		if CheckPassword(hash, other) {
			t.Fatalf("a password that is not the original one verified")
		}
	}
}

func TestHashPasswordIsSaltedPerCall(t *testing.T) {
	const password = "test-password"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash: %v", err)
	}

	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash: %v", err)
	}

	if first == second {
		t.Fatalf("hashing the same password twice gave the same hash")
	}
	if !CheckPassword(second, password) {
		t.Fatalf("the second hash does not verify")
	}
}

func TestWriteInitialPasswordFileIsReadableByTheOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-password")

	err := writeInitialPasswordFile(path, "test-password")
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	requirePrivateFile(t, path)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	if strings.TrimRight(string(body), "\n") != "test-password" {
		t.Fatalf("the file does not hold what was written")
	}
}

func TestWriteInitialPasswordFileTightensAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-password")

	err := os.WriteFile(path, []byte("test-password-2"), 0644)
	if err != nil {
		t.Fatalf("failed to prepare the file: %v", err)
	}

	err = writeInitialPasswordFile(path, "test-password")
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	requirePrivateFile(t, path)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	if strings.Contains(string(body), "test-password-2") {
		t.Fatalf("the earlier content is still in the file")
	}
}

// TestWriteInitialPasswordFileWritesNothingIntoASymlinkItFound pins the same
// thing one step further: a symlink at the path is not followed, so neither
// the password nor a truncation reaches the file it names. Following it would
// let a local user pick any file this process may write to and have it emptied
// and filled with the password.
func TestWriteInitialPasswordFileWritesNothingIntoASymlinkItFound(t *testing.T) {
	const password = "test-password"

	dir := t.TempDir()
	path := filepath.Join(dir, "initial-password")
	target := filepath.Join(dir, "target")

	err := os.WriteFile(target, []byte("target\n"), 0644)
	if err != nil {
		t.Fatalf("failed to prepare the target: %v", err)
	}

	err = os.Symlink(target, path)
	if err != nil {
		t.Skipf("symlinks cannot be made here: %v", err)
	}

	err = writeInitialPasswordFile(path, password)
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	targetBody, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read the target: %v", err)
	}
	if string(targetBody) != "target\n" {
		t.Fatalf("the file the symlink named holds %q", string(targetBody))
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("failed to stat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the path is still a symlink")
	}

	requirePrivateFile(t, path)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if strings.TrimRight(string(body), "\n") != password {
		t.Fatalf("the file does not hold what was written")
	}
}

// TestWriteInitialPasswordFileReportsADirectoryItCannotWriteTo pins that a
// directory the process may not write to comes back as an error, which is what
// stops the startup instead of leaving an account nobody has the password of.
func TestWriteInitialPasswordFileReportsADirectoryItCannotWriteTo(t *testing.T) {
	dir := readOnlyDir(t)

	err := writeInitialPasswordFile(filepath.Join(dir, "initial-password"), "test-password")
	if err == nil {
		t.Fatalf("writing to a directory that is not writable was reported as a success")
	}

	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("the error does not name the path: %v", err)
	}
}

// errStub stands in for a database that answers nothing.
var errStub = errors.New("the database is not there")

// sqliteVersionDB answers the one statement the SQLite dialector runs for
// itself: on being opened it asks the database for its version, so as to know
// which clauses it may build. The pool below answers nothing and has no
// database behind it, so that probe is sent to a real in-memory one instead. It
// takes a database because a *sql.Row holds nothing exported and cannot be
// built by hand.
var sqliteVersionDB = func() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(fmt.Sprintf("failed to open the database the version probe is answered from: %v", err))
	}

	return db
}()

// stubConnPool is a database handle that sends nothing. Every callback that
// would reach it is replaced below, so it only has to exist and to hand out a
// transaction, which gorm opens around a create of its own accord.
type stubConnPool struct{}

func (p *stubConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errStub
}

func (p *stubConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return nil, errStub
}

func (p *stubConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errStub
}

func (p *stubConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return sqliteVersionDB.QueryRowContext(ctx, query, args...)
}

func (p *stubConnPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	return p, nil
}

func (p *stubConnPool) Commit() error { return nil }

func (p *stubConnPool) Rollback() error { return nil }

// accountStub is a database handle whose account table is answered from memory,
// together with the rows the account setup created in it. The rows are taken
// from the callback rather than from the SQL, so what was stored can be read
// field by field.
type accountStub struct {
	db      *gorm.DB
	created []models.User
}

// newAccountStub returns a handle whose account table holds rowCount rows.
func newAccountStub(t *testing.T, rowCount int64) *accountStub {
	t.Helper()

	db, err := gorm.Open(sqlite.Dialector{Conn: &stubConnPool{}}, &gorm.Config{
		Logger:               gormlogger.Discard,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("failed to open the database handle: %v", err)
	}

	stub := &accountStub{db: db}

	err = db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		// The only read the account setup makes is the count of the table.
		if dest, ok := tx.Statement.Dest.(*int64); ok {
			*dest = rowCount
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		if dest, ok := tx.Statement.Dest.(*models.User); ok {
			stub.created = append(stub.created, *dest)
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	return stub
}

// failTheCount makes the read of the account table fail.
func (s *accountStub) failTheCount(t *testing.T) {
	t.Helper()

	err := s.db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		_ = tx.AddError(errStub)
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}
}

// failTheInsert makes the write of the account row fail.
func (s *accountStub) failTheInsert(t *testing.T) {
	t.Helper()

	err := s.db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		_ = tx.AddError(errStub)
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}
}

// passwordFilePath returns a path in a directory of this test that nothing has
// written to yet.
func passwordFilePath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "initial-password")
}

// readPasswordFile returns what was written to the initial password file.
func readPasswordFile(t *testing.T, path string) string {
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

// mustNotExist fails when a file is there.
func mustNotExist(t *testing.T, path string) {
	t.Helper()

	_, err := os.Stat(path)
	if err == nil {
		t.Fatalf("the file %s was written", path)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("failed to check the file %s: %v", path, err)
	}
}

// TestEnsureUserCreatesTheAccountTheSetupIsRunAgainst pins the row the first
// startup writes: no username, setup_required set, and a hash in place of a
// password. A row that arrived with a username or without the flag would let
// the API be used before anyone chose the credentials.
func TestEnsureUserCreatesTheAccountTheSetupIsRunAgainst(t *testing.T) {
	stub := newAccountStub(t, 0)
	passwordFile := passwordFilePath(t)

	created, err := EnsureUser(stub.db, passwordFile)
	if err != nil {
		t.Fatalf("EnsureUser returned error: %v", err)
	}
	if !created {
		t.Fatalf("created = false while the account table was empty")
	}

	if len(stub.created) != 1 {
		t.Fatalf("rows created = %d, want 1", len(stub.created))
	}
	user := stub.created[0]

	if user.Username != "" {
		t.Errorf("username = %q, want an empty one", user.Username)
	}
	if !user.SetupRequired {
		t.Errorf("the account was created without setup_required")
	}
	if !strings.HasPrefix(user.PasswordHash, "$2a$") {
		t.Errorf("password_hash = %q, want a bcrypt hash", user.PasswordHash)
	}
}

// TestEnsureUserWritesTheInitialPasswordForTheOwnerOnly reads the permission
// off the file itself. The password opens the API, and the file sits in a
// directory an operator may share, so anyone but the owner reading it is the
// whole account handed over.
func TestEnsureUserWritesTheInitialPasswordForTheOwnerOnly(t *testing.T) {
	stub := newAccountStub(t, 0)
	passwordFile := passwordFilePath(t)

	_, err := EnsureUser(stub.db, passwordFile)
	if err != nil {
		t.Fatalf("EnsureUser returned error: %v", err)
	}

	requirePrivateFile(t, passwordFile)
}

// TestEnsureUserWritesThePasswordOfTheStoredHash pins that the file and the row
// belong together. A file holding anything else is an account nobody can log
// into, and the row is what keeps the next startup from making another one.
func TestEnsureUserWritesThePasswordOfTheStoredHash(t *testing.T) {
	stub := newAccountStub(t, 0)
	passwordFile := passwordFilePath(t)

	_, err := EnsureUser(stub.db, passwordFile)
	if err != nil {
		t.Fatalf("EnsureUser returned error: %v", err)
	}

	if len(stub.created) != 1 {
		t.Fatalf("rows created = %d, want 1", len(stub.created))
	}

	password := readPasswordFile(t, passwordFile)

	if password == stub.created[0].PasswordHash {
		t.Fatalf("the hash was written to the file in place of the password")
	}
	if !CheckPassword(stub.created[0].PasswordHash, password) {
		t.Fatalf("the stored hash does not verify against the password in the file")
	}
}

// TestEnsureUserWritesOverTheFileAnEarlierStartupLeft pins that a leftover
// password file does not stop the startup. It is reached only while no account
// row exists, so the file is one an earlier run wrote and the password in it
// opens nothing, and an installation whose startup stopped on it could never
// get its account set up at all.
func TestEnsureUserWritesOverTheFileAnEarlierStartupLeft(t *testing.T) {
	stub := newAccountStub(t, 0)
	passwordFile := passwordFilePath(t)

	const leftover = "left-by-an-earlier-startup"

	err := os.WriteFile(passwordFile, []byte(leftover+"\n"), initialPasswordFileMode)
	if err != nil {
		t.Fatalf("failed to prepare the leftover file: %v", err)
	}

	created, err := EnsureUser(stub.db, passwordFile)
	if err != nil {
		t.Fatalf("EnsureUser returned error: %v", err)
	}
	if !created {
		t.Fatalf("created = false while the account table was empty")
	}

	if len(stub.created) != 1 {
		t.Fatalf("rows created = %d, want 1", len(stub.created))
	}

	password := readPasswordFile(t, passwordFile)
	if password == leftover {
		t.Fatalf("the leftover password is still in the file")
	}
	if !CheckPassword(stub.created[0].PasswordHash, password) {
		t.Fatalf("the stored hash does not verify against the password in the file")
	}

	requirePrivateFile(t, passwordFile)
}

// TestEnsureUserLeavesTheAccountThatIsAlreadyThere pins that a later startup
// writes neither a row nor a file, so the password the operator is using is not
// replaced by one they never saw.
func TestEnsureUserLeavesTheAccountThatIsAlreadyThere(t *testing.T) {
	stub := newAccountStub(t, 1)
	passwordFile := passwordFilePath(t)

	created, err := EnsureUser(stub.db, passwordFile)
	if err != nil {
		t.Fatalf("EnsureUser returned error: %v", err)
	}
	if created {
		t.Fatalf("created = true while an account was already there")
	}

	if len(stub.created) != 0 {
		t.Fatalf("rows created = %d, want 0", len(stub.created))
	}
	mustNotExist(t, passwordFile)
}

// TestEnsureUserCreatesNoAccountWhenThePasswordFileCannotBeWritten is the one
// order that matters: a row without a file is an account nobody has the
// password of, and it keeps every later startup from making another one, so the
// application would be locked out for good. The file is written first, and a
// directory that cannot be written to has to end the run before the insert.
func TestEnsureUserCreatesNoAccountWhenThePasswordFileCannotBeWritten(t *testing.T) {
	stub := newAccountStub(t, 0)

	passwordFile := filepath.Join(readOnlyDir(t), "initial-password")

	created, err := EnsureUser(stub.db, passwordFile)
	if err == nil {
		t.Fatalf("a password file that could not be written was reported as a success")
	}
	if created {
		t.Fatalf("created = true while the run failed")
	}

	if len(stub.created) != 0 {
		t.Fatalf("rows created = %d, want 0: an account was left behind that nobody has the password of", len(stub.created))
	}
	mustNotExist(t, passwordFile)
}

// TestEnsureUserWritesNoPasswordFileWhenTheCountFails pins that a read that did
// not come back is not taken for an empty table. The file would otherwise be
// written over on a startup whose database was only unreachable for a moment,
// which is the password of the account in use thrown away.
func TestEnsureUserWritesNoPasswordFileWhenTheCountFails(t *testing.T) {
	stub := newAccountStub(t, 0)
	stub.failTheCount(t)
	passwordFile := passwordFilePath(t)

	created, err := EnsureUser(stub.db, passwordFile)
	if err == nil {
		t.Fatalf("a count that failed was reported as a success")
	}
	if created {
		t.Fatalf("created = true while the count failed")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("the error does not say the count failed: %v", err)
	}

	if len(stub.created) != 0 {
		t.Fatalf("rows created = %d, want 0", len(stub.created))
	}
	mustNotExist(t, passwordFile)
}

// TestEnsureUserReportsAnInsertThatFailed pins that a row that was not stored
// is not reported as a created account. The startup stops on it, which is what
// keeps the operator from being handed a password of an account that is not
// there.
func TestEnsureUserReportsAnInsertThatFailed(t *testing.T) {
	stub := newAccountStub(t, 0)
	stub.failTheInsert(t)
	passwordFile := passwordFilePath(t)

	created, err := EnsureUser(stub.db, passwordFile)
	if err == nil {
		t.Fatalf("an insert that failed was reported as a success")
	}
	if created {
		t.Fatalf("created = true while the insert failed")
	}
	if !strings.Contains(err.Error(), "account") {
		t.Errorf("the error does not say the account could not be created: %v", err)
	}

	// The file is there, since it is written first. It holds the password of no
	// account, and the next startup writes over it.
	_, statErr := os.Stat(passwordFile)
	if statErr != nil {
		t.Fatalf("failed to stat the initial password file: %v", statErr)
	}
}

// TestEnsureUserCreatesNothingWhenTheRandomnessCannotBeRead pins the same order
// one step earlier: a password that could not be made leaves neither a file nor
// a row, so the next startup is the one that sets the account up.
func TestEnsureUserCreatesNothingWhenTheRandomnessCannotBeRead(t *testing.T) {
	stub := newAccountStub(t, 0)
	passwordFile := passwordFilePath(t)

	original := rand.Reader
	rand.Reader = failingReader{}
	t.Cleanup(func() {
		rand.Reader = original
	})

	created, err := EnsureUser(stub.db, passwordFile)
	if err == nil {
		t.Fatalf("a password that could not be generated was reported as a success")
	}
	if created {
		t.Fatalf("created = true while no password could be generated")
	}

	if len(stub.created) != 0 {
		t.Fatalf("rows created = %d, want 0", len(stub.created))
	}
	mustNotExist(t, passwordFile)
}

// failingReader stands in for a source of randomness that is not readable.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errStub
}

// TestGenerateInitialPasswordReportsRandomnessItCouldNotRead pins that a short
// read is an error and not a password. Anything else would hand out a password
// made of the zero bytes the buffer still holds.
func TestGenerateInitialPasswordReportsRandomnessItCouldNotRead(t *testing.T) {
	original := rand.Reader
	rand.Reader = failingReader{}
	t.Cleanup(func() {
		rand.Reader = original
	})

	generated, err := GenerateInitialPassword()
	if err == nil {
		t.Fatalf("a source of randomness that could not be read gave the password %q", generated)
	}
	if generated != "" {
		t.Fatalf("a password came back along with the error: %q", generated)
	}
}

// TestHashPasswordReportsAPasswordBcryptWillNotTake pins that a password bcrypt
// turns down comes back as an error. bcrypt reads at most 72 bytes, and a
// silent truncation would store a hash that a shorter password also verifies
// against.
func TestHashPasswordReportsAPasswordBcryptWillNotTake(t *testing.T) {
	tooLong := strings.Repeat("a", 73)

	hash, err := HashPassword(tooLong)
	if err == nil {
		t.Fatalf("a password of %d bytes was hashed to %q", len(tooLong), hash)
	}
	if hash != "" {
		t.Fatalf("a hash came back along with the error: %q", hash)
	}
}

// TestWriteInitialPasswordFileReportsAPathItCannotReplace pins that a path
// that is taken by something this process may not remove ends the run instead
// of being written into. The password would otherwise go into a file whose
// mode and owner somebody else picked. The path is a device node, which is
// there on every run and belongs to root.
func TestWriteInitialPasswordFileReportsAPathItCannotReplace(t *testing.T) {
	const full = "/dev/full"

	if os.Geteuid() == 0 {
		t.Skip("root removes a device node whatever the directory says")
	}

	_, err := os.Stat(full)
	if err != nil {
		t.Skipf("%s is not there: %v", full, err)
	}

	err = writeInitialPasswordFile(full, "test-password")
	if err == nil {
		t.Fatalf("a path that could not be replaced was reported as a success")
	}
	if !strings.Contains(err.Error(), full) {
		t.Fatalf("the error does not name the path: %v", err)
	}
}
