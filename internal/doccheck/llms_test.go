package doccheck

import (
	"bufio"
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// llmsFiles are the agent-facing documentation files at the repository root.
var llmsFiles = []string{"llms.txt", "llms-full.txt"}

// The link prefixes the llms files use for packages, README sections and
// examples.
const (
	pkgDocURL   = "https://pkg.go.dev/"
	readmeURL   = "https://github.com/aswin-kevin/orchestrix/blob/main/README.md#"
	examplesURL = "https://github.com/aswin-kevin/orchestrix/tree/main/examples/"
)

// apiIndex holds the exported API of every importable package of every
// module, keyed by package name: two packages with one name share an entry.
type apiIndex struct {
	// paths holds the import path of every package.
	paths map[string]bool
	// decls maps a package name to its exported top-level names.
	decls map[string]map[string]bool
	// members maps "pkg.Type" to its exported fields and methods.
	members map[string]map[string]bool
	// embeds maps "pkg.Type" to the "pkg.Type" keys whose members it
	// gains: embedded fields and interfaces, and the target of an alias
	// or a defined type.
	embeds map[string][]string
}

// TestLLMsFiles keeps llms.txt and llms-full.txt in step with the code: the
// Packages section of llms.txt lists every importable package, every
// pkg.Name and pkg.Type.Member reference in code resolves to an exported
// declaration, every linked README anchor exists, and llms.txt lists every
// example.
func TestLLMsFiles(t *testing.T) {
	idx := buildIndex(t)
	docs := map[string]string{}
	for _, name := range llmsFiles {
		docs[name] = readRepoFile(t, name)
	}

	t.Run("packages", func(t *testing.T) {
		listed := map[string]bool{}
		re := regexp.MustCompile(regexp.QuoteMeta(pkgDocURL) + `([^)\s]+)\)`)
		for _, m := range re.FindAllStringSubmatch(section(docs["llms.txt"], "Packages"), -1) {
			listed[m[1]] = true
		}
		for _, p := range sortedKeys(idx.paths) {
			if !listed[p] {
				t.Errorf("llms.txt: Packages section does not list %s", p)
			}
		}
		for _, p := range sortedKeys(listed) {
			if !idx.paths[p] {
				t.Errorf("llms.txt: Packages section lists %s, which is not an importable package", p)
			}
		}
	})

	t.Run("identifiers", func(t *testing.T) {
		for _, name := range llmsFiles {
			for _, ref := range codeRefs(docs[name]) {
				if idx.decls[ref.parts[0]] == nil {
					continue // a local variable or a standard library package
				}
				if !idx.resolves(ref.parts) {
					t.Errorf("%s:%d: %s does not name an exported declaration",
						name, ref.line, strings.Join(ref.parts, "."))
				}
			}
		}
	})

	t.Run("anchors", func(t *testing.T) {
		anchors := headingAnchors(readRepoFile(t, "README.md"))
		re := regexp.MustCompile(regexp.QuoteMeta(readmeURL) + `([^)\s]+)\)`)
		for _, name := range llmsFiles {
			for i, line := range strings.Split(docs[name], "\n") {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					if !anchors[m[1]] {
						t.Errorf("%s:%d: README.md has no heading for #%s", name, i+1, m[1])
					}
				}
			}
		}
	})

	t.Run("examples", func(t *testing.T) {
		entries, err := os.ReadDir(filepath.Join("..", "..", "examples"))
		if err != nil {
			t.Fatal(err)
		}
		listed := section(docs["llms.txt"], "Examples")
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "internal" {
				continue
			}
			if !strings.Contains(listed, examplesURL+e.Name()+")") {
				t.Errorf("llms.txt: Examples section does not list examples/%s", e.Name())
			}
		}
		re := regexp.MustCompile(regexp.QuoteMeta(examplesURL) + `([^)\s]+)\)`)
		for _, m := range re.FindAllStringSubmatch(listed, -1) {
			if fi, err := os.Stat(filepath.Join("..", "..", "examples", m[1])); err != nil || !fi.IsDir() {
				t.Errorf("llms.txt: Examples section lists examples/%s, which does not exist", m[1])
			}
		}
	})
}

// readRepoFile returns the content of a file at the repository root.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// modulePath returns the module path declared in root's go.mod.
func modulePath(t *testing.T, root string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "module "); ok {
			return strings.TrimSpace(p)
		}
	}
	t.Fatalf("%s: no module line", filepath.Join(root, "go.mod"))
	return ""
}

// buildIndex parses every importable package of every module.
func buildIndex(t *testing.T) *apiIndex {
	t.Helper()
	idx := &apiIndex{
		paths:   map[string]bool{},
		decls:   map[string]map[string]bool{},
		members: map[string]map[string]bool{},
		embeds:  map[string][]string{},
	}
	for _, root := range moduleRoots {
		mod := modulePath(t, root)
		walkPackages(t, root, func(dir, pkg string, f *ast.File) {
			rel, err := filepath.Rel(root, dir)
			if err != nil {
				t.Fatal(err)
			}
			p := mod
			if rel != "." {
				p += "/" + filepath.ToSlash(rel)
			}
			idx.paths[p] = true
			idx.addFile(pkg, f)
		})
	}
	return idx
}

