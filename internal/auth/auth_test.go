package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitialPasswordFileSitsNextToTheConfigFile(t *testing.T) {
	cases := []struct {
		configFile string
		want       string
	}{
		{"config/config.yaml", filepath.Join("config", "initial-password")},
		{"/etc/tunnel-manager/config.yaml", filepath.Join("/etc/tunnel-manager", "initial-password")},
		{"config.yaml", "initial-password"},
	}

	for _, c := range cases {
		got := InitialPasswordFile(c.configFile)
		if got != c.want {
			t.Fatalf("the initial password file of the configuration file %q is %q, want %q", c.configFile, got, c.want)
		}
	}
}

func TestGenerateInitialPasswordDoesNotRepeatItself(t *testing.T) {
	const runs = 100

	seen := make(map[string]bool, runs)

	for i := 0; i < runs; i++ {
		generated, err := GenerateInitialPassword()
		if err != nil {
			t.Fatalf("failed to generate: %v", err)
		}

		// base32 spells every 5 bytes in 8 characters and the padding is off.
		wantLen := initialPasswordBytes * 8 / 5
		if initialPasswordBytes%5 != 0 {
			wantLen++
		}
		if len(generated) != wantLen {
			t.Fatalf("the generated password is %d characters long, want %d", len(generated), wantLen)
		}

		if strings.Trim(generated, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
			t.Fatalf("the generated password holds a character outside the base32 alphabet")
		}

		if seen[generated] {
			t.Fatalf("the same password was generated twice within %d runs", runs)
		}
		seen[generated] = true
	}
}

func TestHashPasswordVerifiesTheOriginalOnly(t *testing.T) {
	const password = "test-password"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash: %v", err)
	}

	if hash == password {
		t.Fatalf("the password was stored as it is")
	}

	if !CheckPassword(hash, password) {
		t.Fatalf("the password does not verify against its own hash")
	}

	for _, other := range []string{"test-password-2", "test-passwor", "", "TEST-PASSWORD"} {
		if CheckPassword(hash, other) {
			t.Fatalf("a password that is not the original one verified")
		}
	}
}

func TestHashPasswordIsSaltedPerCall(t *testing.T) {
	const password = "test-password"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash: %v", err)
	}

	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash: %v", err)
	}

	if first == second {
		t.Fatalf("hashing the same password twice gave the same hash")
	}
	if !CheckPassword(second, password) {
		t.Fatalf("the second hash does not verify")
	}
}

func TestWriteInitialPasswordFileIsReadableByTheOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-password")

	err := writeInitialPasswordFile(path, "test-password")
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat: %v", err)
	}

	if info.Mode().Perm() != initialPasswordFileMode {
		t.Fatalf("the file has permission %#o, want %#o", info.Mode().Perm(), initialPasswordFileMode)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	if strings.TrimRight(string(body), "\n") != "test-password" {
		t.Fatalf("the file does not hold what was written")
	}
}

func TestWriteInitialPasswordFileTightensAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-password")

	err := os.WriteFile(path, []byte("test-password-2"), 0644)
	if err != nil {
		t.Fatalf("failed to prepare the file: %v", err)
	}

	err = writeInitialPasswordFile(path, "test-password")
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat: %v", err)
	}

	if info.Mode().Perm() != initialPasswordFileMode {
		t.Fatalf("the file has permission %#o, want %#o", info.Mode().Perm(), initialPasswordFileMode)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	if strings.Contains(string(body), "test-password-2") {
		t.Fatalf("the earlier content is still in the file")
	}
}

// TestWriteInitialPasswordFileReportsADirectoryItCannotWriteTo pins that a
// directory the process may not write to comes back as an error, which is what
// stops the startup instead of leaving an account nobody has the password of.
func TestWriteInitialPasswordFileReportsADirectoryItCannotWriteTo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory whatever its permission says")
	}

	dir := filepath.Join(t.TempDir(), "read-only")

	err := os.Mkdir(dir, 0500)
	if err != nil {
		t.Fatalf("failed to prepare the directory: %v", err)
	}

	err = writeInitialPasswordFile(filepath.Join(dir, "initial-password"), "test-password")
	if err == nil {
		t.Fatalf("writing to a directory that is not writable was reported as a success")
	}

	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("the error does not name the path: %v", err)
	}
}
