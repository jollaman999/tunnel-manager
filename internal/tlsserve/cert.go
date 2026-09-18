// Package tlsserve serves the API port over TLS with a certificate this
// installation makes for itself and keeps in the database.
//
// There is one port, and both kinds of client reach it: a browser that was
// pointed at https:// and one that was pointed at http:// out of habit or out
// of a bookmark written before this release. The splitter in this package tells
// the two apart on the first byte of the connection, hands the TLS ones to the
// server and answers the rest with a redirect to the same address under https.
package tlsserve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

// certValidity is how long a generated certificate is good for.
//
// 825 days is the longest a TLS server certificate may live and still be
// trusted by Apple platforms: a certificate issued after 1 July 2019 whose
// validity period is longer than 825 days is refused by iOS 13 and macOS 10.15
// and later, and that rule is applied to a certificate the operator added to
// the trust store by hand as well, not only to one from a public authority. A
// certificate nobody can trust is worse than a short-lived one, so the limit is
// what is used rather than the ten years a self-signed certificate is often
// given.
const certValidity = 825 * 24 * time.Hour

// clockSkew is how far back the certificate starts. A machine whose clock is a
// few minutes behind the one that generated it would otherwise refuse a
// certificate that is not valid yet, which reads to the operator exactly like a
// certificate that is broken. The validity period is measured from this earlier
// start, so the 825 days above stay 825 days.
const clockSkew = time.Hour

// serialBits is the size of the random serial number. The CA/Browser Forum asks
// for at least 64 bits of entropy in a serial; 128 is what leaves no question
// and is what x509 tooling generates.
const serialBits = 128

// material is a generated certificate and its private key, both PEM encoded,
// which is the form they are stored and served in.
type material struct {
	certPEM []byte
	keyPEM  []byte
	// hosts is what the certificate was made out to, DNS names first and then
	// addresses, so the startup can report what a client may reach it under.
	hosts []string
}

// generate builds a self-signed certificate for this machine.
//
// The key is ECDSA on P-256 rather than RSA. P-256 is the one curve every TLS
// 1.2 and 1.3 client in use supports, the handshake is cheaper, and generating
// the key takes microseconds, while an RSA 2048 key takes a search for primes
// that can run into seconds on a loaded machine. This runs on the first startup,
// in front of everything the process then serves, so a key that is slow to make
// is a startup that hangs for no reason a reader of the log could see. RSA 2048
// would buy compatibility with clients that predate ECDSA, and there are none
// left that can also speak a TLS version this server offers.
func generate(now time.Time) (*material, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate the private key: %w", err)
	}

	dnsNames, ips := subjectNames()

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("failed to generate the serial number: %w", err)
	}

	notBefore := now.Add(-clockSkew).UTC()

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"tunnel-manager"},
			CommonName:   commonName(dnsNames),
		},
		NotBefore: notBefore,
		NotAfter:  notBefore.Add(certValidity),

		// A browser reads these three and says so plainly when they are wrong,
		// while a certificate that leaves them out is refused with a message
		// about the certificate being invalid and nothing about why.
		//
		// DigitalSignature is what an ECDSA server key signs the handshake
		// with. CertSign is there because this certificate is its own issuer:
		// an operator who wants the warning gone imports it as a trust anchor,
		// and a client building a chain up to it checks that the anchor is
		// allowed to have signed something, which is what CertSign and IsCA
		// together say. KeyEncipherment is left out on purpose: it covers RSA
		// key transport, which an ECDSA key never performs.
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,

		// The names live here and not in the Subject. A client has read the
		// common name as a host name for nothing since 2017, and a certificate
		// whose name is only in the Subject is refused outright.
		DNSNames:    dnsNames,
		IPAddresses: ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("failed to create the certificate: %w", err)
	}

	// PKCS#8 is what "PRIVATE KEY" holds and it names the algorithm inside the
	// key, so the stored key can be read back without the reader having to be
	// told what kind it is.
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the private key: %w", err)
	}

	hosts := make([]string, 0, len(dnsNames)+len(ips))
	hosts = append(hosts, dnsNames...)
	for _, ip := range ips {
		hosts = append(hosts, ip.String())
	}

	return &material{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		hosts:   hosts,
	}, nil
}

