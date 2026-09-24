//go:build windows

package crypto

import (
	"fmt"
	"os"
	"slices"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"

	"github.com/jollaman999/tunnel-manager/internal/logid"
)

// keyFileReadAccess are the rights that let whoever holds one of them read the
// key. The generic ones are what an ACE may still carry unmapped, and the right
// to change the DACL or the owner is a right to grant oneself the read.
const keyFileReadAccess = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL |
	windows.MAXIMUM_ALLOWED | windows.WRITE_DAC | windows.WRITE_OWNER

// keyFileSecurityDescriptor is the one security descriptor a key file is given:
// a protected DACL, so nothing is inherited from the directory it sits in, that
// gives full access to the user this process runs as, to SYSTEM and to
// Administrators and to nobody else.
//
// SYSTEM is there for the service, which runs as SYSTEM and has to read a key
// that was first made by a user who started the program by hand. Administrators
// are there because they can take the file over anyway, and a key they cannot
// read is one they cannot back up. When this process runs as SYSTEM or its
// user is Administrators the same SID is not written twice.
//
// os.OpenFile and Chmod do not do this on Windows: the mode reaches only the
// read-only attribute, and the file takes the DACL of its directory, which
// under C:\ lets every user read it.
func keyFileSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("failed to read the user this process runs as: %w", err)
	}

	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("failed to build the SID of SYSTEM: %w", err)
	}

	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("failed to build the SID of Administrators: %w", err)
	}

	if !user.User.Sid.Equals(system) && !user.User.Sid.Equals(admins) {
		sddl += fmt.Sprintf("(A;;FA;;;%s)", user.User.Sid.String())
	}

	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("failed to build the security descriptor of the key file: %w", err)
	}

	return sd, nil
}

// CreatePrivateFile creates a file for a secret, the key file or any other,
// with keyFileSecurityDescriptor already on it, so there is no moment in which
// the secret is on disk under the DACL of its directory. CREATE_NEW refuses a
// file that is already there, as O_EXCL does.
func CreatePrivateFile(path string) (*os.File, error) {
	sd, err := keyFileSecurityDescriptor()
	if err != nil {
		return nil, err
	}

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}

	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))

	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, &sa,
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}

	return os.NewFile(uintptr(handle), path), nil
}

// checkKeyFilePermission narrows a key file that others than the ones
// keyFileSecurityDescriptor names may read, and never refuses one.
//
// os.Stat reports no Unix mode on Windows: every file reads as 0666, or 0444
// when the read-only attribute is set, whatever its ACL allows. So the DACL is
// read instead. A key file made before this package set one, or copied in from
// elsewhere, carries the DACL of its directory, and refusing it would stop a
// startup that worked the day before. It is narrowed and the line says so. A
// narrowing that fails leaves the startup going as well, with the line saying
// that the file is still readable.
func checkKeyFilePermission(path string, info os.FileInfo, logger *zap.Logger) error {
	readers, err := NarrowPrivateFile(path)
	if err != nil {
		logger.Warn("failed to narrow the key file to its owner, SYSTEM and Administrators, "+
			"so other accounts may be able to read it",
			logid.EncryptionKeyFileNarrowFailed.Field(),
			zap.String("key_file", path),
			zap.Strings("readers", readers),
			zap.Error(err))

		return nil
	}

	if len(readers) == 0 {
		return nil
	}

	logger.Warn("other accounts could read the key file, so it was narrowed to its owner, SYSTEM and Administrators",
		logid.EncryptionKeyFileNarrowed.Field(),
		zap.String("key_file", path),
		zap.Strings("readers", readers))

	return nil
}

// NarrowPrivateFile puts the DACL of keyFileSecurityDescriptor on a file of
// secrets that others than the ones it names may read, and returns those
// others. It returns none and changes nothing when the file is narrow already.
//
// This is what a file created before CreatePrivateFile was used for it is
// brought to: such a file carries the DACL of its directory. The accounts are
// returned on a failed narrowing too, so that the caller can say who may still
// read the file. A file that is not there fails with an error that is
// os.ErrNotExist.
func NarrowPrivateFile(path string) ([]string, error) {
	readers, err := keyFileOtherReaders(path)
	if err != nil {
		return nil, err
	}

	if len(readers) == 0 {
		return nil, nil
	}

	err = narrowKeyFile(path)
	if err != nil {
		return readers, err
	}

	return readers, nil
}

// keyFileOtherReaders names the accounts other than the ones
// keyFileSecurityDescriptor names that the DACL of the file lets read it. An
// entry it cannot read the account out of counts as one, since what it grants
// is not known.
func keyFileOtherReaders(path string) ([]string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("failed to read the DACL of %s: %w", path, err)
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("failed to read the DACL of %s: %w", path, err)
	}

	if dacl == nil {
		return []string{"Everyone (no DACL)"}, nil
	}

	allowed, err := keyFileSecurityDescriptor()
	if err != nil {
		return nil, err
	}

	allowedDACL, _, err := allowed.DACL()
	if err != nil {
		return nil, fmt.Errorf("failed to read the DACL of the key file security descriptor: %w", err)
	}

	allowedSIDs, err := aceSIDs(allowedDACL)
	if err != nil {
		return nil, err
	}

	var readers []string

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE

		err = windows.GetAce(dacl, i, &ace)
		if err != nil {
			return nil, fmt.Errorf("failed to read entry %d of the DACL of %s: %w", i, path, err)
		}

		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			readers = append(readers, fmt.Sprintf("an entry of type %d", ace.Header.AceType))

			continue
		}

		if ace.Mask&keyFileReadAccess == 0 {
			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))

		if slices.ContainsFunc(allowedSIDs, sid.Equals) {
			continue
		}

		name := accountName(sid)
		if !slices.Contains(readers, name) {
			readers = append(readers, name)
		}
	}

	return readers, nil
}

// aceSIDs reads the SIDs out of the entries of an ACL built here.
func aceSIDs(acl *windows.ACL) ([]*windows.SID, error) {
	sids := make([]*windows.SID, 0, acl.AceCount)

	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE

		err := windows.GetAce(acl, i, &ace)
		if err != nil {
			return nil, fmt.Errorf("failed to read entry %d of the key file DACL: %w", i, err)
		}

		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}

	return sids, nil
}

// accountName is the DOMAIN\account the SID stands for, or the SID itself when
// it names no account this machine can look up.
func accountName(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return sid.String()
	}

	if domain == "" {
		return account
	}

	return domain + `\` + account
}

// narrowKeyFile puts the DACL of keyFileSecurityDescriptor on the file and
// cuts it off from what its directory hands down.
func narrowKeyFile(path string) error {
	sd, err := keyFileSecurityDescriptor()
	if err != nil {
		return err
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("failed to read the DACL of the key file security descriptor: %w", err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("failed to set the DACL of %s: %w", path, err)
	}

	return nil
}
