package main

import (
	"net"
	"runtime"
	"strconv"
	"testing"
)

// freeLoopbackPort returns a port nothing listens on at the moment it returns.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}

	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	return port
}

// holdLoopbackPort opens a port for the length of the test, the way another
// program holding the stored port would.
func holdLoopbackPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open a port to hold: %v", err)
	}

	t.Cleanup(func() {
		_ = l.Close()
	})

	return l.Addr().(*net.TCPAddr).Port
}

func avoidNothing(int) bool {
	return false
}

func TestListenAPIOpensTheStoredPortWhenItIsFree(t *testing.T) {
	stored := freeLoopbackPort(t)

	l, port, err := listenAPI("127.0.0.1", stored, func(int) bool {
		t.Fatalf("avoid was asked while the stored port was free")
		return false
	})
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port != stored || l.Addr().(*net.TCPAddr).Port != stored {
		t.Fatalf("opened %s and reported %d, want the stored port %d", l.Addr(), port, stored)
	}
}

func TestListenAPIOpensAnotherPortWhenTheStoredOneIsTaken(t *testing.T) {
	stored := holdLoopbackPort(t)

	l, port, err := listenAPI("127.0.0.1", stored, avoidNothing)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port == stored {
		t.Fatalf("reported the taken port %d", stored)
	}

	if l.Addr().(*net.TCPAddr).Port != port {
		t.Fatalf("opened %s and reported %d", l.Addr(), port)
	}
}

func TestListenAPIPicksAgainWhileAvoidRefuses(t *testing.T) {
	stored := holdLoopbackPort(t)

	refused := map[int]bool{}

	l, port, err := listenAPI("127.0.0.1", stored, func(p int) bool {
		if len(refused) < 3 {
			refused[p] = true
			return true
		}

		return false
	})
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if len(refused) != 3 {
		t.Fatalf("avoid refused %d distinct ports, want 3: %v", len(refused), refused)
	}

	if refused[port] || port == stored {
		t.Fatalf("settled on %d, which was refused or is the stored %d", port, stored)
	}

	// The refused ones are let go once a port is settled on.
	for p := range refused {
		again, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			t.Fatalf("refused port %d is still held: %v", p, err)
		}
		_ = again.Close()
	}
}

func TestListenAPIGivesUpWithTheStoredPortError(t *testing.T) {
	stored := holdLoopbackPort(t)

	asked := 0

	l, port, err := listenAPI("127.0.0.1", stored, func(int) bool {
		asked++
		return true
	})
	if err == nil {
		_ = l.Close()
		t.Fatalf("opened port %d while avoid refused every one", port)
	}

	if !addressInUse(err) {
		t.Fatalf("the error is not the one of the taken stored port: %v", err)
	}

	if asked != apiListenAttempts {
		t.Fatalf("avoid was asked %d times, want %d", asked, apiListenAttempts)
	}
}

// A port below 1024 is refused to a process without the privilege for it,
// which is a failure another port must not hide.
func TestListenAPILeavesOtherFailuresAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows lets any process open a port below 1024")
	}

	l, err := net.Listen("tcp", "127.0.0.1:1")
	if err == nil {
		_ = l.Close()
		t.Skip("this process may open port 1, so there is no refusal to observe")
	}

	if addressInUse(err) {
		t.Skip("port 1 is taken here rather than refused")
	}

	l, port, err := listenAPI("127.0.0.1", 1, func(int) bool {
		t.Fatalf("avoid was asked for a failure that was not a taken port")
		return false
	})
	if err == nil {
		_ = l.Close()
		t.Fatalf("opened port %d in place of a refused one", port)
	}

	if port != 1 {
		t.Fatalf("reported port %d, want the stored 1", port)
	}
}
