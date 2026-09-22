package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain shortens the waits between retries. The waits are there for a CDN
// that needs seconds to catch up with a release, and a test that sat through a
// real one would spend that time doing nothing.
//
// It also lets the servers the tests start be asked. Every one of them is on
// the loopback address over plain http, which is what the rule on every address
// turns away; the rule itself still decides every address that is not one of
// those, so an address a test puts in a release document meets the real thing.
func TestMain(m *testing.M) {
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}

	checkTarget = allowLoopback

	os.Exit(m.Run())
}

// allowLoopback is releaseTarget with the addresses of the tests' own servers
// let through.
func allowLoopback(raw string) error {
	parsed, err := url.Parse(raw)
	if err == nil {
		addr := net.ParseIP(parsed.Hostname())
		if addr != nil && addr.IsLoopback() {
			return nil
		}
	}

	return releaseTarget(raw)
}

// onlyThisServer narrows the allowance above to the one server handed in, for
// the length of the test that calls it. Another server started beside it is
// then an address the real rule decides on, which is what a release document
// naming somewhere else looks like from inside Fetch.
func onlyThisServer(t *testing.T, serverURL string) {
	t.Helper()

	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("read the address of the test server: %v", err)
	}

	allowed := parsed.Host
	restore := checkTarget

	checkTarget = func(raw string) error {
		asked, err := url.Parse(raw)
		if err == nil && asked.Host == allowed {
			return nil
		}

		return releaseTarget(raw)
	}

	t.Cleanup(func() { checkTarget = restore })
}

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

// TestFetchFallsBackWhenTheReleaseHasNoChecksumFile is a release published
// before the build wrote SHA256SUMS beside the binaries. Nothing there says
// what the download should hash to, so what arrives cannot be told from what
// was published and the install takes the running executable instead.
func TestFetchFallsBackWhenTheReleaseHasNoChecksumFile(t *testing.T) {
	name := platformAsset(t)

	url := fakeRelease(t, "v3.5.1", map[string][]byte{name: testBinary})

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, url)
	if err != nil {
		t.Fatalf("a release without checksums is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if fetched.Verified {
		t.Error("nothing was checked, so Verified should be false")
	}

	if fetched.Tag != "" {
		t.Errorf("tag = %q, want empty for the running executable", fetched.Tag)
	}

	if !strings.Contains(fetched.Why, sha256SumsAsset) {
		t.Errorf("Why should name the file the release is missing, got %q", fetched.Why)
	}

	running, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	if fetched.Path != running {
		t.Errorf("path = %q, want the running executable %q", fetched.Path, running)
	}

	// The binary is not fetched at all here. A file nothing can check is not one
	// to leave in the staging directory, where a later step or a later operator
	// could take it for the release.
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("the release was downloaded although there was no checksum for it: %v", left[0].Name())
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

// TestFetchRetriesAServerThatIsBusy is the install run right after a release:
// the API names the file and the CDN that serves it answers 504 until it has
// caught up. The install has to wait rather than fall back, or the operator is
// told it worked and is left on the version they started with.
func TestFetchRetriesAServerThatIsBusy(t *testing.T) {
	name := platformAsset(t)
	sums := sha256Line(testBinary, name)

	var tries atomic.Int32

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	info := releaseInfo{TagName: "v9.9.9", Assets: []releaseAsset{
		{Name: name, BrowserDownloadURL: server.URL + "/download/" + name},
		{Name: sha256SumsAsset, BrowserDownloadURL: server.URL + "/download/" + sha256SumsAsset},
	}}

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(info)
	})

	mux.HandleFunc("/download/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(testBinary)
	})

	// The checksums are what the CDN is behind on. The first two asks get the
	// status it gives for a file it has not been handed yet.
	mux.HandleFunc("/download/"+sha256SumsAsset, func(w http.ResponseWriter, _ *http.Request) {
		if tries.Add(1) <= 2 {
			http.Error(w, "gateway time-out", http.StatusGatewayTimeout)

			return
		}

		_, _ = w.Write(sums)
	})

	fetched, err := fetchFrom(context.Background(), t.TempDir(), server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("a server that came back is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRelease {
		t.Fatalf("source = %v, want SourceRelease. Why: %s", fetched.Source, fetched.Why)
	}

	if !fetched.Verified {
		t.Error("the checksums arrived on the third ask, so the binary should be verified")
	}

	if got := tries.Load(); got != 3 {
		t.Errorf("the checksums were asked for %d times, want 3", got)
	}
}

