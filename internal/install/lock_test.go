package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// installLockHelperEnv names the data directory the helper process below takes
// the install lock in. Set, the test binary is that helper and nothing else.
const installLockHelperEnv = "TM_INSTALL_LOCK_HELPER_DIR"

// installLockHelperReady is the line the helper writes once it holds the lock.
const installLockHelperReady = "INSTALL-LOCK-HELD"

// TestInstallLockHelperProcess is the second install in the tests below. It is
// a test only so that it can be reached through the test binary, and it does
// nothing when it is run as part of the normal suite.
//
// It takes the lock, says so, and holds it until its standard input is closed,
// which is the test letting it go.
func TestInstallLockHelperProcess(t *testing.T) {
	dir := os.Getenv(installLockHelperEnv)
	if dir == "" {
		t.Skip("this test is the child process of the install lock tests")
	}

	unlock, err := LockInstall(dir)
	if err != nil {
		t.Fatalf("the helper could not take the lock: %v", err)
	}

	fmt.Println(installLockHelperReady)

	_, _ = io.Copy(io.Discard, os.Stdin)

	unlock(true)
}

// holdInstallLockElsewhere has another process take the install lock in dir,
// and answers what lets it go again.
func holdInstallLockElsewhere(t *testing.T, dir string) func() {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestInstallLockHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), installLockHelperEnv+"="+dir)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to make the input of the helper: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to read the output of the helper: %v", err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatalf("failed to start the helper: %v", err)
	}

	ready := make(chan bool, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		held := false

		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == installLockHelperReady {
				held = true

				break
			}
		}

		ready <- held

		_, _ = io.Copy(io.Discard, stdout)
	}()

	select {
	case held := <-ready:
		if !held {
			_ = cmd.Wait()
			t.Fatal("the helper ended without taking the lock")
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the helper did not take the lock in time")
	}

	released := false

	release := func() {
		if released {
			return
		}

		released = true

		_ = stdin.Close()

		err := cmd.Wait()
		if err != nil {
			t.Errorf("the helper failed: %v", err)
		}
	}

	t.Cleanup(release)

	return release
}

// TestASecondInstallIsRefusedWhileOneRuns has another process hold the lock
// the way an install in progress holds it, and then asks for it here the way a
// second -install would.
func TestASecondInstallIsRefusedWhileOneRuns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	release := holdInstallLockElsewhere(t, dir)

	if !InstallRunning(dir) {
		t.Error("an install that holds the lock is not reported as running")
	}

	unlock, err := LockInstall(dir)
	if err == nil {
		unlock(true)
		t.Fatal("a second install took the lock while the first one held it")
	}

	if !errors.Is(err, ErrInstallRunning) {
		t.Fatalf("the refusal is %v, want ErrInstallRunning", err)
	}

	// The refusal names the file, which is what an operator removes where the
	// process it names is not an install at all.
	if !strings.Contains(err.Error(), InstallLockFile(dir)) {
		t.Errorf("the refusal does not name the lock file: %v", err)
	}

	release()

	if InstallRunning(dir) {
		t.Error("an install that let the lock go is still reported as running")
	}

	unlock, err = LockInstall(dir)
	if err != nil {
		t.Fatalf("the lock could not be taken after the first install let it go: %v", err)
	}

	unlock(true)

	_, err = os.Stat(InstallLockFile(dir))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock is still there after it was let go: %v", err)
	}
}

// TestALockLeftByAnInstallThatEndedIsTakenOver is a lock whose process is gone:
// an install that was killed, or a machine that lost power during one. Every
// install after it would otherwise be refused until somebody removed the file.
func TestALockLeftByAnInstallThatEndedIsTakenOver(t *testing.T) {
	dir := t.TempDir()

	ended := exec.Command(os.Args[0], "-test.run=^$")

	err := ended.Run()
	if err != nil {
		t.Fatalf("failed to run a process that ends: %v", err)
	}

	writeInstallLock(t, dir, strconv.Itoa(ended.Process.Pid), time.Now())

	if InstallRunning(dir) {
		t.Error("a lock whose process has ended is reported as an install running")
	}

	unlock, err := LockInstall(dir)
	if err != nil {
		t.Fatalf("a lock left by a process that ended was not taken over: %v", err)
	}

	defer unlock(true)

	content, err := os.ReadFile(InstallLockFile(dir))
	if err != nil {
		t.Fatalf("failed to read the lock: %v", err)
	}

	if string(content) != strconv.Itoa(os.Getpid()) {
		t.Errorf("the lock carries %q, want this process, %d", content, os.Getpid())
	}
}

