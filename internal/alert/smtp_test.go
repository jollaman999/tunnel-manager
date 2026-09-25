package alert

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"go.uber.org/zap"
)

// mailName is the name every test sends to. The dial is pointed at the fake
// server whatever the name, so the name decides only what the certificate is
// checked against and whether net/smtp takes the server for this system.
const mailName = "mail.example.com"

// selfSigned builds a certificate for mailName and the pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: mailName},
		DNSNames:              []string{mailName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create the certificate: %v", err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("failed to read the certificate back: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(parsed)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// mailSession is what one connection to the fake server did.
type mailSession struct {
	tls      bool
	mech     string
	username string
	password string
	from     string
	rcpts    []string
	data     string
}

// fakeSMTP is as much of a mail server as the sender speaks to: EHLO, STARTTLS,
// AUTH PLAIN and LOGIN, MAIL, RCPT, DATA and QUIT. It offers AUTH on a
// connection without TLS as well, so that the refusal to log in over one is the
// sender's own and not the server's.
type fakeSMTP struct {
	t        *testing.T
	listener net.Listener
	cert     tls.Certificate
	// implicitTLS is the server of port 465, which speaks TLS from the first
	// byte.
	implicitTLS bool
	// offerSTARTTLS is whether EHLO names STARTTLS.
	offerSTARTTLS bool

	mu       sync.Mutex
	sessions []mailSession
	wg       sync.WaitGroup
}

func newFakeSMTP(t *testing.T, cert tls.Certificate, implicitTLS bool, offerSTARTTLS bool) *fakeSMTP {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	server := &fakeSMTP{
		t:             t,
		listener:      listener,
		cert:          cert,
		implicitTLS:   implicitTLS,
		offerSTARTTLS: offerSTARTTLS,
	}

	server.wg.Add(1)

	go func() {
		defer server.wg.Done()

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			server.wg.Add(1)

			go func() {
				defer server.wg.Done()
				server.serve(conn)
			}()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		server.wg.Wait()
	})

	return server
}

func (s *fakeSMTP) record(session mailSession) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions = append(s.sessions, session)
}

// only returns the one session there was.
func (s *fakeSMTP) only(t *testing.T) mailSession {
	t.Helper()

	// The server records a session as its connection ends, which is after the
	// sender has read the answer to QUIT.
	deadline := time.Now().Add(5 * time.Second)

	for {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()

		if n > 0 || time.Now().After(deadline) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.sessions) != 1 {
		t.Fatalf("the server saw %d sessions, want 1", len(s.sessions))
	}

	return s.sessions[0]
}

func (s *fakeSMTP) serve(conn net.Conn) {
	session := mailSession{}

	defer func() {
		_ = conn.Close()
		s.record(session)
	}()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{s.cert}}

	if s.implicitTLS {
		conn = tls.Server(conn, tlsConfig)
		session.tls = true
	}

	reader := bufio.NewReader(conn)

	write := func(line string) {
		_, _ = conn.Write([]byte(line + "\r\n"))
	}

	read := func() (string, bool) {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", false
		}

		return strings.TrimRight(line, "\r\n"), true
	}

	write("220 " + mailName + " ESMTP fake")

	for {
		line, ok := read()
		if !ok {
			return
		}

		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])

		switch verb {
		case "EHLO":
			write("250-" + mailName)

			if !session.tls && s.offerSTARTTLS {
				write("250-STARTTLS")
			}

			write("250-AUTH PLAIN LOGIN")
			write("250 8BITMIME")
		case "HELO", "NOOP", "RSET":
			write("250 ok")
		case "STARTTLS":
			write("220 go ahead")

			tlsConn := tls.Server(conn, tlsConfig)
			if tlsConn.Handshake() != nil {
				return
			}

			conn = tlsConn
			reader = bufio.NewReader(conn)
			session.tls = true
		case "AUTH":
			fields := strings.Fields(line)
			if len(fields) < 2 {
				write("501 syntax")
				continue
			}

			session.mech = strings.ToUpper(fields[1])

			switch session.mech {
			case "PLAIN":
				if len(fields) < 3 {
					write("501 no initial response")
					continue
				}

				decoded, _ := base64.StdEncoding.DecodeString(fields[2])
				parts := strings.Split(string(decoded), "\x00")
				if len(parts) == 3 {
					session.username = parts[1]
					session.password = parts[2]
				}

				write("235 ok")
			case "LOGIN":
				write("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))

				answer, ok := read()
				if !ok {
					return
				}

				user, _ := base64.StdEncoding.DecodeString(answer)
				session.username = string(user)

				write("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))

				answer, ok = read()
				if !ok {
					return
				}

				pass, _ := base64.StdEncoding.DecodeString(answer)
				session.password = string(pass)

				write("235 ok")
			default:
				write("504 unknown mechanism")
			}
		case "MAIL":
			session.from = between(line, "<", ">")
			write("250 ok")
		case "RCPT":
			session.rcpts = append(session.rcpts, between(line, "<", ">"))
			write("250 ok")
		case "DATA":
			write("354 go ahead")

			var data strings.Builder

			for {
				body, ok := read()
				if !ok {
					return
				}

				if body == "." {
					break
				}

				data.WriteString(body + "\n")
			}

			session.data = data.String()
			write("250 queued")
		case "QUIT":
			write("221 bye")
			return
		case "*":
			write("501 cancelled")
		default:
			write("502 not implemented")
		}
	}
}

