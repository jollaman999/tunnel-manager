//go:build !windows

package main

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// Only a taken port moves the start to another one. EACCES is a port below 1024
// without the privilege for it, which is a setting to fix.
func TestPortUnavailableTakesOnlyATakenPort(t *testing.T) {
	cases := []struct {
		errno syscall.Errno
		want  bool
	}{
		{syscall.EADDRINUSE, true},
		{syscall.EACCES, false},
	}

	for _, c := range cases {
		err := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", c.errno)}
		if got := portUnavailable(err); got != c.want {
			t.Errorf("a listen that failed with %v is read as unavailable %v, want %v", c.errno, got, c.want)
		}

		wantReason := ""
		if c.want {
			wantReason = apiPortInUse
		}

		if got := portUnavailableReason(err); got != wantReason {
			t.Errorf("a listen that failed with %v gives the reason %q, want %q", c.errno, got, wantReason)
		}
	}
}
