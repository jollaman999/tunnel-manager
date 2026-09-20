package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// newServer returns an echo instance with nothing but the UI on it. No request
// made against it carries a cookie, so every answer it gives is an answer
// given without a session.
func newServer() *echo.Echo {
	e := echo.New()
	RegisterRoutes(e, "test-version")

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
	for _, code := range catalogCodes {
		want = append(want, "static/lang/"+code+".json")
	}

	sort.Strings(want)

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
		{target: "/ui/lang/en.json", contentType: "application/json"},
		{target: "/ui/lang/pt-BR.json", contentType: "application/json"},
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

	for _, target := range []string{"/ui/login", "/ui/setup", "/ui/hosts", "/ui/service-ports",
		"/ui/logs", "/ui/settings", "/ui/uninstalled"} {
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
	RegisterRoutes(e, "test-version")

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

// TestAssetsCarryATagAndAskToBeChecked pins down that a built-in file goes out
// with an entity tag and with a header asking that the tag be checked.
//
// Neither was there. The answer carried a content type and nothing else, which
// leaves a browser to decide for itself how long the file stays good, and what
// it decides is its own business. A phone went on running the scripts of an
// earlier release after a deployment, so a fix that was measured on the server
// had no effect on the screen and looked like a fix that did not work.
func TestAssetsCarryATagAndAskToBeChecked(t *testing.T) {
	e := newServer()

	for _, name := range []string{"/ui/app.js", "/ui/screens.js", "/ui/style.css", "/ui/"} {
		t.Run(name, func(t *testing.T) {
			rec := get(e, name)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			tag := rec.Header().Get("ETag")
			if tag == "" {
				t.Fatal("the answer carries no ETag, so a browser has nothing to ask about")
			}
			if cache := rec.Header().Get("Cache-Control"); cache != "no-cache" {
				t.Fatalf("Cache-Control is %q, want %q", cache, "no-cache")
			}

			// The same file asked for again with the tag it was given is a file
			// the browser already holds.
			req := httptest.NewRequest(http.MethodGet, name, nil)
			req.Header.Set("If-None-Match", tag)

			again := httptest.NewRecorder()
			e.ServeHTTP(again, req)

			if again.Code != http.StatusNotModified {
				t.Fatalf("asking again with the tag answered %d, want %d",
					again.Code, http.StatusNotModified)
			}
			if again.Body.Len() != 0 {
				t.Fatalf("the answer that says nothing changed carried %d bytes of body",
					again.Body.Len())
			}
		})
	}
}

// TestTwoAssetsDoNotShareATag is what makes the tag worth having. A tag that is
// the same for every file would have every file answered as unchanged once any
// one of them had been fetched.
func TestTwoAssetsDoNotShareATag(t *testing.T) {
	e := newServer()

	seen := map[string]string{}

	for _, name := range []string{"/ui/app.js", "/ui/screens.js", "/ui/style.css"} {
		tag := get(e, name).Header().Get("ETag")

		if was, ok := seen[tag]; ok {
			t.Fatalf("%s carries the tag of %s", name, was)
		}

		seen[tag] = name
	}
}

// catalogCodes is the thirteen languages the UI is drawn in, in the order
// app.js lists them. It is written out here rather than read from app.js so
// that the tests below have something to compare that file against: a list
// taken from the file it is checking would agree with it whatever it said.
var catalogCodes = []string{"en", "ko", "ja", "zh", "es", "fr", "de", "pt-BR", "ru", "ar", "hi", "vi", "th"}

// baseCatalog is the language every other one falls back to, key by key. It has
// to be complete, and the tests below are what hold it to that.
const baseCatalog = "en"

// catalogKey is the shape a key has to have: an area, at least one part naming
// the thing, and a role at the end saying what kind of string it is.
//
// The role is a closed list on purpose. Eight hundred keys written by six
// different pieces of work will only stay findable if "the word on a button"
// is always spelled the same way, and a name that has to end in one of
// thirteen words cannot drift into ".btn" in one file and ".buttonText" in
// another.
var catalogKey = regexp.MustCompile(
	`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*(?:\.[a-z][a-z0-9]*(?:-[a-z0-9]+)*)+` +
		`\.(?:title|label|hint|button|link|column|option|empty|notice|confirm|error|aria|text)$`)

// readCatalog reads one language file and hands back what is in it. A catalog
// is flat, so an entry that is not a string is a file that has been edited into
// a shape the UI cannot read, and that is reported here rather than as a blank
// on a screen.
func readCatalog(t *testing.T, code string) map[string]string {
	t.Helper()

	body, err := fs.ReadFile(staticFS, "static/lang/"+code+".json")
	if err != nil {
		t.Fatalf("failed to read the %s catalog: %v", code, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the %s catalog is not a flat object: %v", code, err)
	}

	table := make(map[string]string, len(raw))

	for key, value := range raw {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			t.Fatalf("%s catalog: %q is not a string but %s", code, key, value)
		}

		table[key] = text
	}

	return table
}

// readStatic reads one UI file as text.
func readStatic(t *testing.T, name string) string {
	t.Helper()

	body, err := fs.ReadFile(staticFS, "static/"+name)
	if err != nil {
		t.Fatalf("failed to read %s: %v", name, err)
	}

	return string(body)
}

// TestEveryLanguageHasACatalog is what makes the list in the corner of the
// screen true. A code that is offered with no file behind it is a pick that
// draws the page in English and says it is in something else.
func TestEveryLanguageHasACatalog(t *testing.T) {
	e := newServer()

	for _, code := range catalogCodes {
		target := "/ui/lang/" + code + ".json"

		rec := get(e, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", target, rec.Code, http.StatusOK)
		}

		if len(readCatalog(t, code)) == 0 {
			t.Fatalf("the %s catalog holds nothing", code)
		}
	}
}

// TestThePageAndTheScriptAgreeOnTheLanguages compares the two lists of codes.
//
// index.html settles the language in the head, before app.js has been fetched,
// so it holds the list a second time. That is the one duplication this
// arrangement costs, and this is what keeps it from costing anything: a
// language added to one file and not the other fails here instead of being a
// code the page picks and the script has never heard of.
func TestThePageAndTheScriptAgreeOnTheLanguages(t *testing.T) {
	page := readStatic(t, indexFile)
	script := readStatic(t, "app.js")

	// The page: var codes = [...] and var rightToLeft = [...].
	pageList := func(name string) []string {
		found := regexp.MustCompile(`var ` + name + ` = \[([^\]]*)\]`).FindStringSubmatch(page)
		if found == nil {
			t.Fatalf("%s names no %s", indexFile, name)
		}

		return regexp.MustCompile(`"([^"]+)"`).FindAllString(found[1], -1)
	}

	// The script: one line per language, each carrying its code and whether it
	// is written right to left.
	entries := regexp.MustCompile(`\{ code: "([^"]+)", name: "[^"]*", rtl: (true|false) \}`).
		FindAllStringSubmatch(script, -1)
	if len(entries) == 0 {
		t.Fatalf("app.js lists no languages in the shape this test reads")
	}

	var codes, rightToLeft []string

	for _, entry := range entries {
		codes = append(codes, `"`+entry[1]+`"`)

		if entry[2] == "true" {
			rightToLeft = append(rightToLeft, `"`+entry[1]+`"`)
		}
	}

	t.Logf("app.js codes: %v", codes)
	t.Logf("app.js right to left: %v", rightToLeft)

	if got, want := strings.Join(pageList("codes"), " "), strings.Join(codes, " "); got != want {
		t.Fatalf("%s codes = %s, app.js = %s", indexFile, got, want)
	}

	if got, want := strings.Join(pageList("rightToLeft"), " "), strings.Join(rightToLeft, " "); got != want {
		t.Fatalf("%s right to left = %s, app.js = %s", indexFile, got, want)
	}

	// And both of them against what this file says the languages are, so that a
	// language dropped from both at once is still caught.
	var want []string
	for _, code := range catalogCodes {
		want = append(want, `"`+code+`"`)
	}

	if got := strings.Join(codes, " "); got != strings.Join(want, " ") {
		t.Fatalf("app.js codes = %s, want %s", got, strings.Join(want, " "))
	}
}

// TestCatalogKeysFollowTheNamingRule holds every catalog to one way of naming a
// string. There are eight hundred of them to come, written by several separate
// pieces of work, and a key that cannot be guessed at from what it names is a
// key that gets written a second time under another name.
func TestCatalogKeysFollowTheNamingRule(t *testing.T) {
	for _, code := range catalogCodes {
		for key := range readCatalog(t, code) {
			if !catalogKey.MatchString(key) {
				t.Errorf("%s catalog: %q does not read as <area>.<thing>.<role>", code, key)
			}
		}
	}
}

// TestEveryCatalogFallsBackToEnglish is the other half of the fallback. A key
// the chosen language is missing is drawn in English, which only works while
// English is the one catalog that is never missing anything: a key in a
// translation that English has never heard of is a typo, and it would be drawn
// as the name of the key.
func TestEveryCatalogFallsBackToEnglish(t *testing.T) {
	base := readCatalog(t, baseCatalog)

	for _, code := range catalogCodes {
		if code == baseCatalog {
			continue
		}

		for key := range readCatalog(t, code) {
			if _, ok := base[key]; !ok {
				t.Errorf("%s catalog: %q is in no English catalog to fall back to", code, key)
			}
		}
	}
}

// TestEveryKeyTheUIAsksForIsInEnglish is what keeps the name of a key off the
// screen. t hands back the key it was given where neither catalog has it, which
// is on purpose and is meant never to happen: this is what makes sure of it,
// for every t("...") in the scripts.
func TestEveryKeyTheUIAsksForIsInEnglish(t *testing.T) {
	base := readCatalog(t, baseCatalog)
	asked := regexp.MustCompile(`\bt\("([^"]+)"`)

	var seen int

	for _, name := range []string{"app.js", "screens.js"} {
		for _, use := range asked.FindAllStringSubmatch(readStatic(t, name), -1) {
			seen++

			if _, ok := base[use[1]]; !ok {
				t.Errorf("%s asks for %q, which the English catalog has not got", name, use[1])
			}
		}
	}

	t.Logf("keys asked for in the scripts: %d", seen)

	if seen == 0 {
		t.Fatalf("the scripts ask for no key at all, so this test checks nothing")
	}
}

// TestCatalogValuesCarryNoMarkup keeps a translation from being a way into the
// page. Every value is put on screen with textContent, so a pointed bracket in
// one would be drawn as a pointed bracket; this is the belt to that brace, and
// it also catches a translator who was handed a string with a tag in it and
// translated around the tag.
func TestCatalogValuesCarryNoMarkup(t *testing.T) {
	for _, code := range catalogCodes {
		for key, value := range readCatalog(t, code) {
			if strings.ContainsAny(value, "<>") {
				t.Errorf("%s catalog: %q carries markup: %q", code, key, value)
			}
		}
	}
}

// TestPlaceholdersAreNamedAndKnown holds the fill-ins of a translation to the
// ones the English string has.
//
// They are named and not numbered so that a language may put them in another
// order, which is the whole reason for naming them; the cost of that freedom is
// that a name can be misspelled, and a misspelled one would be drawn on the
// screen as a word in braces. A translation may leave one out, since not every
// language needs to repeat what is already on the screen, but it may not invent
// one.
func TestPlaceholdersAreNamedAndKnown(t *testing.T) {
	named := regexp.MustCompile(`\{([^}]*)\}`)
	plain := regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	base := readCatalog(t, baseCatalog)

	namesIn := func(value string) map[string]bool {
		names := map[string]bool{}
		for _, found := range named.FindAllStringSubmatch(value, -1) {
			names[found[1]] = true
		}

		return names
	}

	for _, code := range catalogCodes {
		for key, value := range readCatalog(t, code) {
			english, translated := base[key]

			for name := range namesIn(value) {
				if !plain.MatchString(name) {
					t.Errorf("%s catalog: %q holds {%s}, which is not a name app.js fills in", code, key, name)

					continue
				}

				if code == baseCatalog || !translated {
					continue
				}

				if !namesIn(english)[name] {
					t.Errorf("%s catalog: %q holds {%s}, which the English string has not got", code, key, name)
				}
			}
		}
	}
}

// TestTheServerAndTheUIAgreeOnTheLanguages is the fourth list held against the
// other three. The settings hold a default language for the installation, and
// the value they take has to be a code the UI is actually drawn in: one that is
// stored and has no catalog behind it draws the page in the names of its keys
// for every browser that has picked nothing, and one the UI offers and the
// server refuses is a language an operator can pick and cannot save.
//
// It sits here rather than in internal/settings because this is where the UI
// files are. They are embedded in this package, so this is the one place the
// list the server keeps, the list app.js offers, the list index.html reads
// before app.js is fetched, and the catalog files themselves can all be laid
// side by side. internal/settings knows nothing of where those files live, and
// a list read out of the thing it is checking would agree with it whatever it
// said.
func TestTheServerAndTheUIAgreeOnTheLanguages(t *testing.T) {
	served := settings.Languages()

	t.Logf("the settings take: %v", served)

	// The order is compared too, not only the membership. The UI offers the
	// languages in this order and matches a browser language against them in
	// it, so a list that holds the same codes in another order is two lists
	// that have to be read separately to be understood.
	if got, want := strings.Join(served, " "), strings.Join(catalogCodes, " "); got != want {
		t.Fatalf("the settings take %s, the UI is drawn in %s", got, want)
	}

	// And each of them against the file the words come out of, so that a code
	// added to every list at once and never translated is caught as well.
	for _, code := range served {
		if len(readCatalog(t, code)) == 0 {
			t.Errorf("the settings take %s, which no catalog holds any words for", code)
		}
	}
}
