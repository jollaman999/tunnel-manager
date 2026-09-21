package install

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// latestReleaseURL is where the newest release is read from. It is the public
// API and no token is sent with it: the repository is public, and a request
// that needed credentials would mean the release is not reachable the way an
// operator's machine reaches it, which is a reason to fall back rather than a
// reason to hold a secret here.
const latestReleaseURL = "https://api.github.com/repos/jollaman999/tunnel-manager/releases/latest"

// sha256SumsAsset is the name of the file the release build writes beside the
// binaries (Makefile, release target). Releases made before that target existed
// do not carry it.
const sha256SumsAsset = "SHA256SUMS"

// fetchTimeout bounds the whole of Fetch: reading the release, the checksums
// and the binary together.
//
// An install is something an operator watches, so this cannot be the several
// minutes a stalled TCP connection takes to give up on its own. Two minutes is
// far more than a fifteen megabyte download needs on any link an operator would
// install over, and short enough that a machine with no route out ends up on
// the running executable while the operator is still at the keyboard.
const fetchTimeout = 2 * time.Minute

// connectTimeout and headerTimeout make a dead network fail early instead of
// eating the whole of fetchTimeout. A host that is not there is answered by the
// dialer, and a host that accepts and then says nothing is answered by the
// header wait; neither is worth two minutes, because the fallback below is
// always available and always works.
const (
	connectTimeout = 10 * time.Second
	headerTimeout  = 20 * time.Second
)

// retryWaits is how long to wait before each retry of a request that failed in
// a way that may pass. The number of entries is the number of retries: four
// waits are five attempts in all, and the waits together are under a minute so
// that the whole of Fetch still fits in fetchTimeout beside a download.
//
// It is a variable so the tests can shrink it. Nothing else may write to it.
var retryWaits = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second}

// maxAssetBytes and maxMetaBytes cap what is read off the network. The sizes
// come from the release itself, which is not this process's to trust: without a
// cap, a body that never ends fills the disk the install is staged on. The
// binary is around fifteen megabytes and the two text files are a few kilobytes,
// so both caps are far above anything a real release carries.
const (
	maxAssetBytes = 256 << 20
	maxMetaBytes  = 4 << 20
)

// sha256HexLen is how long a SHA-256 digest is when written as hex. A line of
// SHA256SUMS is only read as a checksum line when its first field is this long.
const sha256HexLen = 2 * sha256.Size

// userAgent is sent with every request. The GitHub API refuses a request that
// carries no User-Agent, so leaving it off would turn every install into a
// fallback.
const userAgent = "tunnel-manager"

// Source says where the binary that gets installed came from.
type Source int

const (
	SourceRelease Source = iota // downloaded from the latest GitHub release
	SourceRunning               // the executable this process was started from
)

// Fetched is the binary to install and the story of where it came from, which
// the install prints so the operator knows what landed.
type Fetched struct {
	Path     string // a file on disk, ready to be copied into place
	Source   Source
	Tag      string // the release tag, empty when Source is SourceRunning
	Verified bool   // true when a SHA256SUMS entry matched
	Why      string // why the release was not used, empty when it was
	cleanup  func()
}

// Close removes anything Fetch downloaded.
func (f *Fetched) Close() {
	if f == nil || f.cleanup == nil {
		return
	}

	// The function is dropped as it runs, so a second Close does nothing. The
	// install prints its report after the copy and a deferred Close is the
	// usual way this is called, which makes two calls easy to end up with.
	cleanup := f.cleanup
	f.cleanup = nil

	cleanup()
}

// Fetch works out which binary to install.
//
// The release is the first choice and the running executable is what it falls
// back to. Everything that is a failure of the network or of the release API
// ends on the fallback and not on an error: the operator asked for an install,
// the process holds a binary that works, and refusing to install because GitHub
// could not be reached would be a worse answer than installing what is running.
//
// A checksum that does not match is the one case that is an error. There the
// release was read and the file it named hashes to something else, and putting
// that file in place would be installing content nobody published.
//
// dir is where a downloaded file is put. Close removes what was written into
// it; the directory itself belongs to the caller that made it.
func Fetch(ctx context.Context, dir string) (*Fetched, error) {
	return fetchFrom(ctx, dir, latestReleaseURL)
}

