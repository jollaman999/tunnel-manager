package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()

	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "test.key"))
	if err != nil {
		t.Fatalf("failed to create a key: %v", err)
	}

	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("failed to create a cipher: %v", err)
	}

	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := newTestCipher(t)

	inputs := []string{"", "a", "s3cr3t", "한글 비밀번호", "0123456789012345678901234567890123456789"}

	for _, in := range inputs {
		encrypted, err := c.Encrypt(in)
		if err != nil {
			t.Fatalf("Encrypt returned an error: %v", err)
		}

		decrypted, err := c.Decrypt(encrypted)
		if err != nil {
			t.Fatalf("Decrypt returned an error: %v", err)
		}

		if decrypted != in {
			t.Errorf("the round trip changed the value of length %d", len(in))
		}
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	c := newTestCipher(t)

	first, err := c.Encrypt("same-plaintext")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	second, err := c.Encrypt("same-plaintext")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	if first == second {
		t.Fatal("encrypting the same plaintext twice produced the same value")
	}
}

func TestDecryptWithAnotherKeyFails(t *testing.T) {
	c := newTestCipher(t)
	other := newTestCipher(t)

	encrypted, err := c.Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	_, err = other.Decrypt(encrypted)
	if err == nil {
		t.Fatal("decrypting with another key succeeded")
	}
}

func TestDecryptPlaintextFails(t *testing.T) {
	c := newTestCipher(t)

	// The migration of values stored before encryption relies on every one of
	// these being rejected.
	plaintexts := []string{
		"",
		"pass",
		"s3cr3t!",
		"not base64 at all",
		"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY3ODkw",
	}

	for _, in := range plaintexts {
		_, err := c.Decrypt(in)
		if err == nil {
			t.Errorf("decrypting a value of length %d that was never encrypted succeeded", len(in))
		}
	}
}

func TestNewCipherRejectsShortKey(t *testing.T) {
	_, err := NewCipher(make([]byte, KeySize-1))
	if err == nil {
		t.Fatal("NewCipher accepted a key shorter than KeySize")
	}
}

func TestLoadOrCreateKeyCreatesKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "tunnel-manager.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	if len(key) != KeySize {
		t.Fatalf("the generated key is %d bytes, want %d bytes", len(key), KeySize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file was not created: %v", err)
	}

	if info.Mode().Perm() != 0600 {
		t.Fatalf("the key file permission is %#o, want %#o", info.Mode().Perm(), 0600)
	}

	again, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey on an existing key file returned an error: %v", err)
	}

	if string(again) != string(key) {
		t.Fatal("LoadOrCreateKey replaced an existing key")
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

func TestLoadOrCreateKeyRejectsWrongSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	err := os.WriteFile(path, make([]byte, KeySize-1), 0600)
	if err != nil {
		t.Fatalf("failed to write the key file: %v", err)
	}

	_, err = LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("LoadOrCreateKey accepted a key file of the wrong size")
	}
}
