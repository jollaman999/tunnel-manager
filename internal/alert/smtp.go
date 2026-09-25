package alert

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/settings"
)

// errAuthWithoutTLS is a password that would be sent in the clear. PlainAuth
// of net/smtp refuses the same thing with a sentence of its own.
var errAuthWithoutTLS = errors.New("refusing to send the mail password over a connection that is not protected by TLS")

// loginAuth is the LOGIN mechanism, which net/smtp does not carry. It is the
// user name and then the password, each sent as the answer to a prompt. Some
// servers take this one and not PLAIN.
//
// It refuses to start where PlainAuth does: on a connection that is not TLS,
// unless the server is this system itself, where nothing goes over a network.
type loginAuth struct {
	username string
	password string
	host     string
}

func isLocalhost(name string) bool {
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !isLocalhost(server.Name) {
		return "", nil, errAuthWithoutTLS
	}

	if server.Name != a.host {
		return "", nil, errors.New("wrong host name")
	}

	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}

	// The prompts are "Username:" and "Password:", and a server is free to
	// word them otherwise, so they are told apart by what they begin with and
	// by nothing else.
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))

	switch {
	case strings.HasPrefix(prompt, "user"):
		return []byte(a.username), nil
	case strings.HasPrefix(prompt, "pass"):
		return []byte(a.password), nil
	}

	return nil, fmt.Errorf("the server asked for something LOGIN does not answer: %q", string(fromServer))
}

