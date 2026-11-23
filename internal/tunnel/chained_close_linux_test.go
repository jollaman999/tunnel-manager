// The test runs on Linux alone. It finds the socket of the client among the
// descriptors listed under /dev/fd, which lists every open descriptor on Linux
// and is not relied on to do so elsewhere.

package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
)

// stallingProxy passes TCP between a client and one address, and on stall
// stops reading what the client sends, so the client's writes fill the socket
// buffers and then block, the way they do toward a Host that stopped reading.
//
// Both buffers on the way are made small. The SSH channel the client writes
// into lets 2 MiB through before it waits for the far end (x/crypto/ssh,
// channelWindowSize), and a loopback socket left to size its own send buffer
// grows it past that. A client with that much room stops on the channel window
// with its socket still taking writes, which is not the state being tested.
// shrunk counts the clients whose send buffer was made small. One that closed
// before it was looked for has no socket left and is not counted.
type stallingProxy struct {
	addr    string
	stalled atomic.Bool
	carried atomic.Int64
	shrunk  atomic.Int64
}

func startStallingProxy(t *testing.T, to string) *stallingProxy {
	t.Helper()

	lc := net.ListenConfig{
		Control: func(_, _ string, raw syscall.RawConn) error {
			var serr error
			err := raw.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	p := &stallingProxy{addr: ln.Addr().String()}

	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}

			if shrinkSendBuffer(client.RemoteAddr().(*net.TCPAddr), client.LocalAddr().(*net.TCPAddr)) == nil {
				p.shrunk.Add(1)
			}

			upstream, err := net.Dial("tcp", to)
			if err != nil {
				_ = client.Close()
				continue
			}

			mu.Lock()
			conns = append(conns, client, upstream)
			mu.Unlock()

			go func() {
				_, _ = io.Copy(client, upstream)
			}()
			go func() {
				defer func() {
					_ = upstream.Close()
				}()

				buf := make([]byte, 4096)
				for {
					for p.stalled.Load() {
						time.Sleep(10 * time.Millisecond)
					}
					n, err := client.Read(buf)
					if n > 0 {
						p.carried.Add(int64(n))
						if _, err := upstream.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()

	return p
}

// shrinkSendBuffer sets a small send buffer on the socket of this process that
// is connected from local to peer. The client the proxy serves dials on its
// own and hands nothing of its socket out, so the socket is found among the
// descriptors of the process by its two addresses.
func shrinkSendBuffer(local, peer *net.TCPAddr) error {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return err
	}

	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		if !socketAt(fd, local, peer) {
			continue
		}

		return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096)
	}

	return fmt.Errorf("no socket of this process is connected from %s to %s", local, peer)
}

// socketAt is whether fd is a TCP socket connected from local to peer.
func socketAt(fd int, local, peer *net.TCPAddr) bool {
	matches := func(sa syscall.Sockaddr, want *net.TCPAddr) bool {
		in4, ok := sa.(*syscall.SockaddrInet4)
		return ok && in4.Port == want.Port && net.IP(in4.Addr[:]).Equal(want.IP)
	}

	sa, err := syscall.Getsockname(fd)
	if err != nil || !matches(sa, local) {
		return false
	}

	sa, err = syscall.Getpeername(fd)

	return err == nil && matches(sa, peer)
}

// startSink is a TCP listener that reads and drops whatever it is sent.
func startSink(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()

	return ln.Addr().String()
}

// startStream is a TCP listener that sends to whatever connects to it without
// end, which is a service that has more to say than its reader takes.
func startStream(t *testing.T) (string, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		buf := make([]byte, 32*1024)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() {
					_ = conn.Close()
				}()
				for {
					if _, err := conn.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)

	return addr.IP.String(), addr.Port
}

// waitStalled waits for moved to stop going up for a second.
func waitStalled(t *testing.T, what string, moved func() int64) {
	t.Helper()

	last := moved()
	since := time.Now()
	waitFor(t, localTestTimeout, what, func() bool {
		now := moved()
		if now != last {
			last = now
			since = time.Now()
			return false
		}
		return time.Since(since) > time.Second
	})
}

// returnsWithin runs call and reports whether it came back within limit. When
// it did not, the stacks of every goroutine are logged, the proxy is let go so
// what call waits on can finish, and call is waited for, so a failure does not
// leave the test hung.
func returnsWithin(t *testing.T, what string, limit time.Duration, proxy *stallingProxy, call func()) bool {
	t.Helper()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		call()
	}()

	select {
	case <-done:
		t.Logf("%s returned in %v", what, time.Since(start))
		return true
	case <-time.After(limit):
	}

	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	t.Logf("%s did not return within %v\n%s", what, limit, buf[:n])

	proxy.stalled.Store(false)
	select {
	case <-done:
		t.Logf("%s returned %v after it was called, once the proxy was let go", what, time.Since(start))
	case <-time.After(localTestTimeout):
		t.Logf("%s did not return even after the proxy was let go", what)
	}

	return false
}

