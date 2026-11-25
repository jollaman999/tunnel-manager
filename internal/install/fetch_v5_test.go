package install

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTheV5InstallTakesAReleaseWithAVersionLine is a release made the way the
// release build makes them now, with the version line at the top of
// SHA256SUMS and the signature over the bytes of that file, fetched by the
// install of v5.0.0.
//
// An install of v5.0.0 updates to the next release with the code it already
// carries, so a release it refused would leave every machine on v5.0.0 unable
// to update itself. That code is copied below as it was in v5.0.0, under names
// that end in V5, and the release has to come out of it installed and
// verified. The copy is the point: it must not follow the code above it when
// that changes, because the installs that run it will not either.
func TestTheV5InstallTakesAReleaseWithAVersionLine(t *testing.T) {
	name := platformAsset(t)
	sums := versioned("v5.0.1", sha256Line(testBinary, name))

	url := fakeRelease(t, "v5.0.1", map[string][]byte{
		name:            testBinary,
		sha256SumsAsset: sums,
		signatureAsset:  signed(sums),
	})

	fetched, err := fetchFromV5(context.Background(), t.TempDir(), url)
	if err != nil {
		t.Fatalf("the install of v5.0.0 refused a release with a version line: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRelease {
		t.Fatalf("source = %v, want SourceRelease. Why: %s", fetched.Source, fetched.Why)
	}

	if fetched.Verified == DevBuild {
		t.Errorf("verified = %v in a build where DevBuild is %v", fetched.Verified, DevBuild)
	}

	landed, err := os.ReadFile(fetched.Path)
	if err != nil {
		t.Fatalf("read what was downloaded: %v", err)
	}

	if !bytes.Equal(landed, testBinary) {
		t.Error("what landed on disk is not what the release served")
	}

	if got := sumForV5(sums, name); got != sumFor(sums, name) || got == "" {
		t.Errorf("the v5.0.0 reading of the checksums gives %q, this one %q", got, sumFor(sums, name))
	}
}

// What follows is internal/install/fetch.go of v5.0.0 (6657e33, the same at
// eecc606): fetchFrom, checkSignature and sumFor, renamed and otherwise left as
// they were. The helpers they call are the ones above, which this release does
// not change.

func fetchFromV5(ctx context.Context, dir string, latestURL string) (*Fetched, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	name := assetName(runtime.GOOS, runtime.GOARCH)
	if name == "" {
		return runningExecutable(fmt.Sprintf("a release is not built for %s/%s", runtime.GOOS, runtime.GOARCH))
	}

	client := newFetchClient()

	latest, err := latestRelease(ctx, client, latestURL)
	if err != nil {
		return runningExecutable(fmt.Sprintf("the latest release could not be read: %v", err))
	}

	binary := latest.asset(name)
	if binary == nil {
		return runningExecutable(fmt.Sprintf("release %s carries no %s", latest.TagName, name))
	}

	// The checksums are read before the binary. They are a few kilobytes against
	// fifteen megabytes, and whatever keeps them from arriving keeps the binary
	// from being usable, so failing here costs nothing that was going to be kept.
	sums := latest.asset(sha256SumsAsset)
	if sums == nil {
		// A release that carries no checksums leaves nothing to check a download
		// against, and a file that cannot be checked is whatever answered the
		// request rather than what the release published. Releases made before
		// the build wrote this file are what land here, and the executable this
		// process is already running is content that was checked when it was put
		// in place, so the install takes that instead.
		return runningExecutable(fmt.Sprintf("release %s carries no %s", latest.TagName, sha256SumsAsset))
	}

	body, err := readBody(ctx, client, sums.BrowserDownloadURL, maxMetaBytes)
	if err != nil {
		return runningExecutable(fmt.Sprintf("the %s of release %s could not be read: %v", sha256SumsAsset, latest.TagName, err))
	}

	// The signature is checked before anything in the checksums is believed.
	// Until then they are only what the release says about itself, and the same
	// hands that could swap the binary could have written them to match. The
	// development build does not check it; see DevBuild.
	if !DevBuild {
		fallback, err := checkSignatureV5(ctx, client, latest, body)
		if err != nil {
			return nil, err
		}

		if fallback != "" {
			return runningExecutable(fallback)
		}
	}

	want := sumForV5(body, name)
	if want == "" {
		// The release publishes checksums and this file is not among them.
		// There is nothing to check it against, and a release that says what
		// its files hash to is not one to take an unlisted file from, so this
		// goes to the executable that is already running rather than to an
		// error: falling back installs known content either way.
		return runningExecutable(fmt.Sprintf("the %s of release %s lists no checksum for %s", sha256SumsAsset, latest.TagName, name))
	}

	path := filepath.Join(dir, name)

	got, err := downloadAsset(ctx, client, binary.BrowserDownloadURL, path)
	if err != nil {
		_ = os.Remove(path)

		return runningExecutable(fmt.Sprintf("%s of release %s could not be downloaded: %v", name, latest.TagName, err))
	}

	if !strings.EqualFold(got, want) {
		// The half-written file goes now. It is content nobody vouched for, and
		// leaving it in the staging directory is leaving something for a later
		// step, or a later operator, to pick up by mistake.
		_ = os.Remove(path)

		return nil, fmt.Errorf("%s of release %s hashes to %s and its %s entry says %s",
			name, latest.TagName, got, sha256SumsAsset, want)
	}

	// The download got here only by hashing to what the signed checksums said
	// it would, because the paths where there was nothing to compare it against
	// all left on the fallback above. In the development build the checksums
	// were not held to their signature, and Verified says so.
	return &Fetched{
		Path:     path,
		Source:   SourceRelease,
		Tag:      latest.TagName,
		Verified: !DevBuild,
		cleanup:  func() { _ = os.Remove(path) },
	}, nil
}

func checkSignatureV5(ctx context.Context, client *http.Client, latest *releaseInfo, sums []byte) (string, error) {
	sig := latest.asset(signatureAsset)
	if sig == nil {
		// Releases made before the build signed SHA256SUMS are what land here,
		// and they are taken the way a release without SHA256SUMS is: nothing
		// says the checksums are the ones that were published, so the executable
		// this process is already running, which was checked when it was put in
		// place, is installed instead.
		return fmt.Sprintf("release %s carries no %s", latest.TagName, signatureAsset), nil
	}

	signature, err := readBody(ctx, client, sig.BrowserDownloadURL, maxSignatureBytes)
	if err != nil {
		return fmt.Sprintf("the %s of release %s could not be read: %v", signatureAsset, latest.TagName, err), nil
	}

	if !ed25519.Verify(releaseKey, sums, signature) {
		// The release carries a signature and it is not one the release key
		// made over these checksums. Either the checksums or the signature were
		// changed after the release was signed, and the binary is not fetched
		// at all: there is nothing left that says what it should be.
		return "", fmt.Errorf("the %s of release %s is not signed by the release key: its %s does not match it",
			sha256SumsAsset, latest.TagName, signatureAsset)
	}

	return "", nil
}

func sumForV5(sums []byte, name string) string {
	scanner := bufio.NewScanner(bytes.NewReader(sums))

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}

		if len(fields[0]) != sha256HexLen {
			continue
		}

		listed := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(listed) != name {
			continue
		}

		return strings.ToLower(fields[0])
	}

	return ""
}
