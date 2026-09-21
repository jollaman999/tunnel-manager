//go:build windows

package install

// hasPrivilege says whether this process may write where an install writes.
//
// Windows has no single number to compare the way the Unix side compares the
// effective user: what decides this is whether the token of the process carries
// the Administrators group with it, and asking that means asking the operating
// system, which is the same dependency the SCM backend brings in.
//
// So it stands here refusing, and the backend that talks to the SCM is what
// replaces it with the real check. Refusing is the answer that costs nothing
// while there is no backend to register a service with anyway, and it is the
// only one of the two answers that cannot leave a machine with an executable
// copied into Program Files by a process that was never going to be allowed to
// finish the rest.
func hasPrivilege() bool {
	return false
}

// privilegeHint is what is put in front of the operator. It says both things
// that are true right now: an install needs an elevated prompt, and this build
// refuses it either way.
const privilegeHint = "Run it again from a Command Prompt or PowerShell started with Run as administrator. " +
	"This build cannot install as a Windows service yet in any case."
