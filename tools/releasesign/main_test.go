package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/install"
)

// writeKey puts key into a PKCS#8 PEM file the way openssl genpkey writes one
// and returns where it went, with the PEM itself for the tests that look for it
// in a message.
func writeKey(t *testing.T, dir string, key any) (string, []byte) {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}

	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(dir, "release.key")

	err = os.WriteFile(path, encoded, 0o600)
	if err != nil {
		t.Fatalf("write the key: %v", err)
	}

	return path, encoded
}

// checksumLine is one line of SHA256SUMS the way sha256sum writes it.
const checksumLine = "1111111111111111111111111111111111111111111111111111111111111111  tunnel-manager-linux-amd64\n"

// writeSums writes SHA256SUMS the way the release target does for v5.1.0: the
// version line first, then the checksums.
func writeSums(t *testing.T, dir string) (string, []byte) {
	t.Helper()

	return writeSumsOf(t, dir, "# tunnel-manager v5.1.0\n"+checksumLine)
}

func writeSumsOf(t *testing.T, dir string, content string) (string, []byte) {
	t.Helper()

	sums := []byte(content)
	path := filepath.Join(dir, "SHA256SUMS")

	err := os.WriteFile(path, sums, 0o644)
	if err != nil {
		t.Fatalf("write the checksums: %v", err)
	}

	return path, sums
}

// TestRunSignsWithTheReleaseKey signs a release and holds what was written to
// what an install checks: a signature over every byte of SHA256SUMS, the
// version line included, and a line that names the tag.
func TestRunSignsWithTheReleaseKey(t *testing.T) {
	dir := t.TempDir()

	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	keyPath, _ := writeKey(t, dir, key)
	sumsPath, sums := writeSums(t, dir)

	written, err := run(keyPath, sumsPath, "v5.1.0", public)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if written != sumsPath+".sig" {
		t.Errorf("wrote %q, want the checksums file with .sig after it", written)
	}

	signature, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("read the signature: %v", err)
	}

	if len(signature) != ed25519.SignatureSize {
		t.Errorf("the signature is %d bytes, want the %d raw bytes ed25519 makes", len(signature), ed25519.SignatureSize)
	}

	if !ed25519.Verify(public, sums, signature) {
		t.Error("the signature does not check against the key it was made with")
	}

	if ed25519.Verify(public, append(sums, '\n'), signature) {
		t.Error("the signature checks against checksums it was not made over")
	}

	unversioned := []byte(checksumLine)
	if ed25519.Verify(public, unversioned, signature) {
		t.Error("the signature checks against the checksums without their version line")
	}

	version, err := install.SignedVersion(sums, "v5.1.0")
	if err != nil || version != "v5.1.0" {
		t.Errorf("an install reads the signed checksums as %q, %v, want v5.1.0", version, err)
	}
}

// TestRunRefusesChecksumsThatDoNotNameTheTag is SHA256SUMS whose version line
// is missing, names another version than the tag, or is not where the release
// build writes it. An install refuses or passes over every one of them, so
// nothing is signed.
func TestRunRefusesChecksumsThatDoNotNameTheTag(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"no version line", checksumLine, "no first line"},
		{"another version", "# tunnel-manager v5.0.1\n" + checksumLine, "signed as v5.0.1 and the release is tagged v5.1.0"},
		{"the line after the checksums", checksumLine + "# tunnel-manager v5.1.0\n", "line 2"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()

			keyPath, _ := writeKey(t, dir, key)
			sumsPath, _ := writeSumsOf(t, dir, c.content)

			_, err := run(keyPath, sumsPath, "v5.1.0", key.Public().(ed25519.PublicKey))
			if err == nil {
				t.Fatal("checksums that do not name the tag were signed")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal says %q, want it to say %q", err, c.want)
			}

			_, statErr := os.Stat(sumsPath + ".sig")
			if !os.IsNotExist(statErr) {
				t.Errorf("a signature was left behind for checksums that were refused: %v", statErr)
			}
		})
	}
}

// TestRunRefusesATagAnInstallCannotRead is a release signed under a tag that is
// not v and three numbers. An install takes no release whose tag it cannot
// compare with its own version, so the signature is not made at all.
func TestRunRefusesATagAnInstallCannotRead(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))

	for _, tag := range []string{"", "5.1.0", "v5.1", "v5.1.0-rc1", "v5.1.0\n", " v5.1.0", "latest"} {
		t.Run(tag, func(t *testing.T) {
			dir := t.TempDir()

			keyPath, _ := writeKey(t, dir, key)
			sumsPath, _ := writeSums(t, dir)

			_, err := run(keyPath, sumsPath, tag, key.Public().(ed25519.PublicKey))
			if err == nil {
				t.Fatalf("the checksums were signed under %q", tag)
			}

			if !strings.Contains(err.Error(), "three numbers") {
				t.Errorf("the refusal says %q, want it to say what a tag should be", err)
			}

			_, statErr := os.Stat(sumsPath + ".sig")
			if !os.IsNotExist(statErr) {
				t.Errorf("a signature was left behind for a refused tag: %v", statErr)
			}
		})
	}
}

// TestRunRefusesAKeyThatIsNotTheReleaseKey is the release signed with the wrong
// key. Every install would refuse it, so it must not be made, and nothing about
// the key that was handed in may appear in what is printed.
func TestRunRefusesAKeyThatIsNotTheReleaseKey(t *testing.T) {
	releaseKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	otherKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key of another kind: %v", err)
	}

	cases := []struct {
		name string
		key  any
		want string
	}{
		{"another ed25519 key", otherKey, "not the release key"},
		{"a key that is not ed25519", ecKey, "ed25519"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()

			keyPath, keyPEM := writeKey(t, dir, c.key)
			sumsPath, _ := writeSums(t, dir)

			_, err := run(keyPath, sumsPath, "v5.1.0", releaseKey.Public().(ed25519.PublicKey))
			if err == nil {
				t.Fatal("a key that is not the release key signed the checksums")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal says %q, want it to say %q", err, c.want)
			}

			_, statErr := os.Stat(sumsPath + ".sig")
			if !os.IsNotExist(statErr) {
				t.Errorf("a signature was left behind by a refused key: %v", statErr)
			}

			body := string(bytes.Join(bytes.Split(keyPEM, []byte("\n"))[1:2], nil))
			if body != "" && strings.Contains(err.Error(), body) {
				t.Error("the refusal carries the private key")
			}
		})
	}
}

func TestRunRefusesAFileThatIsNotAKey(t *testing.T) {
	dir := t.TempDir()

	keyPath := filepath.Join(dir, "release.key")

	err := os.WriteFile(keyPath, []byte("not a key\n"), 0o600)
	if err != nil {
		t.Fatalf("write the file: %v", err)
	}

	sumsPath, _ := writeSums(t, dir)
	want := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize)).Public().(ed25519.PublicKey)

	_, err = run(keyPath, sumsPath, "v5.1.0", want)
	if err == nil {
		t.Fatal("a file that holds no key signed the checksums")
	}

	if !strings.Contains(err.Error(), "PKCS#8") {
		t.Errorf("the refusal says %q, want it to say what the file should be", err)
	}
}
