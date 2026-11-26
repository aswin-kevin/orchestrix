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

// moduleRoots are the module directories the checks walk: the root module
// and the satellites, which are parsed from disk (doccheck imports nothing
// from them).
var moduleRoots = []string{
	filepath.Join("..", ".."),
	filepath.Join("..", "..", "orxotel"),
	filepath.Join("..", "..", "orxpg"),
	filepath.Join("..", "..", "orxdocker"),
	filepath.Join("..", "..", "evals"),
}

// walkPackages calls fn for every non-test file of every importable package
// under root. Nested modules, internal packages, examples, commands, test
// data and dot-directories are skipped.
func walkPackages(t *testing.T, root string, fn func(pkg string, f *ast.File)) {
	t.Helper()
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
			fn(f.Name.Name, f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestExportedFieldsDocumented fails for every exported field of an exported
// struct in the importable packages of every module that has neither a doc
// comment nor a trailing line comment. A doc comment on a grouped field list
// (A, B int) covers the group. Embedded fields count only when exported.
func TestExportedFieldsDocumented(t *testing.T) {
	var missing []string
	for _, root := range moduleRoots {
		walkPackages(t, root, func(pkg string, f *ast.File) {
			missing = append(missing, undocumented(pkg, f)...)
		})
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d exported struct fields lack a doc or line comment:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}

// TestExportedDeclsDocumented fails for every exported function, exported
// type and exported method of an exported type whose doc comment does not
// start with its name (types may start with A, An or The first).
func TestExportedDeclsDocumented(t *testing.T) {
	var bad []string
	for _, root := range moduleRoots {
		walkPackages(t, root, func(pkg string, f *ast.File) {
			bad = append(bad, undocumentedDecls(pkg, f)...)
		})
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("%d exported declarations lack a doc comment starting with their name:\n%s",
			len(bad), strings.Join(bad, "\n"))
	}
}

// docStarts reports whether doc's first word is name, after one of the
// words in articles.
func docStarts(doc *ast.CommentGroup, name string, articles bool) bool {
	if doc == nil {
		return false
	}
	words := strings.Fields(doc.Text())
	if articles && len(words) > 1 {
		switch words[0] {
		case "A", "An", "The":
			words = words[1:]
		}
	}
	return len(words) > 0 && strings.TrimRight(words[0], ".,:;") == name
}

func undocumentedDecls(pkg string, f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			label := pkg + "." + d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				base := embeddedName(d.Recv.List[0].Type)
				if !ast.IsExported(base) {
					continue
				}
				label = pkg + "." + base + "." + d.Name.Name
			}
			if !docStarts(d.Doc, d.Name.Name, false) {
				out = append(out, label)
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				if !ts.Name.IsExported() {
					continue
				}
				doc := ts.Doc
				if doc == nil && len(d.Specs) == 1 {
					doc = d.Doc
				}
				if !docStarts(doc, ts.Name.Name, true) {
					out = append(out, pkg+"."+ts.Name.Name)
				}
			}
		}
	}
	return out
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
