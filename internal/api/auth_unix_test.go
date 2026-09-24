//go:build !windows

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// keepFromRemoval takes the write permission off the directory the file is
// in, so that the file cannot be removed until the test is over.
func keepFromRemoval(t *testing.T, path string) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root removes files from a directory it has no write permission on")
	}

	dir := filepath.Dir(path)

	err := os.Chmod(dir, 0500)
	if err != nil {
		t.Fatalf("failed to take the write permission off the directory: %v", err)
	}

	// The cleanup of t.TempDir has to be able to remove the file again.
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0700)
	})
}
