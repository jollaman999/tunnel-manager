// Command releasesign writes SHA256SUMS.sig, the signature an install checks
// before it believes anything SHA256SUMS says (internal/install, fetchFrom).
//
// It is run by the release target of the Makefile:
//
//	go run ./tools/releasesign <private key> <SHA256SUMS>
//
// The private key is a PKCS#8 PEM file holding an ed25519 key, the form
// `openssl genpkey -algorithm ed25519` writes. The signature is the 64 raw bytes
// ed25519 makes over every byte of SHA256SUMS, and it is written beside that
// file under the same name with .sig after it.
//
// It lives in this module rather than beside tools/demo, which is a module of
// its own, so that it can read the key the program carries from the package
// that checks against it, and a key that is not that one is refused before
// anything is signed.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/jollaman999/tunnel-manager/internal/install"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: releasesign <private key file> <SHA256SUMS>")
		os.Exit(2)
	}

	written, err := run(os.Args[1], os.Args[2], install.ReleasePublicKey())
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasesign:", err)
		os.Exit(1)
	}

	fmt.Println("wrote " + written)
}

// run signs the file at sumsPath with the key at keyPath and writes the
// signature beside it. want is the key the signature has to check against,
// handed in so the tests can sign with a key of their own; main passes the one
// the program carries.
//
// What was written is read back and checked against want before this returns,
// so a signature that would be refused by an install never leaves here as one
// that was made.
func run(keyPath string, sumsPath string, want ed25519.PublicKey) (string, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read the private key: %w", err)
	}

	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", fmt.Errorf("read the checksums: %w", err)
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
