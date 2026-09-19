package tlsserve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newDB returns a handle on an empty database file holding the certificate
// table. A real file is used rather than a fake driver because what is under
// test is what comes back out of the database after a write.
func newDB(t *testing.T) *gorm.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tunnel-manager.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&Certificate{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	return db
}

func newCipher(t *testing.T) *crypto.Cipher {
	t.Helper()

	key := make([]byte, crypto.KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("failed to generate a key: %v", err)
	}

	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("failed to build the cipher: %v", err)
	}

	return cipher
}

func storedRow(t *testing.T, db *gorm.DB) Certificate {
	t.Helper()

	var row Certificate

	err := db.First(&row, certificateID).Error
	if err != nil {
		t.Fatalf("reading the stored certificate: %v", err)
	}

	return row
}

// TestAFirstStartupStoresACertificateWithTheKeyEncrypted is the whole reason
// the key is in the database rather than in a file beside it. A database file
// that was copied off the machine must not hand the private key to whoever
// copied it.
func TestAFirstStartupStoresACertificateWithTheKeyEncrypted(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	keyPair, info, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !info.Created {
		t.Error("the first startup reported that it read a certificate, want one that it made")
	}
	if keyPair.Leaf == nil {
		t.Fatal("the certificate came back without its parsed form")
	}

	row := storedRow(t, db)

	if !strings.Contains(row.CertPEM, "BEGIN CERTIFICATE") {
		t.Errorf("the stored certificate is not PEM: %q", row.CertPEM)
	}
	if !crypto.IsEncrypted(row.KeyPEM) {
		t.Errorf("the stored private key carries no encryption marker: %q", row.KeyPEM)
	}
	if strings.Contains(row.KeyPEM, "PRIVATE KEY") {
		t.Error("the private key is in the database as it was generated, want it encrypted")
	}

	opened, err := cipher.Decrypt(row.KeyPEM)
	if err != nil {
		t.Fatalf("the stored private key does not open with the key it was sealed with: %v", err)
	}
	if !strings.Contains(opened, "BEGIN PRIVATE KEY") {
		t.Errorf("what the stored key opens to is not a PEM private key: %q", opened)
	}

	if row.Hosts == "" {
		t.Error("the row does not say what the certificate is made out to")
	}
	if !row.NotAfter.After(row.NotBefore) {
		t.Errorf("the stored validity is %s to %s", row.NotBefore, row.NotAfter)
	}
}

// TestASecondStartupServesTheSameCertificate is what keeps the operator from
// having to trust a new certificate on every restart.
func TestASecondStartupServesTheSameCertificate(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	first, firstInfo, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("the first LoadOrCreate: %v", err)
	}

	second, secondInfo, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("the second LoadOrCreate: %v", err)
	}

	if secondInfo.Created {
		t.Errorf("the second startup made a new certificate, reason: %s", secondInfo.Reason)
	}
	if secondInfo.Fingerprint != firstInfo.Fingerprint {
		t.Errorf("the second startup served %s, want the stored %s",
			secondInfo.Fingerprint, firstInfo.Fingerprint)
	}
	if Fingerprint(second.Leaf) != Fingerprint(first.Leaf) {
		t.Error("the certificate that came back the second time is another one")
	}

	var count int64

	err = db.Model(&Certificate{}).Count(&count).Error
	if err != nil {
		t.Fatalf("counting the stored certificates: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d certificate rows are stored, want 1", count)
	}
}

// TestAKeyThatDoesNotOpenIsReplaced covers the database that was carried to
// another installation, or whose encryption key file was lost. The certificate
// is derived material, so another one is made rather than the startup refusing
// to serve, and the reason is carried out so the log can say why the
// fingerprint changed.
func TestAKeyThatDoesNotOpenIsReplaced(t *testing.T) {
	db := newDB(t)

	_, before, err := LoadOrCreate(db, newCipher(t), time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	other := newCipher(t)

	_, after, err := LoadOrCreate(db, other, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate with another encryption key: %v", err)
	}

	if !after.Created {
		t.Fatal("a stored key that does not open was served as it was")
	}
	if after.Fingerprint == before.Fingerprint {
		t.Error("the replaced certificate has the fingerprint of the one that could not be opened")
	}
	if !strings.Contains(after.Reason, "does not open") {
		t.Errorf("the reason is %q, want it to say the key does not open", after.Reason)
	}

	row := storedRow(t, db)
	if _, err := other.Decrypt(row.KeyPEM); err != nil {
		t.Errorf("the replacement was not stored under the encryption key in use: %v", err)
	}
}

// TestAnExpiredCertificateIsReplaced is the startup that happens after the
// stored certificate has run out. Serving the stored certificate then would be
// serving one every client refuses.
func TestAnExpiredCertificateIsReplaced(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	now := time.Now()

	_, before, err := LoadOrCreate(db, cipher, now)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	_, after, err := LoadOrCreate(db, cipher, now.Add(certValidity+2*time.Hour))
	if err != nil {
		t.Fatalf("LoadOrCreate after the certificate expired: %v", err)
	}

	if !after.Created {
		t.Fatal("an expired certificate was served as it was")
	}
	if after.Fingerprint == before.Fingerprint {
		t.Error("the expired certificate came back with the same fingerprint")
	}
	if !strings.Contains(after.Reason, "expired") {
		t.Errorf("the reason is %q, want it to say the certificate expired", after.Reason)
	}
}

// TestACertificateThatCannotBeReadIsReplaced covers a row that was damaged, by
// a hand at the database or by a write that did not finish.
func TestACertificateThatCannotBeReadIsReplaced(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	_, _, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	err = db.Model(&Certificate{}).Where("id = ?", certificateID).
		Update("cert_pem", "-----BEGIN CERTIFICATE-----\nnot a certificate\n-----END CERTIFICATE-----\n").Error
	if err != nil {
		t.Fatalf("damaging the stored certificate: %v", err)
	}

	_, after, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate on a damaged row: %v", err)
	}
	if !after.Created {
		t.Fatal("a certificate that cannot be read was served as it was")
	}
}

