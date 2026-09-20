package api

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tlsserve"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// CertificateHandler serves the certificate part of the Settings screen: what
// is being served now, a self-signed one made to replace it, and a pair the
// operator was given by somebody else.
//
// It holds the same three things the startup holds for the certificate: the
// database the row is in, the cipher the private key is sealed with, and the
// holder the running listener reads the certificate out of. Storing and serving
// are two steps, and a handler that did only the first would leave the operator
// with a certificate that takes effect at the next restart, which is the thing
// this screen exists to avoid.
type CertificateHandler struct {
	db     *gorm.DB
	cipher *crypto.Cipher
	logger *zap.Logger
	holder *tlsserve.Holder
}

func NewCertificateHandler(db *gorm.DB, cipher *crypto.Cipher, logger *zap.Logger,
	holder *tlsserve.Holder) *CertificateHandler {
	return &CertificateHandler{
		db:     db,
		cipher: cipher,
		logger: logger,
		holder: holder,
	}
}

// The two strings the certificate answers carry: the note every replacement
// comes back with, and the one thing that can be wrong with a certificate that
// was stored anyway.
const (
	textCertificateReplaced    textCode = "certificate.replaced"
	textCertificateNotValidYet textCode = "certificate.not_valid_yet"
)

// certificateReplacedNote is what a replacement answers with, and it is here
// rather than left to the screen because it is a property of TLS and not of one
// client: a handshake settles on a certificate once and the connection never
// looks at it again.
//
// Without it the operator presses the button, sees the same padlock and the
// same fingerprint in the browser for as long as the page stays open, and has
// no way to tell that from a replacement that did not happen.
const certificateReplacedNote = "Every connection that is already open keeps the certificate it " +
	"was opened under, this one included, so the page in front of you is still being served the " +
	"old certificate. Connections made from here on are served the new one. Reload the page, or " +
	"open it again, to see it. A client that was told to trust the old certificate by hand warns " +
	"once more until the new fingerprint is trusted as well."

// certificateView is what this screen is drawn from. There is no field for the
// private key and no code path that reads one: the certificate is handed to
// every client that connects and is public by nature, while the key is the one
// thing that must never leave this process.
type certificateView struct {
	Fingerprint   string    `json:"fingerprint_sha256"`
	Subject       string    `json:"subject"`
	Issuer        string    `json:"issuer"`
	SelfSigned    bool      `json:"self_signed"`
	Hosts         []string  `json:"hosts"`
	NotBefore     time.Time `json:"not_before"`
	NotAfter      time.Time `json:"not_after"`
	DaysRemaining int       `json:"days_remaining"`
	// CertPEM is here so that an operator can take the self-signed certificate
	// into the trust store of a machine without going to the database for it.
	// It is what the server hands to every client during the handshake anyway.
	CertPEM string `json:"cert_pem"`
}

// certificateReplaced is what the two writes answer with. The fingerprint that
// was in place is carried next to the new one so that the answer alone says
// what changed, which is what a client warns about and what the log line holds.
type certificateReplaced struct {
	Certificate         certificateView `json:"certificate"`
	PreviousFingerprint string          `json:"previous_fingerprint_sha256"`
	Note                string          `json:"note"`
	// NoteCode names the note, so that a screen says it in the language it is
	// drawn in instead of showing the English above.
	NoteCode textCode `json:"note_code"`
	// Warning is what is wrong with a certificate that was stored anyway, or ""
	// when there is nothing to say. Install fills it for a certificate whose
	// validity has not started yet.
	Warning string `json:"warning,omitempty"`
	// WarningCode names that warning and WarningArgs holds the value written
	// into it. Both are left out where there is no warning, and the code is
	// left out for a warning this file has no name for, which is the case the
	// screen shows the English sentence for.
	WarningCode textCode `json:"warning_code,omitempty"`
	WarningArgs textArgs `json:"warning_args,omitempty"`
}

