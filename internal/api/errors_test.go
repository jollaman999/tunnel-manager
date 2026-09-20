package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// The tests in this file are what keeps a refusal from reaching a screen
// without a name. Three of them read the source of this package rather than run
// it: a refusal that goes out with no code does so on a path some particular
// request takes, and waiting for a test to take that path is waiting for the
// screen to show an English sentence in Korean surroundings.

// sourceFiles parses every file of this package that is not a test.
func sourceFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("failed to read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		parsed, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", name, err)
		}

		files[name] = parsed
	}

	if len(files) == 0 {
		t.Fatal("no source files were found, so this test checked nothing")
	}

	return fset, files
}

// TestEveryRefusalIsRaisedThroughFailure walks the package and fails on an
// answer that builds models.Response with Success set to false by hand.
//
// That is the shape a refusal had before the codes, and it is the shape one
// written from now on would have: it compiles, it answers, and the sentence in
// it reaches the screen with nothing saying which sentence it is. Nothing in
// the running server can notice that, because the body is well formed and the
// status is the right one, so it is caught here instead.
func TestEveryRefusalIsRaisedThroughFailure(t *testing.T) {
	fset, files := sourceFiles(t)

	for name, file := range files {
		if name == "errors.go" {
			// The one place a refusal is allowed to be built.
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}

			selector, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Response" {
				return true
			}

			for _, element := range lit.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}

				key, ok := pair.Key.(*ast.Ident)
				if !ok {
					continue
				}

				if key.Name == "Error" {
					t.Errorf("%s: a refusal is built here by hand, so it goes out with no "+
						"error_code. Raise it with failure or refuse and give it a code",
						fset.Position(pair.Pos()))
				}

				if key.Name == "Success" {
					value, ok := pair.Value.(*ast.Ident)
					if ok && value.Name == "false" {
						t.Errorf("%s: a refusal is built here by hand, so it goes out with no "+
							"error_code. Raise it with failure or refuse and give it a code",
							fset.Position(pair.Pos()))
					}
				}
			}

			return true
		})
	}
}

// declaredErrorCodes reads the codes out of errors.go: the name of each
// constant, and the code it stands for.
func declaredErrorCodes(t *testing.T, files map[string]*ast.File) map[string]string {
	t.Helper()

	codes := make(map[string]string)

	ast.Inspect(files["errors.go"], func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}

		typeName, ok := spec.Type.(*ast.Ident)
		if !ok || typeName.Name != "errorCode" {
			return true
		}

		for i, name := range spec.Names {
			if name.Name == "errorCodeUnspecified" {
				continue
			}

			if i >= len(spec.Values) {
				continue
			}

			value, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || value.Kind != token.STRING {
				continue
			}

			unquoted, err := strconv.Unquote(value.Value)
			if err != nil {
				t.Fatalf("the code of %s is not a plain string: %v", name.Name, err)
			}

			codes[name.Name] = unquoted
		}

		return true
	})

	if len(codes) == 0 {
		t.Fatal("no codes were found in errors.go, so this test checked nothing")
	}

	return codes
}

// messagedErrorCodes reads the names the errorMessages table has an entry for.
func messagedErrorCodes(t *testing.T, files map[string]*ast.File) map[string]bool {
	t.Helper()

	named := make(map[string]bool)

	ast.Inspect(files["errors.go"], func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "errorMessages" {
			return true
		}

		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("errorMessages is not a composite literal any more")
		}

		for _, element := range lit.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}

			key, ok := pair.Key.(*ast.Ident)
			if !ok {
				continue
			}

			named[key.Name] = true
		}

		return true
	})

	return named
}

