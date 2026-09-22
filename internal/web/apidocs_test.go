package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// testPolicy is what the tests here register the documentation with. It stands
// in for the one main builds, which is the one the test in package main holds
// against the page; what these tests are about is that the files are served,
// that the policy the caller passes is the one that goes out, and that nothing
// on the page reaches off this origin.
const testPolicy = "default-src 'none'; test-policy"

// newDocsServer returns an echo instance carrying the UI and the
// documentation. No request made against it carries a cookie.
func newDocsServer(t *testing.T) *echo.Echo {
	t.Helper()

	e := echo.New()
	RegisterRoutes(e, "test-version")
	RegisterAPIDocs(e, testPolicy)

	return e
}

// docsFile reads one file of the documentation directory out of the embed.
func docsFile(t *testing.T, name string) []byte {
	t.Helper()

	body, err := fs.ReadFile(staticFS, path.Join(staticRoot, apiDocsDir, name))
	if err != nil {
		t.Fatalf("%s is not in the embed. Run `make docs`: %v", name, err)
	}

	return body
}

// TestTheSpecIsInTheBinary checks that the generated description went into the
// embed. The route below is only worth testing if the file it serves is being
// carried, and a description that was never generated fails here rather than as
// a documentation page that comes up empty.
func TestTheSpecIsInTheBinary(t *testing.T) {
	body, err := fs.ReadFile(staticFS, path.Join(staticRoot, specFile))
	if err != nil {
		t.Fatalf("%s is not in the embed. Run `make openapi`: %v", specFile, err)
	}

	var parsed map[string]interface{}

	err = json.Unmarshal(body, &parsed)
	if err != nil {
		t.Fatalf("%s is not JSON: %v", specFile, err)
	}

	if parsed["swagger"] == nil && parsed["openapi"] == nil {
		t.Error("the description says neither swagger nor openapi, so no client will read it")
	}

	if parsed["paths"] == nil {
		t.Error("the description carries no paths")
	}
}

// TestTheSpecIsServedWithoutASession is the whole point of putting the file
// under /ui/. A client reading the description to find out how to log in has no
// session yet, and the description is the same bytes for every installation.
func TestTheSpecIsServedWithoutASession(t *testing.T) {
	rec := get(newDocsServer(t), specPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", specPath, rec.Code, http.StatusOK)
	}

	if got := rec.Header().Get(echo.HeaderContentType); !strings.HasPrefix(got, "application/json") {
		t.Errorf("GET %s came back as %q, want JSON", specPath, got)
	}

	var parsed map[string]interface{}

	err := json.Unmarshal(rec.Body.Bytes(), &parsed)
	if err != nil {
		t.Fatalf("what GET %s served is not JSON: %v", specPath, err)
	}
}

// TestTheDocumentationPageIsServedWithoutASession checks the screens of the
// documentation come up by the prefix, by the name of the page, and that the
// prefix without its trailing slash gets there too. All three are paths an
// operator types, and the relative names inside the page only resolve from the
// first two.
func TestTheDocumentationPageIsServedWithoutASession(t *testing.T) {
	e := newDocsServer(t)

	for _, target := range []string{apiDocsPrefix, apiDocsPrefix + apiDocsIndex} {
		rec := get(e, target)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", target, rec.Code, http.StatusOK)
		}

		if !strings.Contains(rec.Body.String(), "swagger-ui") {
			t.Errorf("GET %s carries no Swagger UI", target)
		}
	}

	rec := get(e, strings.TrimSuffix(apiDocsPrefix, "/"))

	if rec.Code != http.StatusFound {
		t.Fatalf("GET %s = %d, want %d",
			strings.TrimSuffix(apiDocsPrefix, "/"), rec.Code, http.StatusFound)
	}

	if got := rec.Header().Get(echo.HeaderLocation); got != apiDocsPrefix {
		t.Errorf("GET %s redirects to %q, want %q",
			strings.TrimSuffix(apiDocsPrefix, "/"), got, apiDocsPrefix)
	}
}