// certificateInstall is the body of the manual registration. The two are sent
// as separate fields rather than as one blob, because they are two files on the
// operator's disk and because a refusal can then name which of them is wrong.
type certificateInstall struct {
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

// GetCertificate answers with the certificate this process is serving with.
func (h *CertificateHandler) GetCertificate(c echo.Context) error {
	current := h.holder.Current()
	if current == nil {
		return h.noCertificate(c)
	}

	leaf, err := leafOf(current)
	if err != nil {
		h.logger.Error("the certificate being served cannot be parsed",
			logid.CertificateServedUnparsable.Field(),
			zap.Error(err))

		return failure(c, http.StatusInternalServerError, errCertificateServedUnread)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    viewOf(current, leaf, time.Now()),
	})
}

// RenewCertificate makes another self-signed certificate and puts it in front
// of the clients that connect from here on.
func (h *CertificateHandler) RenewCertificate(c echo.Context) error {
	previous := h.holder.Current()
	if previous == nil {
		return h.noCertificate(c)
	}

	now := time.Now()

	keyPair, info, err := tlsserve.Renew(h.db, h.cipher, now)
	if err != nil {
		h.logger.Error("failed to make a new certificate", logid.CertificateCreateFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errCertificateRenewFailed)
	}

	return h.replaced(c, previous, keyPair, info, now, "generated")
}

// InstallCertificate stores the pair the request carries and puts it in front
// of the clients that connect from here on.
func (h *CertificateHandler) InstallCertificate(c echo.Context) error {
	previous := h.holder.Current()
	if previous == nil {
		return h.noCertificate(c)
	}

	var body certificateInstall

	err := c.Bind(&body)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	now := time.Now()

	keyPair, info, err := tlsserve.Install(h.db, h.cipher, body.CertPEM, body.KeyPEM, now)
	if err != nil {
		// What the operator pasted is answered with what is wrong with it. The
		// message is built in tlsserve out of what it checked, not out of a
		// database or a cipher, so there is nothing about this process in it.
		var refused *tlsserve.InputError
		if errors.As(err, &refused) {
			return failure(c, http.StatusBadRequest, errCertificateInstallRefused, errorArgs{"reason": refused.Error()})
		}

		h.logger.Error("failed to store the certificate that was sent", logid.CertificateStoreFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errCertificateStoreFailed)
	}

	return h.replaced(c, previous, keyPair, info, now, "installed")
}

// replaced puts the new certificate in the holder, writes the line that says
// the fingerprint changed and answers.
//
// The log line is the point of this being one function. A fingerprint that
// changes is exactly what a client warns about and what somebody in the middle
// of the connection would produce, so an installation that cannot show when its
// own fingerprint changed and to what cannot tell the two apart afterwards.
// Both fingerprints are on the line, because the old one is what the warning on
// the client is comparing against.
func (h *CertificateHandler) replaced(c echo.Context, previous *tls.Certificate,
	keyPair *tls.Certificate, info *tlsserve.Info, now time.Time, how string) error {
	leaf, err := leafOf(keyPair)
	if err != nil {
		// The certificate is stored and is usable, since tlsserve parsed it to
		// build the Info above. Only the shape of the answer is in doubt here,
		// and putting it in front of the clients would leave the operator with
		// a replacement they were not told about.
		h.logger.Error("the new certificate cannot be parsed and was not put in place",
			logid.CertificateNewUnparsable.Field(),
			zap.Error(err))

		return failure(c, http.StatusInternalServerError, errCertificateReadBackFailed)
	}

	previousFingerprint := ""

	previousLeaf, err := leafOf(previous)
	if err == nil {
		previousFingerprint = tlsserve.Fingerprint(previousLeaf)
	}

	// The holder is set last. Up to here nothing the client connects to has
	// changed, so a failure above leaves the running server on the certificate
	// it came up with.
	h.holder.Set(keyPair)

	h.logger.Info("the TLS certificate of this installation was replaced from the Settings screen. "+
		"Connections that were already open keep the old one; every new connection is served the "+
		"new one",
		logid.CertificateReplaced.Field(),
		zap.String("how", how),
		zap.String("previous_fingerprint_sha256", previousFingerprint),
		zap.String("fingerprint_sha256", info.Fingerprint),
		zap.Strings("hosts", info.Hosts),
		zap.Time("not_after", info.NotAfter),
		zap.String("client", c.RealIP()))

	if info.Warning != "" {
		h.logger.Warn("the certificate that was installed is not usable yet",
			logid.CertificateNotUsableYet.Field(),
			zap.String("fingerprint_sha256", info.Fingerprint),
			zap.String("warning", info.Warning))
	}

	warningCode, warningArgs := certificateWarning(info, now)

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: certificateReplaced{
			Certificate:         viewOf(keyPair, leaf, now),
			PreviousFingerprint: previousFingerprint,
			Note:                certificateReplacedNote,
			NoteCode:            textCertificateReplaced,
			Warning:             info.Warning,
			WarningCode:         warningCode,
			WarningArgs:         warningArgs,
		},
	})
}