// TestTheFingerprintIsSpelledAsOpenSSLSpellsIt keeps the startup log readable
// against what the operator gets from openssl or from the browser.
func TestTheFingerprintIsSpelledAsOpenSSLSpellsIt(t *testing.T) {
	db := newDB(t)

	keyPair, info, err := LoadOrCreate(db, newCipher(t), time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	got := Fingerprint(keyPair.Leaf)
	if got != info.Fingerprint {
		t.Errorf("the reported fingerprint is %s, want %s", info.Fingerprint, got)
	}

	parts := strings.Split(got, ":")
	if len(parts) != 32 {
		t.Fatalf("the fingerprint is %s, want 32 bytes of SHA-256", got)
	}
	for _, part := range parts {
		if len(part) != 2 || strings.ToUpper(part) != part {
			t.Fatalf("the fingerprint is %s, want upper case byte pairs", got)
		}
	}
}

// TestRenewReplacesTheStoredCertificate is the button on the Settings screen.
// What matters is that the row is replaced rather than added to, that the
// fingerprint is another one, and that the key of the new certificate is sealed
// the way the generated one is.
func TestRenewReplacesTheStoredCertificate(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	_, before, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	keyPair, after, err := Renew(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}

	if after.Fingerprint == before.Fingerprint {
		t.Error("the renewal came back with the fingerprint of the certificate it replaced")
	}
	if keyPair.Leaf == nil {
		t.Fatal("the renewal came back without its parsed form")
	}
	if Fingerprint(keyPair.Leaf) != after.Fingerprint {
		t.Error("the reported fingerprint is not the one of the certificate that came back")
	}

	row := storedRow(t, db)

	if strings.Contains(row.KeyPEM, "PRIVATE KEY") {
		t.Error("the renewed private key is in the database as it was generated, want it encrypted")
	}
	if !crypto.IsEncrypted(row.KeyPEM) {
		t.Errorf("the renewed private key carries no encryption marker: %q", row.KeyPEM)
	}

	var count int64

	err = db.Model(&Certificate{}).Count(&count).Error
	if err != nil {
		t.Fatalf("counting the stored certificates: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d certificate rows are stored after a renewal, want 1", count)
	}

	// The next startup has to come up on the renewal and not make one of its
	// own, which is what says the renewal was stored in a form that reads back.
	_, reloaded, err := LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreate after the renewal: %v", err)
	}
	if reloaded.Created {
		t.Errorf("the startup after a renewal made another certificate, reason: %s", reloaded.Reason)
	}
	if reloaded.Fingerprint != after.Fingerprint {
		t.Errorf("the startup after a renewal serves %s, want the renewed %s",
			reloaded.Fingerprint, after.Fingerprint)
	}
}

