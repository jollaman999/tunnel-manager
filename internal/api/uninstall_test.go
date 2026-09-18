package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// uninstallPassword is what the account in these tests is opened with.
const uninstallPassword = "the-correct-horse-battery" // hook:allow

// uninstallExitWait is how long a test waits for the exit that is scheduled
// after the answer. It is far longer than the delay the handler is given below,
// so a machine under load does not turn a passing test into a failing one.
const uninstallExitWait = 5 * time.Second

// installation is the set of files an uninstall removes, laid out under a
// directory of its own. A real layout is built rather than a stub filesystem,
// because what is under test is which files are gone afterwards.
type installation struct {
	dir   string
	paths UninstallPaths
	// rotated and unrelated are the files in the log directory. The rotated ones
	// go with the log file, and the unrelated ones have to survive: this is a
	// delete, and a rule that takes one file too many takes it for good.
	rotated   []string
	unrelated []string
}

// newInstallation writes every file the uninstall knows about. The names are
// the ones a deployment carries, and the rotated log files are named the way
// lumberjack names them.
func newInstallation(t *testing.T) *installation {
	t.Helper()

	dir := t.TempDir()

	inst := &installation{
		dir: dir,
		paths: UninstallPaths{
			DatabaseFile:        filepath.Join(dir, "data", "tunnel-manager.db"),
			KeyFile:             filepath.Join(dir, "keys", "tunnel-manager.key"),
			InitialPasswordFile: filepath.Join(dir, "data", "initial-password"),
			LogFile:             filepath.Join(dir, "logs", "tunnel-manager.log"),
		},
		rotated: []string{
			filepath.Join(dir, "logs", "tunnel-manager-2026-09-18T01-02-03.456.log"),
			filepath.Join(dir, "logs", "tunnel-manager-2026-09-17T23-00-00.000.log.gz"),
		},
		unrelated: []string{
			filepath.Join(dir, "logs", "tunnel-manager-old.log"),
			filepath.Join(dir, "logs", "something-else.log"),
		},
	}

	// The database file itself is not written here. It is made by the driver in
	// newUninstallDB below, and the two files SQLite keeps beside it are put
	// there once that is open, so that opening it does not clear them away.
	files := []string{
		inst.paths.KeyFile,
		inst.paths.InitialPasswordFile,
		inst.paths.LogFile,
	}

	files = append(files, inst.rotated...)
	files = append(files, inst.unrelated...)

	for _, path := range files {
		writeFile(t, path)
	}

	return inst
}

// installedFilePaths is every file that has to be gone after an uninstall.
func (i *installation) installedFilePaths() []string {
	paths := []string{
		i.paths.DatabaseFile,
		i.paths.DatabaseFile + "-wal",
		i.paths.DatabaseFile + "-shm",
		i.paths.KeyFile,
		i.paths.InitialPasswordFile,
		i.paths.LogFile,
	}

	return append(paths, i.rotated...)
}

func writeFile(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		t.Fatalf("failed to create %s: %v", filepath.Dir(path), err)
	}

	err = os.WriteFile(path, []byte("x"), 0600)
	if err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)

	return err == nil
}

// uninstallSteps records what the uninstall did and in which order, together
// with whether the database file was still there at each step. The order is the
// whole point of this handler and cannot be read off the result: a loop that was
// stopped after the files went leaves nothing behind to see.
type uninstallSteps struct {
	mu    sync.Mutex
	names []string
	// fileWasThere says, per step, whether the database file still existed when
	// the step ran.
	fileWasThere []bool
	databaseFile string
}

func (s *uninstallSteps) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := os.Stat(s.databaseFile)

	s.names = append(s.names, name)
	s.fileWasThere = append(s.fileWasThere, err == nil)
}

func (s *uninstallSteps) StopAllTunnels() {
	s.record("tunnels")
}

func (s *uninstallSteps) taken() ([]string, []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.names...), append([]bool(nil), s.fileWasThere...)
}

