// Package crypto encrypts and decrypts the secrets that are kept in the database.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the key length of AES-256 in bytes.
const KeySize = 32

// keyFileMode is the permission a key file is created with and is required to have.
const keyFileMode os.FileMode = 0600

// keyDirMode is the permission the directory of a new key file is created with.
const keyDirMode os.FileMode = 0700

// encryptedPrefix marks a value that Encrypt produced and names the format it
// is in. ':' is not part of the standard base64 alphabet, so a value that was
// never encrypted by this package cannot be a marked one by accident, and the
// version lets a later format be told apart from this one.
const encryptedPrefix = "tmenc:v1:"

// ErrNotEncrypted reports that the value carries no marker and does not have
// the shape of an encrypted one, so it was stored before passwords were
// encrypted. Callers migrate such a value by encrypting it.
var ErrNotEncrypted = errors.New("the value is not encrypted")

// ErrWrongKey reports that the value was encrypted but does not open with the
// key in use. Callers must never overwrite such a value: the plaintext exists
// nowhere else, so writing over it destroys the secret for good.
var ErrWrongKey = errors.New("the value does not decrypt with the encryption key in use")

type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a cipher from a key of KeySize bytes.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("invalid key length: got %d bytes, want %d bytes", len(key), KeySize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create the AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create the GCM mode: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

// Encrypt seals the plaintext with AES-256-GCM under a fresh nonce and returns
// the base64 encoded form of the nonce followed by the sealed bytes.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	_, err := io.ReadFull(rand.Reader, nonce)
	if err != nil {
		return "", fmt.Errorf("failed to generate a nonce: %w", err)
	}

	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)

	return encryptedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// IsEncrypted reports whether the value carries the marker that Encrypt writes.
// A value that decrypts without carrying it was written in the format that had
// no marker yet.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, encryptedPrefix)
}

// Decrypt reverses Encrypt. Every failure is either ErrNotEncrypted, meaning
// the value was stored before passwords were encrypted, or ErrWrongKey, meaning
// the value is encrypted but the key in use is not the one it was sealed with.
// Callers have to tell the two apart, because only the first one may be
// overwritten.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	body, marked := strings.CutPrefix(encoded, encryptedPrefix)

	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		if marked {
			return "", fmt.Errorf("the encrypted value is not base64 encoded: %w", ErrWrongKey)
		}
		return "", fmt.Errorf("the value is not base64 encoded: %w", ErrNotEncrypted)
	}

	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize+c.aead.Overhead() {
		if marked {
			return "", fmt.Errorf("the encrypted value is shorter than a nonce and an authentication tag: %w", ErrWrongKey)
		}
		return "", fmt.Errorf("the value is shorter than a nonce and an authentication tag: %w", ErrNotEncrypted)
	}

	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		// An unmarked value that got this far is base64 of at least a nonce and
		// a tag, which is what the format without a marker looks like, so it is
		// reported as an encrypted value too. Calling it plaintext would let a
		// caller encrypt it again and lose the secret it holds.
		return "", fmt.Errorf("failed to decrypt the value: %w", ErrWrongKey)
	}

	return string(plaintext), nil
}

// LoadOrCreateKey reads the key file at path. The key is generated and stored
// with keyFileMode when the file does not exist yet.
func LoadOrCreateKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to check the key file %s: %w", path, err)
		}
		return createKey(path)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("the key file path %s is a directory", path)
	}

	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		return nil, fmt.Errorf("the key file %s is readable by the group or by others (permission %#o), run 'chmod 600 %s'", path, perm, path)
	}

	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read the key file %s: %w", path, err)
	}

	if len(key) != KeySize {
		return nil, fmt.Errorf("the key file %s holds %d bytes, want %d bytes", path, len(key), KeySize)
	}

	return key, nil
}

func createKey(path string) ([]byte, error) {
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, keyDirMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create the key directory %s: %w", dir, err)
	}

	key := make([]byte, KeySize)
	_, err = io.ReadFull(rand.Reader, key)
	if err != nil {
		return nil, fmt.Errorf("failed to generate a key: %w", err)
	}

	// O_EXCL keeps a key file that appeared in the meantime from being overwritten.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create the key file %s: %w", path, err)
	}

	_, err = f.Write(key)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to write the key file %s: %w", path, err)
	}

	// The mode passed to OpenFile is masked by umask, so it is set again here.
	err = f.Chmod(keyFileMode)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to set the permission of the key file %s: %w", path, err)
	}

	err = f.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to close the key file %s: %w", path, err)
	}

	return key, nil
}
