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
