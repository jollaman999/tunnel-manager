package tlsserve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestTheRedirectKeepsTheAddressAndUsesThePortWeListenOn covers what the
// Location is built from. The host comes from the request and the port from
// this process, because the port in the header is whatever the client typed and
// is the wrong one as soon as anything sits in front of this server.
func TestTheRedirectKeepsTheAddressAndUsesThePortWeListenOn(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		target string
		want   string
	}{
		{"a name with the port", "tunnel.example.com:8080", "/ui/", "https://tunnel.example.com:8888/ui/"},
		{"a name without a port", "tunnel.example.com", "/ui/", "https://tunnel.example.com:8888/ui/"},
		{"an address", "192.0.2.10:80", "/api/host", "https://192.0.2.10:8888/api/host"},
		{"an IPv6 address", "[::1]:80", "/api/host", "https://[::1]:8888/api/host"},
		{"an IPv6 address without a port", "[::1]", "/", "https://[::1]:8888/"},
		{"a query string", "127.0.0.1:1234", "/api/logs?lines=50", "https://127.0.0.1:8888/api/logs?lines=50"},
	}

	handler := RedirectHandler(8888, zap.NewNop())

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Host = tc.host

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusTemporaryRedirect {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Errorf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheRedirectLeavesTheBodyAlone is what 307 is for: the client sends the
// request again over TLS, method and body included, and this server never looks
// at what arrived in the clear.
func TestTheRedirectLeavesTheBodyAlone(t *testing.T) {
	body := &countingReader{Reader: strings.NewReader(`{"password":"unread"}`)} // hook:allow

	req := httptest.NewRequest(http.MethodPost, "/api/login", body)
	req.Host = "127.0.0.1:8888"

	rec := httptest.NewRecorder()
	RedirectHandler(8888, zap.NewNop()).ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("a POST was answered with %d. 301 and 302 would turn it into a GET, which is why "+
			"the status is %d", rec.Code, http.StatusTemporaryRedirect)
	}
	if body.reads > 0 {
		t.Errorf("the body of a request that arrived in the clear was read %d times, want none",
			body.reads)
	}
}

type countingReader struct {
	io.Reader
	reads int
}

func (r *countingReader) Read(b []byte) (int, error) {
	r.reads++

	return r.Reader.Read(b)
}

// TestARequestWithoutAHostIsRefused covers the HTTP/1.0 client that leaves the
// header out. There is nothing to build an address from, and guessing one would
// send the client to a server nobody asked for.
func TestARequestWithoutAHostIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = ""

	rec := httptest.NewRecorder()
	RedirectHandler(8888, zap.NewNop()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if rec.Header().Get("Location") != "" {
		t.Errorf("a Location was sent for a request with no Host: %q", rec.Header().Get("Location"))
	}
}
