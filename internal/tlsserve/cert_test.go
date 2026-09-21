package tlsserve

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
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

// generatedSigner reads the private key back out of what generate produced, so
// that a test can sign with it the way whoever came away with the database file
// and the key file together would.
func generatedSigner(t *testing.T, generated *material) crypto.Signer {
	t.Helper()

	keyPair, err := tls.X509KeyPair(generated.certPEM, generated.keyPEM)
	if err != nil {
		t.Fatalf("the generated certificate and key do not form a pair: %v", err)
	}

	signer, ok := keyPair.PrivateKey.(crypto.Signer)
	if !ok {
		t.Fatalf("the generated private key is %T, which cannot sign", keyPair.PrivateKey)
	}

	return signer
}

// signedUnder issues a server certificate for the names given, signed by the
// generated certificate. It is what the holder of the key would mint.
func signedUnder(t *testing.T, anchor *x509.Certificate, signer crypto.Signer,
	dnsNames []string, ips []net.IP) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key for the issued certificate: %v", err)
	}

	name := "issued"
	if len(dnsNames) > 0 {
		name = dnsNames[0]
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             anchor.NotBefore,
		NotAfter:              anchor.NotAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, anchor, &key.PublicKey, signer)
	if err != nil {
		t.Fatalf("signing a certificate under the generated one: %v", err)
	}

	issued, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the issued certificate: %v", err)
	}

	return issued
}

// verifyAgainst checks a certificate the way a client does whose trust store
// holds the generated certificate: the anchor is the only root, and the name
// asked for is the one the client typed.
func verifyAgainst(anchor *x509.Certificate, leaf *x509.Certificate, name string,
	now time.Time) error {
	pool := x509.NewCertPool()
	pool.AddCert(anchor)

	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		DNSName:     name,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})

	return err
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

