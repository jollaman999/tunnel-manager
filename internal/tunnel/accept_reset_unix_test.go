//go:build !windows

package tunnel

import "syscall"

// acceptResetErrno is the errno Accept returns bare when a connection was
// reset before it was taken off the queue, which net treats as temporary on an
// accept (isConnError in net/error_unix.go).
const acceptResetErrno = syscall.ECONNRESET