func between(line string, open string, close string) string {
	_, rest, ok := strings.Cut(line, open)
	if !ok {
		return ""
	}

	inside, _, _ := strings.Cut(rest, close)

	return inside
}

// mailTest is a sender pointed at a fake server and the settings it sends
// under.
type mailTest struct {
	server *fakeSMTP
	sender *Sender
	config SMTPConfig
}

func newMailTest(t *testing.T, implicitTLS bool, offerSTARTTLS bool, security string, auth string) *mailTest {
	t.Helper()

	cert, pool := selfSigned(t)
	server := newFakeSMTP(t, cert, implicitTLS, offerSTARTTLS)

	key := make([]byte, crypto.KeySize)
	for i := range key {
		key[i] = byte(i)
	}

	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("failed to build the cipher: %v", err)
	}

	sealed, err := cipher.Encrypt("s3cret-pass")
	if err != nil {
		t.Fatalf("failed to seal the password: %v", err)
	}

	sender := NewSender(zap.NewNop(), cipher)
	sender.roots = pool
	sender.mailTimeout = 5 * time.Second
	sender.dial = func(ctx context.Context, network string, address string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", server.listener.Addr().String())
	}

	return &mailTest{
		server: server,
		sender: sender,
		config: SMTPConfig{
			Host:           mailName,
			Port:           587,
			Security:       security,
			Auth:           auth,
			Username:       "alerts",
			SealedPassword: sealed,
			From:           "tunnel-manager@example.com",
			To:             []string{"ops@example.com", "oncall@example.net"},
		},
	}
}

func downEvent() Event {
	return Event{
		Event:        EventDown,
		Kind:         KindServicePort,
		Host:         "ssh.example.com",
		LocalPort:    8080,
		Since:        "2026-09-26T01:00:00Z",
		LastError:    "dial tcp: connection refused\r\nX-Injected: yes",
		Installation: "tm.example.com",
	}
}

// checkDelivered holds what arrived to what was sent.
func checkDelivered(t *testing.T, session mailSession) {
	t.Helper()

	if session.from != "tunnel-manager@example.com" {
		t.Errorf("MAIL FROM was %q", session.from)
	}

	if strings.Join(session.rcpts, ",") != "ops@example.com,oncall@example.net" {
		t.Errorf("RCPT TO was %v", session.rcpts)
	}

	for _, want := range []string{
		"Subject: [tunnel-manager tm.example.com] DOWN: service port 8080 on ssh.example.com\n",
		"To: ops@example.com, oncall@example.net\n",
		"event: down\n",
		"kind: service_port\n",
		"host: ssh.example.com\n",
		"local_port: 8080\n",
		"since: 2026-09-26T01:00:00Z\n",
		"installation: tm.example.com\n",
		// The line break in the error is folded into the line, so what it
		// carries cannot start a line of its own.
		"last_error: dial tcp: connection refused X-Injected: yes\n",
	} {
		if !strings.Contains(session.data, want) {
			t.Errorf("the message lacks %q:\n%s", want, session.data)
		}
	}

	if strings.Contains(session.data, "\nX-Injected") {
		t.Errorf("a line break in the error started a line of its own:\n%s", session.data)
	}
}

// TestMailOverSTARTTLSWithPLAIN is the default: the connection is upgraded
// before anything is logged in with.
func TestMailOverSTARTTLSWithPLAIN(t *testing.T) {
	m := newMailTest(t, false, true, settings.SMTPSecurityStartTLS, settings.SMTPAuthPlain)

	err := m.sender.Mail(context.Background(), m.config, downEvent())
	if err != nil {
		t.Fatalf("Mail: %v", err)
	}

	session := m.server.only(t)

	if !session.tls || session.mech != "PLAIN" || session.username != "alerts" || session.password != "s3cret-pass" {
		t.Fatalf("the server saw tls=%v mech=%q user=%q pass=%q, want TLS, PLAIN and the stored login",
			session.tls, session.mech, session.username, session.password)
	}

	checkDelivered(t, session)
}

