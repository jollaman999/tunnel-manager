package tlsserve

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// tlsHandshakeRecord is the first byte of every TLS connection. A TLS client
// opens with a handshake record, and a record begins with its content type,
// which is 22 for a handshake. Plain HTTP opens with a request line, so its
// first byte is a letter of a method name. The two cannot be confused: 22 is a
// control character and no HTTP method starts with one.
const tlsHandshakeRecord = 0x16

// peekTimeout is how long a connection has to send its first byte.
//
// The byte is what decides which server the connection belongs to, and until it
// arrives the connection belongs to neither. A client that connects and says
// nothing would hold a goroutine and a descriptor for as long as TCP keeps the
// connection up, which is hours, and a few thousand of them is the descriptor
// limit this process already watches.
//
// Ten seconds is far longer than any client needs: the first byte is written
// straight after the connection is established, so what it has to cover is the
// network in between and not any thinking on the client's part. It is short
// enough that a port scanner or a health check that opens a connection and
// leaves is cleared up while the operator is still watching.
const peekTimeout = 10 * time.Second

// acceptRetryMin and acceptRetryMax bound the wait after an accept that failed
// without the listener being closed. Running out of descriptors is the case
// that matters: it is temporary, a spin without a wait would burn a core
// reporting it, and giving up on the listener would take the API down for a
// condition that clears on its own. The bounds are the ones net/http uses.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// Splitter serves one port to two servers.
//
// It accepts every connection itself, looks at the first byte, and hands the
// connection to the TLS side or to the plaintext side. Each side is offered as
// a net.Listener, so the servers behind them are ordinary http.Servers that
// know nothing about any of this and keep their own graceful shutdown.
//
// The looking is done in a goroutine per connection and never in the accept
// loop. A connection that sends nothing would otherwise stop the loop, and with
// it every other client of this port: one silent socket would be an outage.
type Splitter struct {
	inner  net.Listener
	logger *zap.Logger

	// The classified connections are handed over on these. They are never
	// closed: a classifier goroutine may be about to send on one, and a send on
	// a closed channel panics, which would take the process down at exactly the
	// moment it is trying to shut down in order. done below is what says there
	// is nothing left to hand over, and it is closed once.
	tlsConns   chan net.Conn
	plainConns chan net.Conn

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	// peekTimeout is how long a connection has to send its first byte. It is a
	// field rather than the constant read straight so that a test can shorten
	// it: what it guards against is a client that never sends anything, and
	// waiting the real ten seconds out is the whole of the test.
	peekTimeout time.Duration

	tlsSide   *sideListener
	plainSide *sideListener
}

// NewSplitter takes over inner and starts accepting on it. Close stops it and
// closes inner.
func NewSplitter(inner net.Listener, logger *zap.Logger) *Splitter {
	return newSplitter(inner, logger, peekTimeout)
}

func newSplitter(inner net.Listener, logger *zap.Logger, peek time.Duration) *Splitter {
	s := &Splitter{
		inner:  inner,
		logger: logger,
		// The channels are unbuffered. A connection is handed straight to the
		// server that is waiting to accept it, and a buffer would only let
		// connections sit in this process without any server having taken
		// responsibility for them yet.
		tlsConns:    make(chan net.Conn),
		plainConns:  make(chan net.Conn),
		done:        make(chan struct{}),
		peekTimeout: peek,
	}

	s.tlsSide = &sideListener{splitter: s, conns: s.tlsConns}
	s.plainSide = &sideListener{splitter: s, conns: s.plainConns}

	go s.acceptLoop()

	return s
}

// TLS is the listener the TLS server is given. Everything that opened with a
// handshake record arrives on it.
func (s *Splitter) TLS() net.Listener {
	return s.tlsSide
}

// Plain is the listener the redirect server is given. Everything else arrives
// on it.
func (s *Splitter) Plain() net.Listener {
	return s.plainSide
}

// Close stops accepting and closes the port. Connections already handed to a
// server are left alone: closing a listener is not closing what it accepted,
// and the servers drain those themselves on their own shutdown.
func (s *Splitter) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.closeErr = s.inner.Close()
	})

	return s.closeErr
}

