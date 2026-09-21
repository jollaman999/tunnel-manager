package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testBinary is what the fake release serves in place of the real binary. Its
// content does not matter to Fetch; what matters is that the file that lands on
// disk is this and that its hash is what SHA256SUMS is written from.
var testBinary = []byte("#!/bin/sh\necho a release binary\n")

// fakeRelease stands in for the release API. It answers the latest release with
// the assets it is given and serves each of them, so a test drives Fetch the
// whole way through without a real network.
//
// It returns the address of the latest release, which is what fetchFrom is
// handed in place of the constant.
func fakeRelease(t *testing.T, tag string, assets map[string][]byte) string {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	info := releaseInfo{TagName: tag}

	for name, body := range assets {
		info.Assets = append(info.Assets, releaseAsset{
			Name:               name,
			BrowserDownloadURL: server.URL + "/download/" + name,
		})

		mux.HandleFunc("/download/"+name, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		})
	}

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(w).Encode(info)
		if err != nil {
			t.Errorf("failed to write the release answer: %v", err)
		}
	})

	return server.URL + "/releases/latest"
}

// platformAsset is the name the release carries for the platform the tests run
// on. Fetch asks for this one and no other.
func platformAsset(t *testing.T) string {
	t.Helper()

	name := assetName(runtime.GOOS, runtime.GOARCH)
	if name == "" {
		t.Skipf("no release asset is built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	return name
}

func sha256Line(body []byte, name string) []byte {
	sum := sha256.Sum256(body)

	return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
}

func TestFetchTakesTheReleaseWhenTheChecksumMatches(t *testing.T) {
	name := platformAsset(t)

	sums := append([]byte("0000000000000000000000000000000000000000000000000000000000000000  something-else\n"),
		sha256Line(testBinary, name)...)

	url := fakeRelease(t, "v9.9.9", map[string][]byte{
		name:            testBinary,
		sha256SumsAsset: sums,
	})

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, url)
	if err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRelease {
		t.Errorf("source = %v, want SourceRelease", fetched.Source)
	}

	if !fetched.Verified {
		t.Error("the checksum matched, so Verified should be true")
	}

	if fetched.Tag != "v9.9.9" {
		t.Errorf("tag = %q, want v9.9.9", fetched.Tag)
	}

	if fetched.Why != "" {
		t.Errorf("the release was used, so Why should be empty, got %q", fetched.Why)
	}

	if fetched.Path != filepath.Join(dir, name) {
		t.Errorf("path = %q, want the asset in the directory that was handed in", fetched.Path)
	}

	landed, err := os.ReadFile(fetched.Path)
	if err != nil {
		t.Fatalf("failed to read what was downloaded: %v", err)
	}

	if !bytes.Equal(landed, testBinary) {
		t.Error("what landed on disk is not what the release served")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(fetched.Path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}

		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("mode = %v, want the execute bits set", info.Mode().Perm())
		}
	}

	// Close is what the install defers, and what it removes is the download and
	// not the directory, which belongs to the caller.
	fetched.Close()

	_, err = os.Stat(fetched.Path)
	if !os.IsNotExist(err) {
		t.Errorf("Close should have removed the download, stat says %v", err)
	}

	_, err = os.Stat(dir)
	if err != nil {
		t.Errorf("Close removed the directory it was handed: %v", err)
	}
}

func TestFetchTakesTheReleaseWithNoChecksumFile(t *testing.T) {
	name := platformAsset(t)

	url := fakeRelease(t, "v3.5.1", map[string][]byte{name: testBinary})

	fetched, err := fetchFrom(context.Background(), t.TempDir(), url)
	if err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRelease {
		t.Errorf("source = %v, want SourceRelease", fetched.Source)
	}

	if fetched.Verified {
		t.Error("there was nothing to check against, so Verified should be false")
	}

	if fetched.Why != "" {
		t.Errorf("the release was used, so Why should be empty, got %q", fetched.Why)
	}

	landed, err := os.ReadFile(fetched.Path)
	if err != nil {
		t.Fatalf("failed to read what was downloaded: %v", err)
	}

	if !bytes.Equal(landed, testBinary) {
		t.Error("what landed on disk is not what the release served")
	}
}

func TestFetchFailsWhenTheChecksumDoesNotMatch(t *testing.T) {
	name := platformAsset(t)

	url := fakeRelease(t, "v9.9.9", map[string][]byte{
		name:            testBinary,
		sha256SumsAsset: sha256Line([]byte("a different binary"), name),
	})

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, url)
	if err == nil {
		fetched.Close()
		t.Fatal("a file that does not hash to what the release says it does has to be an error")
	}

	// This is the case that must not fall back. Falling back would leave the
	// install going with a binary the operator never asked about, and the reason
	// it was chosen would be a mismatch nobody was told about.
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

