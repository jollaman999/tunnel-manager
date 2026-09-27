package tunnel

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const acceptTestTimeout = 5 * time.Second

// flakyListener is a listener that fails its first Accepts with the errors it
// was given, then hands out what is pushed into conns, and ends with
// net.ErrClosed once it is closed.
type flakyListener struct {
	mu    sync.Mutex
	errs  []error
	calls int

	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newFlakyListener(errs ...error) *flakyListener {
	return &flakyListener{
		errs:   errs,
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls++
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()

	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *flakyListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *flakyListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1080}
}

func (l *flakyListener) acceptCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.calls
}

// push hands one end of a pipe to the next Accept and returns the other.
func (l *flakyListener) push(t *testing.T) net.Conn {
	t.Helper()

	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
	})

	select {
	case l.conns <- server:
	case <-time.After(acceptTestTimeout):
		t.Fatal("the accept loop never asked for the connection")
	}

	return client
}

// acceptEMFILE is what Accept on a TCP listener returns when the process is
// out of descriptors.
func acceptEMFILE() error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
}

// TestSocksAcceptWaitsOutADescriptorShortage is the proxy port running out of
// descriptors for a moment. The loop used to end on the first EMFILE, which
// took the proxy down and the Host's connection with it.
func TestSocksAcceptWaitsOutADescriptorShortage(t *testing.T) {
	listener := newFlakyListener(acceptEMFILE(), acceptEMFILE(), acceptEMFILE())
	p := &socksTunnel{logger: zap.NewNop(), done: make(chan struct{})}

	ended := make(chan error, 1)
	go func() {
		ended <- p.accept(listener, nil, make(chan struct{}))
	}()

	accepted := make(chan net.Conn, 1)
	go func() {
		server, client := net.Pipe()
		t.Cleanup(func() {
			_ = client.Close()
		})
		select {
		case listener.conns <- server:
			accepted <- client
		case <-time.After(acceptTestTimeout):
			close(accepted)
		}
	}()

	select {
	case err := <-ended:
		t.Fatalf("the accept loop ended on %v", err)
	case client, ok := <-accepted:
		if !ok {
			t.Fatal("the accept loop never asked for the connection after the errors")
		}
		_ = client.SetDeadline(time.Now().Add(acceptTestTimeout))
		socksGreet(t, client)
	}

	_ = listener.Close()

	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the accept loop ended on %v after the listener closed", err)
		}
	case <-time.After(acceptTestTimeout):
		t.Fatal("the accept loop did not end after the listener closed")
	}
}

// TestLocalAcceptWaitsOutADescriptorShortage is the same for a local forward:
// the connection after the errors reaches forward, which asks the Host for the
// target and logs the refusal the loopback server answers with.
func TestLocalAcceptWaitsOutADescriptorShortage(t *testing.T) {
	client, cleanup := newLoopbackSSHClient(t)
	defer cleanup()

	core, logs := observer.New(zapcore.InfoLevel)
	listener := newFlakyListener(acceptEMFILE(), acceptEMFILE())
	f := &localTunnel{logger: zap.New(core), target: "127.0.0.1:9", done: make(chan struct{})}

	ended := make(chan error, 1)
	go func() {
		ended <- f.accept(listener, client, make(chan struct{}))
	}()

	accepted := make(chan net.Conn, 1)
	go func() {
		server, peer := net.Pipe()
		t.Cleanup(func() {
			_ = peer.Close()
		})
		select {
		case listener.conns <- server:
			accepted <- peer
		case <-time.After(acceptTestTimeout):
			close(accepted)
		}
	}()

	select {
	case err := <-ended:
		t.Fatalf("the accept loop ended on %v", err)
	case peer, ok := <-accepted:
		if !ok {
			t.Fatal("the accept loop never asked for the connection after the errors")
		}
		_ = peer.SetReadDeadline(time.Now().Add(acceptTestTimeout))
		_, err := peer.Read(make([]byte, 1))
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read from the forwarded connection = %v, want it closed by forward", err)
		}
	}

	if logs.FilterField(logid.TunnelLocalForwardTargetDialFailed.Field()).Len() != 1 {
		t.Fatalf("the connection after the errors never reached forward: %v", logs.All())
	}

	_ = listener.Close()

	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the accept loop ended on %v after the listener closed", err)
		}
	case <-time.After(acceptTestTimeout):
		t.Fatal("the accept loop did not end after the listener closed")
	}
}