// TestFetchFallsBackWhenAServerStaysBusy is the same server that never catches
// up. The retries run out and the install takes the running executable, which
// is what it did before there were any retries.
func TestFetchFallsBackWhenAServerStaysBusy(t *testing.T) {
	var tries atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		http.Error(w, "gateway time-out", http.StatusGatewayTimeout)
	}))
	t.Cleanup(server.Close)

	fetched, err := fetchFrom(context.Background(), t.TempDir(), server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("a server that stays down is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Errorf("source = %v, want SourceRunning", fetched.Source)
	}

	if !strings.Contains(fetched.Why, "504") {
		t.Errorf("Why should carry what the server said, got %q", fetched.Why)
	}

	if want := int32(len(retryWaits) + 1); tries.Load() != want {
		t.Errorf("the server was asked %d times, want %d", tries.Load(), want)
	}
}

// TestFetchDoesNotRetryARefusal is the rate limit. Asking again says the same
// thing, so the install must not spend the waits on it.
func TestFetchDoesNotRetryARefusal(t *testing.T) {
	var tries atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		http.Error(w, "rate limit exceeded", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	fetched, err := fetchFrom(context.Background(), t.TempDir(), server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("an API that refuses is not an error: %v", err)
	}
	defer fetched.Close()

	if tries.Load() != 1 {
		t.Errorf("the API was asked %d times, want 1", tries.Load())
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

// checkOneRedirect runs the redirect policy over one hop to raw, with hops
// requests already made before it.
//
// It takes the policy off newFetchClient rather than calling checkRedirect,
// because what matters is what the client Fetch builds does with a redirect
// and not only what the function would say if it were asked.
func checkOneRedirect(t *testing.T, raw string, hops int) error {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("build a request for %q: %v", raw, err)
	}

	via := make([]*http.Request, hops)
	for i := range via {
		via[i] = req
	}

	policy := newFetchClient().CheckRedirect
	if policy == nil {
		t.Fatal("the client has no redirect policy, so it follows anything ten times")
	}

	return policy(req, via)
}

// TestCheckRedirectTakesOnlyGitHubOverHTTPS is the list of hosts a download may
// be sent to. A redirect is the one place where something other than this code
// picks the address, so a hop to plain http or off GitHub has to stop there.
func TestCheckRedirectTakesOnlyGitHubOverHTTPS(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string // the part of the refusal to look for, empty when it is allowed
	}{
		{"the API", "https://api.github.com/repos/jollaman999/tunnel-manager/releases/latest", ""},
		{"a release download", "https://github.com/jollaman999/tunnel-manager/releases/download/v3.6.1/SHA256SUMS", ""},
		{"the asset host of today", "https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sig=x", ""},
		{"the asset host of before", "https://objects.githubusercontent.com/github-production-release-asset/1/2", ""},
		{"the domain itself", "https://github.com/", ""},
		{"a host spelled in capitals", "https://GITHUB.COM/jollaman999/tunnel-manager", ""},
		{"a host with the port written out", "https://github.com:443/jollaman999/tunnel-manager", ""},

		{"plain http on GitHub", "http://github.com/jollaman999/tunnel-manager", "not https"},
		{"plain http on the asset host", "http://objects.githubusercontent.com/1/2", "not https"},
		{"a scheme that is not the web at all", "file:///etc/passwd", "not https"},
		{"somewhere else entirely", "https://example.com/tunnel-manager-linux-amd64", "not a GitHub host"},
		{"a domain that ends in the same letters", "https://notgithub.com/tunnel-manager-linux-amd64", "not a GitHub host"},
		{"a domain that starts with one of ours", "https://github.com.example.net/tunnel-manager-linux-amd64", "not a GitHub host"},
		{"the same letters with no dot between", "https://evilgithubusercontent.com/1/2", "not a GitHub host"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkOneRedirect(t, c.url, 1)

			if c.want == "" {
				if err != nil {
					t.Fatalf("a redirect to %s is how a release is served, and it was refused: %v", c.url, err)
				}

				return
			}

			if err == nil {
				t.Fatalf("a redirect to %s was followed", c.url)
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal says %q, want it to say %q", err, c.want)
			}
		})
	}
}

