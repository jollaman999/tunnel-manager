//go:build windows

package main

import (
	"context"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE, which winsock2.h defines as the
// complement of SO_REUSEADDR and x/sys/windows does not name.
const soExclusiveAddrUse = ^windows.SO_REUSEADDR

// listenAPIPort opens one address for the API with SO_EXCLUSIVEADDRUSE.
//
// Without it Windows lets a dual-stack socket on [::] bind a port another
// program holds on 0.0.0.0 or 127.0.0.1 alone, and IPv4 clients of that port
// keep reaching the other program while this one reports that it is serving.
// With it the bind fails with WSAEADDRINUSE whichever address the other socket
// holds the port on, so the port is passed over as a taken one.
func listenAPIPort(address string) (net.Listener, error) {
	config := net.ListenConfig{
		Control: func(_, _ string, conn syscall.RawConn) error {
			var optErr error

			err := conn.Control(func(fd uintptr) {
				optErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, soExclusiveAddrUse, 1)
			})
			if err != nil {
				return err
			}

			return optErr
		},
	}

	return config.Listen(context.Background(), "tcp", address)
}

// portUnavailable says whether a listen failed because the port is held by
// something other than this process, so that another port may be tried.
func portUnavailable(err error) bool {
	return portUnavailableReason(err) != ""
}

// portUnavailableReason is why portUnavailable reads the failure as a port to
// move away from, or empty when it does not.
//
// Winsock reports a port another socket holds as WSAEADDRINUSE. syscall.EADDRINUSE
// is a value the syscall package made up for Windows, which no socket call
// returns, so testing for it would never find a taken port here.
//
// A port the system keeps for itself, one http.sys holds or one in a range
// reserved for Hyper-V or WinNAT, and a port another socket holds for itself
// alone are reported as WSAEACCES. Windows asks no privilege for a port below
// 1024, so there is no setting of this program that WSAEACCES points at, and it
// is read as a port to move away from as well, under a reason of its own.
func portUnavailableReason(err error) string {
	switch {
	case errors.Is(err, windows.WSAEADDRINUSE):
		return apiPortInUse
	case errors.Is(err, windows.WSAEACCES):
		return apiPortReserved
	}

	return ""
}
