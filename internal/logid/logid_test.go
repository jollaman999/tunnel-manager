package logid

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// notYetCarryingIDs are the files whose log lines have not been given their IDs
// yet. They are read out of the check rather than out of the walk, so a file
// that is added to a package that is already done is checked from the day it is
// written.
//
// A path that ends in a separator covers the directory under it. Take an entry
// away as its lines are done: what is left here is exactly what the Logs screen
// cannot translate yet.
var notYetCarryingIDs = []string{
	"main.go",
	"internal/api/",
}

// logMethods are the calls that write a line.
var logMethods = map[string]bool{
	"Debug": true,
	"Info":  true,
	"Warn":  true,
	"Error": true,
	"Fatal": true,
	"Panic": true,
}

// loggerBuilders are the calls that hand back a logger, so that a line written
// straight on one of them is found as well.
var loggerBuilders = map[string]bool{
	"New":        true,
	"NewNop":     true,
	"NewExample": true,
	"L":          true,
	"S":          true,
}

// loggerWrappers are the calls that hand back a logger built from another one.
var loggerWrappers = map[string]bool{
	"With":        true,
	"WithOptions": true,
	"Named":       true,
	"Sugar":       true,
	"Desugar":     true,
}

// isLogger reports whether expr is the logger a line is written on. What is
// looked at is the name the expression ends in: every logger in this
// application is held in a field or a variable called "logger", and a line is
// written either on it or on a logger built from it.
func isLogger(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return strings.EqualFold(e.Name, "logger")
	case *ast.SelectorExpr:
		return strings.EqualFold(e.Sel.Name, "logger")
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "zap" && loggerBuilders[sel.Sel.Name] {
			return true
		}

		return loggerWrappers[sel.Sel.Name] && isLogger(sel.X)
	}

	return false
}

// carriesID reports whether the call names an identifier of this package
// anywhere inside it. The field is written beside the message at nearly every
// call, but a call that hands its fields on as a slice puts it into the slice
// instead, so what is asked is that the ID is on the line and not where in the
// call it was written.
func carriesID(call *ast.CallExpr) bool {
	found := false
	ast.Inspect(call, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "logid" {
			found = true
		}

		return !found
	})

	return found
}

// moduleRoot walks up from the working directory to the directory holding
// go.mod, which is the root of this module.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to read the working directory: %v", err)
	}

	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// exempt reports whether the file is one of the ones that have not been done.
func exempt(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, entry := range notYetCarryingIDs {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(rel, entry) {
				return true
			}

			continue
		}

		if rel == entry {
			return true
		}
	}

	return false
}

// TestEveryLogLineCarriesAnID is what makes a line that was left without one
// visible. The Logs screen shows a line it cannot find an ID on in English
// whatever language was selected, which is one line among translated ones and
// is easy to miss on a screen while it is impossible to miss here.
func TestEveryLogLineCarriesAnID(t *testing.T) {
	root := moduleRoot(t)

	var missing []string
	checked := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			name := info.Name()
			if path != root && (name == ".git" || name == "testdata" || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			// A directory with a go.mod of its own is another module and not
			// this application.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if exempt(rel) {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !logMethods[sel.Sel.Name] || !isLogger(sel.X) {
				return true
			}

			checked++

			if carriesID(call) {
				return true
			}

			message := "?"
			if len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					message = lit.Value
				}
			}

			position := fset.Position(call.Lparen)
			missing = append(missing, filepath.ToSlash(rel)+":"+strconv.Itoa(position.Line)+" "+message)

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("failed to read the sources: %v", err)
	}

	// The floor is there so that this cannot pass by finding nothing. A rule
	// above that stopped recognising a logger would leave every line unchecked
	// and the test green, which reads exactly like every line carrying an ID.
	// The number is what the first batch attached, and it only goes up.
	const checkedAtLeast = 61

	if checked < checkedAtLeast {
		t.Fatalf("only %d log lines were found, want at least %d: the rules that recognise a "+
			"log call no longer match how this application logs", checked, checkedAtLeast)
	}

	t.Logf("%d log lines checked", checked)

	if len(missing) > 0 {
		t.Fatalf("%d log lines carry no ID, so the Logs screen cannot translate them. "+
			"Add one from this package as the first field, or list the file in notYetCarryingIDs:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}

// camelCase is the constant name an ID has to be written under.
func camelCase(id string) string {
	var name strings.Builder
	upper := true
	for _, r := range id {
		if r == '.' || r == '_' {
			upper = true

			continue
		}
		if upper {
			name.WriteRune(unicode.ToUpper(r))
			upper = false

			continue
		}
		name.WriteRune(r)
	}

	return name.String()
}

// declaredIDs reads the catalogue out of logid.go: the constant name against
// the ID it carries.
func declaredIDs(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "logid.go", nil, 0)
	if err != nil {
		t.Fatalf("failed to read logid.go: %v", err)
	}

	ids := make(map[string]string)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}

		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}

			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "ID" {
				continue
			}

			for i, name := range value.Names {
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not declared with a string", name.Name)
				}

				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s carries %s, which is not a string: %v", name.Name, lit.Value, err)
				}

				ids[name.Name] = unquoted
			}
		}
	}

	if len(ids) == 0 {
		t.Fatal("no IDs were read out of logid.go")
	}

	return ids
}

// TestConstantNamesFollowTheirIDs holds the naming rule. The ID is what the
// screen looks up and the constant is what the code names, and a constant whose
// name says something other than its ID sends whoever greps for one of the two
// to the wrong line.
func TestConstantNamesFollowTheirIDs(t *testing.T) {
	for name, id := range declaredIDs(t) {
		if want := camelCase(id); name != want {
			t.Errorf("%s carries %q, which is written %s", name, id, want)
		}
	}
}

// TestIDsAreShapedAsAreaAndEvent holds the other half of the rule: an area, a
// dot, and what happened, both in lower case with words joined by underscores.
func TestIDsAreShapedAsAreaAndEvent(t *testing.T) {
	for name, id := range declaredIDs(t) {
		area, event, found := strings.Cut(id, ".")
		if !found || area == "" || event == "" {
			t.Errorf("%s carries %q, want \"<area>.<event>\"", name, id)

			continue
		}

		if strings.Contains(event, ".") {
			t.Errorf("%s carries %q, which has more than one dot in it", name, id)
		}

		for _, r := range id {
			if unicode.IsLower(r) || r == '.' || r == '_' {
				continue
			}

			t.Errorf("%s carries %q, which is not lower case with underscores", name, id)

			break
		}
	}
}

// TestNoTwoIDsAreTheSame is what keeps two places that say the same thing on
// one ID. A second constant carrying an ID that is already there would have the
// screen translate one of the two and leave the other as it was.
func TestNoTwoIDsAreTheSame(t *testing.T) {
	byID := make(map[string]string)
	for name, id := range declaredIDs(t) {
		if first, seen := byID[id]; seen {
			t.Errorf("%s and %s both carry %q", first, name, id)

			continue
		}

		byID[id] = name
	}
}