// TestCheckRedirectKeepsTheQueryOutOfItsMessage guards the signature. The
// address of a release asset carries what authorises the read, and the reason
// a fetch fell back is printed by the install.
func TestCheckRedirectKeepsTheQueryOutOfItsMessage(t *testing.T) {
	err := checkOneRedirect(t, "https://example.com/1/2?sig=asignaturenobodyshouldsee", 1)
	if err == nil {
		t.Fatal("a redirect off GitHub was followed")
	}

	if strings.Contains(err.Error(), "asignaturenobodyshouldsee") {
		t.Errorf("the refusal puts the query of the address in its message: %v", err)
	}
}

// TestCheckRedirectStopsAfterTheHopsAreSpent is the loop: two addresses that
// point at each other, both of them ones this would otherwise follow.
func TestCheckRedirectStopsAfterTheHopsAreSpent(t *testing.T) {
	const url = "https://github.com/jollaman999/tunnel-manager"

	err := checkOneRedirect(t, url, maxRedirects)
	if err != nil {
		t.Fatalf("%d hops are within the limit and were refused: %v", maxRedirects, err)
	}

	err = checkOneRedirect(t, url, maxRedirects+1)
	if err == nil {
		t.Fatalf("a request that has already been redirected %d times was sent on again", maxRedirects+1)
	}

	if !strings.Contains(err.Error(), "redirected more than") {
		t.Errorf("the refusal says %q, want it to say the hops are spent", err)
	}
}

// TestFetchFallsBackWhenTheReleaseRedirectsOffHTTPS drives the policy the way a
// real download meets it: the release names an address, that address answers
// with another one, and the client is what has to refuse. The fake release runs
// on plain http, which is what every hop here is, so the refusal is the scheme.
func TestFetchFallsBackWhenTheReleaseRedirectsOffHTTPS(t *testing.T) {
	name := platformAsset(t)

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sha256Line(testBinary, name))
	}))
	t.Cleanup(elsewhere.Close)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	info := releaseInfo{TagName: "v9.9.9", Assets: []releaseAsset{
		{Name: name, BrowserDownloadURL: server.URL + "/download/" + name},
		{Name: sha256SumsAsset, BrowserDownloadURL: server.URL + "/download/" + sha256SumsAsset},
	}}

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(info)
	})

	mux.HandleFunc("/download/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(testBinary)
	})

	mux.HandleFunc("/download/"+sha256SumsAsset, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/"+sha256SumsAsset, http.StatusFound)
	})

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("a redirect that was refused is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if !strings.Contains(fetched.Why, "not https") {
		t.Errorf("Why should say the redirect was refused, got %q", fetched.Why)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("the release was downloaded although its checksums never arrived: %v", left[0].Name())
	}
}

// TestFetchFallsBackWhenTheReleaseNamesAPlainHTTPAsset is the release document
// that names an address the file would be fetched from in the clear. The
// addresses of the assets are fields of that document, so a document that can
// be answered with is one that picks where the install downloads from, and a
// plain http fetch is one anything on the way past can rewrite.
//
// What the test holds is that nothing was asked of that address at all. A
// refusal that came after the answer would already have made the request.
func TestFetchFallsBackWhenTheReleaseNamesAPlainHTTPAsset(t *testing.T) {
	name := platformAsset(t)

	var asked atomic.Int32

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write(sha256Line(testBinary, name))
	}))
	t.Cleanup(elsewhere.Close)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	info := releaseInfo{TagName: "v9.9.9", Assets: []releaseAsset{
		{Name: name, BrowserDownloadURL: elsewhere.URL + "/download/" + name},
		{Name: sha256SumsAsset, BrowserDownloadURL: elsewhere.URL + "/download/" + sha256SumsAsset},
	}}

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(info)
	})

	onlyThisServer(t, server.URL)

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("an address that was refused is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if !strings.Contains(fetched.Why, "not https") {
		t.Errorf("Why should say the address was refused, got %q", fetched.Why)
	}

	if got := asked.Load(); got != 0 {
		t.Errorf("the address the release named was asked %d times, want 0", got)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("something was downloaded from an address that may not be asked: %v", left[0].Name())
	}
}

