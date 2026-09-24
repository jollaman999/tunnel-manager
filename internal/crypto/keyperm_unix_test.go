//go:build !windows

package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKeyCreatesKeyFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	_, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file was not created: %v", err)
	}

	if info.Mode().Perm() != 0600 {
		t.Fatalf("the key file permission is %#o, want %#o", info.Mode().Perm(), 0600)
	}
}

func TestLoadOrCreateKeyRejectsWidePermission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	_, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	err = os.Chmod(path, 0644)
	if err != nil {
		t.Fatalf("failed to widen the key file permission: %v", err)
	}

	_, err = LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("LoadOrCreateKey accepted a key file readable by others")
	}
}
