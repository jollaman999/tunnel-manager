package tlsserve

import (
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"go.uber.org/zap"
)

// redirectStatus is what a request that arrived in the clear is answered with.
//
// 307 rather than 301 or 302 for two reasons. It keeps the method and the body,
// while 301 and 302 are turned into a GET by every browser, so a POST to
// /api/host sent to the plaintext port would arrive as a GET of the same path
// and be answered with a method error instead of doing what was asked.
//
// It is temporary rather than permanent (308) because HTTPS here is a setting
// that can be turned off. A permanent redirect is cached by the browser and
// kept after the server stopped sending it, so an operator who turns HTTPS off
// to get around a certificate their client refuses would find the browser still
// sending them to https:// with nothing listening for it there, and the way out
// would be clearing the browser's cache rather than changing a setting back.
const redirectStatus = http.StatusTemporaryRedirect

// readHeaderTimeout bounds how long the redirect server waits for the rest of
// the request head. The connection got this far by sending one byte, and the
// deadline that covered it is cleared by then, so this is what keeps a client
// that sends "G" and stops from holding a connection open.
const readHeaderTimeout = peekTimeout

// RedirectHandler answers a request that arrived in the clear with the same
// address under https.
//
// The body is never read and never acted on. It arrived unencrypted, so a
// password or a host password in it is already on the wire, and carrying it out
// would only mean this server handled a secret it was supposed to refuse. The
// client sends it again over TLS, which is what 307 asks it to do.
//
// holder is the certificate the TLS side of this port serves with, and it is
// here to bound the names this handler will build a Location out of. The name
// in the Location comes from the Host header, which is whatever the client
// wrote: a request made with "Host: evil.example" would otherwise be answered
// with "Location: https://evil.example:8888/ui/", which is this server sending
// its own visitors to somebody else's machine under its own address. Anything
// that reads a redirect follows it, and a link that starts at the address the
// operator knows is exactly the one a person checks and then trusts.
func RedirectHandler(port int, holder *Holder, logger *zap.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostWithoutPort(r.Host)
		if host == "" {
			// HTTP/1.0 clients may leave the header out. There is nothing to
			// build an address from then, and guessing one from the interface
			// the connection came in on would send the client somewhere nobody
			// asked for.
			http.Error(w, "the request carries no Host header, so there is no address to send it to",
				http.StatusBadRequest)

			return
		}

		// The certificate is read here rather than once when this handler is
		// built, because Renew and Install swap what the holder carries while
		// the process runs. A list of names taken at startup would go on
		// refusing the names of the certificate that is now being served, and
		// the operator who just installed one would find the redirect broken
		// until a restart.
		leaf, err := servedLeaf(holder)
		if err != nil {
			// Not 400. The request is well formed and there is nothing the
			// client could change about it; what is missing is a certificate
			// this server was supposed to hold, so the fault is here. Saying so
			// with 500 also keeps it apart from the refusals below, which are
			// ordinary and arrive whenever anything probes the port.
			logger.Error("a request that arrived in the clear cannot be answered, because the "+
				"certificate this port serves with cannot be read",
				logid.CertificateServedUnparsable.Field(),
				zap.String("remote_addr", r.RemoteAddr),
				zap.Error(err))

			http.Error(w, "this server cannot read the certificate it serves with, so it cannot "+
				"say where to send this request", http.StatusInternalServerError)

			return
		}

		// The certificate is what the names are read from because it is the
		// only list that is right for every installation. Working the names out
		// from this machine again would answer for the certificate this package
		// generates and for nothing else, and an operator may install one from
		// an authority whose names have no relation to the host name or the
		// interfaces here. It is also the list that decides whether the client
		// can complete the handshake at the other end: sending it to a name the
		// certificate does not carry is sending it to a warning screen.
		//
		// VerifyHostname is the certificate answering for itself. It reads
		// DNSNames and IPAddresses both, folds case on the way, and takes a
		// wildcard in an installed certificate, none of which a comparison
		// written out here would do without repeating what it already does.
		if leaf.VerifyHostname(host) != nil {
			// The name is deliberately not repeated back. It came from the
			// client, and writing it into the body would put the attacker's
			// text on a page served under this server's address.
			http.Error(w, "the Host header names an address this server holds no certificate for, "+
				"so there is nowhere to send this request", http.StatusBadRequest)

			return
		}

		// The port is the one this process listens on and never the one the
		// header carries. The header is whatever the client typed, and behind a
		// proxy that is the proxy's port, so echoing it back builds an address
		// that points at the wrong server. Behind a proxy that terminates TLS
		// itself, this handler is not reached at all.
		target := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + r.URL.RequestURI()

		logger.Debug("answered a request that arrived in the clear with a redirect to https",
			logid.TlsserveRedirectedToHttps.Field(),
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("method", r.Method),
			zap.String("to", target),
			zap.Int("status", redirectStatus))

		http.Redirect(w, r, target, redirectStatus)
	})
}

// servedLeaf is the certificate the TLS side of this port is serving, parsed so
// that the names in it can be read.
//
// Leaf is filled in only by whoever built the pair. The pairs this package
// stores carry it, but a tls.Certificate assembled anywhere else leaves it nil
// and reading the names off it would be a panic rather than a refusal, so the
// DER of the first block is parsed when it is missing. That is the same
// certificate by a longer route.
func servedLeaf(holder *Holder) (*x509.Certificate, error) {
	keyPair := holder.Current()
	if keyPair == nil {
		return nil, errNoCertificate
	}

	if keyPair.Leaf != nil {
		return keyPair.Leaf, nil
	}

	if len(keyPair.Certificate) == 0 {
		return nil, errors.New("the certificate pair holds no certificate")
	}

	return x509.ParseCertificate(keyPair.Certificate[0])
}

// hostWithoutPort returns the host of a Host header, with the port taken off
// and the brackets of an IPv6 address removed, so that JoinHostPort can put the
// address back together with the port this process actually listens on.
func hostWithoutPort(header string) string {
	host := strings.TrimSpace(header)
	if host == "" {
		return ""
	}

	withoutPort, _, err := net.SplitHostPort(host)
	if err == nil {
		return withoutPort
	}

	// No port, so the header is the host as it stands. An IPv6 address is
	// written in brackets there and JoinHostPort puts them back.
	return strings.Trim(host, "[]")
}

// NewRedirectServer is the server the plaintext side of the port is given. It
// is an ordinary http.Server, so the shutdown it offers is the same graceful
// one the API server has.
func NewRedirectServer(port int, holder *Holder, logger *zap.Logger) *http.Server {
	return &http.Server{
		Handler:           RedirectHandler(port, holder, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		// Without this, what the server has to say about a malformed request
		// goes to standard error in a format of its own, next to a log that is
		// JSON and may not even be going to the console.
		ErrorLog: zap.NewStdLog(logger),
	}
}
