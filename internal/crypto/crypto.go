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
)

// KeySize is the key length of AES-256 in bytes.
const KeySize = 32

// keyFileMode is the permission a key file is created with and is required to have.
const keyFileMode os.FileMode = 0600

// keyDirMode is the permission the directory of a new key file is created with.
const keyDirMode os.FileMode = 0700

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

	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. It fails for any input that this cipher did not
// produce, which is how a value stored before encryption is told apart from an
// encrypted one.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("the value is not base64 encoded: %w", err)
	}

	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize+c.aead.Overhead() {
		return "", fmt.Errorf("the value is shorter than a nonce and an authentication tag")
	}

	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt the value: %w", err)
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