// commonName is what the subject is filled with. Nothing validates against it
// any more, but it is what openssl and the certificate viewer of a browser show
// first, so it carries the name the machine calls itself.
func commonName(dnsNames []string) string {
	for _, name := range dnsNames {
		if name != "localhost" {
			return name
		}
	}

	return "tunnel-manager"
}

// subjectNames returns what the certificate is made out to.
//
// A self-signed certificate is only useful under the names the operator types,
// and there is no one name: the same server is reached as localhost from the
// machine itself, under its host name from the office network and under an
// address from a script. Every one of them that can be worked out from this
// machine goes in, because a name that is missing is a warning the operator
// clicks through, and clicking through is the habit this is meant to avoid.
//
// localhost, 127.0.0.1 and ::1 are always there, so that a tunnel to this port
// and a health check both land on a name the certificate carries even on a
// machine with no network at all.
func subjectNames() ([]string, []net.IP) {
	names := map[string]bool{"localhost": true}
	ips := map[string]net.IP{}

	addIP := func(ip net.IP) {
		if ip == nil {
			return
		}
		ips[ip.String()] = ip
	}

	addIP(net.IPv4(127, 0, 0, 1))
	addIP(net.IPv6loopback)

	hostname, err := os.Hostname()
	if err == nil {
		for _, name := range hostnameForms(hostname) {
			// A host name that is really an address belongs in the addresses.
			// Encoded as a DNS name it matches nothing: a client that is given
			// an address compares it against the addresses alone.
			if ip := net.ParseIP(name); ip != nil {
				addIP(ip)
				continue
			}
			names[name] = true
		}
	}

	// The addresses of the interfaces are what this machine is reached under
	// from elsewhere. A failure here is not worth stopping over: the
	// certificate is then made out to the loopback names alone, which is enough
	// to serve, and the operator sees the shortened list in the startup log.
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if !usableAddress(ipNet.IP) {
				continue
			}
			addIP(ipNet.IP)
		}
	}

	dnsNames := make([]string, 0, len(names))
	for name := range names {
		dnsNames = append(dnsNames, name)
	}
	// Both lists are sorted so that two runs on the same machine produce the
	// same certificate contents, and so that the startup log reads the same way
	// every time. Map order alone would shuffle them on every startup.
	sort.Strings(dnsNames)

	keys := make([]string, 0, len(ips))
	for key := range ips {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	addresses := make([]net.IP, 0, len(keys))
	for _, key := range keys {
		addresses = append(addresses, ips[key])
	}

	return dnsNames, addresses
}

// hostnameForms returns the host name and, when it is a fully qualified one,
// the first label as well. Both are typed: a machine called host.example.com is
// reached as that from another network and as host from its own.
func hostnameForms(hostname string) []string {
	hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	if !plausibleHostname(hostname) {
		return nil
	}

	forms := []string{hostname}

	short, _, found := strings.Cut(hostname, ".")
	if found && plausibleHostname(short) {
		forms = append(forms, short)
	}

	return forms
}

// plausibleHostname keeps a host name that no client would ever ask for out of
// the certificate. A name with a space or an empty label is one a resolver
// refuses, and x509 would carry it into the certificate as it stands.
func plausibleHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}

	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}

		for _, r := range label {
			isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			isDigit := r >= '0' && r <= '9'
			if !isLetter && !isDigit && r != '-' {
				return false
			}
		}
	}

	return true
}

// usableAddress reports whether an address of an interface is one a client can
// be pointed at.
//
// A link-local address is left out although it is reachable. It is only
// meaningful together with the interface it belongs to, which a URL carries as
// a zone (https://[fe80::1%25eth0]:8888/), and the zone is not part of what a
// client matches against the certificate, so the entry would not help the one
// case it exists for.
func usableAddress(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}

	return ip.IsLoopback() || ip.IsGlobalUnicast()
}
