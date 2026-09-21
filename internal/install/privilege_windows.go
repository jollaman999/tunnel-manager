//go:build windows

package install

import "golang.org/x/sys/windows"

// hasPrivilege says whether this process may write where an install writes.
//
// Windows has no single number to compare the way the Unix side compares the
// effective user. What decides it is whether the token this process carries has
// the built-in Administrators group in it: that group is what may write under
// Program Files, and it is what the service control manager takes a
// registration from.
//
// The answer is the one that matters under UAC rather than the one about the
// account. An administrator who started an ordinary console has the group in
// their token for deny only, and a deny-only group is not a membership as far
// as this call is concerned, so this says no until the process was started
// elevated - which is exactly the moment the writes below would begin to
// succeed.
//
// Not verified on Windows: this has been built for windows and never run on it.
// What it reports for an elevated prompt, for an ordinary one and for the
// service running as LocalSystem has to be checked on a Windows machine.
func hasPrivilege() bool {
	var administrators *windows.SID

	// The group is named by the authority and the two sub-authorities it is
	// built from rather than by a string, because the string it is shown under
	// is translated: it is Administrators on an English Windows and something
	// else on the rest, while these numbers are the same everywhere.
	err := windows.AllocateAndInitializeSid(&windows.SECURITY_NT_AUTHORITY, 2,
		windows.SECURITY_BUILTIN_DOMAIN_RID, windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0, &administrators)
	if err != nil {
		return false
	}

	defer func() {
		_ = windows.FreeSid(administrators)
	}()

	// The zero token is how this asks about the process itself: with no token
	// handed over, the check is made against the token of this thread, and
	// against the process token when the thread is not impersonating anybody,
	// which no thread of this program does.
	member, err := windows.Token(0).IsMember(administrators)
	if err != nil {
		// A check that could not be made is not a permission that was granted.
		// The steps of an install are several, and the one that would fail
		// first is the one that has already copied a binary into Program Files.
		return false
	}

	return member
}

// privilegeHint is what is put in front of the operator when it is not. It is
// one line and it names what to do, because this is read by somebody who ran an
// install that did nothing.
const privilegeHint = "Start a PowerShell or a Command Prompt with Run as administrator and run it again from there."
