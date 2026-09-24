//go:build windows

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE, which winsock2.h defines as the
// complement of SO_REUSEADDR and x/sys/windows does not name. It is the value
// main_listen_windows.go names for the API port.
const soExclusiveAddrUse = ^windows.SO_REUSEADDR

// openLocalListeners opens the local port of the bind scope with
// SO_EXCLUSIVEADDRUSE and reports which of its addresses it reaches.
//
// Windows lets a socket bind a port another program already holds on an
// address that overlaps it, even with SO_EXCLUSIVEADDRUSE, so long as the new
// socket is not the one covering both: over a program holding [::] for both
// families, 0.0.0.0 and a [::] for IPv6 alone both bind. A port another
// program holds would then open here without an error, and its clients would
// keep reaching that program. A dual-stack socket on [::] with
// SO_EXCLUSIVEADDRUSE is refused with WSAEADDRINUSE whichever address the port
// is held on, so the wildcard scope is opened as that one socket, which takes
// IPv4 and IPv6 alike.
//
// The loopback scope has no single socket that covers both of its addresses,
// so 127.0.0.1 and ::1 are opened apart the way they are on other systems,
// each with SO_EXCLUSIVEADDRUSE.
func openLocalListeners(pair localPair) (*openForwards, error) {
	if !pair.v4.IP.IsUnspecified() {
		return openEachAddress(pair, listenExclusive)
	}

	dual, errDual := listenExclusive("tcp", pair.v6)
	if errDual == nil {
		return &openForwards{v6: dual, reach: openReachBoth}, nil
	}

	// A Windows without IPv6 refuses the IPv6 socket itself with
	// WSAEAFNOSUPPORT, before any address is bound, and only that is read as
	// a machine to open the IPv4 wildcard alone on. Any other refusal of the
	// dual-stack socket is the port being unavailable, and opening 0.0.0.0
	// after it would bind over the program that holds [::].
	if !errors.Is(errDual, windows.WSAEAFNOSUPPORT) {
		return nil, fmt.Errorf("the bind scope could not be opened on this machine (%s: %v)", pair.v6, errDual)
	}

	listenerV4, errV4 := listenExclusive("tcp4", pair.v4)
	if errV4 != nil {
		return nil, fmt.Errorf("neither address of the bind scope could be opened on this machine (%s: %v; %s: %v)",
			pair.v4, errV4, pair.v6, errDual)
	}

	return &openForwards{v4: listenerV4, reach: openReachV4, refused: errDual}, nil
}

// listenExclusive opens address on network with SO_EXCLUSIVEADDRUSE.
//
// A "tcp" listen on [::] is a socket of both families: net opens a wildcard
// listen that names no family as AF_INET6 with IPV6_V6ONLY cleared
// (favoriteAddrFamily, setDefaultSockopts).
func listenExclusive(network string, address *net.TCPAddr) (net.Listener, error) {
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

	return config.Listen(context.Background(), network, address.String())
}
