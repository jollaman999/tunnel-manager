package main

import (
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/api"
)

// TestEveryRouteIsInTheTokenTable holds the routes registered in serve() to the
// table the session middleware decides what an API token may reach by.
//
// A route that is not in the table is refused to every token, so forgetting it
// closes it rather than opening it. That is safe but it is also a route no
// script can call, which nobody finds out about until one tries. This is where
// it shows instead: every route has to be given a scope, or be named as one no
// token reaches, or be one the middleware answers before it reads any
// credential.
func TestEveryRouteIsInTheTokenTable(t *testing.T) {
	for _, route := range routerRoutes(t) {
		method, path, _ := strings.Cut(route, " ")

		if api.TokenRouteRule(method, path) == "" {
			t.Errorf("%s is registered in main.go and is not in the token table in "+
				"internal/api/token.go. Give it a scope, or name it as a route no token reaches", route)
		}
	}
}

// TestTheTokenTableNamesOnlyRegisteredRoutes is the other direction. A route in
// the table that is registered nowhere is one that was renamed or removed, and
// the table would go on saying something about a path this server does not
// serve.
func TestTheTokenTableNamesOnlyRegisteredRoutes(t *testing.T) {
	registered := map[string]bool{}
	for _, route := range routerRoutes(t) {
		registered[route] = true
	}

	for _, route := range api.TokenRoutes() {
		if !registered[route] {
			t.Errorf("%s is in the token table in internal/api/token.go and is registered "+
				"nowhere in main.go", route)
		}
	}
}