// issued is a certificate and its private key in the PEM form an operator
// pastes into the two boxes on the screen.
type issued struct {
	certPEM string
	keyPEM  string
	leaf    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// issue builds one certificate for the Install tests. parent is what signs it,
// or nil for one that signs for itself.
func issue(t *testing.T, template *x509.Certificate, parent *issued) issued {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a key: %v", err)
	}

	if template.SerialNumber == nil {
		serial, serialErr := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
		if serialErr != nil {
			t.Fatalf("failed to generate a serial number: %v", serialErr)
		}

		template.SerialNumber = serial
	}

	signerCert := template
	signerKey := any(key)

	if parent != nil {
		signerCert = parent.leaf
		signerKey = parent.key
	}

	der, err := x509.CreateCertificate(rand.Reader, template, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("failed to create the certificate: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("failed to encode the key: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("failed to parse the certificate that was just created: %v", err)
	}

	return issued{
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		leaf:    leaf,
		key:     key,
	}
}

// serverTemplate is a certificate an operator would be given for this server.
func serverTemplate(now time.Time) *x509.Certificate {
	return &x509.Certificate{
		Subject:               pkix.Name{CommonName: "tunnel-manager.example"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"tunnel-manager.example"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
}

// TestInstallStoresThePairWithTheKeySealed is the manual registration. The
// private key of a certificate somebody paid for is worth no less than the one
// this installation generates, so it gets the same treatment in the database.
func TestInstallStoresThePairWithTheKeySealed(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	now := time.Now()

	_, before, err := LoadOrCreate(db, cipher, now)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	given := issue(t, serverTemplate(now), nil)

	keyPair, info, err := Install(db, cipher, given.certPEM, given.keyPEM, now)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if info.Warning != "" {
		t.Errorf("a certificate that is valid now was installed with the warning %q", info.Warning)
	}
	if info.Fingerprint == before.Fingerprint {
		t.Error("the installed certificate has the fingerprint of the one it replaced")
	}
	if info.Fingerprint != Fingerprint(given.leaf) {
		t.Errorf("the installed certificate is %s, want the one that was handed in, %s",
			info.Fingerprint, Fingerprint(given.leaf))
	}
	if keyPair.Leaf == nil || Fingerprint(keyPair.Leaf) != info.Fingerprint {
		t.Error("the pair that came back is not the certificate that was reported")
	}

	want := []string{"tunnel-manager.example", "127.0.0.1"}
	if strings.Join(info.Hosts, ",") != strings.Join(want, ",") {
		t.Errorf("the certificate is reported as made out to %v, want %v", info.Hosts, want)
	}

	row := storedRow(t, db)

	if strings.Contains(row.KeyPEM, "PRIVATE KEY") {
		t.Error("the private key that was handed in is in the database as it was given, " +
			"want it encrypted")
	}
	if !crypto.IsEncrypted(row.KeyPEM) {
		t.Errorf("the stored private key carries no encryption marker: %q", row.KeyPEM)
	}

	opened, err := cipher.Decrypt(row.KeyPEM)
	if err != nil {
		t.Fatalf("the stored private key does not open: %v", err)
	}
	if strings.TrimSpace(opened) != strings.TrimSpace(given.keyPEM) {
		t.Error("what the stored key opens to is not the key that was handed in")
	}

	// The next startup has to come up on what was installed rather than make a
	// certificate of its own.
	_, reloaded, err := LoadOrCreate(db, cipher, now)
	if err != nil {
		t.Fatalf("LoadOrCreate after the install: %v", err)
	}
	if reloaded.Created {
		t.Errorf("the startup after an install made a certificate, reason: %s", reloaded.Reason)
	}
	if reloaded.Fingerprint != info.Fingerprint {
		t.Errorf("the startup after an install serves %s, want the installed %s",
			reloaded.Fingerprint, info.Fingerprint)
	}
}

// TestInstallTakesAChain covers what a certificate authority actually hands
// back: the server certificate with the intermediate that signed it behind it.
// Refusing the second block would leave the operator trimming the file by hand,
// and serving only the first would leave clients unable to build a chain.
func TestInstallTakesAChain(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	now := time.Now()

	intermediate := issue(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "an intermediate"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}, nil)

	leaf := issue(t, serverTemplate(now), &intermediate)

	chainPEM := leaf.certPEM + intermediate.certPEM

	keyPair, info, err := Install(db, cipher, chainPEM, leaf.keyPEM, now)
	if err != nil {
		t.Fatalf("Install of a chain: %v", err)
	}

	if len(keyPair.Certificate) != 2 {
		t.Fatalf("the installed pair holds %d certificates, want the leaf and the intermediate",
			len(keyPair.Certificate))
	}
	if info.Fingerprint != Fingerprint(leaf.leaf) {
		t.Error("the leaf of the chain is not the first certificate in it")
	}

	row := storedRow(t, db)
	if !strings.Contains(row.CertPEM, "BEGIN CERTIFICATE") ||
		strings.Count(row.CertPEM, "BEGIN CERTIFICATE") != 2 {
		t.Errorf("the stored certificate holds %d blocks, want the chain as it was given",
			strings.Count(row.CertPEM, "BEGIN CERTIFICATE"))
	}
}

// TestInstallRefusesWhatCannotBeServed is the list of things an operator gets
// wrong. Each refusal has to say which of the two boxes is the problem and
// what is wrong with it, because there is nothing else on the screen to work it
// out from.
func TestInstallRefusesWhatCannotBeServed(t *testing.T) {
	now := time.Now()

	good := issue(t, serverTemplate(now), nil)
	other := issue(t, serverTemplate(now), nil)

	expired := issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "an expired certificate"},
		NotBefore:   now.Add(-48 * time.Hour),
		NotAfter:    now.Add(-24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, nil)

	clientOnly := issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "a client certificate"},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"tunnel-manager.example"},
	}, nil)

	cases := []struct {
		name    string
		certPEM string
		keyPEM  string
		says    string
	}{
		{
			name:    "a key that belongs to another certificate",
			certPEM: good.certPEM,
			keyPEM:  other.keyPEM,
			says:    "do not go together",
		},
		{
			name:    "a certificate that is not PEM at all",
			certPEM: "this is not a certificate",
			keyPEM:  good.keyPEM,
			says:    "the certificate is not PEM",
		},
		{
			name:    "a key that is not PEM at all",
			certPEM: good.certPEM,
			keyPEM:  "hunter2",
			says:    "the private key is not PEM",
		},
		{
			name:    "the two boxes filled the other way round",
			certPEM: good.keyPEM,
			keyPEM:  good.certPEM,
			says:    "the other way round",
		},
		{
			name:    "a certificate that has expired",
			certPEM: expired.certPEM,
			keyPEM:  expired.keyPEM,
			says:    "expired on",
		},
		{
			name:    "a certificate that may not be used by a server",
			certPEM: clientOnly.certPEM,
			keyPEM:  clientOnly.keyPEM,
			says:    "serverAuth",
		},
		{
			name:    "nothing in the certificate box",
			certPEM: "",
			keyPEM:  good.keyPEM,
			says:    "the certificate is not PEM",
		},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			db := newDB(t)
			cipher := newCipher(t)

			_, before, err := LoadOrCreate(db, cipher, now)
			if err != nil {
				t.Fatalf("LoadOrCreate: %v", err)
			}

			_, _, err = Install(db, cipher, one.certPEM, one.keyPEM, now)
			if err == nil {
				t.Fatal("the certificate was taken, want it refused")
			}

			var refused *InputError
			if !errors.As(err, &refused) {
				t.Fatalf("the refusal is %T, want one the caller can answer as a bad request", err)
			}
			if !strings.Contains(err.Error(), one.says) {
				t.Errorf("the refusal reads %q, want it to say %q", err.Error(), one.says)
			}

			// Nothing may have been stored. An operator who pasted the wrong
			// file has to be left on the certificate that is being served.
			row := storedRow(t, db)
			if Fingerprint(mustParse(t, row.CertPEM)) != before.Fingerprint {
				t.Error("the refused certificate was stored anyway")
			}
		})
	}
}

