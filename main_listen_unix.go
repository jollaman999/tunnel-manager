//go:build !windows

package main

import (
	"errors"
	"net"
	"syscall"
)

// listenAPIPort opens one address for the API. Here a socket that holds the
// port on any address already makes the bind fail, so a plain listen is
// enough.
func listenAPIPort(address string) (net.Listener, error) {
	return net.Listen("tcp", address)
}

// portUnavailable says whether a listen failed because another socket holds the
// address, so that another port may be tried. EACCES is left out: here it means
// a port below 1024 without the privilege for it, which is a setting to fix.
func portUnavailable(err error) bool {
	return portUnavailableReason(err) != ""
}

// portUnavailableReason is why portUnavailable reads the failure as a port to
// move away from, or empty when it does not. Here the only reason is a port
// another socket holds.
func portUnavailableReason(err error) string {
	if errors.Is(err, syscall.EADDRINUSE) {
		return apiPortInUse
	}

	return ""
}
