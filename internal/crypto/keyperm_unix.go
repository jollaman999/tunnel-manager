//go:build !windows

package crypto

import (
	"fmt"
	"os"

	"go.uber.org/zap"
)

// createKeyFile creates the key file with keyFileMode. O_EXCL keeps a key file
// that appeared in the meantime from being overwritten.
func createKeyFile(path string) (*os.File, error) {
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
