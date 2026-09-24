//go:build !windows

package crypto

import (
	"fmt"
	"os"

	"go.uber.org/zap"
)

// CreatePrivateFile creates a file for a secret with keyFileMode. O_EXCL keeps
// a file that appeared in the meantime from being overwritten. The mode is
// masked by umask, so the caller sets it again with Chmod.
func CreatePrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
}

// NarrowPrivateFile does nothing on Unix. The mode a file of secrets is
// created with already keeps others out, and a key file of a wider mode is
// refused where it is loaded rather than narrowed.
func NarrowPrivateFile(path string) ([]string, error) {
	return nil, nil
}

// NarrowPrivateDir does nothing on Unix, for the reason NarrowPrivateFile does
// nothing: the directories this program makes are made with a mode.
func NarrowPrivateDir(path string) ([]string, error) {
	return nil, nil
}

// MkdirAllPrivate is os.MkdirAll. The mode is what keeps others out on Unix.
func MkdirAllPrivate(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// ReservePrivateFile does nothing on Unix. Whatever opens the file afterwards
// creates it with a mode of its own and the caller narrows that mode, which is
// how these files have been made on Unix from the start.
func ReservePrivateFile(path string) error {
	return nil
}

// checkKeyFilePermission refuses a key file that the group or others may read.
func checkKeyFilePermission(path string, info os.FileInfo, logger *zap.Logger) error {
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		return fmt.Errorf("the key file %s is readable by the group or by others (permission %#o), run 'chmod 600 %s'", path, perm, path)
	}

	return nil
}