// TestFetchFallsBackWhenTheReleaseNamesAnAssetOffGitHub is the same document
// naming a host nobody published, with the checksums served from where they
// belong so that the download is reached at all. The file it would name is one
// the checksums of that same document would agree with, which is why the
// address has to be refused rather than what arrives from it checked.
func TestFetchFallsBackWhenTheReleaseNamesAnAssetOffGitHub(t *testing.T) {
	name := platformAsset(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	info := releaseInfo{TagName: "v9.9.9", Assets: []releaseAsset{
		{Name: name, BrowserDownloadURL: "https://example.com/download/" + name},
		{Name: sha256SumsAsset, BrowserDownloadURL: server.URL + "/download/" + sha256SumsAsset},
	}}

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(info)
	})

	mux.HandleFunc("/download/"+sha256SumsAsset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sha256Line(testBinary, name))
	})

	onlyThisServer(t, server.URL)

	dir := t.TempDir()

	fetched, err := fetchFrom(context.Background(), dir, server.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("an address that was refused is not an error: %v", err)
	}
	defer fetched.Close()

	if fetched.Source != SourceRunning {
		t.Fatalf("source = %v, want SourceRunning. Why: %s", fetched.Source, fetched.Why)
	}

	if !strings.Contains(fetched.Why, "not a GitHub host") {
		t.Errorf("Why should say the address was refused, got %q", fetched.Why)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("something was downloaded from an address that may not be asked: %v", left[0].Name())
	}
}

// TestReleaseTargetTakesOnlyGitHubOverHTTPS is the list of addresses a request
// may start at. It runs against the rule itself and not the variable the tests
// point at their own servers, so what it holds is what an install does.
//
// The address the program is built with is in the table. A rule its own
// constant did not pass would turn every install into a fallback.
func TestReleaseTargetTakesOnlyGitHubOverHTTPS(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string // the part of the refusal to look for, empty when it is allowed
	}{
		{"the address the release is read from", latestReleaseURL, ""},
		{"a release download", "https://github.com/jollaman999/tunnel-manager/releases/download/v3.6.1/SHA256SUMS", ""},
		{"the asset host of today", "https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sig=x", ""},
		{"the asset host of before", "https://objects.githubusercontent.com/github-production-release-asset/1/2", ""},
		{"a host spelled in capitals", "https://GITHUB.COM/jollaman999/tunnel-manager", ""},
		{"a host with the port written out", "https://github.com:443/jollaman999/tunnel-manager", ""},

		{"plain http on GitHub", "http://github.com/jollaman999/tunnel-manager", "not https"},
		{"plain http on the asset host", "http://objects.githubusercontent.com/1/2", "not https"},
		{"the address of a server on this machine", "http://localhost:8080/download/SHA256SUMS", "not https"},
		{"a scheme that is not the web at all", "file:///etc/passwd", "not https"},
		{"somewhere else entirely", "https://example.com/tunnel-manager-linux-amd64", "not a GitHub host"},
		{"a domain that ends in the same letters", "https://notgithub.com/tunnel-manager-linux-amd64", "not a GitHub host"},
		{"a domain that starts with one of ours", "https://github.com.example.net/tunnel-manager-linux-amd64", "not a GitHub host"},
		{"the same letters with no dot between", "https://evilgithubusercontent.com/1/2", "not a GitHub host"},
		{"something that is not an address", "://tunnel-manager", "cannot be read"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := releaseTarget(c.url)

			if c.want == "" {
				if err != nil {
					t.Fatalf("a request to %s is how a release is read, and it was refused: %v", c.url, err)
				}

				return
			}

			if err == nil {
				t.Fatalf("a request to %s was made", c.url)
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal says %q, want it to say %q", err, c.want)
			}
		})
	}
}

// TestReleaseTargetKeepsTheQueryOutOfItsMessage guards the signature, the way
// the redirect policy does. The addresses that carry one are exactly the ones
// that come out of a release document and land here.
func TestReleaseTargetKeepsTheQueryOutOfItsMessage(t *testing.T) {
	err := releaseTarget("https://example.com/1/2?sig=asignaturenobodyshouldsee")
	if err == nil {
		t.Fatal("a request off GitHub was made")
	}

	if strings.Contains(err.Error(), "asignaturenobodyshouldsee") {
		t.Errorf("the refusal puts the query of the address in its message: %v", err)
	}
}
