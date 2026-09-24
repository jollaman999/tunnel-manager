// Package auth sets up the single account the API is served behind.
package auth

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// initialPasswordFileName is the name of the file the initial password is
// written to. It sits next to the database file, which is the directory this
// installation keeps its data in and the one an operator already has to reach.
const initialPasswordFileName = "initial-password"

// initialPasswordBytes is how much randomness an initial password carries.
const initialPasswordBytes = 32

// initialPasswordFileMode is the permission the initial password file is
// created with and the mode it is set back to after the umask has been applied.
const initialPasswordFileMode os.FileMode = 0600

// initialPasswordEncoding spells the random bytes in upper case letters and
// digits without padding, so the password can be read off a screen and typed
// back in. The standard base32 alphabet leaves out 0, 1 and 8, which are the
// characters that are read as O, I and B.
var initialPasswordEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// InitialPasswordFile returns the path the initial password is written to. It
// is derived from the database file the process was started with, so the file
// turns up in the directory the operator pointed the process at and no setting
// has to name it. That directory exists, since the database was opened in it.
func InitialPasswordFile(databaseFile string) string {
	return filepath.Join(filepath.Dir(databaseFile), initialPasswordFileName)
}

// GenerateInitialPassword returns a password made of initialPasswordBytes of
// randomness.
func GenerateInitialPassword() (string, error) {
	raw := make([]byte, initialPasswordBytes)

	_, err := io.ReadFull(rand.Reader, raw)
	if err != nil {
		return "", fmt.Errorf("failed to generate an initial password: %w", err)
	}

	return initialPasswordEncoding.EncodeToString(raw), nil
}

// HashPassword returns the bcrypt hash of password.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash the password: %w", err)
	}

	return string(hash), nil
}

// CheckPassword reports whether password is the one hash was made from.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// writeInitialPasswordFile stores password at path with initialPasswordFileMode.
// The directory is not created: it is the one the configuration file was read
// from, so it is there, and a path that cannot be written to is reported as it
// is instead of being made somewhere else.
//
// The password is only ever written to a file this call created itself, which
// is what O_EXCL is for. Opening whatever is at the path and narrowing it
// afterwards writes the password into a file whose mode somebody else chose,
// and anyone that mode let read it has the account until the narrowing lands.
// Narrowing before the write does not close that either, since a file another
// user owns is one this process may not chmod at all, and a path that is a
// symlink is one whose target gets truncated and written to. A create that
// fails because something is at the path writes nothing at all.
//
// crypto.CreatePrivateFile does the create. On Windows the mode reaches only the
// read-only attribute, so the file is created with the DACL the key file gets,
// which lets only its owner, SYSTEM and Administrators read it, instead of the
// DACL of its directory.
//
// A file that is already there is removed and the create is tried once more.
// This is reached only while no account row exists, so such a file is a
// leftover of an earlier startup and the password in it opens nothing, while
// stopping on it would leave an installation that can never set its account
// up. Removing takes write permission on the directory, which is the same
// permission the create takes. Should the path be taken again between the
// removal and the second create, that create fails and the startup stops,
// which is the outcome the password is not handed out in.
func writeInitialPasswordFile(path, password string) error {
	f, err := crypto.CreatePrivateFile(path)
	if errors.Is(err, os.ErrExist) {
		err = os.Remove(path)
		if err != nil {
			return fmt.Errorf("failed to remove the initial password file %s that was already there: %w", path, err)
		}

		f, err = crypto.CreatePrivateFile(path)
	}
	if err != nil {
		return fmt.Errorf("failed to create the initial password file %s: %w", path, err)
	}

	// The mode passed to OpenFile is masked by umask, which only takes bits
	// away, so the file was created no wider than initialPasswordFileMode.
	// This puts back what the umask took, and it runs while the file is still
	// empty so that the password is never in a file of any other mode.
	err = f.Chmod(initialPasswordFileMode)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to set the permission of the initial password file %s: %w", path, err)
	}

	_, err = f.WriteString(password + "\n")
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to write the initial password file %s: %w", path, err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("failed to close the initial password file %s: %w", path, err)
	}

	return nil
}

// EnsureUser creates the account when there is none, and reports whether it
// created one. The account is made without a username and with SetupRequired
// set, and its initial password is written to passwordFile and nowhere else.
// An account that is already there is left alone, password and all.
func EnsureUser(db *gorm.DB, passwordFile string) (bool, error) {
	var count int64

	err := db.Model(&models.User{}).Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("failed to count the accounts: %w", err)
	}

	if count > 0 {
		return false, nil
	}

	password, err := GenerateInitialPassword()
	if err != nil {
		return false, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}

	// The file is written before the row, because the password exists nowhere
	// else. A row without the file would be an account that nobody can log into
	// and that keeps the next startup from making another one. The other order
	// leaves a file whose password opens nothing, which the next startup writes
	// over.
	err = writeInitialPasswordFile(passwordFile, password)
	if err != nil {
		return false, err
	}

	user := models.User{PasswordHash: hash, SetupRequired: true}

	err = db.Create(&user).Error
	if err != nil {
		return false, fmt.Errorf("failed to create the account: %w", err)
	}

	return true, nil
}
