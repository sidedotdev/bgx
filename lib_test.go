package bgx_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const cliImportPath = "github.com/urfave/cli/v3"

func TestLibraryDependencyGraphExcludesCLIFramework(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("list library dependencies: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == cliImportPath {
			t.Errorf("library transitively depends on %s", cliImportPath)
		}
	}
}

// TestPublicSurfaceHasNoCLIFrameworkTypes guards the typed-library-only
// contract: no exported declaration in the root package may mention a
// urfave/cli type in its signature or definition, and top-level argv runners
// must remain internal.
func TestPublicSurfaceHasNoCLIFrameworkTypes(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg := pkgs["bgx"]
	if pkg == nil {
		t.Fatal("package bgx not found")
	}
	for fileName, file := range pkg.Files {
		aliases := cliImportAliases(file)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || !hasExportedReceiver(d) {
					continue
				}
				if isCLIArgvAdapter(d, file) {
					t.Errorf("%s: exported func %s exposes the top-level CLI action", fileName, d.Name.Name)
				}
				if refsAlias(d.Type, aliases) {
					t.Errorf("%s: exported func %s exposes %s types", fileName, d.Name.Name, cliImportPath)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() && refsAlias(s.Type, aliases) {
							t.Errorf("%s: exported type %s exposes %s types", fileName, s.Name.Name, cliImportPath)
						}
					case *ast.ValueSpec:
						exported := false
						for _, n := range s.Names {
							exported = exported || n.IsExported()
						}
						if exported && refsAlias(s, aliases) {
							t.Errorf("%s: exported value %v exposes %s types", fileName, s.Names, cliImportPath)
						}
					}
				}
			}
		}
	}
}

// cliImportAliases returns the identifiers file uses to refer to the
// urfave/cli import, or an empty map when the file does not import it.
func cliImportAliases(file *ast.File) map[string]bool {
	return importAliases(file, cliImportPath, "cli")
}

// hasExportedReceiver reports whether d is a plain function or a method whose
// receiver type is itself exported (methods on unexported types are private).
func hasExportedReceiver(d *ast.FuncDecl) bool {
	if d.Recv == nil {
		return true
	}
	exported := false
	ast.Inspect(d.Recv, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && ident.IsExported() {
			exported = true
		}
		return true
	})
	return exported
}

// refsAlias reports whether node contains a selector rooted at one of the
// given package aliases.
func refsAlias(node ast.Node, aliases map[string]bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && aliases[ident.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}
func TestCLIArgvAdapterSignatureDetection(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", `package bgx
import (
	"context"
	"io"
)
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error { return nil }
func Launch(args []string) int { return len(args) }
func Run(ctx context.Context, id string, command []string, options struct{}) (any, error) {
	return nil, nil
}
`, 0)
	if err != nil {
		t.Fatalf("parse synthetic API: %v", err)
	}

	detected := map[string]bool{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			detected[fn.Name.Name] = isCLIArgvAdapter(fn, file)
		}
	}
	if !detected["Execute"] {
		t.Error("renamed context-aware argv runner was not detected")
	}
	if !detected["Launch"] {
		t.Error("argv runner without context, writers, or error result was not detected")
	}
	if detected["Run"] {
		t.Error("typed Run operation was detected as an argv runner")
	}
}
func isCLIArgvAdapter(fn *ast.FuncDecl, file *ast.File) bool {
	if fn.Recv != nil || fn.Type.Params == nil {
		return false
	}

	params := flattenFieldTypes(fn.Type.Params)
	if len(params) == 0 {
		return false
	}
	if isStringSlice(params[0]) {
		return true
	}
	if len(params) < 2 {
		return false
	}

	contextAliases := importAliases(file, "context", "context")
	return isImportedSelector(params[0], contextAliases, "Context") && isStringSlice(params[1])
}
func flattenFieldTypes(fields *ast.FieldList) []ast.Expr {
	var types []ast.Expr
	for _, field := range fields.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			types = append(types, field.Type)
		}
	}
	return types
}
func isImportedSelector(expr ast.Expr, aliases map[string]bool, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && aliases[ident.Name]
}
func isStringSlice(expr ast.Expr) bool {
	slice, ok := expr.(*ast.ArrayType)
	if !ok || slice.Len != nil {
		return false
	}
	element, ok := slice.Elt.(*ast.Ident)
	return ok && element.Name == "string"
}
func importAliases(file *ast.File, importPath, defaultName string) map[string]bool {
	aliases := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if imp.Name != nil {
			aliases[imp.Name.Name] = true
		} else {
			aliases[defaultName] = true
		}
	}
	return aliases
}