// TestAcceptWaitIsCutShortByTheListenerClosing is a listener that keeps
// failing, so the loop is always waiting between two Accepts. Closing it, or
// stopping the forward, has to end the loop at once rather than after the
// wait, which Stop would otherwise sit through.
func TestAcceptWaitIsCutShortByTheListenerClosing(t *testing.T) {
	for _, how := range []string{"closing", "stop"} {
		t.Run(how, func(t *testing.T) {
			errs := make([]error, 100)
			for i := range errs {
				errs[i] = acceptEMFILE()
			}
			listener := newFlakyListener(errs...)
			p := &socksTunnel{logger: zap.NewNop(), done: make(chan struct{})}
			closing := make(chan struct{})

			ended := make(chan error, 1)
			go func() {
				ended <- p.accept(listener, nil, closing)
			}()

			// After six failures the waits so far are 5, 10, 20, 40 and 80ms,
			// and the one it is in is 160ms.
			deadline := time.Now().Add(acceptTestTimeout)
			for listener.acceptCalls() < 6 {
				if time.Now().After(deadline) {
					t.Fatalf("the loop stopped accepting after %d calls", listener.acceptCalls())
				}
				time.Sleep(time.Millisecond)
			}

			start := time.Now()
			if how == "closing" {
				close(closing)
				_ = listener.Close()
			} else {
				close(p.done)
			}

			select {
			case err := <-ended:
				if err != nil {
					t.Fatalf("the accept loop ended on %v", err)
				}
			case <-time.After(acceptTestTimeout):
				t.Fatal("the accept loop did not end")
			}

			if took := time.Since(start); took > 100*time.Millisecond {
				t.Fatalf("the loop took %v to end, it sat through the wait", took)
			}
			if calls := listener.acceptCalls(); calls != 6 {
				t.Fatalf("Accept was called %d times, want 6: the loop accepted again after it was told to end", calls)
			}
		})
	}
}

// TestAcceptWaitsGrowAndAreHeldToASecond is the schedule net/http waits on.
func TestAcceptWaitsGrowAndAreHeldToASecond(t *testing.T) {
	var waited []time.Duration

	delay := time.Duration(0)
	for range 10 {
		if delay == 0 {
			delay = acceptRetryFirst
		} else {
			delay = min(delay*2, acceptRetryMax)
		}
		waited = append(waited, delay)
	}

	want := []time.Duration{5, 10, 20, 40, 80, 160, 320, 640, 1000, 1000}
	for i := range want {
		want[i] *= time.Millisecond
	}
	for i := range want {
		if waited[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waited, want)
		}
	}
}

// TestAcceptErrorPassesOnlyForWhatNetHTTPWaitsOut holds the test to the errors
// the listener survives, and every case to what net/http's Server.Serve
// decides on it. An errno inside an os.SyscallError is how Accept on a TCP
// listener returns one.
func TestAcceptErrorPassesOnlyForWhatNetHTTPWaitsOut(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EMFILE", acceptEMFILE(), true},
		{"ENFILE", &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.ENFILE)}, runtime.GOOS != "windows"},
		{"EINTR", &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EINTR)}, true},
		{"ECONNRESET", &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.ECONNRESET)}, false},
		{"bare ECONNRESET", &net.OpError{Op: "accept", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"EINVAL", &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EINVAL)}, false},
		{"closed", &net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed}, false},
		{"EOF", io.EOF, false},
		{"plain", errors.New("broken"), false},
	}

	for _, c := range cases {
		if got := acceptErrorPasses(c.err); got != c.want {
			t.Errorf("acceptErrorPasses(%s) = %v, want %v", c.name, got, c.want)
		}

		ne, ok := c.err.(net.Error)
		if http := ok && ne.Temporary(); http != c.want {
			t.Errorf("net/http waits out %s: %v, the case says %v", c.name, http, c.want)
		}
	}
}

// TestAcceptForwardEndsOnEveryError is the service port: its listener is the
// far end of the SSH connection, and every error it returns is that
// connection gone, which has to end the loop for the reconnect to start. An
// EMFILE is not waited out there either.
func TestAcceptForwardEndsOnEveryError(t *testing.T) {
	for _, want := range []error{io.EOF, errors.New("ssh: write failed"), acceptEMFILE()} {
		listener := newFlakyListener(want)
		tun := &SSHTunnel{logger: zap.NewNop()}

		ended := make(chan error, 1)
		go func() {
			ended <- tun.acceptForward(listener)
		}()

		select {
		case err := <-ended:
			if !errors.Is(err, want) {
				t.Fatalf("acceptForward ended on %v, want %v", err, want)
			}
		case <-time.After(acceptTestTimeout):
			t.Fatalf("acceptForward did not end on %v", want)
		}
	}
}

