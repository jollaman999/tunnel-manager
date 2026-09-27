package tunnel

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// acceptRetryFirst and acceptRetryMax bound the wait after an Accept error the
// listener survives. They are the waits net/http's Server.Serve takes, 5ms
// doubling up to a second: long enough that a process out of descriptors is
// not spun on, and short enough that a client arriving once one is free again
// is taken without a pause anybody notices.
const (
	acceptRetryFirst = 5 * time.Millisecond
	acceptRetryMax   = time.Second
)

// forwardConnReserve is how many descriptors of the soft limit are left to the
// rest of the process: the database, the log file, the listeners, the API
// port and one SSH connection per Host. Every forwarded connection holds one
// descriptor on this machine, the accepted socket of a local port or the
// socket dialled for a service port, so the limit less the reserve is how many
// of them can be open before an Accept or a dial starts failing on EMFILE.
const forwardConnReserve = 256

// forwardConnFloor is the fewest forwarded connections allowed whatever the
// soft limit is, so a process started under a very low limit still carries
// some rather than refusing everything.
const forwardConnFloor = 64

// connLimitLogInterval is how often a connection closed over the limit is
// logged at most per forward, for the reason socksRefusalLogInterval is: a
// client that retries at once would otherwise write a line per attempt.
const connLimitLogInterval = socksRefusalLogInterval

// connLimit counts the forwarded connections that are open against a ceiling.
// The ceiling is read the first time it is needed rather than when the package
// is loaded, because main raises the soft limit on descriptors after that and
// before any forward starts.
type connLimit struct {
	ceiling func() int64
	open    atomic.Int64
}

// forwardConns is the one count every local port, SOCKS5 proxy and service
// port shares. The descriptors run out for the process as a whole, so a
// ceiling per listener would either allow more than there are or keep one busy
// forward from using what the others leave idle.
var forwardConns = &connLimit{ceiling: sync.OnceValue(func() int64 {
	return max(forwardConnCeiling()-forwardConnReserve, forwardConnFloor)
})}

func newConnLimit(ceiling int64) *connLimit {
	return &connLimit{ceiling: func() int64 { return ceiling }}
}

// orShared returns l, or the count of the process when l is nil, which is what
// every forward outside a test carries.
func (l *connLimit) orShared() *connLimit {
	if l == nil {
		return forwardConns
	}

	return l
}

// acquire takes one place and reports whether there was one. Whoever it
// returns true for calls release once the connection is closed.
func (l *connLimit) acquire() bool {
	if l.open.Add(1) > l.ceiling() {
		l.open.Add(-1)
		return false
	}

	return true
}

func (l *connLimit) release() {
	l.open.Add(-1)
}

// connLimitLog is what one forward remembers about the connections it closed
// over the limit, so that they are logged at most once every
// connLimitLogInterval with the number closed since the last line.
type connLimitLog struct {
	mu     sync.Mutex
	logged time.Time
	since  int
}

// note counts one connection closed over the limit and reports whether a line
// is due, with the number it covers.
func (c *connLimitLog) note() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.since++
	if !c.logged.IsZero() && time.Since(c.logged) < connLimitLogInterval {
		return 0, false
	}

	count := c.since
	c.since = 0
	c.logged = time.Now()

	return count, true
}

// acceptErrorPasses reports whether an error from Accept on a listener of this
// machine leaves the listener working, so that the loop waits and accepts
// again instead of ending and taking every forward of the Host down with it.
//
// It is the test net/http's Server.Serve makes, net.Error's Temporary. What
// Accept on a TCP listener returns is a net.OpError around an os.SyscallError
// around the errno (netFD.accept), so the Temporary that decides is the one of
// syscall.Errno: EINTR, EMFILE, ENFILE and the timeouts on Unix, and EINTR,
// EMFILE and the timeouts on Windows. ECONNABORTED never gets this far on
// Unix, where poll.FD.Accept accepts again on it itself. EMFILE and ENFILE are
// the ones that come up: the process or the system is out of descriptors for
// a moment, and a connection closing anywhere else ends it. Temporary is
// deprecated because a timeout is not always temporary, but no deadline is set
// on these listeners, so Accept never times out.
func acceptErrorPasses(err error) bool {
	var temporary interface{ Temporary() bool }

	return errors.As(err, &temporary) && temporary.Temporary()
}

// acceptLoop is what the accept loop of a local port, a SOCKS5 proxy and a
// service port have in common.
type acceptLoop struct {
	// passes reports whether an Accept error is one to wait out. Nil ends the
	// loop on every error.
	passes func(error) bool
	// closing and done end the wait after an error at once. closing is closed
	// when the listener is about to be, and done when the forward stops.
	closing <-chan struct{}
	done    <-chan struct{}
	// admit decides on a connection before it is counted, and closes one it
	// turns away. Nil admits every connection.
	admit func(net.Conn) bool
	// limit is the count the connection takes a place in.
	limit *connLimit
	// over is told about a connection closed because limit had no place left.
	over func(net.Addr)
	// serve carries one connection and closes it.
	serve func(net.Conn)
}

// run accepts until the listener fails in a way passes does not wait out, and
// returns that error. A wait that closing or done ends returns net.ErrClosed,
// which is what the listener is about to say.
func (a acceptLoop) run(listener net.Listener) error {
	var delay time.Duration

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || a.passes == nil || !a.passes(err) {
				return err
			}

			if delay == 0 {
				delay = acceptRetryFirst
			} else {
				delay = min(delay*2, acceptRetryMax)
			}

			if !a.wait(delay) {
				return net.ErrClosed
			}
			continue
		}
		delay = 0

		if a.admit != nil && !a.admit(conn) {
			continue
		}

		if !a.limit.acquire() {
			_ = conn.Close()
			a.over(conn.RemoteAddr())
			continue
		}

		go func() {
			defer a.limit.release()

			a.serve(conn)
		}()
	}
}

// wait waits for delay and reports whether the loop should accept again.
func (a acceptLoop) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-a.closing:
		return false
	case <-a.done:
		return false
	case <-timer.C:
		return true
	}
}
