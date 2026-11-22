//go:build dev

package install

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"strings"
	"testing"
)

// The tests here are the development build (DevBuild), which installs a
// release on its checksum and does not check the signature over SHA256SUMS.
// They run under `go test -tags dev`. What every other build does with the
// same releases is in fetch_signature_test.go.

// TestDevBuildTakesAReleaseWhateverItsSignature is the release with no
// signature, and the release whose signature is not the release key's. Both
// stop or fall back everywhere else; here the checksum decides, and the
// release is installed with Verified false so the report says the signature
// was not looked at.
func TestDevBuildTakesAReleaseWhateverItsSignature(t *testing.T) {
	name := platformAsset(t)
	sums := sha256Line(testBinary, name)

	cases := []struct {
		name   string
		assets map[string][]byte
	}{
		{"no signature", map[string][]byte{
			name:            testBinary,
			sha256SumsAsset: sums,
		}},
		{"a signature the release key did not make", map[string][]byte{
			name:            testBinary,
			sha256SumsAsset: sums,
			signatureAsset:  ed25519.Sign(otherSigningKey, sums),
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, asked := countedRelease(t, "v9.9.9", c.assets)

			fetched, err := fetchFrom(context.Background(), t.TempDir(), url)
			if err != nil {
				t.Fatalf("the development build stopped on the signature: %v", err)
			}
			defer fetched.Close()

			if fetched.Source != SourceRelease {
				t.Fatalf("source = %v, want SourceRelease. Why: %s", fetched.Source, fetched.Why)
			}

			if fetched.Verified {
				t.Error("the signature was not checked, so Verified should be false")
			}

			if got := asked(signatureAsset); got != 0 {
				t.Errorf("the signature was asked for %d times, want 0: this build does not read it", got)
			}

			landed, err := os.ReadFile(fetched.Path)
			if err != nil {
				t.Fatalf("read what was downloaded: %v", err)
			}

			if !bytes.Equal(landed, testBinary) {
				t.Error("what landed on disk is not what the release served")
			}
		})
	}
}

// TestDevBuildStillStopsOnAChecksumThatDoesNotMatch is what the development
// build keeps. It does not look at the signature, and a download that does not
// hash to what SHA256SUMS says is still not put in place.
func TestDevBuildStillStopsOnAChecksumThatDoesNotMatch(t *testing.T) {
	name := platformAsset(t)

	url := fakeRelease(t, "v9.9.9", map[string][]byte{
		name:            testBinary,
		sha256SumsAsset: sha256Line([]byte("a different binary"), name),
	})

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, url)
	if err == nil {
		fetched.Close()
		t.Fatal("a file that does not hash to what the release says it does has to be an error in this build too")
	}

	if fetched != nil {
		t.Errorf("a mismatch must not fall back, got source %v", fetched.Source)
	}

	if !strings.Contains(err.Error(), sha256SumsAsset) {
		t.Errorf("the error should say which file disagreed, got %v", err)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("the download that failed its checksum was left behind: %v", left[0].Name())
	}
}

// TestDevBuildStillFallsBackWithoutChecksums is the release with nothing at
// all to check a download against. Not checking the signature is not a reason
// to take a file nothing describes.
func TestDevBuildStillFallsBackWithoutChecksums(t *testing.T) {
	name := platformAsset(t)

	url, asked := countedRelease(t, "v3.5.1", map[string][]byte{name: testBinary})

	fetched, err := fetchFrom(context.Background(), t.TempDir(), url)
	if err != nil {
		t.Fatalf("a release without checksums is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if got := asked(name); got != 0 {
		t.Errorf("the binary was asked for %d times, want 0", got)
	}
}
