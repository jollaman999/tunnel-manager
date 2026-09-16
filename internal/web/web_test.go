package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// newServer returns an echo instance with nothing but the UI on it. No request
// made against it carries a cookie, so every answer it gives is an answer
// given without a session.
func newServer() *echo.Echo {
	e := echo.New()
	RegisterRoutes(e)

	return e
}

// get runs a GET against e and hands back what was written. No cookie is ever
// attached: the UI has to be reachable by a client that has not logged in.
func get(e *echo.Echo, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()

	e.ServeHTTP(rec, req)

	return rec
}

// TestEmbeddedFilesArePresent lists what the binary carries. The routes below
// are only worth testing if the files they serve went into the embed in the
// first place, and a file that was renamed but not re-listed fails here rather
// than as a 404 somewhere else.
func TestEmbeddedFilesArePresent(t *testing.T) {
	var names []string

	err := fs.WalkDir(staticFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() {
			names = append(names, name)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk the embedded files: %v", err)
	}

	sort.Strings(names)
	t.Logf("embedded files: %v", names)

	want := []string{"static/app.js", "static/index.html", "static/screens.js", "static/style.css"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("embedded files = %v, want %v", names, want)
	}
}

// TestRootRedirectsToTheUI covers the one path that is not under /ui/.
func TestRootRedirectsToTheUI(t *testing.T) {
	rec := get(newServer(), "/")

	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d, want %d", rec.Code, http.StatusFound)
	}

	if location := rec.Header().Get(echo.HeaderLocation); location != uiPrefix {
		t.Fatalf("GET / Location = %q, want %q", location, uiPrefix)
	}
}

// TestUIWithoutTrailingSlashRedirects keeps /ui from being the one spelling of
// the UI that comes back empty handed.
func TestUIWithoutTrailingSlashRedirects(t *testing.T) {
	rec := get(newServer(), "/ui")

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /ui = %d, want %d", rec.Code, http.StatusFound)
	}

	if location := rec.Header().Get(echo.HeaderLocation); location != uiPrefix {
		t.Fatalf("GET /ui Location = %q, want %q", location, uiPrefix)
	}
}

// TestUIRootServesTheIndex checks the page itself, by a marker that is in the
// file and nowhere else.
func TestUIRootServesTheIndex(t *testing.T) {
	rec := get(newServer(), uiPrefix)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", uiPrefix, rec.Code, http.StatusOK)
	}

	if !strings.Contains(rec.Body.String(), "<title>Tunnel Manager</title>") {
		t.Fatalf("GET %s did not serve the index page: %q", uiPrefix, rec.Body.String())
	}

	if contentType := rec.Header().Get(echo.HeaderContentType); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("GET %s Content-Type = %q, want text/html", uiPrefix, contentType)
	}
}

// TestAssetsAreServedWithTheirType covers the two files the page pulls in. The
// type matters on its own: a stylesheet served as something else is ignored by
// the browser and a script served as something else is refused outright.
func TestAssetsAreServedWithTheirType(t *testing.T) {
	cases := []struct {
		target      string
		contentType string
	}{
		{target: "/ui/app.js", contentType: "text/javascript"},
		{target: "/ui/screens.js", contentType: "text/javascript"},
		{target: "/ui/style.css", contentType: "text/css"},
	}

	e := newServer()

	for _, tc := range cases {
		rec := get(e, tc.target)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", tc.target, rec.Code, http.StatusOK)
		}

		if rec.Body.Len() == 0 {
			t.Fatalf("GET %s served an empty body", tc.target)
		}

		contentType := rec.Header().Get(echo.HeaderContentType)
		if !strings.HasPrefix(contentType, tc.contentType) {
			t.Fatalf("GET %s Content-Type = %q, want %s", tc.target, contentType, tc.contentType)
		}
	}
}

// TestUnknownPathServesTheIndex is what makes a reload of a screen work once
// the UI routes on the URL.
func TestUnknownPathServesTheIndex(t *testing.T) {
	e := newServer()

	index := get(e, uiPrefix)
	rec := get(e, "/ui/hosts")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/hosts = %d, want %d", rec.Code, http.StatusOK)
	}

	if rec.Body.String() != index.Body.String() {
		t.Fatalf("GET /ui/hosts did not serve the same body as %s", uiPrefix)
	}
}

// TestEveryScreenPathServesTheIndex walks the paths the UI navigates to. They
// have no route of their own, so a reload or a bookmark of any of them has to
// come back as the page that reads the path and draws the screen.
func TestEveryScreenPathServesTheIndex(t *testing.T) {
	e := newServer()

	index := get(e, uiPrefix)

	for _, target := range []string{"/ui/login", "/ui/setup", "/ui/hosts", "/ui/service-ports"} {
		rec := get(e, target)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", target, rec.Code, http.StatusOK)
		}

		if rec.Body.String() != index.Body.String() {
			t.Fatalf("GET %s did not serve the same body as %s", target, uiPrefix)
		}
	}
}

