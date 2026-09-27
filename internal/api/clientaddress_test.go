package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// loginForwardedFrom sends one login over a connection from socket, carrying
// forwardedFor as its X-Forwarded-For lines. No line at all is sent for an
// empty list.
func loginForwardedFrom(e *echo.Echo, socket string, forwardedFor []string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = socket + ":54321"

	for _, line := range forwardedFor {
		req.Header.Add(echo.HeaderXForwardedFor, line)
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	return rec
}

// newForwardedServer is the login server of the other tests with the address
// rule main puts on it, and an access log written the way main writes one
// but kept in a buffer, one remote_ip per line.
func newForwardedServer(t *testing.T, trust bool) (*echo.Echo, *bytes.Buffer) {
	t.Helper()

	e, authHandler := newTestServer(t, newTestAccount(t, false))
	e.IPExtractor = ClientAddress(trust)
	authHandler.TrustProxyHeaders(trust)

	var accessLog bytes.Buffer
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: "${remote_ip}\n",
		Output: &accessLog,
	}))

	return e, &accessLog
}

// loggedAddresses is what the access log above holds, one address per request.
func loggedAddresses(accessLog *bytes.Buffer) []string {
	return strings.Fields(accessLog.String())
}

func TestClientAddressReadsTheSocketOrTheRightmostEntry(t *testing.T) {
	const socket = "192.0.2.10"

	cases := []struct {
		name         string
		trust        bool
		forwardedFor []string
		want         string
	}{
		{"off, no header", false, nil, socket},
		{"off, a header naming somebody else", false, []string{"198.51.100.1"}, socket},
		{"off, a list", false, []string{"198.51.100.1, 203.0.113.9"}, socket},
		{"on, no header", true, nil, socket},
		{"on, one entry", true, []string{"203.0.113.9"}, "203.0.113.9"},
		{"on, the rightmost of a list", true, []string{"198.51.100.1, 198.51.100.2, 203.0.113.9"}, "203.0.113.9"},
		{"on, a loopback rightmost is still the one", true, []string{"198.51.100.1, 127.0.0.2"}, "127.0.0.2"},
		{"on, the last of several lines", true, []string{"198.51.100.1", "198.51.100.2, 203.0.113.9"}, "203.0.113.9"},
		{"on, IPv6", true, []string{"198.51.100.1, 2001:db8::7"}, "2001:db8::7"},
		{"on, IPv6 in brackets", true, []string{"[2001:db8::7]"}, "2001:db8::7"},
		{"on, not an address", true, []string{"198.51.100.1, somebody"}, socket},
		{"on, an address with a port", true, []string{"203.0.113.9:8080"}, socket},
		{"on, an empty rightmost entry", true, []string{"203.0.113.9, "}, socket},
		{"on, an empty line", true, []string{""}, socket},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = socket + ":54321"

			for _, line := range tc.forwardedFor {
				req.Header.Add(echo.HeaderXForwardedFor, line)
			}

			if got := ClientAddress(tc.trust)(req); got != tc.want {
				t.Errorf("address of %q = %q, want %q", tc.forwardedFor, got, tc.want)
			}
		})
	}
}

// TestAForgedForwardedForIsNotBelievedWithoutTheFlag is a guesser writing a
// new X-Forwarded-For on every login from one connection. The limit holds it
// all the same, and the access log names the connection.
func TestAForgedForwardedForIsNotBelievedWithoutTheFlag(t *testing.T) {
	e, accessLog := newForwardedServer(t, false)

	const socket = "198.51.100.7"

	for i := 0; i < loginAddressFailureLimit; i++ {
		forged := []string{fmt.Sprintf("203.0.113.%d", i+1)}
		if rec := loginForwardedFrom(e, socket, forged, badLogin); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want %d, body: %s",
				i+1, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}

	rec := loginForwardedFrom(e, socket, []string{"203.0.113.200"}, badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d with yet another header: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	logged := loggedAddresses(accessLog)
	if len(logged) != loginAddressFailureLimit+1 {
		t.Fatalf("the access log has %d lines, want %d: %q", len(logged), loginAddressFailureLimit+1, logged)
	}

	for i, address := range logged {
		if address != socket {
			t.Errorf("access log line %d: remote_ip = %q, want %q", i+1, address, socket)
		}
	}
}

// TestTheRightmostForwardedForIsWhatIsCountedWithTheFlag is the same guesser
// behind a proxy. What it writes is on the left and changes every time, the
// entry the proxy appended is on the right and does not, and the limit holds
// it by that entry. Another rightmost entry is another client and is checked.
func TestTheRightmostForwardedForIsWhatIsCountedWithTheFlag(t *testing.T) {
	e, accessLog := newForwardedServer(t, true)

	const (
		proxy   = "192.0.2.1"
		guesser = "203.0.113.7"
	)

	for i := 0; i < loginAddressFailureLimit; i++ {
		forwarded := []string{fmt.Sprintf("198.51.100.%d, %s", i+1, guesser)}
		if rec := loginForwardedFrom(e, proxy, forwarded, badLogin); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want %d, body: %s",
				i+1, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}

	rec := loginForwardedFrom(e, proxy, []string{"198.51.100.3, " + guesser}, badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d with another leftmost entry: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	rec = loginForwardedFrom(e, proxy, []string{"198.51.100.2, 203.0.113.8"}, badLogin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("another rightmost entry: status = %d, want %d, body: %s",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	logged := loggedAddresses(accessLog)
	if len(logged) != loginAddressFailureLimit+2 {
		t.Fatalf("the access log has %d lines, want %d: %q", len(logged), loginAddressFailureLimit+2, logged)
	}

	for i, address := range logged[:loginAddressFailureLimit+1] {
		if address != guesser {
			t.Errorf("access log line %d: remote_ip = %q, want %q", i+1, address, guesser)
		}
	}

	if last := logged[len(logged)-1]; last != "203.0.113.8" {
		t.Errorf("access log last line: remote_ip = %q, want %q", last, "203.0.113.8")
	}
}

// TestAMissingOrUnreadableForwardedForIsCountedByTheSocket covers a request
// with the flag on that names nobody the rule can read. Such requests are
// counted by the connection, so a missing header and one that is not an
// address land on the same counter.
func TestAMissingOrUnreadableForwardedForIsCountedByTheSocket(t *testing.T) {
	e, accessLog := newForwardedServer(t, true)

	const socket = "198.51.100.7"

	unreadable := [][]string{nil, {"somebody"}, {"203.0.113.9, "}, {""}, {"203.0.113.9:8080"}}

	for i := 0; i < loginAddressFailureLimit; i++ {
		forwarded := unreadable[i%len(unreadable)]
		if rec := loginForwardedFrom(e, socket, forwarded, badLogin); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d with %q: status = %d, want %d, body: %s",
				i+1, forwarded, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}

	rec := loginForwardedFrom(e, socket, nil, badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d with no header: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	for i, address := range loggedAddresses(accessLog) {
		if address != socket {
			t.Errorf("access log line %d: remote_ip = %q, want %q", i+1, address, socket)
		}
	}
}