// fetchFrom is Fetch with the API address handed in, so the tests run against
// an httptest server. Nothing else may point this anywhere but the constant:
// the address decides what gets installed.
func fetchFrom(ctx context.Context, dir string, latestURL string) (*Fetched, error) {
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
	var want string

	if sums := latest.asset(sha256SumsAsset); sums != nil {
		body, err := readBody(ctx, client, sums.BrowserDownloadURL, maxMetaBytes)
		if err != nil {
			return runningExecutable(fmt.Sprintf("the %s of release %s could not be read: %v", sha256SumsAsset, latest.TagName, err))
		}

		want = sumFor(body, name)
		if want == "" {
			// The release publishes checksums and this file is not among them.
			// There is nothing to check it against, and a release that says what
			// its files hash to is not one to take an unlisted file from, so this
			// goes to the executable that is already running rather than to an
			// error: falling back installs known content either way.
			return runningExecutable(fmt.Sprintf("the %s of release %s lists no checksum for %s", sha256SumsAsset, latest.TagName, name))
		}
	}

	path := filepath.Join(dir, name)

	got, err := downloadAsset(ctx, client, binary.BrowserDownloadURL, path)
	if err != nil {
		_ = os.Remove(path)

		return runningExecutable(fmt.Sprintf("%s of release %s could not be downloaded: %v", name, latest.TagName, err))
	}

	if want != "" && !strings.EqualFold(got, want) {
		// The half-written file goes now. It is content nobody vouched for, and
		// leaving it in the staging directory is leaving something for a later
		// step, or a later operator, to pick up by mistake.
		_ = os.Remove(path)

		return nil, fmt.Errorf("%s of release %s hashes to %s and its %s entry says %s",
			name, latest.TagName, got, sha256SumsAsset, want)
	}

	return &Fetched{
		Path:     path,
		Source:   SourceRelease,
		Tag:      latest.TagName,
		Verified: want != "",
		cleanup:  func() { _ = os.Remove(path) },
	}, nil
}

// runningExecutable is the fallback: install the file this process was started
// from. why travels with it so the install can print the one sentence that says
// what the operator is getting instead of the release.
func runningExecutable(why string) (*Fetched, error) {
	path, err := os.Executable()
	if err != nil {
		// Both sources are gone at this point, so this is the one place the
		// fallback itself reports a failure. The reason the release was not used
		// goes into the message, because it is the part that says what to fix.
		return nil, fmt.Errorf("the release was not used (%s) and this process cannot name its own executable either: %w", why, err)
	}

	// Nothing was downloaded, so there is nothing for Close to remove, and the
	// path is the running process's own image: a cleanup here would delete it.
	return &Fetched{
		Path:   path,
		Source: SourceRunning,
		Why:    why,
	}, nil
}

// assetName is what the release calls the binary for one platform. The names
// are the ones the release build writes (Makefile, RELEASE_PLATFORMS), and a
// platform that is not built for has no name, which sends Fetch to the running
// executable instead of to a download that would 404.
//
// The platform is handed in rather than read from runtime here, so that the
// mapping can be checked for platforms the tests do not run on.
func assetName(goos string, goarch string) string {
	const base = "tunnel-manager"

	switch {
	case goos == "linux" && goarch == "amd64":
		return base + "-linux-amd64"
	case goos == "linux" && goarch == "arm64":
		return base + "-linux-arm64"
	case goos == "darwin" && goarch == "amd64":
		return base + "-darwin-amd64"
	case goos == "darwin" && goarch == "arm64":
		return base + "-darwin-arm64"
	case goos == "windows" && goarch == "amd64":
		return base + "-windows-amd64.exe"
	default:
		return ""
	}
}

// releaseInfo is the part of the release API answer this needs. Everything else
// in that document is left out on purpose: a field that is not read is a field
// that cannot change what gets installed.
type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// asset finds one file of the release by name, or nil when the release does not
// carry it.
func (r *releaseInfo) asset(name string) *releaseAsset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}

	return nil
}

// latestRelease reads the newest release.
func latestRelease(ctx context.Context, client *http.Client, url string) (*releaseInfo, error) {
	body, err := readBody(ctx, client, url, maxMetaBytes)
	if err != nil {
		return nil, err
	}

	var info releaseInfo

	err = json.Unmarshal(body, &info)
	if err != nil {
		return nil, fmt.Errorf("the answer is not a release: %w", err)
	}

	if info.TagName == "" {
		// An answer that parses but names no release is not one to act on. It is
		// what an API that changed shape, or a proxy that answered instead of
		// GitHub, looks like from here.
		return nil, fmt.Errorf("the answer names no release")
	}

	return &info, nil
}

