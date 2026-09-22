package tlsserve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// certificateFor is a certificate made out to names and addresses and nothing
// else, so that a test can say what the redirect handler will accept. Calling
// generate here would make the answers depend on the host name and the
// interfaces of whichever machine runs the test.
func certificateFor(t *testing.T, dnsNames []string, ips []net.IP) *tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a key for the test certificate: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create the test certificate: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("failed to parse the test certificate: %v", err)
	}

	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// servedCertificate is what the handler under test is checking against
// throughout this file. It carries the shape a generated certificate has: a
// name this machine is known by, the loopback names, and an address reached
// from elsewhere.
func servedCertificate(t *testing.T) *tls.Certificate {
	t.Helper()

	return certificateFor(t,
		[]string{"tunnel.example.com", "localhost"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), net.ParseIP("192.0.2.10")})
}

// TestTheRedirectKeepsTheAddressAndUsesThePortWeListenOn covers what the
// Location is built from. The host comes from the request and the port from
// this process, because the port in the header is whatever the client typed and
// is the wrong one as soon as anything sits in front of this server.
//
// Every host here is one the certificate carries. A name it does not carry is
// what the test below is about.
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
		{"a nested path and a query", "localhost:80", "/api/host/3/ports?page=2&size=10",
			"https://localhost:8888/api/host/3/ports?page=2&size=10"},
		{"a name in another case", "TUNNEL.Example.COM:80", "/ui/", "https://TUNNEL.Example.COM:8888/ui/"},
	}

	handler := RedirectHandler(8888, NewHolder(servedCertificate(t)), zap.NewNop())

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

// TestTheRedirectRefusesAHostTheCertificateDoesNotCarry is the open redirect.
// The Location used to be built from the Host header alone, so a request made
// with "Host: evil.example" came back as a link to evil.example wearing this
// server's address in front of it, and anything that reads a redirect follows
// one.
func TestTheRedirectRefusesAHostTheCertificateDoesNotCarry(t *testing.T) {
	cases := []struct {
		name string
		host string
	}{
		{"another name", "evil.example"},
		{"another name with a port", "evil.example:8888"},
		{"a name the certificate name is a prefix of", "tunnel.example.com.evil.example"},
		{"a name that ends with a certificate name", "eviltunnel.example.com"},
		{"an address the certificate does not carry", "198.51.100.7:80"},
		{"an IPv6 address the certificate does not carry", "[2001:db8::1]:80"},
		{"a name where the certificate carries only addresses", "127.0.0.1.evil.example"},
		{"a wildcard the certificate never granted", "*.example.com"},
	}

	handler := RedirectHandler(8888, NewHolder(servedCertificate(t)), zap.NewNop())

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
			req.Host = tc.host

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := rec.Header().Get("Location"); got != "" {
				t.Errorf("a Location was sent for a host the certificate does not carry: %q", got)
			}
			if body := rec.Body.String(); strings.Contains(body, "evil.example") {
				t.Errorf("the refusal repeats the host the client sent, which puts the client's "+
					"text on a page served under this address: %q", body)
			}
		})
	}
}

// TestTheRedirectTakesTheWildcardOfAnInstalledCertificate covers the operator
// who installed a certificate from an authority. What such a certificate is
// made out to is usually a wildcard, and refusing it would leave the redirect
// broken for every name the installation actually answers to.
func TestTheRedirectTakesTheWildcardOfAnInstalledCertificate(t *testing.T) {
	handler := RedirectHandler(8888,
		NewHolder(certificateFor(t, []string{"*.example.com"}, nil)), zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	req.Host = "tunnel.example.com"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	if got := rec.Header().Get("Location"); got != "https://tunnel.example.com:8888/ui/" {
		t.Errorf("Location = %q, want %q", got, "https://tunnel.example.com:8888/ui/")
	}
}

// TestTheRedirectReadsTheCertificateThatIsBeingServedNow is why the holder is
// read per request rather than once when the handler is built. Renew and
// Install swap what it carries while the process runs, and a list of names
// taken at startup would go on refusing the names of the certificate now in
// front of the clients.
func TestTheRedirectReadsTheCertificateThatIsBeingServedNow(t *testing.T) {
	holder := NewHolder(certificateFor(t, []string{"first.example"}, nil))
	handler := RedirectHandler(8888, holder, zap.NewNop())

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
		req.Host = "second.example"

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec
	}

	if got := request().Code; got != http.StatusBadRequest {
		t.Errorf("before the certificate was replaced, status = %d, want %d",
			got, http.StatusBadRequest)
	}

	holder.Set(certificateFor(t, []string{"second.example"}, nil))

	after := request()
	if after.Code != http.StatusTemporaryRedirect {
		t.Errorf("after the certificate was replaced, status = %d, want %d",
			after.Code, http.StatusTemporaryRedirect)
	}
	if got := after.Header().Get("Location"); got != "https://second.example:8888/ui/" {
		t.Errorf("Location = %q, want %q", got, "https://second.example:8888/ui/")
	}
}

// TestTheRedirectReadsTheNamesOfAPairThatCarriesNoLeaf covers a tls.Certificate
// that was assembled without its Leaf filled in. The names are in the DER
// either way, and reading them off a nil Leaf would take the process down.
func TestTheRedirectReadsTheNamesOfAPairThatCarriesNoLeaf(t *testing.T) {
	served := servedCertificate(t)
	served.Leaf = nil

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	req.Host = "tunnel.example.com"

	rec := httptest.NewRecorder()
	RedirectHandler(8888, NewHolder(served), zap.NewNop()).ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	if got := rec.Header().Get("Location"); got != "https://tunnel.example.com:8888/ui/" {
		t.Errorf("Location = %q, want %q", got, "https://tunnel.example.com:8888/ui/")
	}
}

// TestARequestIsRefusedWhileNoCertificateIsHeld covers a wiring mistake: the
// plaintext side of the port is serving while the holder is still empty. There
// are no names to check against, so no Location can be built, and the answer is
// 500 because the request was fine and what is missing is here.
func TestARequestIsRefusedWhileNoCertificateIsHeld(t *testing.T) {
	cases := []struct {
		name string
		held *tls.Certificate
	}{
		{"an empty holder", nil},
		{"a pair with no certificate in it", &tls.Certificate{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
			req.Host = "tunnel.example.com"

			rec := httptest.NewRecorder()
			RedirectHandler(8888, NewHolder(tc.held), zap.NewNop()).ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
			}
			if got := rec.Header().Get("Location"); got != "" {
				t.Errorf("a Location was sent while no certificate was held: %q", got)
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
	RedirectHandler(8888, NewHolder(servedCertificate(t)), zap.NewNop()).ServeHTTP(rec, req)

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
	RedirectHandler(8888, NewHolder(servedCertificate(t)), zap.NewNop()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if rec.Header().Get("Location") != "" {
		t.Errorf("a Location was sent for a request with no Host: %q", rec.Header().Get("Location"))
	}
}