// addFile records the exported declarations of one file of package pkg.
func (idx *apiIndex) addFile(pkg string, f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv == nil || len(d.Recv.List) == 0 {
				idx.add(idx.decls, pkg, d.Name.Name)
				continue
			}
			if base := embeddedName(d.Recv.List[0].Type); ast.IsExported(base) {
				idx.add(idx.members, pkg+"."+base, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							idx.add(idx.decls, pkg, n.Name)
						}
					}
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						idx.add(idx.decls, pkg, s.Name.Name)
						idx.addType(pkg, pkg+"."+s.Name.Name, s.Type)
					}
				}
			}
		}
	}
}

// addType records the fields, methods and embedded types of type key.
func (idx *apiIndex) addType(pkg, key string, expr ast.Expr) {
	switch x := expr.(type) {
	case *ast.StructType:
		for _, fld := range x.Fields.List {
			if len(fld.Names) == 0 {
				idx.addEmbed(pkg, key, fld.Type)
			}
			for _, n := range fld.Names {
				if n.IsExported() {
					idx.add(idx.members, key, n.Name)
				}
			}
		}
	case *ast.InterfaceType:
		for _, m := range x.Methods.List {
			if len(m.Names) == 0 {
				idx.addEmbed(pkg, key, m.Type)
			}
			for _, n := range m.Names {
				if n.IsExported() {
					idx.add(idx.members, key, n.Name)
				}
			}
		}
	default:
		idx.addEmbed(pkg, key, expr)
	}
}

// addEmbed records that key gains the members of the named type expr, an
// embedded field, an embedded interface or an alias target.
func (idx *apiIndex) addEmbed(pkg, key string, expr ast.Expr) {
	switch x := expr.(type) {
	case *ast.StarExpr:
		idx.addEmbed(pkg, key, x.X)
	case *ast.IndexExpr:
		idx.addEmbed(pkg, key, x.X)
	case *ast.IndexListExpr:
		idx.addEmbed(pkg, key, x.X)
	case *ast.Ident:
		idx.embeds[key] = append(idx.embeds[key], pkg+"."+x.Name)
		idx.add(idx.members, key, x.Name)
	case *ast.SelectorExpr:
		if p, ok := x.X.(*ast.Ident); ok {
			idx.embeds[key] = append(idx.embeds[key], p.Name+"."+x.Sel.Name)
			idx.add(idx.members, key, x.Sel.Name)
		}
	}
}

func (idx *apiIndex) add(m map[string]map[string]bool, key, name string) {
	if m[key] == nil {
		m[key] = map[string]bool{}
	}
	m[key][name] = true
}

// resolves reports whether parts, a package name followed by one or two
// exported names, names a declaration: pkg.Name, or pkg.Type.Member where
// Member is a field or method of Type or of a type Type embeds.
func (idx *apiIndex) resolves(parts []string) bool {
	if !idx.decls[parts[0]][parts[1]] {
		return false
	}
	return len(parts) == 2 || idx.hasMember(parts[0]+"."+parts[1], parts[2], map[string]bool{})
}

func (idx *apiIndex) hasMember(key, name string, seen map[string]bool) bool {
	if seen[key] {
		return false
	}
	seen[key] = true
	if idx.members[key][name] {
		return true
	}
	for _, e := range idx.embeds[key] {
		if idx.hasMember(e, name, seen) {
			return true
		}
	}
	return false
}

// codeRef is a pkg.Name or pkg.Type.Member reference found in code.
type codeRef struct {
	line  int
	parts []string
}

// refPattern matches pkg.Name and pkg.Type.Member: a lowercase package name
// not preceded by a name character, a dot or a slash, then one or two
// exported names.
var refPattern = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_./])([a-z][a-z0-9]*)\.([A-Z][A-Za-z0-9_]*)(?:\.([A-Z][A-Za-z0-9_]*))?`)

// codeRefs returns the references inside the fenced code blocks and inline
// code spans of a Markdown document. Their first part may name no package
// of the index (a local variable, a standard library package); the caller
// skips those.
func codeRefs(doc string) []codeRef {
	var refs []codeRef
	inFence := false
	span := regexp.MustCompile("`[^`]+`")
	for i, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		code := []string{line}
		if !inFence {
			code = span.FindAllString(line, -1)
		}
		for _, c := range code {
			for _, m := range refPattern.FindAllStringSubmatch(c, -1) {
				parts := []string{m[1], m[2]}
				if m[3] != "" {
					parts = append(parts, m[3])
				}
				refs = append(refs, codeRef{line: i + 1, parts: parts})
			}
		}
	}
	return refs
}

// section returns the body of the "## title" section of a Markdown
// document, up to the next level-two heading.
func section(doc, title string) string {
	_, body, ok := strings.Cut(doc, "\n## "+title+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(body, "\n## "); i >= 0 {
		body = body[:i]
	}
	return body
}

// headingAnchors returns the anchors GitHub gives the headings of a
// Markdown document: lowercased, with characters other than letters, digits,
// spaces, hyphens and underscores removed, spaces turned into hyphens, and
// "-1", "-2", ... appended to repeats.
func headingAnchors(doc string) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	inFence := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		if inFence || hashes == 0 || hashes > 6 || !strings.HasPrefix(line[hashes:], " ") {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(line[hashes:])) {
			switch {
			case r == ' ':
				b.WriteRune('-')
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
				b.WriteRune(r)
			}
		}
		a := b.String()
		if n := seen[a]; n > 0 {
			a = fmt.Sprintf("%s-%d", a, n)
		}
		seen[b.String()]++
		anchors[a] = true
	}
	return anchors
}

// sortedKeys returns the keys of m in order.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
