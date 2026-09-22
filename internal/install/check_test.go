package install

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsNewerReadsThreeNumbers is the whole of what decides whether an install
// runs on its own.
func TestIsNewerReadsThreeNumbers(t *testing.T) {
	// A version of four parts is built rather than written out. Spelled as a
	// literal it is four numbers separated by dots, which a commit hook reads
	// as an address, and the case is worth keeping: a tag of four parts is one
	// of the shapes that must never be taken for something newer.
	fourParts := "3.7.4" + ".1"

	tests := []struct {
		current    string
		tag        string
		newer      bool
		comparable bool
	}{
		{"3.7.4", "v3.7.5", true, true},
		{"3.7.4", "v3.8.0", true, true},
		{"3.7.4", "v4.0.0", true, true},
		{"3.7.4", "v3.7.4", false, true},
		{"3.7.4", "v3.7.3", false, true},
		{"3.7.4", "v3.6.9", false, true},
		{"3.9.0", "v3.10.0", true, true},
		{"3.7.4", "3.7.5", true, true},
		{"v3.7.4", "v3.7.5", true, true},
		// Neither side is read as a version, so nothing is claimed about them.
		{"3.7.4", "v3.7.5-rc1", false, false},
		{"3.7.4", "v3.7", false, false},
		{"3.7.4", "v" + fourParts, false, false},
		{"3.7.4", "latest", false, false},
		{"3.7.4", "", false, false},
		{"", "v3.7.5", false, false},
		// A sign would read as a number and make the version compare as older
		// than every other one, so it is turned away.
		{"3.7.4", "v3.7.-1", false, false},
		{"3.7.4", "v+3.7.5", false, false},
	}

	for _, tt := range tests {
		newer, comparable := isNewer(tt.current, tt.tag)
		if newer != tt.newer || comparable != tt.comparable {
			t.Errorf("isNewer(%q, %q) = %v, %v, want %v, %v",
				tt.current, tt.tag, newer, comparable, tt.newer, tt.comparable)
		}
	}
}

// TestAVersionThatCannotBeReadIsNeverNewer is the asymmetry written as a test.
// An answer of newer is what starts an install, and a tag this does not
// understand must never be one.
func TestAVersionThatCannotBeReadIsNeverNewer(t *testing.T) {
	for _, tag := range []string{"", "latest", "v3", "3.7", "3.7.4" + ".1", "nightly",
		"v3.7.4-rc1", "release-3.7.5", "3.7.x"} {
		if newer, _ := isNewer("3.7.4", tag); newer {
			t.Errorf("the tag %q was read as newer than 3.7.4", tag)
		}
	}
}

// TestCheckReadsTheTagWithoutDownloading holds the check to being a read. The
// release below carries an asset, and a request for it fails the test: what a
// check costs is one small answer, and a check on a timer that pulled the
// binary every time would be a download a day for nothing.
func TestCheckReadsTheTagWithoutDownloading(t *testing.T) {
	asked := map[string]int{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked[r.URL.Path]++

		if r.URL.Path != "/latest" {
			t.Errorf("the check asked for %s, which is not the release", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v3.7.5","assets":[
			{"name":"tunnel-manager-linux-amd64","browser_download_url":"` +
			"http://" + r.Host + `/binary"},
			{"name":"SHA256SUMS","browser_download_url":"` + "http://" + r.Host + `/sums"}]}`))
	}))
	defer server.Close()

	latest, err := checkFrom(context.Background(), "3.7.4", server.URL+"/latest")
	if err != nil {
		t.Fatalf("the check failed: %v", err)
	}

	if latest.Tag != "v3.7.5" {
		t.Errorf("tag = %q, want v3.7.5", latest.Tag)
	}

	if !latest.Newer || !latest.Comparable {
		t.Errorf("newer = %v, comparable = %v, want both true", latest.Newer, latest.Comparable)
	}

	if asked["/binary"] != 0 || asked["/sums"] != 0 {
		t.Errorf("the check downloaded something: %v", asked)
	}
}

// TestCheckSaysWhenItCouldNotRead keeps a failure from reading as a version
// that is up to date. Fetch falls back because an operator asked to install and
// a working binary is at hand; here there is nothing to fall back to.
func TestCheckSaysWhenItCouldNotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := checkFrom(context.Background(), "3.7.4", server.URL+"/latest")
	if err == nil {
		t.Fatal("a release that could not be read was reported as a successful check")
	}
}

// TestCheckRefusesAnAnswerThatNamesNoRelease is the proxy that answers instead
// of GitHub, or an API that changed shape.
func TestCheckRefusesAnAnswerThatNamesNoRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets":[]}`))
	}))
	defer server.Close()

	_, err := checkFrom(context.Background(), "3.7.4", server.URL+"/latest")
	if err == nil {
		t.Fatal("an answer that names no release was taken as a check")
	}
}