// certificateWarning names the warning an install came back with.
//
// tlsserve writes the English of it and does not name it, so the name is worked
// out here from what it warned about: the one certificate it stores and warns
// over is one whose validity has not started yet, and the time it starts at is
// on the Info beside the warning. A warning this does not recognise is left
// unnamed rather than named wrongly, and the screen shows the English for it.
func certificateWarning(info *tlsserve.Info, now time.Time) (textCode, textArgs) {
	if info.Warning == "" {
		return "", nil
	}

	if now.Before(info.NotBefore) {
		return textCertificateNotValidYet, textArgs{
			"not_before": info.NotBefore.UTC().Format(time.RFC3339),
		}
	}

	return "", nil
}

// noCertificate is the answer while nothing is being served over TLS. It is a
// conflict rather than a 404, because the route is there and the thing it acts
// on is not: the setting that decides it is on the same screen, and the message
// says so.
func (h *CertificateHandler) noCertificate(c echo.Context) error {
	return failure(c, http.StatusConflict, errCertificateHTTPSOff)
}

// viewOf is what one certificate looks like on the screen.
func viewOf(keyPair *tls.Certificate, leaf *x509.Certificate, now time.Time) certificateView {
	return certificateView{
		Fingerprint: tlsserve.Fingerprint(leaf),
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		// A certificate that signed for itself is the one this installation
		// makes, and it is the one a browser warns about. The subject and the
		// issuer are compared as they were encoded, which is what a client
		// building a chain compares as well.
		SelfSigned:    bytes.Equal(leaf.RawSubject, leaf.RawIssuer),
		Hosts:         certificateHosts(leaf),
		NotBefore:     leaf.NotBefore,
		NotAfter:      leaf.NotAfter,
		DaysRemaining: daysRemaining(leaf.NotAfter, now),
		CertPEM:       certificateChainPEM(keyPair),
	}
}

// leafOf is the parsed certificate of a pair. It is normally already there,
// since everything tlsserve hands back carries it, and it is parsed again
// rather than assumed so that a pair built some other way is not a nil
// dereference.
func leafOf(keyPair *tls.Certificate) (*x509.Certificate, error) {
	if keyPair.Leaf != nil {
		return keyPair.Leaf, nil
	}

	if len(keyPair.Certificate) == 0 {
		return nil, errors.New("the certificate pair holds no certificate")
	}

	return x509.ParseCertificate(keyPair.Certificate[0])
}

// certificateHosts is what the certificate is made out to: the names first and
// then the addresses, which is the order the startup log and the stored row use.
func certificateHosts(leaf *x509.Certificate) []string {
	hosts := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	hosts = append(hosts, leaf.DNSNames...)

	for _, ip := range leaf.IPAddresses {
		hosts = append(hosts, ip.String())
	}

	return hosts
}

// daysRemaining is how long the certificate has left, rounded down, and
// negative once it has run out. Whole days is the unit an operator acts on, and
// rounding down means a certificate with a few hours left reads as 0 rather
// than as a day that is not really there.
func daysRemaining(notAfter time.Time, now time.Time) int {
	return int(notAfter.Sub(now) / (24 * time.Hour))
}

// certificateChainPEM is the certificate, and the intermediates behind it,
// encoded the way they were handed in. It is built from the DER in the pair
// rather than read from the database again, so that what is shown is what is
// being served.
func certificateChainPEM(keyPair *tls.Certificate) string {
	var out bytes.Buffer

	for _, der := range keyPair.Certificate {
		err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err != nil {
			return ""
		}
	}

	return out.String()
}
