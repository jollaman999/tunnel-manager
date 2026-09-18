package tlsserve

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// newKeyPair is a freshly generated certificate and its key, parsed the way
// everything in this package hands them out. Two calls produce two different
// certificates: the key and the serial number are random.
func newKeyPair(t *testing.T) *tls.Certificate {
	t.Helper()

	generated, err := generate(time.Now())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	keyPair, err := tls.X509KeyPair(generated.certPEM, generated.keyPEM)
	if err != nil {
		t.Fatalf("the generated certificate and key do not form a pair: %v", err)
	}

	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		t.Fatalf("the generated certificate cannot be parsed: %v", err)
	}

	keyPair.Leaf = leaf

	return &keyPair
}

// TestAnEmptyHolderRefusesTheHandshake covers the state the holder is in while
// HTTPS is off. Nothing listens over TLS then, so this cannot be reached from
// outside; what it buys is that a mistake in the wiring ends as a refusal and
// not as a panic in the middle of a handshake.
func TestAnEmptyHolderRefusesTheHandshake(t *testing.T) {
	holder := NewHolder(nil)

	if holder.Current() != nil {
		t.Error("an empty holder reports a certificate")
	}

	cert, err := holder.GetCertificate(&tls.ClientHelloInfo{})
	if err == nil {
		t.Fatalf("an empty holder handed out %v, want a refusal", cert)
	}
}

// TestTheHolderHandsOutWhatWasLastPutInIt is the whole of what the handler
// does to replace a certificate.
func TestTheHolderHandsOutWhatWasLastPutInIt(t *testing.T) {
	first := newKeyPair(t)
	second := newKeyPair(t)

	holder := NewHolder(first)

	served, err := holder.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if served != first {
		t.Error("the holder handed out something other than what it was built with")
	}

	holder.Set(second)

	served, err = holder.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after Set: %v", err)
	}
	if served != second {
		t.Error("the holder handed out the old certificate after it was replaced")
	}
	if holder.Current() != second {
		t.Error("Current reports the old certificate after it was replaced")
	}
}

// TestAReplacementReachesTheNextHandshakeAndLeavesOpenConnectionsAlone is why
// the certificate is behind a holder at all.
//
// The two halves are both the point. The next client to connect is served the
// new certificate without the listener having been touched, which is what makes
// the button on the Settings screen possible; and the connection that was
// already open keeps the old certificate, which is what lets the answer to that
// button travel back over the connection the button was pressed on.
func TestAReplacementReachesTheNextHandshakeAndLeavesOpenConnectionsAlone(t *testing.T) {
	first := newKeyPair(t)
	second := newKeyPair(t)

	holder := NewHolder(first)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	tlsListener := tls.NewListener(listener, ServerConfig(holder))

	// The server answers a handshake and holds the connection open, so that the
	// connection opened before the replacement is still there to be checked
	// after it.
	accepted := make(chan net.Conn, 2)

	go func() {
		for {
			conn, acceptErr := tlsListener.Accept()
			if acceptErr != nil {
				return
			}

			// The handshake is run here on purpose. A listener wrapped in TLS
			// hands back a connection that has not spoken yet, and the client
			// waits for the server to answer, so an accept that only files the
			// connection away leaves both ends waiting on each other.
			tlsConn, ok := conn.(*tls.Conn)
			if ok {
				_ = tlsConn.Handshake()
			}

			accepted <- conn
		}
	}()

	dial := func() *tls.Conn {
		t.Helper()

		// The certificate is self-signed and is made out to the names of this
		// machine, so nothing here verifies it. What is under test is which
		// certificate the server presents, which is read off the handshake.
		conn, dialErr := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
			InsecureSkipVerify: true,
		})
		if dialErr != nil {
			t.Fatalf("failed to connect: %v", dialErr)
		}

		return conn
	}

	before := dial()
	defer func() { _ = before.Close() }()

	firstFingerprint := Fingerprint(first.Leaf)

	if got := Fingerprint(before.ConnectionState().PeerCertificates[0]); got != firstFingerprint {
		t.Fatalf("the first connection was served %s, want %s", got, firstFingerprint)
	}

	holder.Set(second)

	after := dial()
	defer func() { _ = after.Close() }()

	secondFingerprint := Fingerprint(second.Leaf)

	if firstFingerprint == secondFingerprint {
		t.Fatal("the two generated certificates have the same fingerprint")
	}

	if got := Fingerprint(after.ConnectionState().PeerCertificates[0]); got != secondFingerprint {
		t.Errorf("the connection made after the replacement was served %s, want %s",
			got, secondFingerprint)
	}

	if got := Fingerprint(before.ConnectionState().PeerCertificates[0]); got != firstFingerprint {
		t.Errorf("the connection opened before the replacement now reports %s, want the "+
			"certificate it was opened under, %s", got, firstFingerprint)
	}

	for i := 0; i < 2; i++ {
		select {
		case conn := <-accepted:
			_ = conn.Close()
		case <-time.After(5 * time.Second):
			t.Fatal("the server did not accept both connections")
		}
	}
}
