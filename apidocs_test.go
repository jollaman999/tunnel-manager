package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/web"
	"github.com/labstack/echo/v4"
)

// documentationPath is the page of the Swagger UI, and uiPath is a screen of
// this product. They are written out here rather than taken from internal/web,
// which keeps them to itself: what these tests are about is that the addresses
// a browser is sent to in docs/reference.md answer, and an address read off
// the code that serves it would answer whatever that code moved to.
const (
	documentationPath = "/ui/api-docs/index.html"
	uiPath            = "/ui/"
)

// hardenedServer is the server main builds, as far as the headers and the
// routes go: the same harden, the same UI and the same documentation. A test
// that wired these by hand would be testing its own wiring.
func hardenedServer() *echo.Echo {
	e := echo.New()

	harden(e, noCertificate)
	web.RegisterRoutes(e, version)
	web.RegisterAPIDocs(e, apiDocsContentSecurityPolicy)

	return e
}

// fetch runs a GET against e without a cookie of any kind.
func fetch(e *echo.Echo, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()

	e.ServeHTTP(rec, req)

	return rec
}

// inlineScripts is the text of every script written inside html, which is what
// a browser hashes when it checks a policy.
func inlineScripts(html []byte) [][]byte {
	var scripts [][]byte

	opening := regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

	for _, match := range opening.FindAllSubmatch(html, -1) {
		if strings.Contains(string(match[1]), "src") {
			continue
		}

		scripts = append(scripts, match[2])
	}

	return scripts
}

// TestTheDocumentationWritesNoScriptIntoItsPage is what keeps script-src at
// 'self' alone.
//
// A script written inside a page can only be named in a policy by the hash of
// its text, and that hash then has to be produced wherever the page is served
// from and reproduced whenever the library is upgraded. The page here names
// its two scripts as files instead, so there is nothing to hash. This fails if
// one is ever moved back inside, because the policy would then be refusing a
// script the page needs and the documentation would come up blank with the
// reason only in the browser console.
func TestTheDocumentationWritesNoScriptIntoItsPage(t *testing.T) {
	rec := fetch(hardenedServer(), documentationPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", documentationPath, rec.Code, http.StatusOK)
	}

	policy := rec.Header().Get(echo.HeaderContentSecurityPolicy)
	if policy == "" {
		t.Fatalf("GET %s carries no Content-Security-Policy", documentationPath)
	}

	if scripts := inlineScripts(rec.Body.Bytes()); len(scripts) != 0 {
		t.Errorf("%s writes %d script(s) inside itself. script-src names no hash, so a "+
			"browser refuses to run them and the page comes up blank.\nfirst one: %q",
			documentationPath, len(scripts), string(scripts[0]))
	}

	if got := scriptSources(policy); got != "script-src 'self'" {
		t.Errorf("the documentation policy has %q, want %q", got, "script-src 'self'")
	}
}

// scriptSources is the script-src directive of a policy, or the empty string.
func scriptSources(policy string) string {
	for _, directive := range strings.Split(policy, ";") {
		directive = strings.TrimSpace(directive)

		if strings.HasPrefix(directive, "script-src ") {
			return directive
		}
	}

	return ""
}

// TestTheDocumentationPolicyReachesNowhereButThisServer checks that what was
// widened for the documentation was widened towards this origin and not towards
// the network. Serving the Swagger UI out of the binary is undone by a policy
// that would have let it be fetched from somewhere else anyway.
func TestTheDocumentationPolicyReachesNowhereButThisServer(t *testing.T) {
	rec := fetch(hardenedServer(), documentationPath)

	policy := rec.Header().Get(echo.HeaderContentSecurityPolicy)

	// Everything a source may be. data: is here because the icons of the
	// Swagger UI are written into its stylesheet as data URLs.
	allowed := map[string]bool{
		"'none'":          true,
		"'self'":          true,
		"'unsafe-inline'": true,
		"data:":           true,
	}

	for _, directive := range strings.Split(policy, ";") {
		fields := strings.Fields(strings.TrimSpace(directive))
		if len(fields) == 0 {
			continue
		}

		for _, source := range fields[1:] {
			if allowed[source] {
				continue
			}

			t.Errorf("the documentation policy allows %q in %s, which is not this server",
				source, fields[0])
		}
	}
}

// TestTheDocumentationPolicyStopsAtItsOwnPrefix is the reason the policy is put
// on by the route and not by harden. What the Swagger UI needs is wider than
// what the screens of this product run under, and a widening that reached them
// would be this one page lowering the defence of every screen.
func TestTheDocumentationPolicyStopsAtItsOwnPrefix(t *testing.T) {
	e := hardenedServer()

	documentation := fetch(e, documentationPath).Header().Get(echo.HeaderContentSecurityPolicy)
	screens := fetch(e, uiPath).Header().Get(echo.HeaderContentSecurityPolicy)

	if screens == "" {
		t.Fatalf("GET %s carries no Content-Security-Policy", uiPath)
	}

	if screens == documentation {
		t.Fatal("the screens and the documentation carry the same policy, so the widening " +
			"for the Swagger UI reached every screen of this product")
	}

	if screens != contentSecurityPolicy(indexHTML) {
		t.Errorf("GET %s no longer carries the policy of this product.\n got: %s\nwant: %s",
			uiPath, screens, contentSecurityPolicy(indexHTML))
	}

	// The two widenings, named so that one of them turning up on the screens
	// fails here. Neither belongs anywhere but on the documentation.
	for _, widening := range []string{"'unsafe-inline'", "data:"} {
		if strings.Contains(screens, widening) {
			t.Errorf("the policy of the screens carries %s", widening)
		}
	}
}