// TestALockThatNamesNoProcessIsHeldForAWhile covers the moment between making
// the file and writing the process id into it. A lock read then names nothing,
// and taking it over would hand one lock to two installs.
func TestALockThatNamesNoProcessIsHeldForAWhile(t *testing.T) {
	t.Run("just made", func(t *testing.T) {
		dir := t.TempDir()
		writeInstallLock(t, dir, "", time.Now())

		unlock, err := LockInstall(dir)
		if err == nil {
			unlock(true)
			t.Fatal("a lock that is still being written was taken over")
		}

		if !errors.Is(err, ErrInstallRunning) {
			t.Fatalf("the refusal is %v, want ErrInstallRunning", err)
		}
	})

	t.Run("left like that", func(t *testing.T) {
		dir := t.TempDir()
		writeInstallLock(t, dir, "", time.Now().Add(-2*installLockWriteGrace))

		unlock, err := LockInstall(dir)
		if err != nil {
			t.Fatalf("a lock that named no process for longer than the grace was not taken over: %v", err)
		}

		unlock(true)
	})
}

// TestAFileThatIsNotALockIsLeftAlone is a file under the name of the lock that
// an install did not write. Taking a lock over removes the file, and what is in
// this one is not a process id to judge it by, so it is kept and refused.
func TestAFileThatIsNotALockIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	writeInstallLock(t, dir, "something that is not a process id", time.Now().Add(-24*time.Hour))

	unlock, err := LockInstall(dir)
	if err == nil {
		unlock(true)
		t.Fatal("a file that is not a lock was taken over")
	}

	if !errors.Is(err, ErrInstallRunning) {
		t.Fatalf("the refusal is %v, want ErrInstallRunning", err)
	}

	content, err := os.ReadFile(InstallLockFile(dir))
	if err != nil || string(content) != "something that is not a process id" {
		t.Errorf("the file is now %q (%v), want what was in it", content, err)
	}
}

// TestALockThatNamesThisProcessWasLeftByAnother is a container that starts its
// processes afresh: the install that left the lock had the id this one has.
func TestALockThatNamesThisProcessWasLeftByAnother(t *testing.T) {
	dir := t.TempDir()
	writeInstallLock(t, dir, strconv.Itoa(os.Getpid()), time.Now())

	unlock, err := LockInstall(dir)
	if err != nil {
		t.Fatalf("a lock that names this process was not taken over: %v", err)
	}

	unlock(true)
}

// TestTheLockTakesAwayWhatItMadeForAnInstallThatDidNotGoThrough holds the
// release to what it removes. A refused install says it touched nothing, so
// the directories made for its lock go; an install that went through keeps
// them, and a directory that was there before, or that something has been
// put in since, is never removed.
func TestTheLockTakesAwayWhatItMadeForAnInstallThatDidNotGoThrough(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		root := t.TempDir()
		top := filepath.Join(root, "opt")

		unlock, err := LockInstall(filepath.Join(top, "tm"))
		if err != nil {
			t.Fatalf("LockInstall failed: %v", err)
		}

		unlock(false)

		if _, err := os.Stat(top); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after an install that did not go through: %v", top, err)
		}

		if _, err := os.Stat(root); err != nil {
			t.Errorf("the directory that was there before was removed: %v", err)
		}
	})

	t.Run("installed", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "opt", "tm")

		unlock, err := LockInstall(dataDir)
		if err != nil {
			t.Fatalf("LockInstall failed: %v", err)
		}

		unlock(true)

		if _, err := os.Stat(dataDir); err != nil {
			t.Errorf("the data directory of an install that went through was removed: %v", err)
		}
	})

	t.Run("written to meanwhile", func(t *testing.T) {
		top := filepath.Join(t.TempDir(), "opt")
		dataDir := filepath.Join(top, "tm")

		unlock, err := LockInstall(dataDir)
		if err != nil {
			t.Fatalf("LockInstall failed: %v", err)
		}

		err = os.WriteFile(filepath.Join(dataDir, "tm.db"), []byte("rows"), 0o600)
		if err != nil {
			t.Fatalf("failed to write into the data directory: %v", err)
		}

		unlock(false)

		if _, err := os.Stat(filepath.Join(dataDir, "tm.db")); err != nil {
			t.Errorf("a file put in the data directory went with it: %v", err)
		}

		if _, err := os.Stat(InstallLockFile(dataDir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the lock is still there: %v", err)
		}
	})
}

// writeInstallLock leaves a lock in dir carrying content, as if made at made.
func writeInstallLock(t *testing.T, dir string, content string, made time.Time) {
	t.Helper()

	path := InstallLockFile(dir)

	err := os.WriteFile(path, []byte(content), installLockMode)
	if err != nil {
		t.Fatalf("failed to write the lock: %v", err)
	}

	err = os.Chtimes(path, made, made)
	if err != nil {
		t.Fatalf("failed to date the lock: %v", err)
	}
}
