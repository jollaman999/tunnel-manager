//go:build !windows

package install

import "os"

// hasPrivilege says whether this process may write where an install writes.
//
// It is the effective user and not the real one, because that is the identity
// the kernel checks every one of these writes against: a binary run through
// sudo has a real user of whoever typed it.
//
// Nothing finer is asked. The paths an install writes are /usr/local/bin and
// the data directory, and the service manager takes a registration from root
// only, so a process that is not root fails at some step of this whether or not
// it happens to own one of the directories.
func hasPrivilege() bool {
	return os.Geteuid() == 0
}

// privilegeHint is what is put in front of the operator when it is not. It is
// one line and it names the command, because this is read by somebody who ran
// an install that did nothing.
const privilegeHint = "Run it again through sudo, or as root."
