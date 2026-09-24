//go:build !windows

package auth

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requirePrivateFile fails unless the file is at 0600. The number is spelled
// out rather than read from the constant, so a constant that is widened is a
// failure here and not a passing test.
func requirePrivateFile(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the initial password file: %v", err)
	}

	if info.Mode().Perm() != 0600 {
		t.Fatalf("the initial password file has permission %#o, want %#o", info.Mode().Perm(), 0600)
	}
}

// readOnlyDir returns a directory this process may read and may not write to.
func readOnlyDir(t *testing.T) string {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory whatever its permission says")
	}

	dir := filepath.Join(t.TempDir(), "read-only")

	err := os.Mkdir(dir, 0500)
	if err != nil {
		t.Fatalf("failed to prepare the directory: %v", err)
	}

	return dir
}

// TestWriteInitialPasswordFileWritesNothingIntoAFileItFound is the file that a
// local user put at the path before the first startup. The mode on such a file
// is the one that user chose, so a password written into it is readable by
// them, and narrowing the file afterwards is too late. The descriptor opened
// here before the call is what that user would be holding: it reads the file
// they made, whatever the path points at afterwards, and the password must not
// come out of it.
func TestWriteInitialPasswordFileWritesNothingIntoAFileItFound(t *testing.T) {
	const written = "test-password"

	path := filepath.Join(t.TempDir(), "initial-password")

	err := os.WriteFile(path, []byte("planted\n"), 0666)
	if err != nil {
		t.Fatalf("failed to prepare the file: %v", err)
	}

	// os.WriteFile creates through the umask, so the mode is set on its own.
	err = os.Chmod(path, 0666)
	if err != nil {
		t.Fatalf("failed to prepare the permission: %v", err)
	}

	planted, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open the prepared file: %v", err)
	}
	defer func() {
		_ = planted.Close()
	}()

	plantedInfo, err := planted.Stat()
	if err != nil {
		t.Fatalf("failed to stat the prepared file: %v", err)
	}

	err = writeInitialPasswordFile(path, written)
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	plantedBody, err := io.ReadAll(planted)
	if err != nil {
		t.Fatalf("failed to read the prepared file: %v", err)
	}
	if strings.Contains(string(plantedBody), written) {
		t.Fatalf("the password was written into the file that was already there")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat: %v", err)
	}

	if os.SameFile(plantedInfo, info) {
		t.Fatalf("the password was written into the file that was already there")
	}

	// The number is spelled out rather than read from the constant, so a
	// constant that is widened is a failure here and not a passing test.
	if info.Mode().Perm() != 0600 {
		t.Fatalf("the file has permission %#o, want %#o", info.Mode().Perm(), 0600)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if strings.TrimRight(string(body), "\n") != written {
		t.Fatalf("the file does not hold what was written")
	}
}