// TestForwardedConnectionsOverTheLimitAreClosed gives a proxy a limit of two.
// A third connection is closed as soon as it is accepted and logged once for
// the two turned away; once one of the two held ends, the next is carried
// again, and when everything is closed the count is back to zero.
func TestForwardedConnectionsOverTheLimitAreClosed(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	listener := newFlakyListener()
	limit := newConnLimit(2)
	p := &socksTunnel{logger: zap.New(core), done: make(chan struct{}), conns: limit}

	ended := make(chan error, 1)
	go func() {
		ended <- p.accept(listener, nil, make(chan struct{}))
	}()

	first := listener.push(t)
	_ = first.SetDeadline(time.Now().Add(acceptTestTimeout))
	socksGreet(t, first)

	second := listener.push(t)
	_ = second.SetDeadline(time.Now().Add(acceptTestTimeout))
	socksGreet(t, second)

	if open := limit.open.Load(); open != 2 {
		t.Fatalf("open = %d, want 2", open)
	}

	for range 2 {
		over := listener.push(t)
		_ = over.SetReadDeadline(time.Now().Add(acceptTestTimeout))
		_, err := over.Read(make([]byte, 1))
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read from a connection over the limit = %v, want it closed", err)
		}
	}

	limited := logs.FilterField(logid.TunnelConnectionLimitReached.Field()).All()
	if len(limited) != 1 {
		t.Fatalf("%d lines about the limit, want 1 for the first and the second held back: %v",
			len(limited), logs.All())
	}
	if closed := limited[0].ContextMap()["closed"]; closed != int64(1) {
		t.Fatalf("closed = %v, want 1 on the first line", closed)
	}
	if got := limited[0].ContextMap()["limit"]; got != int64(2) {
		t.Fatalf("limit = %v, want 2", got)
	}
	if open := limit.open.Load(); open != 2 {
		t.Fatalf("open = %d after the refusals, want 2", open)
	}

	_ = first.Close()
	waitOpen(t, limit, 1)

	third := listener.push(t)
	_ = third.SetDeadline(time.Now().Add(acceptTestTimeout))
	socksGreet(t, third)

	_ = second.Close()
	_ = third.Close()
	waitOpen(t, limit, 0)

	_ = listener.Close()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the accept loop ended on %v", err)
		}
	case <-time.After(acceptTestTimeout):
		t.Fatal("the accept loop did not end after the listener closed")
	}

	if open := limit.open.Load(); open != 0 {
		t.Fatalf("open = %d at the end, want 0", open)
	}
}

// TestTheLimitIsSharedAcrossForwards is a local forward and a service port
// drawing on one count: what one holds, the other cannot take.
func TestTheLimitIsSharedAcrossForwards(t *testing.T) {
	limit := newConnLimit(1)
	proxyListener := newFlakyListener()
	serviceListener := newFlakyListener()
	p := &socksTunnel{logger: zap.NewNop(), done: make(chan struct{}), conns: limit}
	tun := &SSHTunnel{logger: zap.NewNop(), conns: limit, Remote: "127.0.0.1:9"}

	go func() {
		_ = p.accept(proxyListener, nil, make(chan struct{}))
	}()
	go func() {
		_ = tun.acceptForward(serviceListener)
	}()
	t.Cleanup(func() {
		_ = proxyListener.Close()
		_ = serviceListener.Close()
	})

	held := proxyListener.push(t)
	_ = held.SetDeadline(time.Now().Add(acceptTestTimeout))
	socksGreet(t, held)

	over := serviceListener.push(t)
	_ = over.SetReadDeadline(time.Now().Add(acceptTestTimeout))
	_, err := over.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read from the service port over the limit = %v, want it closed", err)
	}

	_ = held.Close()
	waitOpen(t, limit, 0)
}

func TestTheSharedLimitLeavesTheReserve(t *testing.T) {
	ceiling := forwardConns.ceiling()
	want := max(forwardConnCeiling()-forwardConnReserve, forwardConnFloor)
	if ceiling != want {
		t.Fatalf("ceiling = %d, want %d", ceiling, want)
	}
	if ceiling < forwardConnFloor {
		t.Fatalf("ceiling = %d, below the floor of %d", ceiling, forwardConnFloor)
	}
}

func waitOpen(t *testing.T, limit *connLimit, want int64) {
	t.Helper()

	deadline := time.Now().Add(acceptTestTimeout)
	for limit.open.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open = %d, want %d", limit.open.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
