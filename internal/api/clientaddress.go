package api

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/labstack/echo/v4"
)

// ClientAddress is the rule the address of a request is read by, and it is
// meant to be set once as the IPExtractor of the server. Everything that asks
// echo for RealIP then gets the same answer from it: the login limiter counts
// by it, the access log writes it as remote_ip and the certificate screen logs
// it as the client. Left unset, echo believes the leftmost X-Forwarded-For
// entry, which is whatever the client wrote there.
//
// With trustProxyHeaders off the address is the one of the socket, since no
// header says anything a client could not have written itself.
//
// With it on, the address is the rightmost X-Forwarded-For entry. A proxy
// appends the address of the peer it was reached from to the end of the list,
// so the last entry is the one written by the proxy in front of this server
// and everything left of it came from the client or from what was before that
// proxy. This takes one proxy to be in front. echo's ExtractIPFromXFFHeader is
// not used because it walks leftwards past loopback and private addresses by
// default: a client on a private network would be named by the entry it wrote
// itself.
func ClientAddress(trustProxyHeaders bool) echo.IPExtractor {
	if !trustProxyHeaders {
		return echo.ExtractIPDirect()
	}

	return lastForwardedFor
}

// lastForwardedFor returns the rightmost X-Forwarded-For entry, or the address
// of the socket where there is no entry or the entry is not an address.
//
// A header sent on several lines is one list read in order, so the rightmost
// entry is the last one of the last line. It is parsed rather than passed on,
// so that a proxy that forwards something other than an address leaves the
// request counted by the socket rather than by an arbitrary string.
func lastForwardedFor(req *http.Request) string {
	lines := req.Header.Values(echo.HeaderXForwardedFor)
	if len(lines) == 0 {
		return echo.ExtractIPDirect()(req)
	}

	last := lines[len(lines)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}

	last = strings.TrimSpace(last)
	last = strings.TrimSuffix(strings.TrimPrefix(last, "["), "]")

	addr, err := netip.ParseAddr(last)
	if err != nil {
		return echo.ExtractIPDirect()(req)
	}

	return addr.String()
}