// TestThePageNamesItsAssetsAbsolutely pins what makes the screens above work.
// The same HTML is served at /ui/hosts as at /ui/, so a src or href without a
// leading slash would be resolved against the screen path and asked for as
// /ui/hosts/app.js, which is not where the file is.
func TestThePageNamesItsAssetsAbsolutely(t *testing.T) {
	reference := regexp.MustCompile(`(?:src|href)="([^"]+)"`)

	page, err := fs.ReadFile(staticFS, "static/"+indexFile)
	if err != nil {
		t.Fatalf("failed to read the page: %v", err)
	}

	matches := reference.FindAllStringSubmatch(string(page), -1)
	if len(matches) == 0 {
		t.Fatalf("the page pulls in nothing, so the assets are not reachable at all")
	}

	e := newServer()

	for _, match := range matches {
		target := match[1]

		if !strings.HasPrefix(target, uiPrefix) {
			t.Fatalf("the page pulls in %q, which is not under %s", target, uiPrefix)
		}

		if rec := get(e, target); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", target, rec.Code, http.StatusOK)
		}
	}
}

// TestNothingIsFetchedFromTheNetwork keeps the UI working on a host that
// reaches this server and nothing else. A script or a stylesheet pulled from
// somewhere else would leave the screens blank there, and it would hand a third
// party the page the SSH passwords are typed into.
func TestNothingIsFetchedFromTheNetwork(t *testing.T) {
	external := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)

	err := fs.WalkDir(staticFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		body, err := fs.ReadFile(staticFS, name)
		if err != nil {
			return err
		}

		if match := external.Find(body); match != nil {
			t.Errorf("%s names something outside this server: %q", name, match)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to read the embedded files: %v", err)
	}
}

// TestMissingFileWithAnExtensionIsNotFound keeps a mistyped asset name from
// being answered with the page. A 200 full of HTML where a script was asked
// for hides the name that was wrong.
func TestMissingFileWithAnExtensionIsNotFound(t *testing.T) {
	for _, target := range []string{"/ui/nope.js", "/ui/nope.css", "/ui/sub/nope.html"} {
		rec := get(newServer(), target)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want %d", target, rec.Code, http.StatusNotFound)
		}
	}
}

// TestWalkingUpStaysInsideTheEmbeddedTree checks that a path that climbs out of
// /ui/ names nothing outside it. Such a path has no extension after cleaning in
// the case below, so it lands on the page rather than on a file.
func TestWalkingUpStaysInsideTheEmbeddedTree(t *testing.T) {
	rec := get(newServer(), "/ui/../../etc/passwd")

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "root:") {
		t.Fatalf("GET /ui/../../etc/passwd served something from outside the embedded tree")
	}
}

// TestTheAPIStaysBehindTheSession is the other half of serving the UI without
// one. The UI group being open must not open anything else, so the same server
// carries both and the API path is asked for without a cookie.
func TestTheAPIStaysBehindTheSession(t *testing.T) {
	e := echo.New()
	RegisterRoutes(e)

	// The handler is never reached and the session lookup fails before the
	// account is read, so this needs no database.
	authHandler := api.NewAuthHandler(nil, zap.NewNop(), "")

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.GET("/host", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	if rec := get(e, uiPrefix); rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", uiPrefix, rec.Code, http.StatusOK)
	}

	rec := get(e, "/api/host")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/host = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestTheStaticFilesCarryNoSecret reads everything that is embedded and looks
// for a credential written into it. These files are handed to anyone who asks,
// session or not, so a secret placed in one of them is a published secret.
//
// What is looked for is a value, not a word: a login form has to say
// "password" and a request has to name the field, and neither of those is a
// secret. A quoted literal sitting behind such a name is.
func TestTheStaticFilesCarryNoSecret(t *testing.T) {
	credentialLiteral := regexp.MustCompile("(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key)[\"'`]?\\s*[:=]+\\s*[\"'`][^\"'`]+[\"'`]")
	pemHeader := regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

	err := fs.WalkDir(staticFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		body, err := fs.ReadFile(staticFS, name)
		if err != nil {
			return err
		}

		if match := credentialLiteral.Find(body); match != nil {
			t.Errorf("%s holds what looks like a credential: %q", name, match)
		}

		if match := pemHeader.Find(body); match != nil {
			t.Errorf("%s holds a private key: %q", name, match)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to read the embedded files: %v", err)
	}
}
