package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/labstack/echo/v4"
)

// This file serves the API documentation: the OpenAPI description of /api/**
// and the Swagger UI that reads it. Both are built into the binary the way the
// operator UI is, and for the same reason: an installation on a host that
// reaches nothing but this server has to be able to show them.

// specPath is where the OpenAPI description is served from. The file sits in
// the embedded directory beside the rest of the UI, so the wildcard route
// serveAsset answers for it already: nothing here registers it, and it arrives
// with the entity tag and the JSON content type every other embedded file
// does.
//
// It is named here all the same, because the test that reads the file and the
// documentation that tells a client where to fetch it both need the path, and
// a path typed three times drifts.
const specPath = uiPrefix + "openapi.json"

// specFile is the same file as it sits inside the embedded directory.
const specFile = "openapi.json"

// apiDocsPrefix is where the Swagger UI is served from. It hangs under the UI
// prefix rather than beside it, so it stays inside the one directory that can
// never take a path away from /api, and echo matches it ahead of the wildcard
// because a static segment is preferred to a catch-all.
const apiDocsPrefix = uiPrefix + "api-docs/"

// apiDocsDir is the directory inside the embedded tree that holds the page,
// the copy of Swagger UI it loads and the licence that copy is under.
const apiDocsDir = "api-docs"

// apiDocsIndex is the page itself.
const apiDocsIndex = "index.html"

// RegisterAPIDocs puts the Swagger UI on e.
//
// The routes go on the echo instance and not on the /api group, so no session
// is required to reach them. What is served here is the same bytes for every
// installation and holds nothing of this one: the description is generated
// from the source at build time and is in the public repository, and the rest
// is a copy of Swagger UI. Behind the session it would also be unreachable by
// the client that needs it most, the one that has not logged in yet and is
// reading this to find out how, since the login call is the first thing the
// description documents.
//
// Pressing "Try it out" is a different matter, and needs no grant from here.
// Those calls go to /api/**, which stays behind the session check whoever
// sent them.
//
// policy is the Content-Security-Policy everything under the prefix is served
// under. It is built by the caller because that is where every other header
// this server sends is decided.
func RegisterAPIDocs(e *echo.Echo, policy string) {
	header := apiDocsPolicy(policy)

	// The prefix without its trailing slash is registered too. Without it the
	// page would be reached only by a path ending in a slash, and the relative
	// names inside the page would resolve against /ui/ rather than against
	// /ui/api-docs/, which is a page whose every file comes back as the
	// operator UI. It is a 302 and not a 301 for the reason redirectToUI is: a
	// permanent redirect is cached, and it would keep sending clients here
	// after the path is given something else to serve.
	e.GET(strings.TrimSuffix(apiDocsPrefix, "/"), func(c echo.Context) error {
		return c.Redirect(http.StatusFound, apiDocsPrefix)
	})

	// The prefix itself names no file, and serveAsset reads a path with no
	// extension as one the operator UI routes on its own. Left to the wildcard
	// it would hand back the operator UI, so the page is named here.
	e.GET(apiDocsPrefix, func(c echo.Context) error {
		return serveAPIDocsFile(c, apiDocsIndex)
	}, header)

	// Everything else in the directory. It cannot be left to the /ui/*
	// wildcard, because a route registered at this prefix strips the prefix
	// from what the wildcard captures: the file asked for would be looked up
	// beside the operator UI rather than inside this directory. It is
	// registered rather than left alone so that the policy below reaches the
	// files as well as the page.
	e.GET(apiDocsPrefix+"*", func(c echo.Context) error {
		return serveAPIDocsFile(c, assetName(c.Param("*")))
	}, header)
}

// serveAPIDocsFile hands back one file of the documentation directory. The
// name has already been cleaned against a root by assetName, so it cannot
// climb out of the embedded tree.
func serveAPIDocsFile(c echo.Context, name string) error {
	body, err := fs.ReadFile(staticFS, path.Join(staticRoot, apiDocsDir, name))
	if err != nil {
		// Unlike the operator UI there is nothing here that routes paths of
		// its own, so a name that is not a file is a name that is wrong and is
		// reported as missing. Handing back the page instead would answer a
		// script request with HTML and move the failure away from the name.
		return echo.NewHTTPError(http.StatusNotFound)
	}

	return serveBody(c, contentTypeOf(name, body), body)
}

// apiDocsPolicy puts the policy of the documentation over the one the server
// carries.
//
// It is route middleware and not part of the handler, so it runs inside the
// middleware that sets the general policy and overwrites what that one wrote.
// The general policy is the stricter of the two and every other route keeps
// it: what is widened here is widened for this prefix and for nothing else.
func apiDocsPolicy(header string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set(echo.HeaderContentSecurityPolicy, header)

			return next(c)
		}
	}
}
