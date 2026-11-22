package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
)

// ErrInstallRunning reports that another install of the same installation is
// running. Two at once would each download a release, each stop the service and
// each copy an executable over the other's, and the service manager would be
// told to start a service while the other install had it stopped.
var ErrInstallRunning = errors.New("another install of this installation is running")

// installLockFileName is the file an install holds in the data directory for
// as long as it runs. The data directory is what an installation is, since -db
// names it, so two installs of one installation meet there whoever started
// them: a press on the Update screen, the loop that installs on its own, or an
// operator typing -install.
const installLockFileName = "install.lock"

// installLockMode is what the lock is created with. It holds a process id and
// nothing else, but it sits in a directory closed to everybody but the account
// the service runs as, and it is made the way the rest of that directory is.
const installLockMode fs.FileMode = 0o600

// installLockWriteGrace is how long a lock that names no process is taken to
// be one that is still being written. The file is created first and the
// process id written into it after, so for a moment it is there and empty, and
// an install that read it then as left behind would take a lock that another
// one is holding. A lock that still names nothing after this long was left by
// an install that ended between the two steps.
const installLockWriteGrace = time.Minute

// InstallLockFile is where an install of the installation whose data directory
// is dataDir holds its lock.
func InstallLockFile(dataDir string) string {
	return filepath.Join(dataDir, installLockFileName)
}

// LockInstall takes the lock an install holds for as long as it runs, and
// hands back what lets go of it. It fails with ErrInstallRunning where another
// install holds it.
//
// The lock is a file made with O_EXCL, which every platform this runs on makes
// atomic: of two installs that make it at once, one is refused. A lock does not
// go away with the process that held it, though. An install that was killed, or
// a machine that lost power during one, leaves the file behind, and every
// install after it would then be refused until somebody found the file and
// removed it by hand. So the file carries the process id of the install that
// made it, and a lock whose process is no longer running is taken over.
//
// Two things are left to chance and named here. A process id the system has
// handed to another process since reads as an install still running, which is
// a refusal that names the file and the id so that it can be removed by hand.
// And two installs that find the same lock left behind at the same moment may
// both take it over, which needs three installs of one installation started
// within the same few milliseconds with a crashed one before them.
//
// The data directory is made here if it is not there yet, because the lock
// has to be taken before the release is downloaded, and on a first install
// nothing has made the directory by then. An install that is refused, by this
// lock or by anything after it, says that it touched nothing, so what was made
// for the lock is taken away again unless the install went through: the
// release is told so with installed. Only what this call made and what is
// empty is removed, so a directory that was there before, the installation
// being upgraded, stays, and so does one the service has started writing to.
func LockInstall(dataDir string) (func(installed bool), error) {
	made := missingDirs(dataDir)

	err := crypto.MkdirAllPrivate(dataDir, dataDirMode)
	if err != nil {
		removeEmptyDirs(made)

		return nil, fmt.Errorf("failed to make the data directory %s: %w", dataDir, err)
	}

	path := InstallLockFile(dataDir)

	err = takeInstallLock(path)
	if err != nil {
		removeEmptyDirs(made)

		return nil, err
	}

	return func(installed bool) {
		_ = os.Remove(path)

		if !installed {
			removeEmptyDirs(made)
		}
	}, nil
}

// takeInstallLock makes the lock at path, taking over one that was left
// behind.
func takeInstallLock(path string) error {
	// The second pass is for a lock that was left behind and has just been
	// removed. A lock that is there again after that was made by another
	// install in between, which is the refusal and not a third try.
	for range 2 {
		taken, err := createInstallLock(path)
		if err != nil {
			return err
		}

		if taken {
			return nil
		}

		holder, held := installLockHolder(path, time.Now())
		if held {
			if holder > 0 {
				return fmt.Errorf("%w: process %d holds %s. If no install is running, remove that file",
					ErrInstallRunning, holder, path)
			}

			return fmt.Errorf("%w: %s names no process yet, or holds something other than a process id. "+
				"If no install is running, look at what is in it and remove it", ErrInstallRunning, path)
		}

		err = os.Remove(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove %s, which an install that is no longer running left behind: %w",
				path, err)
		}
	}

	return fmt.Errorf("%w: %s was taken by another install", ErrInstallRunning, path)
}

// missingDirs is dir and each of its parents that is not there, the deepest
// first, which is what making dir is going to make and the order they can be
// removed in.
func missingDirs(dir string) []string {
	var missing []string

	for current := filepath.Clean(dir); ; {
		_, err := os.Lstat(current)
		if !errors.Is(err, fs.ErrNotExist) {
			return missing
		}

		missing = append(missing, current)

		parent := filepath.Dir(current)
		if parent == current {
			return missing
		}

		current = parent
	}
}

// removeEmptyDirs removes the directories, the deepest first, and stops at the
// first that cannot go. os.Remove takes only an empty directory, so one that
// something has been put in since stays, and every one above it holds it and
// stays as well. Two installs that met here each made the directory as far as
// they know; the one that holds the lock has its lock in it, so the other one
// cannot take it away.
func removeEmptyDirs(dirs []string) {
	for _, dir := range dirs {
		if os.Remove(dir) != nil {
			return
		}
	}
}

// InstallRunning reports whether an install of the installation whose data
// directory is dataDir holds its lock now. It reads the lock the way
// LockInstall does, so a lock that was left behind is not an install running.
func InstallRunning(dataDir string) bool {
	_, held := installLockHolder(InstallLockFile(dataDir), time.Now())

	return held
}

// createInstallLock makes the lock with this process in it. It answers false
// where the file is already there, and an error for anything else.
func createInstallLock(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, installLockMode)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("failed to make the install lock %s: %w", path, err)
	}

	_, err = file.WriteString(strconv.Itoa(os.Getpid()))

	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}

	if err != nil {
		_ = os.Remove(path)

		return false, fmt.Errorf("failed to write the install lock %s: %w", path, err)
	}

	return true, nil
}

// installLockHolder reads the process id out of the lock and says whether the
// lock is held: there is a lock, and its process is running, or it is empty
// and younger than installLockWriteGrace, or it holds something other than a
// process id.
func installLockHolder(path string, now time.Time) (int, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		// A lock that cannot be read is not one this can judge. Only a lock
		// that is not there is free; anything else is left to the creation,
		// which fails on it and says why.
		return 0, !errors.Is(err, fs.ErrNotExist)
	}

	text := strings.TrimSpace(string(content))
	if text == "" {
		info, err := os.Stat(path)
		if err != nil {
			return 0, !errors.Is(err, fs.ErrNotExist)
		}

		return 0, now.Sub(info.ModTime()) < installLockWriteGrace
	}

	// Anything but a process id is a file this did not write, and a lock taken
	// over is a file removed. It is held instead, so that what is in it stays
	// and the refusal names the file for somebody to look at.
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0, true
	}

	// A lock that names this process was not made by it, since this process
	// asks for the lock once. It was left by an earlier process that had the
	// same id, which in a container that starts its processes afresh every
	// time is the usual case rather than a rare one.
	if pid == os.Getpid() {
		return pid, false
	}

	return pid, processRunning(pid)
}
