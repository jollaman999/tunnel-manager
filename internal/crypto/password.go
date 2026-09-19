package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// passwordEncryptedPrefix marks a value that EncryptWithPassword produced and
// names the format it is in. It is not the prefix of Encrypt, because the two
// formats open with different keys: this one with a password the operator
// types, the other one with the key file of this installation. The version
// lets the parameters below be raised later without making the files written
// today unreadable, since the reader picks the layout by the version first.
const passwordEncryptedPrefix = "tmpwenc:v1:"

// The scrypt parameters of the v1 format. scrypt needs 128 * r * N bytes and
// about as much time as it needs memory, so N sets both. With N of 2^15, r of
// 8 and p of 1 it works in 32 MiB, and one call took 62 ms to seal and 62 ms to
// open on an i7-9700K (go test -bench, 2026-09-19). Raising N by
// one step doubles both. Lower than this would hand an attacker who took the
// exported file more guesses per second than a typed password can stand, and
// higher than this would make an export on a small machine look stuck, which is
// what the 32 MiB is chosen against: a machine that small still has it free.
//
// A file carries the parameters it was written with, so these three only apply
// to files written from now on.
const (
	scryptLogN = 15
	scryptR    = 8
	scryptP    = 1
)

// passwordSaltSize is the length of the scrypt salt in bytes. The salt is drawn
// fresh for every call, so two exports of the same settings under the same
// password share no key.
const passwordSaltSize = 16

// passwordHeaderSize is the length of the header that precedes the nonce: one
// byte of log2(N), one of r, one of p, then the salt.
const passwordHeaderSize = 3 + passwordSaltSize

// maxScryptMemory caps the memory a file may ask for while it is opened. A file
// states its own N and r, so a hostile file could otherwise name a size that no
// machine can allocate and end the process instead of failing the import.
const maxScryptMemory = 512 << 20

// ErrPasswordRequired reports that the password is empty. A file sealed under
// an empty password would be open to anyone who holds it, so an empty password
// is refused on both sides rather than treated as "no encryption".
var ErrPasswordRequired = errors.New("the password is empty")

// ErrNotPasswordEncrypted reports that the value carries no marker of this
// format, so it was not produced by EncryptWithPassword. Callers report this as
// a file that does not belong here, not as a wrong password.
var ErrNotPasswordEncrypted = errors.New("the value is not encrypted with a password")

// ErrPasswordEncryptedDamaged reports that the value carries the marker but its
// body cannot be read: the base64 is broken, the body is shorter than a header,
// a nonce and a tag, or the stated parameters are outside what this build
// accepts. The file was cut or altered, and no password opens it.
var ErrPasswordEncryptedDamaged = errors.New("the password encrypted value is damaged")

// ErrWrongPassword reports that the value was read as far as its layout goes
// but did not authenticate. The usual cause is a mistyped password. A body that
// was altered after the base64 still decoded fails here as well, because AES-GCM
// cannot tell an altered body from a wrong key; anything that can be told apart
// is reported as ErrPasswordEncryptedDamaged instead.
var ErrWrongPassword = errors.New("the value does not decrypt with this password")

// EncryptWithPassword derives a key from the password with scrypt under a fresh
// salt, seals the plaintext with AES-256-GCM under a fresh nonce, and returns a
// single string that carries the parameters, the salt and the nonce along with
// the sealed bytes, so that any machine can open it with the password alone.
func EncryptWithPassword(plaintext string, password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("failed to encrypt with a password: %w", ErrPasswordRequired)
	}

	salt := make([]byte, passwordSaltSize)
	_, err := io.ReadFull(rand.Reader, salt)
	if err != nil {
		return "", fmt.Errorf("failed to generate a salt: %w", err)
	}

	header := make([]byte, 0, passwordHeaderSize)
	header = append(header, scryptLogN, scryptR, scryptP)
	header = append(header, salt...)

	aead, err := passwordAEAD(password, salt, scryptLogN, scryptR, scryptP)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	_, err = io.ReadFull(rand.Reader, nonce)
	if err != nil {
		return "", fmt.Errorf("failed to generate a nonce: %w", err)
	}

	// The header is authenticated but not encrypted, so an altered salt or an
	// altered parameter is reported instead of quietly deriving another key.
	body := make([]byte, 0, passwordHeaderSize+len(nonce)+len(plaintext)+aead.Overhead())
	body = append(body, header...)
	body = append(body, nonce...)
	body = aead.Seal(body, nonce, []byte(plaintext), header)

	return passwordEncryptedPrefix + base64.StdEncoding.EncodeToString(body), nil
}