// mustParse is the first certificate of a stored PEM.
func mustParse(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("the stored certificate is not PEM: %q", certPEM)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("the stored certificate cannot be parsed: %v", err)
	}

	return cert
}

// TestInstallWarnsAboutACertificateThatIsNotValidYet is the one case that is
// taken rather than refused. A clock that is a few minutes off is ordinary
// between two machines, and an operator who cannot install a certificate at all
// because of it has nowhere to go; one who installs it and is told when it
// starts can decide for themselves.
func TestInstallWarnsAboutACertificateThatIsNotValidYet(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	now := time.Now()

	later := issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "a certificate that starts later"},
		NotBefore:   now.Add(2 * time.Hour),
		NotAfter:    now.Add(48 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"tunnel-manager.example"},
	}, nil)

	_, info, err := Install(db, cipher, later.certPEM, later.keyPEM, now)
	if err != nil {
		t.Fatalf("Install of a certificate that starts later: %v", err)
	}

	if info.Warning == "" {
		t.Fatal("a certificate that is not valid yet was installed without a word about it")
	}
	if !strings.Contains(info.Warning, "not valid until") {
		t.Errorf("the warning reads %q, want it to say when the certificate starts", info.Warning)
	}

	row := storedRow(t, db)
	if Fingerprint(mustParse(t, row.CertPEM)) != info.Fingerprint {
		t.Error("the certificate was not stored")
	}
}

// TestInstallTakesACertificateWithNoExtendedKeyUsage keeps the check above from
// refusing a certificate that names no extended key usage at all. An empty list
// is not a restriction, and a client applies none.
func TestInstallTakesACertificateWithNoExtendedKeyUsage(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t)

	now := time.Now()

	plain := issue(t, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "no extended key usage"},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(48 * time.Hour),
		DNSNames:  []string{"tunnel-manager.example"},
	}, nil)

	_, info, err := Install(db, cipher, plain.certPEM, plain.keyPEM, now)
	if err != nil {
		t.Fatalf("Install of a certificate with no extended key usage: %v", err)
	}
	if info.Fingerprint != Fingerprint(plain.leaf) {
		t.Error("the certificate that was stored is not the one that was handed in")
	}
}
