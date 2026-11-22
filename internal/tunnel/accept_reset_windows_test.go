//go:build windows

package tunnel

import "syscall"

// acceptResetErrno is the errno Accept returns bare when a connection was
// reset before it was taken off the queue. On Windows that is the Winsock
// code, and it is the one net treats as temporary on an accept (isConnError in
// net/error_windows.go). syscall.ECONNRESET there is a number Go made up for
// the name, which no socket call returns and net does not look for.
const acceptResetErrno = syscall.WSAECONNRESET
