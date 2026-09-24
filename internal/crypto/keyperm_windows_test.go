//go:build windows

package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKeyReadsKeyFileWhateverItsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Chmod(path, 0600)
	})

	for _, mode := range []os.FileMode{0644, 0444} {
		err = os.Chmod(path, mode)
		if err != nil {
			t.Fatalf("failed to set the key file mode to %#o: %v", mode, err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("failed to stat the key file: %v", err)
		}

		if info.Mode().Perm()&0077 == 0 {
			t.Fatalf("os.Stat reported %#o, which no longer looks group or world readable", info.Mode().Perm())
		}

		again, err := LoadOrCreateKey(path)
		if err != nil {
			t.Fatalf("LoadOrCreateKey refused the key file at mode %#o: %v", info.Mode().Perm(), err)
		}

		if string(again) != string(key) {
			t.Fatal("LoadOrCreateKey returned another key")
		}
	}
}