func (s *Splitter) acceptLoop() {
	var delay time.Duration

	for {
		conn, err := s.inner.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}

			if errors.Is(err, net.ErrClosed) {
				return
			}

			if delay == 0 {
				delay = acceptRetryMin
			} else {
				delay *= 2
			}
			if delay > acceptRetryMax {
				delay = acceptRetryMax
			}

			s.logger.Warn("failed to accept a connection on the API port",
				zap.Error(err),
				zap.Duration("retry_in", delay))

			select {
			case <-time.After(delay):
			case <-s.done:
				return
			}

			continue
		}

		delay = 0

		go s.classify(conn)
	}
}

// classify looks at the first byte of conn and hands it to the side it belongs
// to. It runs in its own goroutine, so the waiting it does holds up nothing but
// this one connection.
func (s *Splitter) classify(conn net.Conn) {
	err := conn.SetReadDeadline(time.Now().Add(s.peekTimeout))
	if err != nil {
		s.logger.Debug("failed to set the deadline for reading the first byte",
			zap.String("remote_addr", conn.RemoteAddr().String()),
			zap.Error(err))
		_ = conn.Close()

		return
	}

	// The byte is looked at through a bufio.Reader and left where it is. Read
	// off the connection it would be gone, and the server behind this would be
	// handed a TLS stream missing the start of its handshake or a request
	// missing the G of GET. The same reader is handed on below, so whatever it
	// buffered while looking is read by the server and nothing is lost.
	reader := bufio.NewReader(conn)

	first, err := reader.Peek(1)
	if err != nil {
		// This is where the silent client ends up, along with every scanner
		// that connects and hangs up. It is one line at debug: on a port that
		// faces a network it happens all day and says nothing about this
		// process.
		s.logger.Debug("closed a connection that sent nothing to tell TLS from plain HTTP by",
			zap.String("remote_addr", conn.RemoteAddr().String()),
			zap.Duration("waited", s.peekTimeout),
			zap.Error(err))
		_ = conn.Close()

		return
	}

	// The deadline has done its job and has to go. It is a deadline on the
	// connection and not on this read: left armed, it would end the next read
	// the server performs, so a browser holding a keep-alive connection open
	// would have it dropped mid-request once the ten seconds were up.
	err = conn.SetReadDeadline(time.Time{})
	if err != nil {
		s.logger.Debug("failed to clear the deadline after reading the first byte",
			zap.String("remote_addr", conn.RemoteAddr().String()),
			zap.Error(err))
		_ = conn.Close()

		return
	}

	target := s.plainConns
	if first[0] == tlsHandshakeRecord {
		target = s.tlsConns
	}

	select {
	case target <- &peekedConn{Conn: conn, reader: reader}:
	case <-s.done:
		// The server this belongs to is gone. Nothing will accept the
		// connection, so it is closed here rather than left to the client to
		// time out on.
		_ = conn.Close()
	}
}

// peekedConn is a connection whose first byte was looked at. Every read goes
// through the reader that holds it, so the server reads the stream from its
// first byte as if nothing had looked at it.
type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// sideListener is one of the two views of the port. It is a net.Listener that
// accepts from a channel instead of from a socket.
type sideListener struct {
	splitter *Splitter
	conns    <-chan net.Conn
}

func (l *sideListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.splitter.done:
		// http.Server.Serve stops on this and answers its own Shutdown with
		// ErrServerClosed, which is what it does when a real listener is closed
		// under it. The error is shaped like the one a real listener returns so
		// that errors.Is finds net.ErrClosed in it either way.
		return nil, &net.OpError{
			Op:   "accept",
			Net:  l.splitter.inner.Addr().Network(),
			Addr: l.splitter.inner.Addr(),
			Err:  net.ErrClosed,
		}
	}
}

// Close closes the whole splitter and with it the other side.
//
// The two sides are two views of one socket, and this process puts them up and
// takes them down together: the shutdown closes the API server, which closes
// the listener it was given, and the redirect server has nothing left to
// redirect to at that point. Closing only one view would leave the port open
// with half of its clients answered.
func (l *sideListener) Close() error {
	return l.splitter.Close()
}

func (l *sideListener) Addr() net.Addr {
	return l.splitter.inner.Addr()
}
