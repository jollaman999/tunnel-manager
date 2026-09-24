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

// checkKeyFilePermission refuses a key file that the group or others may read.
func checkKeyFilePermission(path string, info os.FileInfo, logger *zap.Logger) error {
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		return fmt.Errorf("the key file %s is readable by the group or by others (permission %#o), run 'chmod 600 %s'", path, perm, path)
	}

	return nil
}
