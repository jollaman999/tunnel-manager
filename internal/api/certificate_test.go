package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/tlsserve"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newCertificateDB returns a handle on an empty database file holding the
// certificate table, the same one the startup migrates.
func newCertificateDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&tlsserve.Certificate{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	return db
}

func newCertificateCipher(t *testing.T) *crypto.Cipher {
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

// newCertificateHandler returns the handler with a certificate already in
// place, which is the state a process serving over HTTPS is in, together with
// the pieces the tests read: the database, the cipher, the holder the listener
// would be reading through, and the log.
func newCertificateHandler(t *testing.T) (*CertificateHandler, *gorm.DB, *crypto.Cipher,
	*tlsserve.Holder, *observer.ObservedLogs) {
	t.Helper()

	db := newCertificateDB(t)
	cipher := newCertificateCipher(t)

	cert, _, err := tlsserve.LoadOrCreate(db, cipher, time.Now())
	if err != nil {
		t.Fatalf("failed to prepare a certificate: %v", err)
	}

	holder := tlsserve.NewHolder(cert)
	core, logs := observer.New(zapcore.InfoLevel)

	return NewCertificateHandler(db, cipher, zap.New(core), holder), db, cipher, holder, logs
}

// certificateRequest runs one call against the handler and hands back what it
// wrote.
func certificateRequest(t *testing.T, h *CertificateHandler, method string,
	body string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, "/api/certificate", reader)
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	var err error

	switch method {
	case http.MethodGet:
		err = h.GetCertificate(c)
	case http.MethodPost:
		err = h.RenewCertificate(c)
	case http.MethodPut:
		err = h.InstallCertificate(c)
	default:
		t.Fatalf("no handler for %s", method)
	}

	if err != nil {
		t.Fatalf("the handler returned an error: %v", err)
	}

	return rec
}

