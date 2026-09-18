package tlsserve

import (
	"crypto/rand"
	"io"
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

// TestAnExpiredCertificateIsReplaced is the startup that happens more than 825
// days after the first one. Serving the stored certificate then would be
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