// TestTheDocumentationAssetsComeOutOfTheBinary asks for each file the page
// names. A request for one that comes back as anything but the file is a page
// that would be blank in a browser and a 200 to curl.
//
// The names are read off the page rather than listed here, so that renaming a
// file and forgetting the page fails.
func TestTheDocumentationAssetsComeOutOfTheBinary(t *testing.T) {
	e := newDocsServer(t)
	page := docsFile(t, apiDocsIndex)

	named := regexp.MustCompile(`(?:href|src)="(?:\./)?([A-Za-z0-9._-]+)"`)

	matches := named.FindAllSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("the documentation page names no file of its own")
	}

	for _, match := range matches {
		name := string(match[1])

		rec := get(e, apiDocsPrefix+name)

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want %d", apiDocsPrefix+name, rec.Code, http.StatusOK)

			continue
		}

		if rec.Body.Len() == 0 {
			t.Errorf("GET %s came back empty", apiDocsPrefix+name)
		}
	}
}

// TestTheDocumentationPageFetchesNothingFromTheNetwork holds the two files
// written here to the rule the rest of the UI is held to. The files are served
// out of the binary so that an installation on a host that reaches nothing but
// this server can read its own documentation, and a page that names somewhere
// else undoes that.
//
// The copy of Swagger UI is not scanned. It is a bundle of somebody else's
// code and holds addresses in strings it never fetches, so reading its bytes
// says nothing; what holds it to this origin is the policy over the page,
// whose connect-src is 'self', and that is tested in package main.
//
// The one fetch the library makes that is not a file of its own is the badge
// that says whether the description is valid, which it works out by sending
// the description to validator.swagger.io. It is turned off in the
// configuration, and that is checked here rather than left to the policy,
// because a policy is what a browser enforces and this is what the page asks
// for.
func TestTheDocumentationPageFetchesNothingFromTheNetwork(t *testing.T) {
	external := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)

	for _, name := range []string{apiDocsIndex, "init.js"} {
		body := string(docsFile(t, name))

		if match := external.FindString(body); match != "" {
			t.Errorf("%s names something outside this server: %q", name, match)
		}
	}

	if !strings.Contains(string(docsFile(t, "init.js")), "validatorUrl: null") {
		t.Error("the documentation page does not turn the online validator off, so a " +
			"browser showing it would send the description to swagger.io")
	}
}

// TestTheDocumentationCarriesItsOwnPolicy checks that what the caller passed is
// what goes out, and that it goes out on every file of the documentation and
// not on the page alone: the scripts and the stylesheet are subject to the
// policy of the document that loads them, but the header is what a browser
// reads on each of them.
func TestTheDocumentationCarriesItsOwnPolicy(t *testing.T) {
	e := newDocsServer(t)

	for _, target := range []string{
		apiDocsPrefix,
		apiDocsPrefix + apiDocsIndex,
		apiDocsPrefix + "init.js",
		apiDocsPrefix + "swagger-ui.css",
		apiDocsPrefix + "swagger-ui-bundle.js",
	} {
		rec := get(e, target)

		if got := rec.Header().Get(echo.HeaderContentSecurityPolicy); got != testPolicy {
			t.Errorf("GET %s carries the policy %q, want %q", target, got, testPolicy)
		}
	}
}

// TestTheDocumentationDoesNotTakeThePathsOfTheUI is the other half of hanging
// the documentation under /ui/. echo matches a static segment ahead of the
// wildcard that serves the rest, and a router that stopped doing so would
// answer every screen of this product with the Swagger UI.
func TestTheDocumentationDoesNotTakeThePathsOfTheUI(t *testing.T) {
	e := newDocsServer(t)

	for _, target := range []string{uiPrefix, uiPrefix + "app.js", uiPrefix + "style.css", versionPath} {
		rec := get(e, target)

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, http.StatusOK)

			continue
		}

		if strings.Contains(rec.Body.String(), "SwaggerUIBundle") {
			t.Errorf("GET %s is being answered with the documentation page", target)
		}
	}
}

// TestAMissingDocumentationFileIsNotFound keeps a mistyped name from being
// answered with the operator UI. Nothing under this prefix routes paths of its
// own, so a name that is not a file is a name that is wrong.
func TestAMissingDocumentationFileIsNotFound(t *testing.T) {
	for _, target := range []string{apiDocsPrefix + "nope.js", apiDocsPrefix + "nope"} {
		rec := get(newDocsServer(t), target)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, http.StatusNotFound)
		}
	}
}
