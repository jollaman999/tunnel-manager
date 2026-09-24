//go:build !windows

package crypto

import (
	"fmt"
	"os"
)

// checkKeyFilePermission refuses a key file that the group or others may read.
func checkKeyFilePermission(path string, info os.FileInfo) error {
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		return fmt.Errorf("the key file %s is readable by the group or by others (permission %#o), run 'chmod 600 %s'", path, perm, path)
	}

	return nil
}