// decodeCertificateAnswer reads the envelope every answer of this API carries.
func decodeCertificateAnswer(t *testing.T, rec *httptest.ResponseRecorder) (bool, string,
	map[string]any) {
	t.Helper()

	var answer struct {
		Success bool           `json:"success"`
		Error   string         `json:"error"`
		Data    map[string]any `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("the answer is not JSON: %v, body %q", err, rec.Body.String())
	}

	return answer.Success, answer.Error, answer.Data
}

// fingerprintOf is the fingerprint of what the holder is serving.
func fingerprintOf(t *testing.T, cert *tls.Certificate) string {
	t.Helper()

	if cert == nil {
		t.Fatal("the holder carries no certificate")
	}

	leaf := cert.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatalf("the certificate cannot be parsed: %v", err)
		}

		leaf = parsed
	}

	return tlsserve.Fingerprint(leaf)
}

// TestGetCertificateReportsWhatIsBeingServed is the screen. Everything on it
// comes from this one answer.
func TestGetCertificateReportsWhatIsBeingServed(t *testing.T) {
	h, _, _, holder, _ := newCertificateHandler(t)

	rec := certificateRequest(t, h, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	success, failure, data := decodeCertificateAnswer(t, rec)
	if !success {
		t.Fatalf("the answer is a refusal: %s", failure)
	}

	want := fingerprintOf(t, holder.Current())
	if data["fingerprint_sha256"] != want {
		t.Errorf("the answer reports %v, want the certificate being served, %s",
			data["fingerprint_sha256"], want)
	}

	if data["self_signed"] != true {
		t.Error("the certificate this installation made for itself is not reported as self-signed")
	}

	hosts, ok := data["hosts"].([]any)
	if !ok || len(hosts) == 0 {
		t.Errorf("the answer does not say what the certificate is made out to: %v", data["hosts"])
	}

	days, ok := data["days_remaining"].(float64)
	if !ok || days <= 0 {
		t.Errorf("the answer reports %v days remaining on a certificate that was just made",
			data["days_remaining"])
	}

	if data["subject"] == "" || data["issuer"] == "" {
		t.Error("the answer leaves out the subject or the issuer")
	}
}

// TestNoAnswerCarriesThePrivateKey is the one thing that must hold for all
// three calls. The certificate is public and is handed to every client that
// connects; the key is what serving as this installation takes.
func TestNoAnswerCarriesThePrivateKey(t *testing.T) {
	h, _, _, _, _ := newCertificateHandler(t)

	pair := issuedPair(t, time.Now())

	calls := []struct {
		name   string
		method string
		body   string
	}{
		{name: "the certificate that is being served", method: http.MethodGet},
		{name: "a renewal", method: http.MethodPost},
		{name: "a manual registration", method: http.MethodPut, body: installBody(pair)},
	}

	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			rec := certificateRequest(t, h, call.method, call.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("the answer is %d, want 200. Body: %s", rec.Code, rec.Body.String())
			}

			if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
				t.Errorf("the answer carries a private key: %s", rec.Body.String())
			}
		})
	}
}

// TestRenewPutsTheNewCertificateInFrontOfTheNextClient is the whole point of
// the holder. Nothing restarts, and the next handshake is served the new one.
func TestRenewPutsTheNewCertificateInFrontOfTheNextClient(t *testing.T) {
	h, _, _, holder, logs := newCertificateHandler(t)

	before := fingerprintOf(t, holder.Current())

	rec := certificateRequest(t, h, http.MethodPost, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	success, failure, data := decodeCertificateAnswer(t, rec)
	if !success {
		t.Fatalf("the answer is a refusal: %s", failure)
	}

	after := fingerprintOf(t, holder.Current())
	if after == before {
		t.Fatal("the holder still carries the certificate that was there before the renewal")
	}

	// What the handshake would hand out is what the answer says it is.
	served, err := holder.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("the holder refuses a handshake after the renewal: %v", err)
	}
	if fingerprintOf(t, served) != after {
		t.Error("the holder hands out something other than what it reports")
	}

	certificate, ok := data["certificate"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no certificate: %v", data)
	}
	if certificate["fingerprint_sha256"] != after {
		t.Errorf("the answer reports %v, want the certificate now being served, %s",
			certificate["fingerprint_sha256"], after)
	}
	if data["previous_fingerprint_sha256"] != before {
		t.Errorf("the answer reports the old fingerprint as %v, want %s",
			data["previous_fingerprint_sha256"], before)
	}

	note, _ := data["note"].(string)
	if !strings.Contains(note, "already open") {
		t.Errorf("the answer does not say what happens to the connections that are open: %q", note)
	}

	// The line is what tells a replacement that was asked for from a
	// fingerprint that changed on its own.
	found := false

	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields["previous_fingerprint_sha256"] == before &&
			fields["fingerprint_sha256"] == after {
			found = true
		}
	}

	if !found {
		t.Errorf("no log line carries both the old and the new fingerprint. Lines: %v",
			logs.All())
	}
}

// TestInstallServesThePairThatWasSentAndSealsTheKey is the manual registration
// seen from the API: what is served afterwards is what was sent, and what is in
// the database is the key under encryption.
func TestInstallServesThePairThatWasSentAndSealsTheKey(t *testing.T) {
	h, db, cipher, holder, _ := newCertificateHandler(t)

	before := fingerprintOf(t, holder.Current())
	pair := issuedPair(t, time.Now())

	rec := certificateRequest(t, h, http.MethodPut, installBody(pair))
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	success, failure, data := decodeCertificateAnswer(t, rec)
	if !success {
		t.Fatalf("the answer is a refusal: %s", failure)
	}

	after := fingerprintOf(t, holder.Current())
	if after == before {
		t.Fatal("the holder still carries the certificate that was there before")
	}
	if after != pair.fingerprint {
		t.Errorf("the server is holding %s, want the certificate that was sent, %s",
			after, pair.fingerprint)
	}

	certificate, _ := data["certificate"].(map[string]any)
	if certificate["self_signed"] != true {
		t.Error("a certificate that signed for itself is not reported as self-signed")
	}

	var row tlsserve.Certificate

	err := db.First(&row, 1).Error
	if err != nil {
		t.Fatalf("reading the stored certificate: %v", err)
	}

	if strings.Contains(row.KeyPEM, "PRIVATE KEY") {
		t.Error("the private key is in the database as it was sent, want it encrypted")
	}
	if !crypto.IsEncrypted(row.KeyPEM) {
		t.Errorf("the stored private key carries no encryption marker: %q", row.KeyPEM)
	}

	opened, err := cipher.Decrypt(row.KeyPEM)
	if err != nil {
		t.Fatalf("the stored private key does not open: %v", err)
	}
	if !strings.Contains(opened, "PRIVATE KEY") {
		t.Error("what the stored key opens to is not a PEM private key")
	}
}

// TestInstallSaysWhatIsWrongWithWhatWasSent is what the operator is left with
// when the paste was wrong. A refusal that only says the certificate was not
// stored leaves them with two boxes and nothing to go on.
func TestInstallSaysWhatIsWrongWithWhatWasSent(t *testing.T) {
	now := time.Now()

	good := issuedPair(t, now)
	other := issuedPair(t, now)
	expired := expiredPair(t, now)

	cases := []struct {
		name string
		body string
		says string
	}{
		{
			name: "a key that belongs to another certificate",
			body: `{"cert_pem":` + quote(good.certPEM) + `,"key_pem":` + quote(other.keyPEM) + `}`,
			says: "do not go together",
		},
		{
			name: "something that is not PEM at all",
			body: `{"cert_pem":"nonsense","key_pem":"nonsense"}`,
			says: "not PEM",
		},
		{
			name: "a certificate that has expired",
			body: `{"cert_pem":` + quote(expired.certPEM) + `,"key_pem":` +
				quote(expired.keyPEM) + `}`,
			says: "expired on",
		},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h, _, _, holder, _ := newCertificateHandler(t)

			before := fingerprintOf(t, holder.Current())

			rec := certificateRequest(t, h, http.MethodPut, one.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the answer is %d, want 400. Body: %s", rec.Code, rec.Body.String())
			}

			_, failure, _ := decodeCertificateAnswer(t, rec)
			if !strings.Contains(failure, one.says) {
				t.Errorf("the refusal reads %q, want it to say %q", failure, one.says)
			}

			if fingerprintOf(t, holder.Current()) != before {
				t.Error("a refused certificate was put in front of the clients anyway")
			}
		})
	}
}

// TestTheCertificateCallsSayWhenHTTPSIsOff covers the process that came up in
// the clear. The routes are registered either way, and there is no certificate
// to report or replace.
func TestTheCertificateCallsSayWhenHTTPSIsOff(t *testing.T) {
	db := newCertificateDB(t)
	cipher := newCertificateCipher(t)
	core, _ := observer.New(zapcore.InfoLevel)

	h := NewCertificateHandler(db, cipher, zap.New(core), tlsserve.NewHolder(nil))

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := certificateRequest(t, h, method, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s answered %d, want 409. Body: %s", method, rec.Code, rec.Body.String())
		}

		_, failure, _ := decodeCertificateAnswer(t, rec)
		if !strings.Contains(failure, "HTTPS is turned off") {
			t.Errorf("%s answered %q, want it to name the setting", method, failure)
		}
	}
}

// testPair is a certificate and its key in the PEM form the operator pastes,
// with the fingerprint the server should report once it is installed.
type testPair struct {
	certPEM     string
	keyPEM      string
	fingerprint string
}

// issuedPair is a certificate an operator would be handed for this server. It
// is built here rather than taken from tlsserve, because what is under test is
// the handler taking material it did not make.
func issuedPair(t *testing.T, now time.Time) testPair {
	t.Helper()

	return pairFrom(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "tunnel-manager.example"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"tunnel-manager.example"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	})
}

// expiredPair is a certificate whose time has passed. Installing it would leave
// a server no client connects to.
func expiredPair(t *testing.T, now time.Time) testPair {
	t.Helper()

	return pairFrom(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "tunnel-manager.example"},
		NotBefore:   now.Add(-48 * time.Hour),
		NotAfter:    now.Add(-24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"tunnel-manager.example"},
	})
}

func pairFrom(t *testing.T, template *x509.Certificate) testPair {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate a serial number: %v", err)
	}

	template.SerialNumber = serial

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
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

	return testPair{
		certPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		fingerprint: tlsserve.Fingerprint(leaf),
	}
}

// installBody is the request the screen sends.
func installBody(pair testPair) string {
	return `{"cert_pem":` + quote(pair.certPEM) + `,"key_pem":` + quote(pair.keyPEM) + `}`
}

// quote puts a PEM block into a JSON string, newlines and all.
func quote(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		return `""`
	}

	return string(encoded)
}