// TestEveryErrorCodeHasAMessage holds the codes against the table. A code with
// no entry answers with its own name as the sentence, and an entry with no code
// is a sentence nothing can raise.
func TestEveryErrorCodeHasAMessage(t *testing.T) {
	_, files := sourceFiles(t)

	codes := declaredErrorCodes(t, files)
	named := messagedErrorCodes(t, files)

	for name := range codes {
		if !named[name] {
			t.Errorf("the code %s has no entry in errorMessages, so a refusal raised under it "+
				"answers with the name of the code", name)
		}
	}

	for name := range named {
		if _, ok := codes[name]; !ok {
			t.Errorf("errorMessages has an entry for %s, which is not a declared code", name)
		}
	}

	if len(errorMessages) != len(codes) {
		t.Errorf("errorMessages holds %d entries and %d codes are declared", len(errorMessages), len(codes))
	}
}

// codeNaming is the shape a code is allowed to take: lowercase words joined by
// underscores, in segments divided by dots.
var codeNaming = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*(\.[a-z0-9]+(_[a-z0-9]+)*)+$`)

// TestEveryErrorCodeIsNamedByTheRule holds the codes to the rule errorCode
// states. A screen keys its phrase book by these, so a code in another shape is
// one somebody has to look up rather than guess.
func TestEveryErrorCodeIsNamedByTheRule(t *testing.T) {
	_, files := sourceFiles(t)

	for name, code := range declaredErrorCodes(t, files) {
		if !codeNaming.MatchString(code) {
			t.Errorf("%s is %q, which is not lowercase dot separated segments", name, code)
		}
	}
}

// TestEveryCallSiteHandsOverTheValuesItsSentenceAsksFor walks the package and
// holds each refusal against the sentence it raises.
//
// A sentence asks for its values by name. A call site that hands over none of
// them, or hands over one under another name, answers with {name} where the
// value should be, and a screen writing its own sentence has nothing to put
// there either. The running server logs that, but only once the request that
// takes that path is made.
func TestEveryCallSiteHandsOverTheValuesItsSentenceAsksFor(t *testing.T) {
	fset, files := sourceFiles(t)

	codes := declaredErrorCodes(t, files)

	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			function, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}

			// failure takes the context first, refuse the status. The code is
			// the argument after it in both.
			codeAt := 0

			switch function.Name {
			case "failure":
				codeAt = 2
			case "refuse":
				codeAt = 1
			default:
				return true
			}

			if len(call.Args) <= codeAt {
				return true
			}

			codeName, ok := call.Args[codeAt].(*ast.Ident)
			if !ok {
				// The code is carried in a value here, which is how a refusal
				// that was built elsewhere is written out.
				return true
			}

			code, known := codes[codeName.Name]
			if !known {
				return true
			}

			wanted := errorCodePlaceholders(errorCode(code))

			given := make([]string, 0)

			if len(call.Args) > codeAt+1 {
				args, ok := call.Args[codeAt+1].(*ast.CompositeLit)
				if !ok {
					t.Errorf("%s: the values of %s are not written out here, so nothing can hold "+
						"them against the sentence", fset.Position(call.Pos()), code)

					return true
				}

				for _, element := range args.Elts {
					pair, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}

					key, ok := pair.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						continue
					}

					unquoted, err := strconv.Unquote(key.Value)
					if err != nil {
						continue
					}

					given = append(given, unquoted)
				}
			}

			sort.Strings(given)

			if len(wanted) == 0 && len(given) == 0 {
				return true
			}

			if !reflect.DeepEqual(wanted, given) {
				t.Errorf("%s: %s is raised with %v, and its sentence asks for %v (%s)",
					fset.Position(call.Pos()), code, given, wanted, name)
			}

			return true
		})
	}
}

// TestARefusalCarriesTheShapeItAlwaysDid writes one out and reads the body back,
// so that the two fields a client has always read are still where they were and
// the two new ones are next to them.
func TestARefusalCarriesTheShapeItAlwaysDid(t *testing.T) {
	e := echo.New()
	request := httptest.NewRequest(http.MethodGet, "/api/host/1", nil)
	recorder := httptest.NewRecorder()
	c := e.NewContext(request, recorder)

	err := failure(c, http.StatusNotFound, errHostNotFound)
	if err != nil {
		t.Fatalf("the refusal was not written: %v", err)
	}

	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}

	if body["success"] != false {
		t.Errorf("success = %v, want false", body["success"])
	}

	if body["error"] != "Host not found" {
		t.Errorf("error = %v, want %q", body["error"], "Host not found")
	}

	if body["error_code"] != "host.not_found" {
		t.Errorf("error_code = %v, want %q", body["error_code"], "host.not_found")
	}

	if _, ok := body["error_args"]; ok {
		t.Errorf("error_args is in a body whose sentence takes no values: %v", body["error_args"])
	}
}

// TestARefusalCarriesTheValuesInItsSentence checks that a value written into
// the English sentence is handed over on its own as well.
func TestARefusalCarriesTheValuesInItsSentence(t *testing.T) {
	e := echo.New()
	request := httptest.NewRequest(http.MethodPut, "/api/host/1/service-port", nil)
	recorder := httptest.NewRecorder()
	c := e.NewContext(request, recorder)

	err := failure(c, http.StatusBadRequest, errAssignmentServicePortsMissing, errorArgs{"ids": "99"})
	if err != nil {
		t.Fatalf("the refusal was not written: %v", err)
	}

	var body struct {
		Error string            `json:"error"`
		Code  string            `json:"error_code"`
		Args  map[string]string `json:"error_args"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}

	if body.Error != "No such service port: 99" {
		t.Errorf("error = %q, want %q", body.Error, "No such service port: 99")
	}

	if body.Code != "assignment.service_port.not_found" {
		t.Errorf("error_code = %q", body.Code)
	}

	if body.Args["ids"] != "99" {
		t.Errorf("error_args[ids] = %q, want %q", body.Args["ids"], "99")
	}
}

