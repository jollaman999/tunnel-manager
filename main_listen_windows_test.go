//go:build windows

package main

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// A port in a range the system reserved is refused with WSAEACCES, and that is
// read as a port to move away from, the same as one another socket holds.
func TestPortUnavailableTakesAReservedPort(t *testing.T) {
	for _, errno := range []windows.Errno{windows.WSAEADDRINUSE, windows.WSAEACCES} {
		err := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
		if !portUnavailable(err) {
			t.Errorf("a listen that failed with %d is not read as an unavailable port", errno)
		}
	}
}

// A port another socket holds and one the system refused are both moved away
// from, and the log says which of the two it was.
func TestPortUnavailableReasonTellsATakenPortFromAReservedOne(t *testing.T) {
	cases := []struct {
		errno windows.Errno
		want  string
	}{
		{windows.WSAEADDRINUSE, apiPortInUse},
		{windows.WSAEACCES, apiPortReserved},
		{windows.WSAEINVAL, ""},
	}

	for _, c := range cases {
		err := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", c.errno)}
		if got := portUnavailableReason(err); got != c.want {
			t.Errorf("a listen that failed with %d gives the reason %q, want %q", c.errno, got, c.want)
		}
	}
}
