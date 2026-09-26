package alert

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"go.uber.org/zap"
)

// webhookTimeout bounds one post to the webhook, from the dial to the last byte
// of the answer. There is no retry: an alert that arrives late is still read,
// while one retried until it goes through would hold up every alert behind it.
const webhookTimeout = 10 * time.Second

// maxWebhookRedirects is how many redirects a post follows. A webhook that has
// moved answers with one, and a chain longer than this is a loop.
const maxWebhookRedirects = 3

// mailTimeout bounds one message, from the dial to the answer to QUIT. A
// server that accepts the connection and then says nothing would otherwise
// hold the sender for as long as TCP lets it.
const mailTimeout = 30 * time.Second

// Config is the alert settings as the sender needs them.
type Config struct {
	After      time.Duration
	WebhookURL string
	SMTP       SMTPConfig
}

// SMTPConfig is the mail settings. The password stays sealed with the key of
// this installation until the moment it is sent.
type SMTPConfig struct {
	Host           string
	Port           int
	Security       string
	Auth           string
	Username       string
	SealedPassword string
	From           string
	To             []string
	SkipVerify     bool
}

// Enabled says whether there is anywhere to send an alert to.
func (c Config) Enabled() bool {
	return c.WebhookURL != "" || c.SMTP.Enabled()
}

// Enabled says whether mail is switched on.
func (c SMTPConfig) Enabled() bool {
	return c.Host != ""
}

// ConfigOf reads the alert settings out of a stored set.
func ConfigOf(s *settings.Settings) Config {
	return Config{
		After:      time.Duration(s.AlertAfterSec) * time.Second,
		WebhookURL: s.AlertWebhookURL,
		SMTP: SMTPConfig{
			Host:           s.SMTPHost,
			Port:           s.SMTPPort,
			Security:       s.SMTPSecurity,
			Auth:           s.SMTPAuth,
			Username:       s.SMTPUsername,
			SealedPassword: s.SMTPPassword,
			From:           s.SMTPFrom,
			To:             settings.MailRecipients(s.SMTPTo),
			SkipVerify:     s.SMTPSkipVerify,
		},
	}
}

// Sender posts to the webhook and sends mail.
type Sender struct {
	logger *zap.Logger
	cipher *crypto.Cipher
	client *http.Client
	// dial opens the connection to the mail server. It is a field so that a
	// test can send a message meant for a name to a server of its own.
	dial func(ctx context.Context, network string, address string) (net.Conn, error)
	// roots is the pool the certificate of the mail server is checked
	// against. nil is the pool of the system, which is what it is outside the
	// tests.
	roots *x509.CertPool
	// mailTimeout is mailTimeout, a field so that a test need not wait it out.
	mailTimeout time.Duration
}

// NewSender builds a sender. The cipher is the one the password of the mail
// server was sealed with.
func NewSender(logger *zap.Logger, cipher *crypto.Cipher) *Sender {
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	return &Sender{
		logger: logger,
		cipher: cipher,
		client: &http.Client{
			Timeout: webhookTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > maxWebhookRedirects {
					return fmt.Errorf("stopped after %d redirects", maxWebhookRedirects)
				}

				return nil
			},
		},
		dial:        dialer.DialContext,
		mailTimeout: mailTimeout,
	}
}

// Send sends one alert every way that is switched on. A failure is logged and
// nothing else: there is no retry, and the scan that queued it has moved on.
func (s *Sender) Send(ctx context.Context, config Config, event Event) {
	if config.WebhookURL != "" {
		err := s.Webhook(ctx, config.WebhookURL, event)
		if err != nil {
			s.logger.Warn("failed to post an alert to the webhook",
				logid.AlertWebhookFailed.Field(),
				zap.String("event", event.Event),
				zap.String("host", event.Host),
				zap.Int("local_port", event.LocalPort),
				zap.Error(err))
		} else {
			s.logger.Info("posted an alert to the webhook",
				logid.AlertWebhookSent.Field(),
				zap.String("event", event.Event),
				zap.String("host", event.Host),
				zap.Int("local_port", event.LocalPort))
		}
	}

	if config.SMTP.Enabled() {
		err := s.Mail(ctx, config.SMTP, event)
		if err != nil {
			s.logger.Warn("failed to send an alert by mail",
				logid.AlertMailFailed.Field(),
				zap.String("event", event.Event),
				zap.String("host", event.Host),
				zap.Int("local_port", event.LocalPort),
				zap.Error(err))
		} else {
			s.logger.Info("sent an alert by mail",
				logid.AlertMailSent.Field(),
				zap.String("event", event.Event),
				zap.String("host", event.Host),
				zap.Int("local_port", event.LocalPort))
		}
	}
}

// Webhook posts one alert as JSON. Any answer outside 2xx is a failure, since
// a webhook that did not take the alert is one nobody was told through.
func (s *Sender) Webhook(ctx context.Context, url string, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return webhookError(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tunnel-manager")

	resp, err := s.client.Do(req)
	if err != nil {
		return webhookError(err)
	}
	defer resp.Body.Close()

	// What the webhook answered is read and dropped, so that the connection
	// can be used again, and only so much of it: nothing in it is used.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the webhook answered %s", resp.Status)
	}

	return nil
}

// webhookError is a failure to post with the address of the webhook taken
// out. A *url.Error names the address it failed on, the one a redirect led to
// among them, and the path and the query of a webhook address are where its
// token is. What was done and why it failed are kept.
func webhookError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s to the webhook: %w", urlErr.Op, urlErr.Err)
	}

	return err
}

// errNoPassword is the mail password that is asked for and not stored.
var errNoPassword = errors.New("the mail server is logged in to with a password and none is stored")

// password opens the stored password of the mail server.
func (s *Sender) password(sealed string) (string, error) {
	if sealed == "" {
		return "", errNoPassword
	}

	plaintext, err := s.cipher.Decrypt(sealed)
	if err != nil {
		return "", fmt.Errorf("the stored mail password does not open with the encryption key in use: %w", err)
	}

	return plaintext, nil
}