// TestARefusalWithNoMessageSaysSo is the last line of the three. A code that
// got past the tests above still must not go out looking like one a screen can
// translate.
func TestARefusalWithNoMessageSaysSo(t *testing.T) {
	e := echo.New()
	request := httptest.NewRequest(http.MethodGet, "/api/host", nil)
	recorder := httptest.NewRecorder()
	c := e.NewContext(request, recorder)

	err := failure(c, http.StatusInternalServerError, errorCode("host.nothing_stands_for_this"))
	if err != nil {
		t.Fatalf("the refusal was not written: %v", err)
	}

	var body struct {
		Error string `json:"error"`
		Code  string `json:"error_code"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}

	if body.Code != string(errorCodeUnspecified) {
		t.Errorf("error_code = %q, want %q", body.Code, errorCodeUnspecified)
	}

	if body.Error != "host.nothing_stands_for_this" {
		t.Errorf("error = %q, want the code itself", body.Error)
	}
}

// TestASentenceIsWrittenOnce checks the renderer: a value that happens to hold
// braces is written out as it is rather than read as another place to fill, and
// a place with no value left for it is reported.
func TestASentenceIsWrittenOnce(t *testing.T) {
	tests := []struct {
		name     string
		template string
		args     errorArgs
		want     string
		filled   bool
	}{
		{
			name:     "nothing to write",
			template: "Host not found",
			want:     "Host not found",
			filled:   true,
		},
		{
			name:     "one value",
			template: "Invalid Host ID: {reason}",
			args:     errorArgs{"reason": "not a number"},
			want:     "Invalid Host ID: not a number",
			filled:   true,
		},
		{
			name:     "two values, and the first holds the name of the second",
			template: "The log file at {path} cannot be read: {reason}",
			args:     errorArgs{"path": "/var/{reason}/tm.log", "reason": "permission denied"},
			want:     "The log file at /var/{reason}/tm.log cannot be read: permission denied",
			filled:   true,
		},
		{
			name:     "a value that was not handed over",
			template: "Invalid Host ID: {reason}",
			args:     errorArgs{"cause": "not a number"},
			want:     "Invalid Host ID: {reason}",
			filled:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, filled := renderErrorMessage(tt.template, tt.args)
			if got != tt.want {
				t.Errorf("message = %q, want %q", got, tt.want)
			}

			if filled != tt.filled {
				t.Errorf("filled = %v, want %v", filled, tt.filled)
			}
		})
	}
}