// TestTheValidityIsWhatWasChosen holds the period to the constant and the
// constant to a range, so that a value nobody meant cannot slip in. The upper
// bound is not a rule any client enforces on a certificate like this one: it is
// the point past which the number stops meaning anything to the operator, and
// the lower bound is the point where renewing becomes a chore.
func TestTheValidityIsWhatWasChosen(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	leaf, _ := parseGenerated(t, now)

	validity := leaf.NotAfter.Sub(leaf.NotBefore)
	if validity != certValidity {
		t.Errorf("the certificate is valid for %v, want %v", validity, certValidity)
	}
	if validity > 10*365*24*time.Hour {
		t.Errorf("the certificate is valid for %v, which is long enough that it says nothing",
			validity)
	}
	if validity < 365*24*time.Hour {
		t.Errorf("the certificate is valid for %v, which makes renewing it a chore", validity)
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
		t.Error("the certificate is not a CA, so a trust store that insists on one refuses to " +
			"take it as an anchor")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("the certificate may not sign, so it cannot serve a TLS handshake")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("the certificate may not sign a certificate, so it cannot be its own issuer")
	}
	if !leaf.MaxPathLenZero || leaf.MaxPathLen != 0 {
		t.Errorf("the path length is %d (zero recorded: %v), want a recorded zero so that no "+
			"authority can be made under this certificate", leaf.MaxPathLen, leaf.MaxPathLenZero)
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

// TestTheCertificateMayOnlySignItsOwnNames reads the constraints off the
// certificate and holds them to the names it is made out to. A constraint that
// is wider than the names is a certificate that may vouch for something this
// machine is not.
func TestTheCertificateMayOnlySignItsOwnNames(t *testing.T) {
	leaf, _ := parseGenerated(t, time.Now())

	if !leaf.PermittedDNSDomainsCritical {
		t.Error("the name constraints are not critical, so a client that cannot read them takes " +
			"the certificate without the limit")
	}

	if !sameStrings(leaf.PermittedDNSDomains, leaf.DNSNames) {
		t.Errorf("the certificate may sign for the names %v while it is made out to %v",
			leaf.PermittedDNSDomains, leaf.DNSNames)
	}

	wanted := make([]string, 0, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		bits := 128
		if ip.To4() != nil {
			bits = 32
		}
		wanted = append(wanted, (&net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}).String())
	}

	got := make([]string, 0, len(leaf.PermittedIPRanges))
	for _, permitted := range leaf.PermittedIPRanges {
		got = append(got, permitted.String())
	}

	if !sameStrings(got, wanted) {
		t.Errorf("the certificate may sign for the addresses %v while it is made out to %v",
			got, wanted)
	}
}

// TestTheCertificateCannotSignForAnotherName is what the constraints are there
// for, seen from the client.
//
// The operator is told to put this certificate in the trust store of their
// machine to be rid of the warning, and from that moment it is allowed to have
// signed. Whoever comes away with the database file and the key file together
// would otherwise mint a certificate for any name at all and that browser would
// accept it, which is an interception of every site it visits rather than of
// this one. The issuing still works for the names this machine answers to, so
// what fails below is the limit and not the signing.
func TestTheCertificateCannotSignForAnotherName(t *testing.T) {
	now := time.Now()
	anchor, generated := parseGenerated(t, now)
	signer := generatedSigner(t, generated)

	forged := signedUnder(t, anchor, signer, []string{"bank.example.com"}, nil)

	err := verifyAgainst(anchor, forged, "bank.example.com", now)
	if err == nil {
		t.Fatal("a certificate for bank.example.com signed with the key of this machine is " +
			"accepted by a client that trusts this machine")
	}

	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.CANotAuthorizedForThisName {
		t.Errorf("bank.example.com was refused with %v, want the name constraint to be what "+
			"refused it", err)
	}

	// The same for an address outside the ones this machine answers to. The
	// address is from the range reserved for documentation, which no interface
	// carries, and the test says so rather than guessing when it does.
	outside := net.IPv4(198, 51, 100, 7)
	for _, permitted := range anchor.PermittedIPRanges {
		if permitted.Contains(outside) {
			t.Skipf("this machine answers to %s, so it is not an address to test the limit with",
				outside)
		}
	}

	forgedIP := signedUnder(t, anchor, signer, nil, []net.IP{outside})

	err = verifyAgainst(anchor, forgedIP, outside.String(), now)
	if err == nil {
		t.Errorf("a certificate for %s signed with the key of this machine is accepted by a "+
			"client that trusts this machine", outside)
	}
}

// TestTheCertificateIsStillItsOwnAnchor is the other side of the constraints:
// the server it was made for is still reached under every name it carries by a
// client that holds it as the only root. A limit that also shut this out would
// be a machine nobody can connect to.
func TestTheCertificateIsStillItsOwnAnchor(t *testing.T) {
	now := time.Now()
	anchor, generated := parseGenerated(t, now)

	for _, host := range generated.hosts {
		if err := verifyAgainst(anchor, anchor, host, now); err != nil {
			t.Errorf("a client that trusts this certificate cannot reach the server at %s: %v",
				host, err)
		}
	}

	// A certificate issued under it for a name it carries verifies as well,
	// which is what the CA bits are kept for.
	signer := generatedSigner(t, generated)
	issued := signedUnder(t, anchor, signer, []string{"localhost"}, nil)

	if err := verifyAgainst(anchor, issued, "localhost", now); err != nil {
		t.Errorf("a certificate issued under this one for a name it carries is refused: %v", err)
	}
}

// sameStrings reports whether two lists hold the same entries, in whatever
// order. The generated lists are sorted, and a test that pins the order as well
// would fail for a reason that is not what it is about.
func sameStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	leftSorted := append([]string(nil), left...)
	rightSorted := append([]string(nil), right...)
	sort.Strings(leftSorted)
	sort.Strings(rightSorted)

	for i := range leftSorted {
		if leftSorted[i] != rightSorted[i] {
			return false
		}
	}

	return true
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
