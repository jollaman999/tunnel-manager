//go:build !windows

package tunnel

import (
	"math"
	"syscall"
)

// forwardConnCeilingUnread is the ceiling taken when the soft limit cannot be
// read. It is the soft limit main raises the process to (checkUlimit).
const forwardConnCeilingUnread = 65535

// forwardConnCeiling returns the soft limit on descriptors of this process.
//
// The fields of syscall.Rlimit are int64 on some Unix systems and uint64 on
// others, so the value is converted before it is compared, and an unlimited
// one, which is the largest value of either, is held to what an int64 takes.
func forwardConnCeiling() int64 {
	var rLimit syscall.Rlimit

	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return forwardConnCeilingUnread
	}

	current := uint64(rLimit.Cur)
	if current > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(current)
}