// newUninstallDB returns a handle on a database file holding the one account,
// set up with uninstallPassword.
func newUninstallDB(t *testing.T, path string) *gorm.DB {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		t.Fatalf("failed to create %s: %v", filepath.Dir(path), err)
	}

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.User{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := auth.HashPassword(uninstallPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	err = db.Create(&models.User{Username: "operator", PasswordHash: hash}).Error
	if err != nil {
		t.Fatalf("failed to create the account: %v", err)
	}

	return db
}

// newUninstallHandler returns the handler together with what it was handed, so
// a test can see what it reached for.
func newUninstallHandler(t *testing.T, inst *installation) (*UninstallHandler, *uninstallSteps,
	chan struct{}, *gorm.DB) {
	t.Helper()

	db := newUninstallDB(t, inst.paths.DatabaseFile)

	// The write ahead log and the shared memory file are put in place after the
	// database is open, which is where they are in a running deployment. They
	// are the two that are easy to leave behind, since nothing names them: they
	// are worked out from the name of the database file.
	writeFile(t, inst.paths.DatabaseFile+"-wal")
	writeFile(t, inst.paths.DatabaseFile+"-shm")

	steps := &uninstallSteps{databaseFile: inst.paths.DatabaseFile}
	exited := make(chan struct{})

	h := NewUninstallHandler(db, zap.NewNop(), steps, func() {
		steps.record("reconcile")
	}, inst.paths, func() {
		close(exited)
	})

	// The wait between the answer and the exit is what the browser draws the
	// last screen in. A test has nothing to draw, so it is cut to something a
	// test can sit through while still being long enough to tell an exit that
	// waited from one that did not.
	h.exitAfter = 50 * time.Millisecond

	return h, steps, exited, db
}

// postUninstallAs runs one call against the handler. withAccount says
// whether the account the middleware leaves on the context is there, which is
// how a route hung outside that middleware is told apart.
func postUninstallAs(t *testing.T, h *UninstallHandler, body string,
	withAccount bool) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/uninstall", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if withAccount {
		c.Set(contextUserIDKey, uint(1))
	}

	err := h.Uninstall(c)
	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec
}

func postUninstall(t *testing.T, h *UninstallHandler, body string) *httptest.ResponseRecorder {
	t.Helper()

	return postUninstallAs(t, h, body, true)
}