// IsEncryptedWithPassword reports whether the value carries the marker that
// EncryptWithPassword writes.
func IsEncryptedWithPassword(value string) bool {
	return strings.HasPrefix(value, passwordEncryptedPrefix)
}

// DecryptWithPassword reverses EncryptWithPassword. Every failure is one of
// ErrPasswordRequired, ErrNotPasswordEncrypted, ErrPasswordEncryptedDamaged or
// ErrWrongPassword, so that the caller can tell the operator whether to pick
// another file or to type the password again.
func DecryptWithPassword(encoded string, password string) (string, error) {
	body, marked := strings.CutPrefix(encoded, passwordEncryptedPrefix)
	if !marked {
		return "", fmt.Errorf("the value does not start with %q: %w", passwordEncryptedPrefix, ErrNotPasswordEncrypted)
	}

	if password == "" {
		return "", fmt.Errorf("failed to decrypt with a password: %w", ErrPasswordRequired)
	}

	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("the encrypted value is not base64 encoded: %w", ErrPasswordEncryptedDamaged)
	}

	if len(raw) < passwordHeaderSize {
		return "", fmt.Errorf("the encrypted value is %d bytes, shorter than the %d byte header: %w", len(raw), passwordHeaderSize, ErrPasswordEncryptedDamaged)
	}

	header := raw[:passwordHeaderSize]
	logN, r, p := int(header[0]), int(header[1]), int(header[2])
	salt := header[3:passwordHeaderSize]

	err = checkScryptParameters(logN, r, p)
	if err != nil {
		return "", err
	}

	aead, err := passwordAEAD(password, salt, logN, r, p)
	if err != nil {
		return "", err
	}

	rest := raw[passwordHeaderSize:]
	nonceSize := aead.NonceSize()
	if len(rest) < nonceSize+aead.Overhead() {
		return "", fmt.Errorf("the encrypted value is shorter than a nonce and an authentication tag: %w", ErrPasswordEncryptedDamaged)
	}

	plaintext, err := aead.Open(nil, rest[:nonceSize], rest[nonceSize:], header)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt the value: %w", ErrWrongPassword)
	}

	return string(plaintext), nil
}

// passwordAEAD derives the AES-256 key from the password with scrypt and
// returns the GCM mode over it.
func passwordAEAD(password string, salt []byte, logN int, r int, p int) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(password), salt, 1<<logN, r, p, KeySize)
	if err != nil {
		return nil, fmt.Errorf("failed to derive a key from the password: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create the AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create the GCM mode: %w", err)
	}

	return aead, nil
}

// checkScryptParameters rejects what scrypt itself rejects and, on top of that,
// what would take more memory than maxScryptMemory allows.
func checkScryptParameters(logN int, r int, p int) error {
	if logN < 1 || logN > 62 || r < 1 || p < 1 {
		return fmt.Errorf("the encrypted value states unusable scrypt parameters (N=2^%d, r=%d, p=%d): %w", logN, r, p, ErrPasswordEncryptedDamaged)
	}

	// 128 * r * N is what scrypt allocates. It is computed in this order and
	// against a limit well below the word size, so the shift cannot wrap.
	if logN > 40 || int64(128*r)<<logN > maxScryptMemory {
		return fmt.Errorf("the encrypted value asks for more than %d bytes of memory (N=2^%d, r=%d): %w", maxScryptMemory, logN, r, ErrPasswordEncryptedDamaged)
	}

	return nil
}
