package tlsserve

import (
	"net"
	"net/http"
	"strconv"
	"strings"

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
func RedirectHandler(port int, logger *zap.Logger) http.Handler {
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

		// The port is the one this process listens on and never the one the
		// header carries. The header is whatever the client typed, and behind a
		// proxy that is the proxy's port, so echoing it back builds an address
		// that points at the wrong server. Behind a proxy that terminates TLS
		// itself, this handler is not reached at all.
		target := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + r.URL.RequestURI()

		logger.Debug("answered a request that arrived in the clear with a redirect to https",
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("method", r.Method),
			zap.String("to", target),
			zap.Int("status", redirectStatus))

		http.Redirect(w, r, target, redirectStatus)
	})
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
func NewRedirectServer(port int, logger *zap.Logger) *http.Server {
	return &http.Server{
		Handler:           RedirectHandler(port, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		// Without this, what the server has to say about a malformed request
		// goes to standard error in a format of its own, next to a log that is
		// JSON and may not even be going to the console.
		ErrorLog: zap.NewStdLog(logger),
	}
}
