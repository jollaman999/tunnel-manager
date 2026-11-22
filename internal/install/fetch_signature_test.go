//go:build !dev

package install

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// The tests here are what the signature does to an install, and the
// development build does not check it (DevBuild). What that build does with
// the same releases is in fetch_dev_test.go.

// TestFetchFallsBackWhenTheReleaseHasNoSignature is a release published before
// the build signed SHA256SUMS. Its checksums are only what the release says
// about itself, so it is taken the way a release with no checksums is: the
// running executable is installed and the binary is never asked for.
func TestFetchFallsBackWhenTheReleaseHasNoSignature(t *testing.T) {
	name := platformAsset(t)

	url, asked := countedRelease(t, "v3.6.1", map[string][]byte{
		name:            testBinary,
		sha256SumsAsset: sha256Line(testBinary, name),
	})

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, url)
	if err != nil {
		t.Fatalf("a release without a signature is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if fetched.Verified {
		t.Error("nothing was signed, so Verified should be false")
	}

	if !strings.Contains(fetched.Why, signatureAsset) {
		t.Errorf("Why should name the file the release is missing, got %q", fetched.Why)
	}

	if got := asked(name); got != 0 {
		t.Errorf("the binary was asked for %d times, want 0", got)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("the release was downloaded although its checksums were not signed: %v", left[0].Name())
	}
}

// TestFetchFallsBackWhenTheSignatureCannotBeRead is a release that names a
// signature the server will not hand over. That is the network failing and not
// the release lying, so it ends where a SHA256SUMS that cannot be read does.
func TestFetchFallsBackWhenTheSignatureCannotBeRead(t *testing.T) {
	name := platformAsset(t)
	sums := sha256Line(testBinary, name)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	info := releaseInfo{TagName: "v9.9.9", Assets: []releaseAsset{
		{Name: name, BrowserDownloadURL: server.URL + "/download/" + name},
		{Name: sha256SumsAsset, BrowserDownloadURL: server.URL + "/download/" + sha256SumsAsset},
		{Name: signatureAsset, BrowserDownloadURL: server.URL + "/download/" + signatureAsset},
	}}

	var binaryAsked atomic.Int32

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(info)
	})

	mux.HandleFunc("/download/"+name, func(w http.ResponseWriter, _ *http.Request) {
		binaryAsked.Add(1)
		_, _ = w.Write(testBinary)
	})

	mux.HandleFunc("/download/"+sha256SumsAsset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sums)
	})

	mux.HandleFunc("/download/"+signatureAsset, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	fetched, err := fetchFrom(context.Background(), t.TempDir(), server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("a signature that cannot be read is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if !strings.Contains(fetched.Why, signatureAsset) || !strings.Contains(fetched.Why, "404") {
		t.Errorf("Why should name the signature and what the server said, got %q", fetched.Why)
	}

	if got := binaryAsked.Load(); got != 0 {
		t.Errorf("the binary was asked for %d times, want 0", got)
	}
}

// TestFetchFailsWhenTheSignatureDoesNotMatch is the release somebody changed
// after it was signed. In each case the checksums agree with the binary, which
// is all a release without a signature could be held to; what gives it away is
// the signature alone, and that has to stop the install before the binary is
// even asked for.
func TestFetchFailsWhenTheSignatureDoesNotMatch(t *testing.T) {
	name := platformAsset(t)
	sums := sha256Line(testBinary, name)

	flipped := signed(sums)
	flipped[10] ^= 0x01

	cases := []struct {
		name      string
		signature []byte
	}{
		{"checksums changed after they were signed", signed(sha256Line([]byte("the binary that was released"), name))},
		{"signed with a key that is not the release key", ed25519.Sign(otherSigningKey, sums)},
		{"a signature with one bit turned", flipped},
		{"a file that is not a signature at all", []byte("not a signature\n")},
		{"an empty file", []byte{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, asked := countedRelease(t, "v9.9.9", map[string][]byte{
				name:            testBinary,
				sha256SumsAsset: sums,
				signatureAsset:  c.signature,
			})

			dir := t.TempDir()

			fetched, err := fetchFrom(context.Background(), dir, url)
			if err == nil {
				source := fetched.Source
				fetched.Close()
				t.Fatalf("a signature the release key did not make has to be an error, got source %v", source)
			}

			// Falling back here would install the running executable and call
			// the install a success, with nobody told that the release they
			// asked for was changed after it was signed.
			if fetched != nil {
				t.Errorf("a bad signature must not fall back, got source %v", fetched.Source)
			}

			if !strings.Contains(err.Error(), signatureAsset) {
				t.Errorf("the error should name the signature, got %v", err)
			}

			if got := asked(name); got != 0 {
				t.Errorf("the binary was asked for %d times, want 0", got)
			}

			left, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read the staging directory: %v", err)
			}

			if len(left) != 0 {
				t.Errorf("something was downloaded for a release whose signature failed: %v", left[0].Name())
			}
		})
	}
}
