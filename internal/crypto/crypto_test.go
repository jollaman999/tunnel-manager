package crypto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestEncryptMarksTheValue(t *testing.T) {
	c := newTestCipher(t)

	encrypted, err := c.Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	if !IsEncrypted(encrypted) {
		t.Fatal("Encrypt produced a value without the marker")
	}

	// The marker has to hold a character that standard base64 never produces,
	// otherwise a value stored in the format without a marker could look marked.
	const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

	outside := false
	for _, r := range encryptedPrefix {
		if !strings.ContainsRune(base64Alphabet, r) {
			outside = true
			break
		}
	}

	if !outside {
		t.Fatalf("the marker of length %d is made of base64 characters only", len(encryptedPrefix))
	}
}

func TestDecryptWithAnotherKeyReportsWrongKey(t *testing.T) {
	c := newTestCipher(t)
	other := newTestCipher(t)

	encrypted, err := c.Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	_, err = other.Decrypt(encrypted)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("decrypting with another key did not report a wrong key: %v", err)
	}
	if errors.Is(err, ErrNotEncrypted) {
		t.Fatal("decrypting with another key reported the value as plaintext")
	}
}

func TestDecryptReportsPlaintextAsNotEncrypted(t *testing.T) {
	c := newTestCipher(t)

	// None of these is base64 of at least a nonce and a tag, so each one is a
	// value stored before passwords were encrypted.
	plaintexts := []string{
		"",
		"pass",
		"s3cr3t!",
		"not base64 at all",
		"tmenc",
	}

	for _, in := range plaintexts {
		_, err := c.Decrypt(in)
		if !errors.Is(err, ErrNotEncrypted) {
			t.Errorf("a value of length %d that was never encrypted was not reported as plaintext: %v", len(in), err)
		}
		if errors.Is(err, ErrWrongKey) {
			t.Errorf("a value of length %d that was never encrypted was reported as a wrong key", len(in))
		}
	}
}

func TestDecryptUnmarkedCipherTextRoundTrips(t *testing.T) {
	c := newTestCipher(t)

	encrypted, err := c.Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	// The format stored before the marker existed is the bare base64 body.
	unmarked := strings.TrimPrefix(encrypted, encryptedPrefix)
	if IsEncrypted(unmarked) {
		t.Fatal("the unmarked value still carries the marker")
	}

	decrypted, err := c.Decrypt(unmarked)
	if err != nil {
		t.Fatalf("Decrypt of a value in the format without a marker returned an error: %v", err)
	}
	if decrypted != "s3cr3t" {
		t.Fatal("Decrypt of a value in the format without a marker returned another password")
	}
}

func TestDecryptUnmarkedCipherTextWithAnotherKeyReportsWrongKey(t *testing.T) {
	c := newTestCipher(t)
	other := newTestCipher(t)

	encrypted, err := c.Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	unmarked := strings.TrimPrefix(encrypted, encryptedPrefix)

	// This is the case that destroyed passwords: a value stored in the format
	// without a marker, read with the wrong key. It must not look like plaintext.
	_, err = other.Decrypt(unmarked)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("a value in the format without a marker read with another key was not reported as a wrong key: %v", err)
	}
}

func TestDecryptReportsBase64ShapedPlaintextAsWrongKey(t *testing.T) {
	c := newTestCipher(t)

	// A value that was never encrypted but happens to be base64 of at least a
	// nonce and a tag cannot be told from a value stored in the format without
	// a marker. It is reported as a wrong key so that it is never overwritten,
	// which leaves it unusable until the password is set again through the API.
	_, err := c.Decrypt("YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY3ODkw")
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("a base64 shaped value was not reported as a wrong key: %v", err)
	}
}