// decodeUninstall reads the answer of an uninstall.
func decodeUninstall(t *testing.T, rec *httptest.ResponseRecorder) uninstallResult {
	t.Helper()

	var resp struct {
		Success bool            `json:"success"`
		Data    uninstallResult `json:"data"`
		Error   string          `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer %q: %v", rec.Body.String(), err)
	}

	return resp.Data
}

// removedPaths is what the answer says went, sorted so two runs compare.
func removedPaths(files []removedFile) []string {
	paths := make([]string, 0, len(files))

	for _, file := range files {
		paths = append(paths, file.Path)
	}

	sort.Strings(paths)

	return paths
}

// TestUninstallRefusesAPasswordThatDoesNotOpenTheAccount is the check the whole
// screen hangs on. Nothing may be stopped and no file may go.
func TestUninstallRefusesAPasswordThatDoesNotOpenTheAccount(t *testing.T) {
	inst := newInstallation(t)
	h, steps, exited, db := newUninstallHandler(t, inst)

	rec := postUninstall(t, h, `{"password":"not the password"}`) // hook:allow

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/uninstall = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	for _, path := range inst.installedFilePaths() {
		if !exists(t, path) {
			t.Fatalf("%s was removed by an uninstall that was refused", path)
		}
	}

	names, _ := steps.taken()
	if len(names) != 0 {
		t.Fatalf("a refused uninstall took these steps: %v", names)
	}

	select {
	case <-exited:
		t.Fatalf("a refused uninstall ended the process")
	case <-time.After(2 * h.exitAfter):
	}

	// The database is still the one the API is served on.
	err := db.Exec("SELECT 1").Error
	if err != nil {
		t.Fatalf("a refused uninstall left the database unusable: %v", err)
	}
}

// TestUninstallRemovesTheFilesOfTheInstallation covers the -wal and the -shm
// files as well, which are the two that are easy to leave behind: they are not
// named anywhere, they are worked out from the name of the database file.
func TestUninstallRemovesTheFilesOfTheInstallation(t *testing.T) {
	inst := newInstallation(t)
	h, _, _, _ := newUninstallHandler(t, inst)

	rec := postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/uninstall = %d (%s), want %d", rec.Code, rec.Body.String(),
			http.StatusOK)
	}

	for _, path := range inst.installedFilePaths() {
		if exists(t, path) {
			t.Fatalf("%s is still there after the uninstall", path)
		}
	}

	result := decodeUninstall(t, rec)

	if len(result.Failed) != 0 {
		t.Fatalf("the uninstall reported files it could not remove: %v", result.Failed)
	}

	// What the answer names is what the uninstall itself removed. The -wal and
	// the -shm file are left out of this comparison on purpose: SQLite writes
	// the log back and takes those two away as the last connection closes, so
	// whether they were still there by the time the files were removed is up to
	// the driver. That they are gone is checked above, which is what matters.
	want := []string{}

	for _, path := range inst.installedFilePaths() {
		if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") {
			continue
		}

		want = append(want, path)
	}

	sort.Strings(want)

	got := removedPaths(result.Removed)

	for _, path := range want {
		found := false

		for _, reported := range got {
			if reported == path {
				found = true
			}
		}

		if !found {
			t.Fatalf("the uninstall did not report %s as removed. It reported\n%s", path,
				strings.Join(got, "\n"))
		}
	}

	// Nothing outside the installation may be named, since everything named
	// here was deleted.
	for _, path := range got {
		known := false

		for _, installed := range inst.installedFilePaths() {
			if installed == path {
				known = true
			}
		}

		if !known {
			t.Fatalf("the uninstall removed %s, which is no part of the installation", path)
		}
	}

	if result.ExitInSec != int(h.exitAfter/time.Second) {
		t.Fatalf("exit_in_sec = %d, want %d", result.ExitInSec, int(h.exitAfter/time.Second))
	}
}

// TestUninstallSaysHowLongTheProcessStaysUp reads the number the screen puts in
// front of the operator. It is the wait the handler actually keeps, which is
// why the default one is used here rather than the shortened one.
func TestUninstallSaysHowLongTheProcessStaysUp(t *testing.T) {
	inst := newInstallation(t)
	h, _, _, _ := newUninstallHandler(t, inst)

	h.exitAfter = uninstallExitDelay

	rec := postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	result := decodeUninstall(t, rec)

	if result.ExitInSec != int(uninstallExitDelay/time.Second) || result.ExitInSec < 1 {
		t.Fatalf("exit_in_sec = %d, want %d", result.ExitInSec,
			int(uninstallExitDelay/time.Second))
	}
}

// TestTheDatabaseSidecarsAreNamedAfterTheDatabaseFile pins how the two files
// SQLite keeps beside the database are found. Nothing configures them, so the
// only thing that puts them on the list is this rule.
func TestTheDatabaseSidecarsAreNamedAfterTheDatabaseFile(t *testing.T) {
	h := &UninstallHandler{paths: UninstallPaths{DatabaseFile: filepath.Join("data", "tm.db")}}

	var named []string

	for _, file := range h.installedFiles() {
		named = append(named, file.Path)
	}

	for _, want := range []string{
		absolutePath(filepath.Join("data", "tm.db")),
		absolutePath(filepath.Join("data", "tm.db-wal")),
		absolutePath(filepath.Join("data", "tm.db-shm")),
	} {
		found := false

		for _, path := range named {
			if path == want {
				found = true
			}
		}

		if !found {
			t.Fatalf("the uninstall does not name %s. It names %v", want, named)
		}
	}

	if len(named) != 3 {
		t.Fatalf("the uninstall names %v, want the database file and its two companions", named)
	}
}

// TestUninstallLeavesFilesThatTheRotationDidNotWrite pins the rule that finds
// the rotated logs. It is a delete, so a name that merely starts the same way
// must not be taken.
func TestUninstallLeavesFilesThatTheRotationDidNotWrite(t *testing.T) {
	inst := newInstallation(t)
	h, _, _, _ := newUninstallHandler(t, inst)

	rec := postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/uninstall = %d (%s), want %d", rec.Code, rec.Body.String(),
			http.StatusOK)
	}

	for _, path := range inst.rotated {
		if exists(t, path) {
			t.Fatalf("the rotated log %s is still there", path)
		}
	}

	for _, path := range inst.unrelated {
		if !exists(t, path) {
			t.Fatalf("%s was removed, and the rotation never wrote it", path)
		}
	}
}

// TestUninstallStopsTheLoopAndTheTunnelsBeforeTheFilesGo is the order the whole
// thing turns on. A loop that is still running builds again what was stopped,
// and a tunnel that is left up holds a listener on a remote host.
func TestUninstallStopsTheLoopAndTheTunnelsBeforeTheFilesGo(t *testing.T) {
	inst := newInstallation(t)
	h, steps, _, _ := newUninstallHandler(t, inst)

	rec := postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/uninstall = %d (%s), want %d", rec.Code, rec.Body.String(),
			http.StatusOK)
	}

	names, fileWasThere := steps.taken()

	if strings.Join(names, ",") != "reconcile,tunnels" {
		t.Fatalf("the uninstall took the steps %v, want the loop stopped and then the tunnels",
			names)
	}

	for index, there := range fileWasThere {
		if !there {
			t.Fatalf("%s was stopped after the database file was already removed", names[index])
		}
	}
}

// TestUninstallClosesTheDatabase is what keeps the -wal and the -shm files from
// being written back out after they were removed.
func TestUninstallClosesTheDatabase(t *testing.T) {
	inst := newInstallation(t)
	h, _, _, db := newUninstallHandler(t, inst)

	postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	err := db.Exec("SELECT 1").Error
	if err == nil {
		t.Fatalf("the database is still open after the uninstall")
	}
}

// TestUninstallGoesOnWhenAFileCannotBeRemoved is the Windows case: the log file
// is open while this runs and cannot be deleted there. The run has to carry on
// and say what was left behind, because stopping at the first failure would
// leave the database and the key in place over a file that was never going to go.
func TestUninstallGoesOnWhenAFileCannotBeRemoved(t *testing.T) {
	inst := newInstallation(t)

	// A directory with something in it stands in for the file that cannot be
	// removed. os.Remove refuses it on every platform, which is what an open
	// file looks like from here.
	err := os.Remove(inst.paths.KeyFile)
	if err != nil {
		t.Fatalf("failed to remove %s: %v", inst.paths.KeyFile, err)
	}

	writeFile(t, filepath.Join(inst.paths.KeyFile, "in-the-way"))

	h, _, _, _ := newUninstallHandler(t, inst)

	rec := postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/uninstall = %d (%s), want %d", rec.Code, rec.Body.String(),
			http.StatusOK)
	}

	result := decodeUninstall(t, rec)

	if len(result.Failed) != 1 || result.Failed[0].Path != inst.paths.KeyFile {
		t.Fatalf("the uninstall reported %v as left behind, want %s", result.Failed,
			inst.paths.KeyFile)
	}

	if result.Failed[0].Error == "" {
		t.Fatalf("the uninstall reported no reason for %s", inst.paths.KeyFile)
	}

	for _, path := range inst.installedFilePaths() {
		if path == inst.paths.KeyFile {
			continue
		}

		if exists(t, path) {
			t.Fatalf("%s is still there, so one file that stayed stopped the rest", path)
		}
	}
}

// TestUninstallEndsTheProcessAfterTheAnswer is what the completion screen
// depends on. The answer has to be written while the process is still up.
func TestUninstallEndsTheProcessAfterTheAnswer(t *testing.T) {
	inst := newInstallation(t)
	h, _, exited, _ := newUninstallHandler(t, inst)

	rec := postUninstall(t, h, `{"password":"`+uninstallPassword+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/uninstall = %d (%s), want %d", rec.Code, rec.Body.String(),
			http.StatusOK)
	}

	if rec.Body.Len() == 0 {
		t.Fatalf("the uninstall wrote no answer")
	}

	select {
	case <-exited:
		t.Fatalf("the process ended before the answer had any time to reach the browser")
	default:
	}

	select {
	case <-exited:
	case <-time.After(uninstallExitWait):
		t.Fatalf("the process did not end within %s of the answer", uninstallExitWait)
	}
}

