package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// lateSessionServer is an SSH server that holds every session channel it is
// asked for until release is called, which is a Host slow to open one. A
// command that arrives on a session is taken and never finished.
type lateSessionServer struct {
	addr    string
	release func()
	// execs counts the commands the Host was asked to run.
	execs atomic.Int64
	// closed is closed once the client closes a session it was given.
	closed chan struct{}
}

func startLateSessionServer(t *testing.T) *lateSessionServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	gate := make(chan struct{})
	var once sync.Once

	s := &lateSessionServer{
		addr:    ln.Addr().String(),
		release: func() { once.Do(func() { close(gate) }) },
		closed:  make(chan struct{}),
	}

	var mu sync.Mutex
	var conns []net.Conn

	t.Cleanup(func() {
		s.release()
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			go s.serve(conn, config, gate)
		}
	}()

	return s
}

func (s *lateSessionServer) serve(conn net.Conn, config *ssh.ServerConfig, gate <-chan struct{}) {
	defer func() {
		_ = conn.Close()
	}()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() {
		_ = sshConn.Close()
	}()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		<-gate

		channel, requests, err := newChan.Accept()
		if err != nil {
			continue
		}

		go func() {
			_, _ = io.Copy(io.Discard, channel)
		}()

		// The requests of a channel end when the channel is closed, and not
		// on the end of what the client writes, which a session sends as soon
		// as a command runs with nothing on its stdin.
		go func() {
			for req := range requests {
				if req.Type == "exec" {
					s.execs.Add(1)
				}
				if req.WantReply {
					_ = req.Reply(req.Type == "exec", nil)
				}
			}
			close(s.closed)
		}()
	}
}

// countRemoteCommandGoroutines reports how many goroutines sit in the one
// runRemoteCommand starts.
func countRemoteCommandGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("tunnel.runRemoteCommand.func1"))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestRunRemoteCommandClosesASessionThatOpenedAfterItGaveUp is a Host that
// opens the session only after the bound has run out. Nobody is waiting for
// the answer by then, so the session is closed as soon as it opens, the
// command is never sent, and the goroutine that asked for the session ends
// with it rather than running the command for as long as the Host keeps it
// going.
func TestRunRemoteCommandClosesASessionThatOpenedAfterItGaveUp(t *testing.T) {
	server := startLateSessionServer(t)

	client, _, cleanup := dialTestSSHClient(t, server.addr)
	defer cleanup()

	before := countRemoteCommandGoroutines()

	if got := runRemoteCommand(client, "ss -ltnH sport = :19601", 200*time.Millisecond); got != "" {
		t.Fatalf("runRemoteCommand = %q, want empty: the Host had not opened the session", got)
	}

	server.release()

	select {
	case <-server.closed:
	case <-time.After(5 * time.Second):
		t.Error("the session that opened after the bound was still open 5s later")
	}

	if execs := server.execs.Load(); execs != 0 {
		t.Errorf("the Host was asked to run %d commands on a session opened after the bound, want 0", execs)
	}

	deadline := time.Now().Add(5 * time.Second)
	for countRemoteCommandGoroutines() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if left := countRemoteCommandGoroutines() - before; left > 0 {
		t.Errorf("%d goroutines of runRemoteCommand were left after it gave up, want 0", left)
	}
}
