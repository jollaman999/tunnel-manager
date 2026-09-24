//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// portUnavailable says whether a listen failed because the port is held by
// something other than this process, so that another port may be tried.
//
// Winsock reports a port another socket holds as WSAEADDRINUSE. syscall.EADDRINUSE
// is a value the syscall package made up for Windows, which no socket call
// returns, so testing for it would never find a taken port here.
//
// A port the system keeps for itself, one http.sys holds or one in a range
// reserved for Hyper-V or WinNAT, and a port another socket holds for itself
// alone are reported as WSAEACCES. Windows asks no privilege for a port below
// 1024, so there is no setting of this program that WSAEACCES points at, and it
// is read the same way as a taken port.
func portUnavailable(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}
