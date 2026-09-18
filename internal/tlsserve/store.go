package tlsserve

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"gorm.io/gorm"
)

// certificateID is the primary key of the single row this package keeps. One
// process serves one port, so a second certificate would be one nothing is
// served with. The fixed key makes every read and every write name the same row.
const certificateID = 1

// Certificate is the stored certificate of this installation.
//
// It is in the database and not in a pair of files beside it because the
// database file is what this installation is: it already holds the settings,
// the hosts and the account, an uninstall removes it, and a backup that copies
// it takes the certificate along. Two files beside it would be two more things
// to keep together and two more ways for the certificate to go missing while
// the row that describes it stays.
type Certificate struct {
	ID uint `gorm:"primaryKey" json:"-"`

	// CertPEM is public by nature. Every client that connects is handed it.
	CertPEM string `json:"cert_pem"`

	// KeyPEM holds the private key as crypto.Cipher sealed it, never as it was
	// generated. The key opens every session that was recorded off the wire and
	// lets anyone who holds it serve as this installation, so a database file
	// that was copied off the machine must not be enough to have it. It is the
	// same treatment, and the same encryption key, as the SSH password of a
	// host.
	KeyPEM string `json:"-"`

	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`

	// Hosts is what the certificate was made out to, as one line, so that the
	// row says what it is good for without anybody having to parse the PEM.
	Hosts string `json:"hosts"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName keeps the table from being called "certificates", which says
// nothing about what is in it next to the hosts and the settings.
func (Certificate) TableName() string {
	return "tls_certificates"
}

// Info is what the startup reports about the certificate it is serving with.
// The fingerprint is in it so that an operator can hold what the browser shows
// against what this process loaded, and so that two startups can be told to
// have used the same certificate.
type Info struct {
	Fingerprint string
	NotBefore   time.Time
	NotAfter    time.Time
	Hosts       []string
	// Created reports that this startup generated the certificate, and Reason
	// says why it had to. A certificate that is generated on every startup is a
	// certificate the operator has to trust again on every startup, so the
	// reason belongs in the log rather than in nobody's hands.
	Created bool
	Reason  string
}

// LoadOrCreate returns the certificate this installation serves with, making
// one and storing it when there is none to read.
//
// A stored certificate that cannot be used is replaced rather than reported as
// a failure that stops the startup. It is derived material: unlike the password
// of a host, whose plaintext exists nowhere else, nothing is lost by making
// another one but the fingerprint the operator may have trusted, and refusing
// to start would leave them with a server they cannot reach to fix it. Why it
// was replaced is carried back in Info so that the log names it, because a
// fingerprint that changes on its own is exactly what a client warns about.
func LoadOrCreate(db *gorm.DB, cipher *crypto.Cipher, now time.Time) (*tls.Certificate, *Info, error) {
	stored, err := read(db)
	if err != nil {
		return nil, nil, err
	}

	if stored == nil {
		return create(db, cipher, now, "the database held no certificate")
	}

	keyPEM, err := cipher.Decrypt(stored.KeyPEM)
	if err != nil {
		return create(db, cipher, now,
			fmt.Sprintf("the stored private key does not open with the encryption key in use: %v", err))
	}

	keyPair, err := tls.X509KeyPair([]byte(stored.CertPEM), []byte(keyPEM))
	if err != nil {
		return create(db, cipher, now, fmt.Sprintf("the stored certificate cannot be read: %v", err))
	}

	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return create(db, cipher, now, fmt.Sprintf("the stored certificate cannot be parsed: %v", err))
	}

	if now.After(leaf.NotAfter) {
		return create(db, cipher, now,
			fmt.Sprintf("the stored certificate expired on %s", leaf.NotAfter.Format(time.RFC3339)))
	}

	keyPair.Leaf = leaf

	return &keyPair, &Info{
		Fingerprint: Fingerprint(leaf),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Hosts:       splitHosts(stored.Hosts),
	}, nil
}

// read returns the stored row, or nil when there is none.
func read(db *gorm.DB) (*Certificate, error) {
	var stored Certificate

	err := db.First(&stored, certificateID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the stored certificate: %w", err)
	}

	return &stored, nil
}

// create generates a certificate, stores it and returns it. The private key is
// encrypted before it is written, so what lands in the database file is never
// the key itself.
func create(db *gorm.DB, cipher *crypto.Cipher, now time.Time, reason string) (*tls.Certificate, *Info, error) {
	generated, err := generate(now)
	if err != nil {
		return nil, nil, err
	}

	keyPair, err := tls.X509KeyPair(generated.certPEM, generated.keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("the generated certificate cannot be read back: %w", err)
	}

	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("the generated certificate cannot be parsed: %w", err)
	}

	keyPair.Leaf = leaf

	sealed, err := cipher.Encrypt(string(generated.keyPEM))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encrypt the private key: %w", err)
	}

	row := Certificate{
		ID:        certificateID,
		CertPEM:   string(generated.certPEM),
		KeyPEM:    sealed,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Hosts:     strings.Join(generated.hosts, ","),
	}

	err = db.Save(&row).Error
	if err != nil {
		return nil, nil, fmt.Errorf("failed to store the certificate: %w", err)
	}

	return &keyPair, &Info{
		Fingerprint: Fingerprint(leaf),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Hosts:       generated.hosts,
		Created:     true,
		Reason:      reason,
	}, nil
}

// Fingerprint is the SHA-256 of the certificate, spelled the way openssl
// x509 -fingerprint -sha256 spells it, so that the two can be held against each
// other without anybody converting between formats.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)

	pairs := make([]string, 0, len(sum))
	for _, b := range sum {
		pairs = append(pairs, strings.ToUpper(hex.EncodeToString([]byte{b})))
	}

	return strings.Join(pairs, ":")
}

func splitHosts(joined string) []string {
	if joined == "" {
		return nil
	}

	return strings.Split(joined, ",")
}

// ServerConfig is what the TLS listener is built with.
//
// TLS 1.2 is the floor. Everything below it has been refused by every browser
// in use for years, and the clients that are not browsers here are curl and the
// scripts around it, which have spoken 1.2 for longer than that.
//
// HTTP/2 is not offered. The server serves a listener that is already wrapped,
// so net/http only speaks h2 if this config advertises it through ALPN, and
// nothing this API does needs the multiplexing: the screens make a handful of
// small requests. Leaving it out keeps one protocol on the wire to reason about.
func ServerConfig(cert *tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	}
}
