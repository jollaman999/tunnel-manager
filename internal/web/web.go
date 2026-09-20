// Package web serves the operator UI that is built into the binary.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/labstack/echo/v4"
)

// staticFS holds the UI as it was at build time. It is embedded rather than
// read from disk so that the binary is the whole deployment: no directory has
// to travel next to it, no path has to be configured, and the working
// directory the process is started from does not matter.
//
// The pattern reaches the language catalogs under static/lang as well, so the
// thirteen of them travel in the binary with everything else: a UI that was
// built to work on a host that reaches nothing but this server cannot have its
// words fetched from somewhere that host cannot reach.
//
//go:embed static
var staticFS embed.FS

// staticRoot is the directory the embedded files sit under. The embed
// directive keeps the directory name in the stored paths, so every lookup has
// to go through it.
const staticRoot = "static"

// uiPrefix is where the UI is served from. It is a directory of its own rather
// than the site root, so that a UI file can never take a path away from /api.
const uiPrefix = "/ui/"

// indexFile is the page the UI is; a request that names no file and a request
// for a path the page routes itself are both answered with it.
const indexFile = "index.html"

// contentTypes maps an extension to what is sent for it. The types are held
// here instead of being taken from mime.TypeByExtension because that one reads
// the system tables (/etc/mime.types), which would let the same binary answer
// with a different type on a different host.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8",
}

// versionPath answers what version of the binary is serving the UI. It sits
// under /ui/ and not under /api/, so it is reachable without a session: the
// login screen shows the version too, and the number is on the release page of
// a public repository either way.
const versionPath = uiPrefix + "version.json"

// RegisterRoutes puts the UI on e.
//
// The routes go on the echo instance itself and not on the /api group, so the
// session middleware that group carries does not reach them. That is on
// purpose: every byte served from here is the same for every client and holds
// no data of its own. The hosts, the service ports and the tunnel state are
// fetched from /api/**, and the session still guards those. Putting the
// session in front of these files instead would shut the login screen behind
// the login, leaving no way to get a session in the first place.
func RegisterRoutes(e *echo.Echo, version string) {
	e.GET("/", redirectToUI)
	e.GET(strings.TrimSuffix(uiPrefix, "/"), redirectToUI)

	// The version route is added before the wildcard so that echo matches it
	// first. A request for it would otherwise be looked up as a file and
	// answered with a 404, since it carries an extension.
	e.GET(versionPath, func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"version": version})
	})

	e.GET(uiPrefix+"*", serveAsset)
}

// redirectToUI sends a client that asked for the site root to the UI. It is a
// 302 rather than a 301, because a permanent redirect is cached by the browser
// and would keep sending clients to /ui/ even after the root is given
// something else to serve.
func redirectToUI(c echo.Context) error {
	return c.Redirect(http.StatusFound, uiPrefix)
}

// serveAsset answers everything under /ui/.
func serveAsset(c echo.Context) error {
	name := assetName(c.Param("*"))

	body, err := fs.ReadFile(staticFS, path.Join(staticRoot, name))
	if err != nil {
		// A request that carries an extension is asking for one particular
		// file, so a missing one is reported as missing. Handing back the page
		// instead would answer a script request with HTML, and the failure
		// would then surface far away from the name that was wrong.
		if path.Ext(name) != "" {
			return echo.NewHTTPError(http.StatusNotFound)
		}

		// Anything else is a path the page routes on its own, so the page is
		// what gets served and it reads the path itself. Without this a reload
		// of a screen the UI navigated to would come back as a 404.
		body, err = fs.ReadFile(staticFS, path.Join(staticRoot, indexFile))
		if err != nil {
			return err
		}

		name = indexFile
	}

	return serveBody(c, contentTypeOf(name, body), body)
}

// serveBody hands back a built-in file with an entity tag over it, and asks
// that the tag be checked every time rather than the file be taken on trust.
//
// Without either header a browser is left to guess how long the file stays
// good, and what it guesses is its own business: a screen carried on after a
// release with the scripts of the one before it, which reads as a fix that was
// deployed and did nothing. The files are built into the binary, so what they
// hold is settled at build time and a hash of the bytes names that version
// exactly.
//
// no-cache does not mean do not store it. It means ask before using what is
// stored, and the answer to that question is 304 and no body whenever the file
// has not moved, which is nearly always. The cost of being right here is one
// conditional request per file per page load.
func serveBody(c echo.Context, contentType string, body []byte) error {
	sum := sha256.Sum256(body)
	tag := `"` + hex.EncodeToString(sum[:16]) + `"`

	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("ETag", tag)

	// The browser is holding this very file already, so there is nothing to
	// send it. Handing over the bytes again would be the whole saving thrown
	// away at the last step.
	if c.Request().Header.Get("If-None-Match") == tag {
		return c.NoContent(http.StatusNotModified)
	}

	return c.Blob(http.StatusOK, contentType, body)
}

// assetName turns the wildcard of a /ui/ request into a name inside the
// embedded directory. Cleaning the path against a root first keeps a request
// that walks upwards ("../../etc/passwd") from naming anything outside it.
func assetName(wildcard string) string {
	name := strings.TrimPrefix(path.Clean("/"+wildcard), "/")
	if name == "" || name == "." {
		return indexFile
	}

	return name
}

// contentTypeOf reports what to send a file as. An extension that is not in
// the table is sniffed from the bytes, so a file added later is still served
// as something, just not as a guess taken from its name.
func contentTypeOf(name string, body []byte) string {
	contentType, ok := contentTypes[path.Ext(name)]
	if !ok {
		return http.DetectContentType(body)
	}

	return contentType
}
