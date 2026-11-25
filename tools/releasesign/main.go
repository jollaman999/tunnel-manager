// Command releasesign writes SHA256SUMS.sig, the signature an install checks
// before it believes anything SHA256SUMS says (internal/install, fetchFrom).
//
// It is run by the release target of the Makefile:
//
//	go run ./tools/releasesign <private key> <SHA256SUMS> <tag>
//
// The private key is a PKCS#8 PEM file holding an ed25519 key, the form
// `openssl genpkey -algorithm ed25519` writes. The tag is the one the release
// goes up under, v and the version the binaries were built as, and the first
// line of SHA256SUMS has to name that version ("# tunnel-manager v5.0.1"): an
// install refuses a release whose signed version line and tag disagree, so
// checksums whose line is missing or says anything else are not signed. The
// signature is the 64 raw bytes ed25519 makes over every byte of SHA256SUMS,
// the line included, and it is written beside that file under the same name
// with .sig after it.
//
// It lives in this module rather than beside tools/demo, which is a module of
// its own, so that it can read the key the program carries from the package
// that checks against it, and a key that is not that one is refused before
// anything is signed. The version line is read by that package too
// (install.SignedVersion), so what is held to the tag here is what an install
// holds to it.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/jollaman999/tunnel-manager/internal/install"
)

// releaseTag is the form of a tag a release is signed under: v and three
// numbers. An install compares the tag with the version it runs and takes
// nothing whose tag it cannot read as a version, so a release signed under any
// other tag is one no install would take.
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: releasesign <private key file> <SHA256SUMS> <tag>")
		os.Exit(2)
	}

	written, err := run(os.Args[1], os.Args[2], os.Args[3], install.ReleasePublicKey())
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasesign:", err)
		os.Exit(1)
	}

	fmt.Println("wrote " + written + " for " + os.Args[3])
}

// run signs the file at sumsPath with the key at keyPath, once its version line
// has been found to name tag, and writes the signature beside it. want is the
// key the signature has to check against, handed in so the tests can sign with
// a key of their own; main passes the one the program carries.
//
// What was written is read back and checked against want before this returns,
// so a signature that would be refused by an install never leaves here as one
// that was made.
func run(keyPath string, sumsPath string, tag string, want ed25519.PublicKey) (string, error) {
	if !releaseTag.MatchString(tag) {
		return "", fmt.Errorf("the tag %q is not v and three numbers, and an install takes no release whose tag it cannot read as a version", tag)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read the private key: %w", err)
	}

	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", fmt.Errorf("read the checksums: %w", err)
	}

	// A release signed without the line, or with a line that names another
	// version than its tag, is one this version of the program does not
	// install, and that is better found out here than after it was published.
	_, err = install.SignedVersion(sums, tag)
	if errors.Is(err, install.ErrNoSignedVersion) {
		return "", fmt.Errorf("%s has no first line %q naming the version, and an install does not take a release without it",
			sumsPath, "# tunnel-manager "+tag)
	}

	if err != nil {
		return "", fmt.Errorf("%s cannot be signed as %s: %w", sumsPath, tag, err)
	}

	signature, err := sign(keyPEM, sums, want)
	if err != nil {
		return "", err
	}

	out := sumsPath + ".sig"

	err = os.WriteFile(out, signature, 0o644)
	if err != nil {
		return "", fmt.Errorf("write the signature: %w", err)
	}

	written, err := os.ReadFile(out)
	if err != nil {
		return "", fmt.Errorf("read back the signature: %w", err)
	}

	if !ed25519.Verify(want, sums, written) {
		// A file that is there and wrong is worse than no file: a release made
		// with it would stop every install that reads it.
		_ = os.Remove(out)

		return "", fmt.Errorf("the signature written to %s does not check against the release key", out)
	}

	return out, nil
}

// sign makes the signature of sums with the private key in keyPEM, refusing a
// key whose public half is not want.
//
// Nothing of the key goes into a message, not even its public half: what this
// prints ends up in the terminal and the logs of whoever cut the release, and
// saying the key is the wrong one is all the operator needs to look for the
// right one.
func sign(keyPEM []byte, sums []byte, want ed25519.PublicKey) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("the private key file is not a PKCS#8 PEM private key")
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the private key file holds no PKCS#8 private key that can be read")
	}

	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the private key is a %T, and releases are signed with ed25519", parsed)
	}

	public, ok := key.Public().(ed25519.PublicKey)
	if !ok || !public.Equal(want) {
		// A release signed with any other key is one every install refuses, and
		// it would only be found out when somebody tried to install it.
		return nil, errors.New("the private key is not the release key: its public half is not the one internal/install checks releases against")
	}

	return ed25519.Sign(key, sums), nil
}
