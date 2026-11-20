// Package doccheck holds a test that keeps the public API documented.
package doccheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// skipDirs are directory names never walked: non-importable code, test data
// and the satellite modules that have their own go.mod.
var skipDirs = map[string]bool{
	"internal": true, "examples": true, "cmd": true, "testdata": true,
	"orxotel": true, "orxpg": true, "orxdocker": true, "evals": true,
	"docs": true, "vendor": true, ".git": true, ".superpowers": true,
}

// TestExportedFieldsDocumented fails for every exported field of an exported
// struct in the root module's importable packages that has neither a doc
// comment nor a trailing line comment. A doc comment on a grouped field list
// (A, B int) covers the group. Embedded fields count only when exported.
func TestExportedFieldsDocumented(t *testing.T) {
	root := filepath.Join("..", "..")
	var missing []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && path != root {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(path, name), nil, parser.ParseComments)
			if err != nil {
				return err
			}
			missing = append(missing, undocumented(f.Name.Name, f)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d exported struct fields lack a doc or line comment:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}

func undocumented(pkg string, f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ts.Name.IsExported() || !ok {
				continue
			}
			for _, fld := range st.Fields.List {
				if fld.Doc != nil || fld.Comment != nil {
					continue
				}
				if len(fld.Names) == 0 {
					if n := embeddedName(fld.Type); ast.IsExported(n) {
						out = append(out, pkg+"."+ts.Name.Name+"."+n)
					}
					continue
				}
				for _, n := range fld.Names {
					if n.IsExported() {
						out = append(out, pkg+"."+ts.Name.Name+"."+n.Name)
					}
				}
			}
		}
	}
	return out
}

func embeddedName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return embeddedName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.IndexExpr:
		return embeddedName(x.X)
	case *ast.IndexListExpr:
		return embeddedName(x.X)
	}
	return ""
}
