package tlsserve

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
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
	// Warning is what is wrong with a certificate that was taken anyway. It is
	// empty for everything this package generates itself and is filled by
	// Install, which takes a certificate whose validity has not started yet
	// rather than refusing it. Empty means there is nothing to say.
	Warning string
}

// InputError marks the refusals that are about what the operator handed in,
// as against a database that could not be written to or a cipher that could not
// seal the key. The caller answers the first with a bad request carrying this
// message, because the message is what says which of the two PEM boxes to go
// back to, and the second with a plain failure, because a message about the
// database says nothing anybody outside this process can act on.
type InputError struct {
	Reason string
}

func (e *InputError) Error() string {
	return e.Reason
}

func inputError(format string, args ...any) error {
	return &InputError{Reason: fmt.Sprintf(format, args...)}
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

// Renew makes another self-signed certificate, stores it over the one that is
// there and hands it back.
//
// It is the same generation the first startup performs, so the names the new
// certificate is made out to are the ones this machine answers to now, which is
// half of what an operator presses this for: a machine that was given another
// address is served under a certificate that never carried it until this runs.
// The other half is a certificate that is running out.
//
// Putting the result in front of the clients is the caller's to do, by way of
// the Holder. Nothing here touches the running listener.
func Renew(db *gorm.DB, cipher *crypto.Cipher, now time.Time) (*tls.Certificate, *Info, error) {
	return create(db, cipher, now, "a new certificate was asked for")
}

// Install stores the certificate and private key an operator supplied and hands
// the pair back.
//
// certPEM may be a chain. What a certificate authority hands back is usually
// the server certificate followed by the intermediates that sign up to a root,
// and a server that serves only the first of them is one a client cannot build
// a chain from unless it happens to hold the intermediate already. The whole of
// what was given is stored and served, and the leaf is the first block, which
// is the order TLS itself requires.
//
// The private key is sealed with the cipher before it is written, the same as
// the generated one: what lands in the database file is never the key.
func Install(db *gorm.DB, cipher *crypto.Cipher, certPEM string, keyPEM string,
	now time.Time) (*tls.Certificate, *Info, error) {
	certPEM = strings.TrimSpace(certPEM) + "\n"
	keyPEM = strings.TrimSpace(keyPEM) + "\n"

	err := checkCertificatePEM(certPEM)
	if err != nil {
		return nil, nil, err
	}

	err = checkKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, err
	}

	keyPair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		// This is where a key that belongs to another certificate is caught.
		// The two are read separately up to here, and only putting them
		// together shows that the public key in the certificate is not the one
		// this private key goes with.
		return nil, nil, inputError("the certificate and the private key do not go together: %v", err)
	}

	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return nil, nil, inputError("the certificate cannot be parsed: %v", err)
	}

	if now.After(leaf.NotAfter) {
		return nil, nil, inputError("the certificate expired on %s, so nothing would connect to it",
			leaf.NotAfter.UTC().Format(time.RFC3339))
	}

	// An extended key usage that leaves serverAuth out is refused rather than
	// warned about. Unlike a clock, which two machines disagree about by
	// minutes, this is a statement inside the certificate that it is not for
	// serving TLS, and every client enforces it: what would be installed is a
	// certificate that fails the handshake for every one of them, which is a
	// server nobody can reach and no screen left to correct it from. A
	// certificate that names no extended key usage at all is not restricted and
	// is taken.
	if !servesTLS(leaf) {
		return nil, nil, inputError("the certificate is not made out for serving TLS: its extended " +
			"key usage does not include serverAuth, and a client refuses such a certificate " +
			"from a server")
	}

	// A certificate whose validity has not started is taken, with a warning,
	// rather than refused. The two machines involved are this one and whichever
	// signed the certificate, and a few minutes between their clocks is
	// ordinary: this package shifts its own certificates an hour back for that
	// very reason. Refusing here would turn a clock that is a minute fast into
	// a certificate the operator cannot install at all, while taking it leaves
	// a server that works from the moment the start time passes and a line that
	// says when that is.
	warning := ""
	if now.Before(leaf.NotBefore) {
		warning = fmt.Sprintf("the certificate is not valid until %s. A client that connects "+
			"before then refuses it. Check the clock of this machine if that time looks wrong",
			leaf.NotBefore.UTC().Format(time.RFC3339))
	}

	keyPair.Leaf = leaf

	sealed, err := cipher.Encrypt(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encrypt the private key: %w", err)
	}

	hosts := certificateHosts(leaf)

	row := Certificate{
		ID:        certificateID,
		CertPEM:   certPEM,
		KeyPEM:    sealed,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Hosts:     strings.Join(hosts, ","),
	}

	err = db.Save(&row).Error
	if err != nil {
		return nil, nil, fmt.Errorf("failed to store the certificate: %w", err)
	}

	return &keyPair, &Info{
		Fingerprint: Fingerprint(leaf),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Hosts:       hosts,
		Warning:     warning,
	}, nil
}