// Mail sends one alert as a plain text message.
func (s *Sender) Mail(ctx context.Context, config SMTPConfig, event Event) error {
	from, err := mail.ParseAddress(config.From)
	if err != nil {
		return fmt.Errorf("the sender address cannot be read: %w", err)
	}

	if len(config.To) == 0 {
		return errors.New("no recipient address is stored")
	}

	recipients := make([]string, 0, len(config.To))
	for _, to := range config.To {
		parsed, err := mail.ParseAddress(to)
		if err != nil {
			return fmt.Errorf("the recipient address %q cannot be read: %w", to, err)
		}

		recipients = append(recipients, parsed.Address)
	}

	// The password is opened before anything is dialled, so that a password
	// that cannot be opened is reported as that rather than after a
	// connection has been made for nothing.
	var auth smtp.Auth

	switch config.Auth {
	case settings.SMTPAuthPlain, settings.SMTPAuthLogin:
		password, err := s.password(config.SealedPassword)
		if err != nil {
			return err
		}

		if config.Auth == settings.SMTPAuthPlain {
			auth = smtp.PlainAuth("", config.Username, password, config.Host)
		} else {
			auth = &loginAuth{username: config.Username, password: password, host: config.Host}
		}
	case settings.SMTPAuthNone:
	default:
		return fmt.Errorf("the mail login method %q is not one this program knows", config.Auth)
	}

	ctx, cancel := context.WithTimeout(ctx, s.mailTimeout)
	defer cancel()

	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))

	conn, err := s.dial(ctx, "tcp", address)
	if err != nil {
		return err
	}

	// The deadline covers the whole conversation, and closing the
	// connection when the context ends covers a caller that gives up first.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stop()

	tlsConfig := &tls.Config{
		ServerName: config.Host,
		RootCAs:    s.roots,
		// The check is turned off only where the setting says so, and the
		// Settings screen says beside the box what that costs.
		InsecureSkipVerify: config.SkipVerify, //nolint:gosec // Only where the stored setting asks for it.
		MinVersion:         tls.VersionTLS12,
	}

	switch config.Security {
	case settings.SMTPSecurityTLS:
		tlsConn := tls.Client(conn, tlsConfig)

		err = tlsConn.HandshakeContext(ctx)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("the TLS handshake with the mail server failed: %w", err)
		}

		conn = tlsConn
	case settings.SMTPSecurityStartTLS, settings.SMTPSecurityNone:
	default:
		_ = conn.Close()
		return fmt.Errorf("the mail connection security %q is not one this program knows", config.Security)
	}

	client, err := smtp.NewClient(conn, config.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()

	err = client.Hello(helloName(event.Installation))
	if err != nil {
		return err
	}

	if config.Security == settings.SMTPSecurityStartTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return errors.New("the mail server does not offer STARTTLS, so the connection cannot be protected")
		}

		err = client.StartTLS(tlsConfig)
		if err != nil {
			return fmt.Errorf("STARTTLS with the mail server failed: %w", err)
		}
	}

	if auth != nil {
		ok, _ := client.Extension("AUTH")
		if !ok {
			return errors.New("the mail server does not offer to log in, so the stored login cannot be used")
		}

		err = client.Auth(auth)
		if err != nil {
			return fmt.Errorf("logging in to the mail server failed: %w", err)
		}
	}

	err = client.Mail(from.Address)
	if err != nil {
		return err
	}

	for _, to := range recipients {
		err = client.Rcpt(to)
		if err != nil {
			return fmt.Errorf("the mail server refused the recipient %s: %w", to, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}

	_, err = writer.Write(message(config, event, time.Now()))
	if err != nil {
		_ = writer.Close()
		return err
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	return client.Quit()
}

// helloName is the name this program greets the server with. The host name of
// the system is what a server expects there; a name that is empty or would not
// pass as one falls back to what net/smtp says on its own.
func helloName(installation string) string {
	if installation == "" || strings.ContainsAny(installation, " \r\n\t") {
		return "localhost"
	}

	return installation
}

// headerValue keeps a value on the one header line it is written into.
func headerValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// kindNames is how each kind is written in a message.
var kindNames = map[string]string{
	KindServicePort:  "service port",
	KindLocalForward: "local forward",
	KindSocks:        "SOCKS5 proxy",
}

// message builds the message: a few headers and a plain text body that names
// the same things the webhook carries.
func message(config SMTPConfig, event Event, now time.Time) []byte {
	kind := kindNames[event.Kind]
	if kind == "" {
		kind = event.Kind
	}

	var subject string

	switch event.Event {
	case EventDown:
		subject = fmt.Sprintf("DOWN: %s %d on %s", kind, event.LocalPort, event.Host)
	case EventUp:
		subject = fmt.Sprintf("UP: %s %d on %s", kind, event.LocalPort, event.Host)
	default:
		subject = "Test alert"
	}

	if event.Installation != "" {
		subject = "[tunnel-manager " + event.Installation + "] " + subject
	} else {
		subject = "[tunnel-manager] " + subject
	}

	var b strings.Builder

	b.WriteString("From: " + headerValue(config.From) + "\r\n")
	b.WriteString("To: " + headerValue(strings.Join(config.To, ", ")) + "\r\n")
	b.WriteString("Subject: " + mimeHeader(headerValue(subject)) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")

	switch event.Event {
	case EventDown:
		b.WriteString("A forward has been without a connection for longer than the alert delay.\r\n")
	case EventUp:
		b.WriteString("A forward that was reported down is connected again.\r\n")
	default:
		b.WriteString("This is a test message sent from the Settings screen. Nothing is down.\r\n")
	}

	b.WriteString("\r\n")
	b.WriteString("event: " + event.Event + "\r\n")

	if event.Event != EventTest {
		b.WriteString("kind: " + event.Kind + "\r\n")
		b.WriteString("host: " + event.Host + "\r\n")
		b.WriteString("local_port: " + strconv.Itoa(event.LocalPort) + "\r\n")
		b.WriteString("since: " + event.Since + "\r\n")
		b.WriteString("last_error: " + headerValue(event.LastError) + "\r\n")
	}

	b.WriteString("installation: " + event.Installation + "\r\n")

	return []byte(b.String())
}

// mimeHeader writes a header value that is not ASCII as an encoded word, which
// is how a header carries it (RFC 2047). A host name is ASCII, but the name of
// the system is whatever it was set to.
func mimeHeader(value string) string {
	for _, r := range value {
		if r > 127 {
			return mime.QEncoding.Encode("UTF-8", value)
		}
	}

	return value
}
