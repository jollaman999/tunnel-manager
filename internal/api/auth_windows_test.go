//go:build windows

package api

import (
	"os"
	"testing"
)

// keepFromRemoval holds the file open until the test is over. Windows does not
// remove a file that is open without FILE_SHARE_DELETE, which os.Open does not
// pass.
func keepFromRemoval(t *testing.T, path string) {
	t.Helper()

	held, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open the file: %v", err)
	}

	t.Cleanup(func() {
		_ = held.Close()
	})
}
