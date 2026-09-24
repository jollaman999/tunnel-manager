//go:build windows

package database

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// widenDir puts on dir what a directory under C:\ hands down: Users may read,
// and every entry is handed on to what is created inside.
func widenDir(t *testing.T, dir string) {
	t.Helper()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("failed to read the user of this process: %v", err)
	}

	wide, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() +
		")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;BU)")
	if err != nil {
		t.Fatalf("failed to build a wide security descriptor: %v", err)
	}

	wideDACL, _, err := wide.DACL()
	if err != nil {
		t.Fatalf("failed to read the wide DACL: %v", err)
	}

	err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, wideDACL, nil)
	if err != nil {
		t.Fatalf("failed to widen %s: %v", dir, err)
	}
}

// otherSIDs returns the SIDs other than the user of this process, SYSTEM and
// Administrators that the DACL of path lets in, whether the entries are its own
// or handed down.
func otherSIDs(t *testing.T, path string) []string {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("failed to read the security descriptor of %s: %v", path, err)
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("failed to read the DACL of %s: %v", path, err)
	}

	if dacl == nil {
		return []string{"Everyone (no DACL)"}
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("failed to read the user of this process: %v", err)
	}

	allowed := map[string]bool{"S-1-5-18": true, "S-1-5-32-544": true, user.User.Sid.String(): true}

	var others []string

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE

		err = windows.GetAce(dacl, i, &ace)
		if err != nil {
			t.Fatalf("failed to read entry %d of the DACL of %s: %v", i, path, err)
		}

		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !allowed[sid] {
			others = append(others, sid)
		}
	}

	return others
}

// TestNewDatabaseMakesItsDirectoryAndFilesForOwnerSystemAndAdminsOnly covers a
// data directory this program makes under one that lets Users read: the
// directory, the database file and the files SQLite makes beside it while the
// database is open are all kept to the owner, SYSTEM and Administrators.
func TestNewDatabaseMakesItsDirectoryAndFilesForOwnerSystemAndAdminsOnly(t *testing.T) {
	parent := t.TempDir()
	widenDir(t, parent)

	path := filepath.Join(parent, "data", "tunnel-manager.db")

	db, _, err := NewDatabase(path, zap.NewNop(), "error")
	if err != nil {
		t.Fatalf("NewDatabase returned an error: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	for _, p := range append([]string{filepath.Dir(path)}, Files(path)...) {
		_, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s is not there while the database is open: %v", p, err)

			continue
		}

		others := otherSIDs(t, p)
		if len(others) != 0 {
			t.Errorf("%s lets %v in", p, others)
		}
	}
}

// TestNewDatabaseKeepsItsFilesNarrowInADirectoryItDidNotMake covers -db
// pointed at a directory the operator made: its DACL is left as it is, and the
// files this program puts in it are narrow all the same.
func TestNewDatabaseKeepsItsFilesNarrowInADirectoryItDidNotMake(t *testing.T) {
	dir := t.TempDir()
	widenDir(t, dir)

	path := filepath.Join(dir, "tunnel-manager.db")

	db, _, err := NewDatabase(path, zap.NewNop(), "error")
	if err != nil {
		t.Fatalf("NewDatabase returned an error: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	if len(otherSIDs(t, dir)) == 0 {
		t.Error("NewDatabase narrowed a directory it did not make")
	}

	for _, p := range Files(path) {
		_, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s is not there while the database is open: %v", p, err)

			continue
		}

		others := otherSIDs(t, p)
		if len(others) != 0 {
			t.Errorf("%s lets %v in", p, others)
		}
	}

	// The rows written go through the write-ahead log, which has to be the
	// file made here and not one SQLite put in its place.
	err = db.Exec("CREATE TABLE probe (value TEXT)").Error
	if err != nil {
		t.Fatalf("failed to write to the database: %v", err)
	}

	others := otherSIDs(t, path+"-wal")
	if len(others) != 0 {
		t.Errorf("the write-ahead log lets %v in after a write", others)
	}
}