// downloadAsset writes url into path and returns what the bytes hash to.
//
// The hash is taken from the same pass that writes the file, so what is
// reported is what landed on disk and not what a second read of it says.
func downloadAsset(ctx context.Context, client *http.Client, url string, path string) (string, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// The mode is set at creation and again below. Creation goes through the
	// umask, which on a machine with a strict one would leave the file without
	// the bits the install needs to run what it copied.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}

	digest := sha256.New()

	err = copyInto(file, digest, resp.Body)

	closeErr := file.Close()

	if err != nil {
		return "", err
	}

	if closeErr != nil {
		// A write that only fails at close is a short file, and a short file
		// would be caught by the checksum on a verified release and silently
		// installed on one without checksums.
		return "", closeErr
	}

	if runtime.GOOS != "windows" {
		// Windows has no execute bit, and the release binary for it is named
		// .exe, which is what makes it runnable there.
		err = os.Chmod(path, 0o755)
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// copyInto writes the body into the file and the hash at once, refusing a body
// that runs past the cap.
func copyInto(file io.Writer, digest hash.Hash, body io.Reader) error {
	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, maxAssetBytes+1))
	if err != nil {
		return err
	}

	if written > maxAssetBytes {
		return fmt.Errorf("the download is larger than %d bytes", int64(maxAssetBytes))
	}

	return nil
}

// readBody reads a whole small answer, refusing one that runs past limit.
func readBody(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}

	if int64(len(body)) > limit {
		return nil, fmt.Errorf("the answer is larger than %d bytes", limit)
	}

	return body, nil
}

// get asks for a URL until it gets an answer that said 200, or until the last
// try is spent. The caller closes the body.
//
// It retries because a release that was published a moment ago is not yet on
// the CDN that serves its files, which answers 502 or 504 until it is. That is
// the shape of an install run right after a release: the API already names the
// files and one of them cannot be fetched yet. Waiting a few seconds is a
// better answer than falling back to the running executable, which leaves the
// operator told the install worked and holding the version they started with.
//
// A refusal is not retried. A rate limit answers 403 and a repository with no
// release answers 404, and asking those again says the same thing.
func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; ; attempt++ {
		resp, err := getOnce(ctx, client, url)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		if attempt >= len(retryWaits) || !worthRetrying(err) {
			return nil, lastErr
		}

		// A context that is already spent has no time for another try, and the
		// wait itself must end with it rather than hold the install past the
		// deadline the caller set.
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(retryWaits[attempt]):
		}
	}
}

// retryableError marks a failure that asking again may get past.
type retryableError struct {
	err error
}

func (e *retryableError) Error() string { return e.err.Error() }

func (e *retryableError) Unwrap() error { return e.err }

// worthRetrying says whether get should ask again.
func worthRetrying(err error) bool {
	var retryable *retryableError

	return errors.As(err, &retryable)
}

// getOnce performs one request and hands back an answer that said 200.
func getOnce(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		// A connection that was refused, reset or timed out says nothing about
		// whether the file is there, so it is one to ask about again. A context
		// that ended is not: the time this was given is gone.
		if ctx.Err() != nil {
			return nil, err
		}

		return nil, &retryableError{err: err}
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		// The status is the whole of the message.
		statusErr := fmt.Errorf("%s answered %s", url, resp.Status)

		// A server that says it is overloaded or that it could not reach the
		// one behind it is describing a moment, not the file.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			return nil, &retryableError{err: statusErr}
		}

		return nil, statusErr
	}

	return resp, nil
}

// sumFor pulls the checksum of one file out of a SHA256SUMS body.
//
// The format is what sha256sum writes: the digest, two spaces, the name. A
// name may carry the * that the binary mode of sha256sum puts in front of it,
// and the names in a release carry no directory, so only the last element is
// compared.
func sumFor(sums []byte, name string) string {
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

// newFetchClient is the client every request here goes through.
//
// It is built rather than http.DefaultClient because the default has no timeout
// at all: a connection that is accepted and then left silent would hold the
// install until the operator gave up on it.
func newFetchClient() *http.Client {
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: connectTimeout}).DialContext,
			TLSHandshakeTimeout:   connectTimeout,
			ResponseHeaderTimeout: headerTimeout,
		},
	}
}
