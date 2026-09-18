package tlsserve

import (
	"crypto/tls"
	"errors"
	"sync/atomic"
)

// Holder is the one place the certificate this process serves with is read
// from, and the one place it is replaced in.
//
// The alternative is tls.Config.Certificates, which is a slice the listener is
// built around: replacing what is in it means building another config and
// another listener, which means taking the port down and putting it up again.
// An operator who presses "renew" is pressing it on a screen that is served
// over the very connection that would be dropped, so the answer to the press
// would never arrive and nothing would say whether the renewal happened.
//
// tls.Config.GetCertificate is read once per handshake instead, so a store here
// is in place for the next client that connects and for no one else. The
// connections that are already open keep the certificate their handshake chose:
// TLS settles on it once and never looks again, which is what lets the answer
// to the renewal come back over the old one.
type Holder struct {
	// current is an atomic pointer rather than a value behind a mutex because
	// this is read on every handshake and written about as often as an operator
	// presses a button. The pointer is swapped whole, so a handshake reads
	// either the certificate before the change or the one after it and never a
	// half-written struct.
	current atomic.Pointer[tls.Certificate]
}

// NewHolder returns a holder carrying cert, which may be nil. It is nil while
// HTTPS is off: the routes that read this are registered either way, and a
// holder that is empty answers them with "nothing is being served over TLS"
// rather than with a nil dereference.
func NewHolder(cert *tls.Certificate) *Holder {
	holder := &Holder{}
	holder.current.Store(cert)

	return holder
}

// Set puts cert in place for every handshake from here on.
func (h *Holder) Set(cert *tls.Certificate) {
	h.current.Store(cert)
}

// Current is the certificate being served, or nil when there is none.
func (h *Holder) Current() *tls.Certificate {
	return h.current.Load()
}

// errNoCertificate is what a handshake is refused with while the holder is
// empty. It cannot happen on a port that came up over TLS, since the startup
// fills the holder before it listens; it is here so that a mistake in the
// wiring ends as a refused connection with a sentence on it rather than as a
// panic that takes the process down.
var errNoCertificate = errors.New("this server holds no TLS certificate to serve with")

// GetCertificate is what tls.Config calls for every handshake.
func (h *Holder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := h.current.Load()
	if cert == nil {
		return nil, errNoCertificate
	}

	return cert, nil
}
