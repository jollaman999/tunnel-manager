package main

import (
	"path/filepath"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
)

// newTestCipher returns a cipher over a key that no other test shares, so two
// of them stand for two different key files.
func newTestCipher(t *testing.T) *crypto.Cipher {
	t.Helper()

	key, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "tunnel-manager.key"))
	if err != nil {
		t.Fatalf("failed to create a key: %v", err)
	}

	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("failed to create a cipher: %v", err)
	}

	return cipher
}

// encryptedWith returns plaintext sealed with cipher.
func encryptedWith(t *testing.T, cipher *crypto.Cipher, plaintext string) string {
	t.Helper()

	encrypted, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	return encrypted
}

func TestCheckStoredPasswordsReportsAWrongKeyWhenNoPasswordOpens(t *testing.T) {
	stored := newTestCipher(t)
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, IP: "192.0.2.1", Password: encryptedWith(t, stored, "test-password")},
		{ID: 2, IP: "192.0.2.2", Password: encryptedWith(t, stored, "test-password-2")},
		{ID: 3, IP: "192.0.2.3", Password: "plain"},
	}

	check := checkStoredPasswords(hosts, inUse)

	if check.decrypted != 0 {
		t.Fatalf("%d passwords opened with a key that sealed none of them", check.decrypted)
	}
	if len(check.wrongKeyHosts) != 2 {
		t.Fatalf("%d Hosts were reported as sealed with another key, want 2", len(check.wrongKeyHosts))
	}
	if check.notEncrypted != 1 {
		t.Fatalf("%d passwords were reported as never encrypted, want 1", check.notEncrypted)
	}
	if !check.keyIsWrong() {
		t.Fatal("a key that opens none of the encrypted passwords was not reported as the wrong one")
	}
}

func TestCheckStoredPasswordsAcceptsTheKeyWhenOnePasswordOpens(t *testing.T) {
	inUse := newTestCipher(t)
	other := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, IP: "192.0.2.1", Password: encryptedWith(t, inUse, "test-password")},
		{ID: 2, IP: "192.0.2.2", Password: encryptedWith(t, other, "test-password-2")},
	}

	check := checkStoredPasswords(hosts, inUse)

	if check.decrypted != 1 {
		t.Fatalf("%d passwords opened, want 1", check.decrypted)
	}
	if len(check.wrongKeyHosts) != 1 || check.wrongKeyHosts[0] != 2 {
		t.Fatalf("the Hosts that do not open were reported as %v, want [2]", check.wrongKeyHosts)
	}

	// One password that opens proves the key is the one in use, so the row that
	// does not open is a fault of that row and must not stop the startup. A
	// value that was never encrypted but happens to look encrypted lands here,
	// and refusing to start over it would take the service down for good.
	if check.keyIsWrong() {
		t.Fatal("a key that opens a stored password was reported as the wrong one")
	}
}

func TestCheckStoredPasswordsHasNothingToCheckWithoutCipherText(t *testing.T) {
	inUse := newTestCipher(t)

	cases := map[string][]models.Host{
		"no hosts at all": nil,
		"every password stored before encryption": {
			{ID: 1, IP: "192.0.2.1", Password: "test-password"},
			{ID: 2, IP: "192.0.2.2", Password: "test-password-2"},
		},
	}

	for name, hosts := range cases {
		t.Run(name, func(t *testing.T) {
			check := checkStoredPasswords(hosts, inUse)

			if len(check.wrongKeyHosts) != 0 {
				t.Fatalf("%d Hosts were reported as sealed with another key, want 0", len(check.wrongKeyHosts))
			}
			if check.notEncrypted != len(hosts) {
				t.Fatalf("%d of %d passwords were reported as never encrypted", check.notEncrypted, len(hosts))
			}
			if check.keyIsWrong() {
				t.Fatal("the key was reported as the wrong one although no password is encrypted")
			}
		})
	}
}