// TestUninstallRefusesARequestWithNoAccountOnTheContext covers the route being
// hung outside the middleware that puts the account there. Removing the
// installation for a request nobody is behind is the one thing worse than
// refusing one that is.
func TestUninstallRefusesARequestWithNoAccountOnTheContext(t *testing.T) {
	inst := newInstallation(t)
	h, steps, _, _ := newUninstallHandler(t, inst)

	rec := postUninstallAs(t, h, `{"password":"`+uninstallPassword+`"}`, false)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /api/uninstall = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	for _, path := range inst.installedFilePaths() {
		if !exists(t, path) {
			t.Fatalf("%s was removed for a request with no account behind it", path)
		}
	}

	names, _ := steps.taken()
	if len(names) != 0 {
		t.Fatalf("a request with no account behind it took these steps: %v", names)
	}
}

// TestUninstallIsBehindTheSessionAndTheCSRFCheck pins where the route hangs.
// The uninstall is on the /api group, so it is refused for a client that has no
// session before the handler is reached at all.
func TestUninstallIsBehindTheSessionAndTheCSRFCheck(t *testing.T) {
	inst := newInstallation(t)
	h, _, _, db := newUninstallHandler(t, inst)

	e := echo.New()
	e.Use(middleware.Recover())

	g := e.Group("/api")
	g.Use(NewAuthHandler(db, zap.NewNop(), filepath.Join(inst.dir, "initial")).RequireSession())
	g.POST("/uninstall", h.Uninstall)

	req := httptest.NewRequest(http.MethodPost, "/api/uninstall",
		strings.NewReader(`{"password":"`+uninstallPassword+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/uninstall without a session = %d, want %d", rec.Code,
			http.StatusUnauthorized)
	}

	for _, path := range inst.installedFilePaths() {
		if !exists(t, path) {
			t.Fatalf("%s was removed for a request that carried no session", path)
		}
	}
}

// TestRemoveFilesTakesTheDatabaseSidecarsThatAreStillThere is the backstop for
// the -wal and the -shm file. Closing the database usually takes them away, and
// then there is nothing here to remove; this is what happens when it does not,
// which is a close that failed and a file left in a directory nobody looks in.
func TestRemoveFilesTakesTheDatabaseSidecarsThatAreStillThere(t *testing.T) {
	dir := t.TempDir()
	databaseFile := filepath.Join(dir, "tunnel-manager.db")

	writeFile(t, databaseFile)
	writeFile(t, databaseFile+"-wal")
	writeFile(t, databaseFile+"-shm")

	h := &UninstallHandler{
		logger: zap.NewNop(),
		paths:  UninstallPaths{DatabaseFile: databaseFile},
	}

	result := h.removeFiles()

	for _, path := range []string{databaseFile, databaseFile + "-wal", databaseFile + "-shm"} {
		if exists(t, path) {
			t.Fatalf("%s is still there", path)
		}
	}

	if len(result.Removed) != 3 || len(result.Failed) != 0 {
		t.Fatalf("the uninstall reported %d removed and %d left behind, want three and none",
			len(result.Removed), len(result.Failed))
	}
}
