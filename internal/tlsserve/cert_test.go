package tlsserve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"net"
	"sort"
	"testing"
	"time"
)

// parseGenerated builds a certificate and hands back what is inside it, which
// is what every test here looks at.
func parseGenerated(t *testing.T, now time.Time) (*x509.Certificate, *material) {
	t.Helper()

	generated, err := generate(now)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	keyPair, err := tls.X509KeyPair(generated.certPEM, generated.keyPEM)
	if err != nil {
		t.Fatalf("the generated certificate and key do not form a pair: %v", err)
	}

	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		t.Fatalf("parsing the generated certificate: %v", err)
	}

	return leaf, generated
}

// TestTheCertificateCarriesTheLoopbackNames pins the three names that have to
// be there on any machine. A tunnel to this port, a health check and the
// browser on the machine itself all use one of them, and a name that is missing
// is a warning the operator has to click through.
func TestTheCertificateCarriesTheLoopbackNames(t *testing.T) {
	leaf, _ := parseGenerated(t, time.Now())

	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("the certificate is not valid for localhost: %v", err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("the certificate is not valid for 127.0.0.1: %v", err)
	}
	if err := leaf.VerifyHostname("::1"); err != nil {
		t.Errorf("the certificate is not valid for ::1: %v", err)
	}
}

// TestTheCertificateCarriesTheAddressesOfThisMachine is the other half of the
// names: what this machine is reached under from elsewhere.
func TestTheCertificateCarriesTheAddressesOfThisMachine(t *testing.T) {
	leaf, _ := parseGenerated(t, time.Now())

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("the addresses of this machine cannot be read: %v", err)
	}

	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || !usableAddress(ipNet.IP) {
			continue
		}

		if err := leaf.VerifyHostname(ipNet.IP.String()); err != nil {
			t.Errorf("the certificate is not valid for the address %s of this machine: %v",
				ipNet.IP, err)
		}
	}
}

// TestTheValidityIsWithinWhatClientsAccept is the 825 days. A certificate that
// lives longer is refused by Apple platforms even when the operator trusted it
// by hand, so the period is held here rather than left to be discovered on
// somebody's laptop.
func TestTheValidityIsWithinWhatClientsAccept(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	leaf, _ := parseGenerated(t, now)

	validity := leaf.NotAfter.Sub(leaf.NotBefore)
	if validity != certValidity {
		t.Errorf("the certificate is valid for %v, want %v", validity, certValidity)
	}
	if validity > 825*24*time.Hour {
		t.Errorf("the certificate is valid for %v, which is longer than the 825 days clients accept",
			validity)
	}

	// It has to be valid at the moment it is made, on a machine whose clock is
	// a few minutes behind as well.
	if !leaf.NotBefore.Before(now) {
		t.Errorf("the certificate starts at %s, which is not before %s", leaf.NotBefore, now)
	}
	if leaf.NotBefore.After(now.Add(-clockSkew + time.Minute)) {
		t.Errorf("the certificate starts at %s, want about %v before %s", leaf.NotBefore, clockSkew, now)
	}
}

// TestTheCertificateSaysWhatItMayBeUsedFor covers the fields a browser reads
// before it will even show the warning that can be clicked through.
func TestTheCertificateSaysWhatItMayBeUsedFor(t *testing.T) {
	leaf, _ := parseGenerated(t, time.Now())

	if !leaf.BasicConstraintsValid {
		t.Error("the certificate carries no basic constraints")
	}
	if !leaf.IsCA {
		t.Error("the certificate is not a CA, so it cannot be imported as a trust anchor")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("the certificate may not sign, so it cannot serve a TLS handshake")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("the certificate may not sign a certificate, so it cannot be its own issuer")
	}

	serverAuth := false
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		t.Error("the certificate is not marked for server authentication")
	}

	if leaf.SerialNumber == nil || leaf.SerialNumber.Sign() <= 0 {
		t.Errorf("the serial number is %v, want a positive random one", leaf.SerialNumber)
	}
}

// TestTheKeyIsOnTheCurveEveryClientSupports pins the key type. A key of another
// kind is not wrong on its own, but it is a decision worth noticing when it
// changes: P-256 is what every TLS client in use can verify.
func TestTheKeyIsOnTheCurveEveryClientSupports(t *testing.T) {
	leaf, _ := parseGenerated(t, time.Now())

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("the public key is %T, want an ECDSA key", leaf.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("the key is on %s, want P-256", pub.Curve.Params().Name)
	}
}

// TestTheNamesAreSortedAndHeldOnce keeps the list stable. Read out of a map it
// would come out in a different order on every startup, which makes two
// certificates that carry the same names look different in the log.
func TestTheNamesAreSortedAndHeldOnce(t *testing.T) {
	dnsNames, ips := subjectNames()

	if !sort.StringsAreSorted(dnsNames) {
		t.Errorf("the DNS names are not sorted: %v", dnsNames)
	}

	seen := map[string]bool{}
	for _, name := range dnsNames {
		if seen[name] {
			t.Errorf("the DNS name %q is in the certificate twice", name)
		}
		seen[name] = true
	}

	addresses := map[string]bool{}
	for _, ip := range ips {
		if addresses[ip.String()] {
			t.Errorf("the address %s is in the certificate twice", ip)
		}
		addresses[ip.String()] = true
	}
}

func TestUsableAddress(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		// TEST-NET-1, which stands in for a routable address here.
		{"192.0.2.10", true},
		{"0.0.0.0", false},
		{"::", false},
		{"169.254.10.1", false},
		{"fe80::1", false},
		{"224.0.0.1", false},
	}

	for _, tc := range cases {
		got := usableAddress(net.ParseIP(tc.address))
		if got != tc.want {
			t.Errorf("usableAddress(%s) = %v, want %v", tc.address, got, tc.want)
		}
	}
}

func TestHostnameForms(t *testing.T) {
	cases := []struct {
		hostname string
		want     []string
	}{
		{"host", []string{"host"}},
		{"host.example.com", []string{"host.example.com", "host"}},
		{"host.example.com.", []string{"host.example.com", "host"}},
		{"", nil},
		{"a host", nil},
		{"host..example", nil},
	}

	for _, tc := range cases {
		got := hostnameForms(tc.hostname)

		if len(got) != len(tc.want) {
			t.Errorf("hostnameForms(%q) = %v, want %v", tc.hostname, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("hostnameForms(%q) = %v, want %v", tc.hostname, got, tc.want)
				break
			}
		}
	}
}