// checkCertificatePEM reports what is wrong with the certificate box before the
// pair is put together, so that a refusal names the box to go back to. The
// error tls.X509KeyPair gives for the same input says "failed to find any PEM
// data in certificate input", which does not tell an operator which of the two
// boxes it read that way.
func checkCertificatePEM(certPEM string) error {
	types := pemTypes(certPEM)

	if len(types) == 0 {
		return inputError("the certificate is not PEM: no -----BEGIN----- block was found in it. " +
			"Paste the file that starts with -----BEGIN CERTIFICATE-----")
	}

	if types[0] != "CERTIFICATE" {
		return inputError("the certificate box holds a %q block, not a CERTIFICATE block. "+
			"The two boxes may have been filled the other way round", types[0])
	}

	return nil
}

// checkKeyPEM is the same for the private key box.
func checkKeyPEM(keyPEM string) error {
	types := pemTypes(keyPEM)

	if len(types) == 0 {
		return inputError("the private key is not PEM: no -----BEGIN----- block was found in it")
	}

	if types[0] == "CERTIFICATE" {
		return inputError("the private key box holds a CERTIFICATE block. The two boxes may " +
			"have been filled the other way round")
	}

	// A key that is protected by a passphrase is refused with what to do about
	// it. Handed to X509KeyPair it comes back as "failed to parse private key",
	// which reads like a broken file rather than one that is only locked.
	if strings.Contains(types[0], "ENCRYPTED") {
		return inputError("the private key is protected by a passphrase. Take the passphrase off "+
			"it first, with openssl pkey -in key.pem -out plain.pem, and paste the result. "+
			"The block reads %q", types[0])
	}

	if !strings.Contains(types[0], "PRIVATE KEY") {
		return inputError("the private key box holds a %q block, not a private key", types[0])
	}

	return nil
}

// pemTypes is the label of every PEM block in text, in order. Anything between
// the blocks, which is where openssl writes the human readable summary of a
// certificate, is skipped by pem.Decode and is not an error here.
func pemTypes(text string) []string {
	var types []string

	rest := []byte(text)

	for {
		block, remainder := pem.Decode(rest)
		if block == nil {
			break
		}

		types = append(types, block.Type)
		rest = remainder
	}

	return types
}

// servesTLS reports whether the certificate says it may be used by a server.
// An empty list is not a restriction: it means the certificate was issued
// without one, and a client applies none.
func servesTLS(leaf *x509.Certificate) bool {
	if len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0 {
		return true
	}

	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
			return true
		}
	}

	return false
}

// certificateHosts is what the certificate is made out to, in the same shape
// the generated ones are stored in: the DNS names first and then the addresses.
// It is read off the certificate rather than off this machine, because an
// installed certificate carries whatever the authority put in it.
func certificateHosts(leaf *x509.Certificate) []string {
	hosts := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	hosts = append(hosts, leaf.DNSNames...)

	for _, ip := range leaf.IPAddresses {
		hosts = append(hosts, ip.String())
	}

	return hosts
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
//
// The certificate is reached through the holder and not through Certificates,
// so that replacing it takes no restart. See the comment on Holder for what
// that costs and for what the connections that are already open keep serving.
func ServerConfig(holder *Holder) *tls.Config {
	return &tls.Config{
		GetCertificate: holder.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}
