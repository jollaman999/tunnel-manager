package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The description of this API is generated from the annotations on the
// handlers and committed, so nothing rebuilds it when a route is added. These
// tests are what makes the difference show: a route registered without an
// annotation, or an annotation left behind by a route that went, fails here
// rather than being found by somebody whose generated client calls a path this
// server does not answer.
//
// The routes are read out of the source of this file rather than off a running
// echo instance, because they are registered inside serve() along with a
// database, a tunnel manager and a listener, and none of that has to exist for
// the question being asked here.

// specFile is the description as it is committed, and as internal/web embeds
// it. The test reads the file and not the embed, so that a file regenerated
// without being rebuilt is still what is being checked.
const specFile = "internal/web/static/openapi.json"

// routerMethods are the registrations the reading below looks for. They are
// the methods echo names its route registration after.
var routerMethods = map[string]bool{
	"GET":     true,
	"POST":    true,
	"PUT":     true,
	"DELETE":  true,
	"PATCH":   true,
	"HEAD":    true,
	"OPTIONS": true,
}

// apiGroupIdent is the variable the /api group is registered through in
// serve(). A rename leaves the reading below with nothing to find, which is
// what routerRoutes refuses to return quietly.
const apiGroupIdent = "g"

// spec is the little of the description these tests read.
type spec struct {
	Swagger string `json:"swagger"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	BasePath string                            `json:"basePath"`
	Paths    map[string]map[string]interface{} `json:"paths"`
}

// readSpec reads the committed description.
func readSpec(t *testing.T) spec {
	t.Helper()

	body, err := os.ReadFile(specFile)
	if err != nil {
		t.Fatalf("the description could not be read. Run `make openapi`: %v", err)
	}

	var parsed spec

	err = json.Unmarshal(body, &parsed)
	if err != nil {
		t.Fatalf("%s is not JSON: %v", specFile, err)
	}

	return parsed
}

// mainFileConstants is every top level string constant declared in main.go,
// under the name it is declared with. The registrations below name two of
// their paths by constant rather than writing them out, and a reading that
// could not follow that would report them as routes the description invented.
func mainFileConstants(file *ast.File) map[string]string {
	values := map[string]string{}

	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}

		for _, item := range general.Specs {
			value, ok := item.(*ast.ValueSpec)
			if !ok {
				continue
			}

			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}

				text, ok := stringValue(value.Values[i], nil)
				if ok {
					values[name.Name] = text
				}
			}
		}
	}

	return values
}

// stringValue reads a path out of an argument. It takes a quoted literal, a
// constant named in the table, and the two joined with +, which is how a path
// under a prefix is written.
func stringValue(node ast.Expr, constants map[string]string) (string, bool) {
	switch value := node.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}

		text, err := strconv.Unquote(value.Value)
		if err != nil {
			return "", false
		}

		return text, true
	case *ast.Ident:
		text, ok := constants[value.Name]

		return text, ok
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}

		left, ok := stringValue(value.X, constants)
		if !ok {
			return "", false
		}

		right, ok := stringValue(value.Y, constants)
		if !ok {
			return "", false
		}

		return left + right, true
	}

	return "", false
}

// routerRoutes is every path registered on the /api group, as "METHOD path"
// with the prefix in front of it. The paths are echo's, so an id in one is
// written :id.
func routerRoutes(t *testing.T) []string {
	t.Helper()

	fileSet := token.NewFileSet()

	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("main.go could not be read: %v", err)
	}

	constants := mainFileConstants(file)

	prefix, ok := constants["apiPrefix"]
	if !ok {
		t.Fatal("main.go declares no apiPrefix constant")
	}

	var routes []string

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != apiGroupIdent {
			return true
		}

		if !routerMethods[selector.Sel.Name] {
			return true
		}

		path, ok := stringValue(call.Args[0], constants)
		if !ok {
			t.Errorf("the path of %s.%s is written in a way this test cannot read. "+
				"Write it as a literal or as a constant declared in main.go",
				receiver.Name, selector.Sel.Name)

			return true
		}

		routes = append(routes, selector.Sel.Name+" "+prefix+path)

		return true
	})

	// A reading that found nothing is a reading that broke, not a server with
	// no routes. Without this the comparison below passes by finding the same
	// nothing on both sides only when the description is empty as well, and
	// fails confusingly when it is not.
	if len(routes) == 0 {
		t.Fatalf("no route was found on %q in main.go. Has the /api group been renamed?",
			apiGroupIdent)
	}

	sort.Strings(routes)

	return routes
}

// specRoutes is every operation the description carries, written the way
// routerRoutes writes a route. The path parameters are turned back from
// {id} into :id, which is the same path in echo's spelling.
func specRoutes(t *testing.T, parsed spec) []string {
	t.Helper()

	var routes []string

	for path, operations := range parsed.Paths {
		for method := range operations {
			routes = append(routes,
				strings.ToUpper(method)+" "+parsed.BasePath+echoPath(path))
		}
	}

	sort.Strings(routes)

	return routes
}

// echoPath rewrites {name} as :name, one segment at a time.
func echoPath(path string) string {
	segments := strings.Split(path, "/")

	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[i] = ":" + strings.Trim(segment, "{}")
		}
	}

	return strings.Join(segments, "/")
}

// TestTheSpecAndTheRouterAgree holds the description to the routes. A route
// added without an annotation and an annotation left behind by a route that
// went both fail here, and both fail naming the path.
func TestTheSpecAndTheRouterAgree(t *testing.T) {
	parsed := readSpec(t)

	registered := routerRoutes(t)
	described := specRoutes(t, parsed)

	inSpec := map[string]bool{}
	for _, route := range described {
		inSpec[route] = true
	}

	inRouter := map[string]bool{}
	for _, route := range registered {
		inRouter[route] = true
	}

	for _, route := range registered {
		if !inSpec[route] {
			t.Errorf("%s is registered in main.go and is not in %s. Annotate the handler "+
				"and run `make openapi`", route, specFile)
		}
	}

	for _, route := range described {
		if !inRouter[route] {
			t.Errorf("%s is in %s and is registered nowhere in main.go. Remove the "+
				"annotation and run `make openapi`", route, specFile)
		}
	}

	if t.Failed() {
		t.Logf("registered: %d, described: %d", len(registered), len(described))
	}
}

// TestTheSpecNamesTheVersionThisBuildIs holds the version in the annotation to
// the one the binary reports. They are two places because swag reads the
// version out of the comment and cannot read a constant, so a release that
// bumps one and not the other hands out a description that says it describes
// the version before.
func TestTheSpecNamesTheVersionThisBuildIs(t *testing.T) {
	parsed := readSpec(t)

	if parsed.Info.Version != version {
		t.Errorf("the description says version %q and this build is %q. Change @version "+
			"above func main and run `make openapi`", parsed.Info.Version, version)
	}
}

// TestTheSpecIsTheShapeAClientExpects reads the description the way a client
// would, and refuses one that is missing what every client reads first.
func TestTheSpecIsTheShapeAClientExpects(t *testing.T) {
	parsed := readSpec(t)

	if parsed.Swagger != "2.0" {
		t.Errorf("the description says swagger %q, and swag writes OpenAPI 2.0",
			parsed.Swagger)
	}

	if parsed.Info.Title == "" {
		t.Error("the description carries no title")
	}

	fileSet := token.NewFileSet()

	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("main.go could not be read: %v", err)
	}

	prefix := mainFileConstants(file)["apiPrefix"]

	if parsed.BasePath != prefix {
		t.Errorf("the description hangs its paths under %q and this server serves them "+
			"under %q", parsed.BasePath, prefix)
	}

	for path, operations := range parsed.Paths {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("path %q does not begin with a slash", path)
		}

		for method, operation := range operations {
			if !routerMethods[strings.ToUpper(method)] {
				t.Errorf("%s %s is not a method this server registers routes for",
					strings.ToUpper(method), path)
			}

			body, ok := operation.(map[string]interface{})
			if !ok {
				t.Errorf("%s %s does not describe an operation", strings.ToUpper(method), path)

				continue
			}

			if fmt.Sprint(body["summary"]) == "" || body["summary"] == nil {
				t.Errorf("%s %s carries no summary", strings.ToUpper(method), path)
			}

			if body["responses"] == nil {
				t.Errorf("%s %s says nothing about what it answers", strings.ToUpper(method), path)
			}
		}
	}
}