func TestFetchFallsBackWhenTheAPIIsDown(t *testing.T) {
	// A server that is started and stopped gives an address nothing listens on,
	// which is what a machine with no route to GitHub looks like from here.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL + "/releases/latest"
	server.Close()

	fetched, err := fetchFrom(context.Background(), t.TempDir(), url)
	if err != nil {
		t.Fatalf("a release that cannot be read is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Errorf("source = %v, want SourceRunning", fetched.Source)
	}

	if fetched.Why == "" {
		t.Error("the fallback has to say why the release was not used")
	}

	if fetched.Verified {
		t.Error("nothing was checked, so Verified should be false")
	}

	if fetched.Tag != "" {
		t.Errorf("tag = %q, want empty for the running executable", fetched.Tag)
	}

	running, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	if fetched.Path != running {
		t.Errorf("path = %q, want the running executable %q", fetched.Path, running)
	}
}

// TestFetchFallsBackWhenTheAPIAnswersAnError covers the other way the API goes
// away: it answers, and what it answers is not a release. A rate limit looks
// like this.
func TestFetchFallsBackWhenTheAPIAnswersAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limit exceeded", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	fetched, err := fetchFrom(context.Background(), t.TempDir(), server.URL)
	if err != nil {
		t.Fatalf("an API that refuses is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Errorf("source = %v, want SourceRunning", fetched.Source)
	}

	if !strings.Contains(fetched.Why, "403") {
		t.Errorf("Why should carry what the API said, got %q", fetched.Why)
	}
}

// TestFetchFallsBackWhenTheReleaseHasNoAssetForThisPlatform is the release that
// was published without the file this machine needs.
func TestFetchFallsBackWhenTheReleaseHasNoAssetForThisPlatform(t *testing.T) {
	url := fakeRelease(t, "v9.9.9", map[string][]byte{"tunnel-manager-plan9-mips": testBinary})

	fetched, err := fetchFrom(context.Background(), t.TempDir(), url)
	if err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Errorf("source = %v, want SourceRunning", fetched.Source)
	}

	if !strings.Contains(fetched.Why, platformAsset(t)) {
		t.Errorf("Why should name the file that was missing, got %q", fetched.Why)
	}
}

// TestFetchFallsBackWhenTheChecksumsDoNotListTheAsset is the release that
// publishes checksums with this file left out of them. There is nothing to
// check against on a release that says it checks everything, so the install
// takes what is running instead.
func TestFetchFallsBackWhenTheChecksumsDoNotListTheAsset(t *testing.T) {
	name := platformAsset(t)

	url := fakeRelease(t, "v9.9.9", map[string][]byte{
		name:            testBinary,
		sha256SumsAsset: sha256Line(testBinary, "tunnel-manager-some-other-platform"),
	})

	fetched, err := fetchFrom(context.Background(), t.TempDir(), url)
	if err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Errorf("source = %v, want SourceRunning", fetched.Source)
	}

	if !strings.Contains(fetched.Why, sha256SumsAsset) {
		t.Errorf("Why should say the checksums did not list it, got %q", fetched.Why)
	}
}

func TestAssetNameCoversTheReleasePlatforms(t *testing.T) {
	// The names are the ones the release build writes, so this table is what
	// keeps a rename of one side from quietly turning every install into a
	// fallback.
	cases := []struct {
		goos   string
		goarch string
		want   string
	}{
		{"linux", "amd64", "tunnel-manager-linux-amd64"},
		{"linux", "arm64", "tunnel-manager-linux-arm64"},
		{"darwin", "amd64", "tunnel-manager-darwin-amd64"},
		{"darwin", "arm64", "tunnel-manager-darwin-arm64"},
		{"windows", "amd64", "tunnel-manager-windows-amd64.exe"},
		{"linux", "386", ""},
		{"windows", "arm64", ""},
		{"freebsd", "amd64", ""},
	}

	for _, c := range cases {
		got := assetName(c.goos, c.goarch)
		if got != c.want {
			t.Errorf("assetName(%q, %q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestSumForReadsWhatSha256sumWrites(t *testing.T) {
	sums := []byte(strings.Join([]string{
		"# a comment nobody writes but nothing should trip over",
		"aa  tunnel-manager-linux-amd64",
		"1111111111111111111111111111111111111111111111111111111111111111  tunnel-manager-linux-arm64",
		"2222222222222222222222222222222222222222222222222222222222222222 *tunnel-manager-linux-amd64",
		"3333333333333333333333333333333333333333333333333333333333333333  SHA256SUMS",
	}, "\n"))

	got := sumFor(sums, "tunnel-manager-linux-amd64")
	if got != "2222222222222222222222222222222222222222222222222222222222222222" {
		t.Errorf("sumFor = %q, want the line for the asset in binary mode", got)
	}

	if sumFor(sums, "tunnel-manager-darwin-arm64") != "" {
		t.Error("a file that is not listed has no checksum")
	}
}