// TestClosingARouteIsNotHeldByAHostPassedThroughThatStoppedReading is the
// client at the end of a route of two, closed while what it sends toward the
// Host passed through can no longer be written because that Host stopped
// reading.
func TestClosingARouteIsNotHeldByAHostPassedThroughThatStoppedReading(t *testing.T) {
	jump := startJumpTestServer(t)
	target := startJumpTestServer(t)
	proxy := startStallingProxy(t, jump.addr)
	sink := startSink(t)

	client, _, err := dialSSHClient([]sshHop{
		{hostID: 2, addr: proxy.addr, config: testClientConfig(localTestTimeout)},
		{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
	})
	if err != nil {
		t.Fatalf("failed to connect over the route: %v", err)
	}
	defer func() {
		proxy.stalled.Store(false)
		_ = client.Close()
	}()

	conn, err := client.Dial("tcp", sink)
	if err != nil {
		t.Fatalf("failed to open a channel over the route: %v", err)
	}

	var written atomic.Int64
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Write(buf)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()

	waitFor(t, localTestTimeout, "data to flow over the route", func() bool {
		return written.Load() > 1<<20
	})

	if proxy.shrunk.Load() == 0 {
		t.Fatal("the send buffer of the client was not made small")
	}

	proxy.stalled.Store(true)
	waitStalled(t, "the writes to stop moving", written.Load)
	t.Logf("written %d bytes, %d carried by the proxy", written.Load(), proxy.carried.Load())

	if !returnsWithin(t, "closing the client", 5*time.Second, proxy, func() {
		_ = client.Close()
	}) {
		t.Fatal("closing the client was held by the Host passed through")
	}
}

// TestStoppingATunnelOverAStalledRouteDoesNotHoldTheManager is a tunnel of a
// Host reached through another, stopped while the service it carries sends
// more than the Host passed through reads. Stopping it closes the route under
// the lock of the manager, so a close held by the Host passed through would
// hold every call that reads the state of the tunnels as well.
func TestStoppingATunnelOverAStalledRouteDoesNotHoldTheManager(t *testing.T) {
	serviceIP, servicePort := startStream(t)
	localPort := freeDualStackPort(t)

	var proxy *stallingProxy
	f := newJumpFixture(t, func(_, jump *models.Host) {
		proxy = startStallingProxy(t, net.JoinHostPort(jump.Address, strconv.Itoa(jump.Port)))
		jump.Address, jump.Port = splitTestAddr(t, proxy.addr)
	})
	t.Cleanup(func() {
		proxy.stalled.Store(false)
	})
	f.addTunnel(t, serviceIP, servicePort, localPort)

	result := f.reconcile(t)
	if result.Started != 1 || result.Failed != 0 {
		t.Fatalf("the first pass reported %+v, want one started", result)
	}

	row := f.waitTunnelRow(t, "the tunnel to connect", func(row models.Tunnel) bool {
		return row.Status == statusConnected
	})

	// The port was free when it was picked and is asked for on both loopback
	// addresses. A listener another process opens on one of them in between
	// leaves the tunnel connected on the other alone, so the address dialled is
	// the one the row says was opened rather than 127.0.0.1, which would then
	// reach that listener instead of the tunnel.
	reader, err := net.DialTimeout("tcp", row.Local, localTestTimeout)
	if err != nil {
		t.Fatalf("failed to connect to the forwarded port %s: %v", row.Local, err)
	}
	defer func() {
		_ = reader.Close()
	}()

	var read atomic.Int64
	readEnded := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := reader.Read(buf)
			read.Add(int64(n))
			if err != nil {
				readEnded <- err
				return
			}
		}
	}()

	flowing := time.Now().Add(localTestTimeout)
	for read.Load() <= 1<<20 && time.Now().Before(flowing) {
		time.Sleep(5 * time.Millisecond)
	}
	if read.Load() <= 1<<20 {
		var ended error
		select {
		case ended = <-readEnded:
		default:
		}

		t.Fatalf("%d bytes came through the tunnel on %s (open on %s) within %v, the read ended with %v, "+
			"and the Host at the end was logged into %d times",
			read.Load(), row.Local, row.OpenReach, localTestTimeout, ended, f.target.handshakes.Load())
	}

	if proxy.shrunk.Load() == 0 {
		t.Fatal("the send buffer of the client was not made small")
	}

	proxy.stalled.Store(true)
	waitStalled(t, "the tunnel to stop carrying", read.Load)
	t.Logf("read %d bytes, %d carried by the proxy", read.Load(), proxy.carried.Load())

	stopped := make(chan error, 1)
	go func() {
		stopped <- f.m.StopTunnel(1, 1)
	}()

	time.Sleep(100 * time.Millisecond)

	held := !returnsWithin(t, "reading the tunnels of the Host", 5*time.Second, proxy, func() {
		_, _ = f.m.GetHostTunnels(1)
	})

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopTunnel returned an error: %v", err)
		}
	case <-time.After(localTestTimeout):
		t.Fatal("StopTunnel did not return")
	}

	if held {
		t.Fatal("stopping the tunnel held the manager")
	}
}