// TestMailOverImplicitTLSWithLOGIN is port 465 and the mechanism net/smtp does
// not carry.
func TestMailOverImplicitTLSWithLOGIN(t *testing.T) {
	m := newMailTest(t, true, false, settings.SMTPSecurityTLS, settings.SMTPAuthLogin)
	m.config.Port = 465

	err := m.sender.Mail(context.Background(), m.config, downEvent())
	if err != nil {
		t.Fatalf("Mail: %v", err)
	}

	session := m.server.only(t)

	if !session.tls || session.mech != "LOGIN" || session.username != "alerts" || session.password != "s3cret-pass" {
		t.Fatalf("the server saw tls=%v mech=%q user=%q pass=%q, want TLS, LOGIN and the stored login",
			session.tls, session.mech, session.username, session.password)
	}

	checkDelivered(t, session)
}

// TestMailWithoutTLSOrLogin is a relay that takes mail from this system without
// either.
func TestMailWithoutTLSOrLogin(t *testing.T) {
	m := newMailTest(t, false, false, settings.SMTPSecurityNone, settings.SMTPAuthNone)
	m.config.SealedPassword = ""

	err := m.sender.Mail(context.Background(), m.config, downEvent())
	if err != nil {
		t.Fatalf("Mail: %v", err)
	}

	session := m.server.only(t)

	if session.tls || session.mech != "" {
		t.Fatalf("the server saw tls=%v mech=%q, want neither", session.tls, session.mech)
	}

	checkDelivered(t, session)
}

// TestNoPasswordIsSentWithoutTLS is both mechanisms on a connection that is not
// protected, to a server that is not this system. The server offers AUTH, so
// the refusal is the sender's, and it comes before any password is written.
func TestNoPasswordIsSentWithoutTLS(t *testing.T) {
	for _, auth := range []string{settings.SMTPAuthPlain, settings.SMTPAuthLogin} {
		t.Run(auth, func(t *testing.T) {
			m := newMailTest(t, false, false, settings.SMTPSecurityNone, auth)

			err := m.sender.Mail(context.Background(), m.config, downEvent())
			if err == nil {
				t.Fatal("a login over a connection without TLS was made")
			}

			session := m.server.only(t)

			if session.password != "" || session.from != "" {
				t.Fatalf("the server was sent password=%q from=%q, want nothing", session.password, session.from)
			}

			if auth == settings.SMTPAuthLogin && !errors.Is(err, errAuthWithoutTLS) {
				t.Fatalf("LOGIN refused with %v, want errAuthWithoutTLS", err)
			}
		})
	}
}

// TestLOGINRefusesWhatPLAINRefuses holds the LOGIN mechanism to the rule
// PlainAuth of net/smtp keeps, which is what it is written after.
func TestLOGINRefusesWhatPLAINRefuses(t *testing.T) {
	cases := []struct {
		name   string
		server smtp.ServerInfo
		ok     bool
	}{
		{"TLS to the server", smtp.ServerInfo{Name: mailName, TLS: true}, true},
		{"no TLS to the server", smtp.ServerInfo{Name: mailName}, false},
		{"no TLS to this system", smtp.ServerInfo{Name: "localhost"}, true},
		{"TLS to another name", smtp.ServerInfo{Name: "other.example.com", TLS: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login := &loginAuth{username: "u", password: "p", host: mailName}
			if tc.server.Name == "localhost" {
				login.host = "localhost"
			}

			plain := smtp.PlainAuth("", "u", "p", login.host)

			_, _, loginErr := login.Start(&tc.server)
			_, _, plainErr := plain.Start(&tc.server)

			if (loginErr == nil) != tc.ok || (plainErr == nil) != tc.ok {
				t.Fatalf("LOGIN gave %v and PLAIN gave %v, want ok=%v from both", loginErr, plainErr, tc.ok)
			}
		})
	}
}

// TestTheCertificateOfTheMailServerIsChecked is a server whose certificate
// nothing here trusts: refused unless the check is switched off.
func TestTheCertificateOfTheMailServerIsChecked(t *testing.T) {
	m := newMailTest(t, false, true, settings.SMTPSecurityStartTLS, settings.SMTPAuthPlain)
	m.sender.roots = x509.NewCertPool()

	err := m.sender.Mail(context.Background(), m.config, downEvent())
	if err == nil {
		t.Fatal("a certificate nothing trusts was accepted")
	}

	m.config.SkipVerify = true

	err = m.sender.Mail(context.Background(), m.config, downEvent())
	if err != nil {
		t.Fatalf("with the check switched off: %v", err)
	}
}

// TestSTARTTLSThatIsNotOfferedIsAFailure keeps a server that does not offer
// STARTTLS from being spoken to in the clear.
func TestSTARTTLSThatIsNotOfferedIsAFailure(t *testing.T) {
	m := newMailTest(t, false, false, settings.SMTPSecurityStartTLS, settings.SMTPAuthPlain)

	err := m.sender.Mail(context.Background(), m.config, downEvent())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("got %v, want a failure naming STARTTLS", err)
	}

	if session := m.server.only(t); session.password != "" || session.from != "" {
		t.Fatalf("the server was sent password=%q from=%q, want nothing", session.password, session.from)
	}
}
