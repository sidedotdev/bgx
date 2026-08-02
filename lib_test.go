package bgx_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/sidedotdev/bgx"
)

// TestCLIAdapterSurface pins the thin CLI adapter that cmd/bgx builds on: the
// process hooks are exported while the urfave/cli command tree stays internal.
func TestCLIAdapterSurface(t *testing.T) {
	var _ func(context.Context, []string) error = bgx.Run
	var _ func() = bgx.InterceptDaemon
}

const cliImportPath = "github.com/urfave/cli/v3"

// TestPublicSurfaceHasNoCLIFrameworkTypes guards the typed-library-only
// contract: no exported declaration in the root package may mention a
// urfave/cli type in its signature or definition.
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
		if len(aliases) == 0 {
			continue
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || !hasExportedReceiver(d) {
					continue
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
	aliases := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != cliImportPath {
			continue
		}
		name := "cli"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		aliases[name] = true
	}
	return aliases
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
