package snippets

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Files are the Markdown files whose Go blocks are compiled, relative to the
// repository root.
var Files = []string{"README.md", "llms.txt", "llms-full.txt"}

// The modules a block can belong to; Core is the root module.
const (
	Core    = ""
	OrxOtel = "orxotel"
	OrxPg   = "orxpg"
)

// freeName is a declaration that stands in for a name a fragment says comes
// from surrounding code.
type freeName struct {
	// decl is the Go declaration.
	decl string
	// imports are the import paths decl needs, relative to the core module
	// when they start with "./".
	imports []string
}

// freeNames are the names fragments use without declaring them.
var freeNames = map[string]freeName{
	"ctx":    {"var ctx = context.Background()", []string{"context"}},
	"model":  {`var model = "anthropic:claude-sonnet-4-5"`, nil},
	"backup": {"var backup llm.Provider", []string{"./llm"}},
	"remote": {"var remote backend.Executor", []string{"./backend"}},
	"th":     {"var th *agent.Thread", []string{"./agent"}},
	"tm":     {"var tm *jobs.Middleware", []string{"./middleware/jobs"}},
	"m":      {"var m *mcp.Middleware", []string{"./mcp"}},
	"tp":     {"var tp trace.TracerProvider", []string{"go.opentelemetry.io/otel/trace"}},
	"d":      {"var d llm.Decider", []string{"./llm"}},
	"Triage": {"type Triage struct {\n\tSeverity string `json:\"severity\"`\n\tSummary  string `json:\"summary\"`\n}", nil},
}

// stdPackages maps the standard library package names fragments use to
// their import paths.
var stdPackages = map[string]string{
	"bufio": "bufio", "bytes": "bytes", "context": "context",
	"errors": "errors", "fmt": "fmt", "http": "net/http", "io": "io",
	"json": "encoding/json", "log": "log", "os": "os", "slog": "log/slog",
	"strings": "strings", "time": "time", "filepath": "path/filepath",
	"signal": "os/signal", "sync": "sync", "slices": "slices",
	"strconv": "strconv", "testing": "testing",
}

// thirdParty maps the names of the satellites' dependencies that fragments
// use to their import paths.
var thirdParty = map[string]string{
	"pgxpool":  "github.com/jackc/pgx/v5/pgxpool",
	"otel":     "go.opentelemetry.io/otel",
	"trace":    "go.opentelemetry.io/otel/trace",
	"sdktrace": "go.opentelemetry.io/otel/sdk/trace",
}

// Snippet is one Go block of a Markdown file.
type Snippet struct {
	// File is the Markdown file, relative to the repository root.
	File string
	// Line is the line of the block's opening fence.
	Line int
	// Code is the block's content.
	Code string
}

// String returns the block's location, file:line.
func (s Snippet) String() string { return fmt.Sprintf("%s:%d", s.File, s.Line) }

// Extract returns the Go blocks of a Markdown document: those whose opening
// fence is exactly ```go.
func Extract(file, doc string) []Snippet {
	var out []Snippet
	var cur *Snippet
	var body []string
	for i, line := range strings.Split(doc, "\n") {
		line = strings.TrimRight(line, " \r")
		switch {
		case cur == nil && line == "```go":
			cur = &Snippet{File: file, Line: i + 1}
			body = nil
		case cur != nil && strings.HasPrefix(line, "```"):
			cur.Code = strings.Join(body, "\n") + "\n"
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			body = append(body, line)
		}
	}
	return out
}

// pkg is an importable package of the repository.
type pkg struct {
	path    string
	exports map[string]bool
}

// resolver turns the names a fragment leaves undeclared into imports.
type resolver struct {
	core string
	// pkgs maps a package name to the packages of that name.
	pkgs map[string][]pkg
}

// newResolver indexes the importable packages under root: the core module
// and its satellites, without internal packages, commands, examples and
// test data.
func newResolver(root, core string) (*resolver, error) {
	r := &resolver{core: core, pkgs: map[string][]pkg{}}
	skip := map[string]bool{"internal": true, "cmd": true, "examples": true,
		"testdata": true, "evals": true, "docs": true, "vendor": true}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (skip[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var name string
		exports := map[string]bool{}
		fset := token.NewFileSet()
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(path, n), nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			name = f.Name.Name
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil {
						exports[d.Name.Name] = true
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							exports[s.Name.Name] = true
						case *ast.ValueSpec:
							for _, n := range s.Names {
								exports[n.Name] = true
							}
						}
					}
				}
			}
		}
		if name == "" || name == "main" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		p := core
		if rel != "." {
			p += "/" + filepath.ToSlash(rel)
		}
		r.pkgs[name] = append(r.pkgs[name], pkg{path: p, exports: exports})
		return nil
	})
	return r, err
}

// importPath returns the import path for name, a package name a fragment
// selects the names sel from, or "" when no package has that name.
func (r *resolver) importPath(name string, sel map[string]bool) (string, error) {
	if cands := r.pkgs[name]; len(cands) > 0 {
		var fit []string
		for _, c := range cands {
			if len(sel) > 0 && !hasAll(c.exports, sel) {
				continue
			}
			fit = append(fit, c.path)
		}
		if len(fit) != 1 {
			return "", fmt.Errorf("package name %s fits %d packages (%s)",
				name, len(fit), strings.Join(fit, ", "))
		}
		return fit[0], nil
	}
	if p := stdPackages[name]; p != "" {
		return p, nil
	}
	return thirdParty[name], nil
}

// hasAll reports whether every key of sub is in set.
func hasAll(set, sub map[string]bool) bool {
	for k := range sub {
		if !set[k] {
			return false
		}
	}
	return true
}

// unit is one snippet ready to compile: the files of its package.
type unit struct {
	snip    Snippet
	files   map[string]string
	imports []string
}

// module returns the module whose test compiles u.
func (u unit) module(core string) (string, error) {
	mods := map[string]bool{}
	for _, p := range u.imports {
		switch {
		case p == core+"/orxotel" || strings.HasPrefix(p, "go.opentelemetry.io/"):
			mods[OrxOtel] = true
		case p == core+"/orxpg" || strings.HasPrefix(p, "github.com/jackc/"):
			mods[OrxPg] = true
		}
	}
	switch len(mods) {
	case 0:
		return Core, nil
	case 1:
		return slices.Collect(maps.Keys(mods))[0], nil
	}
	return "", fmt.Errorf("imports both orxotel and orxpg; split the block")
}

// prepare wraps s into the files of package name.
func (r *resolver) prepare(s Snippet, name string) (unit, error) {
	u := unit{snip: s, files: map[string]string{}}
	code := s.Code
	if strings.HasPrefix(code, "package ") {
		file := "main.go"
		if strings.Contains(code, "\nfunc Test") {
			file = "main_test.go"
		}
		u.files[file] = code
		return u, u.collectImports(code)
	}
	src := "package " + name + "\n\n" + code
	if f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly); err == nil && len(f.Imports) > 0 {
		file := "snippet.go"
		if strings.Contains(code, "\nfunc Test") || strings.HasPrefix(code, "func Test") {
			file = "snippet_test.go"
			u.files["doc.go"] = "package " + name + "\n"
		}
		u.files[file] = src
		return u, u.collectImports(src)
	}
	return u, r.wrapFragment(&u, name)
}

// collectImports records the import paths of a complete file.
func (u *unit) collectImports(src string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return err
	}
	for _, im := range f.Imports {
		u.imports = append(u.imports, strings.Trim(im.Path.Value, `"`))
	}
	return nil
}

// parsesAsDecls reports whether code is a list of top-level declarations.
func parsesAsDecls(code string) bool {
	_, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+code, parser.SkipObjectResolution)
	return err == nil
}

// stmtPrefix is what a fragment's statements are wrapped in to parse them.
const stmtPrefix = "package p\nfunc _() {\n"

// parseStmts parses code as the statements of a function body.
func parseStmts(code string) (*ast.FuncDecl, *token.FileSet, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", stmtPrefix+code+"\n}\n", parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	return f.Decls[0].(*ast.FuncDecl), fset, nil
}

// splitFragment splits code into leading declarations and the statements
// after them; either may be empty.
func splitFragment(code string) (decls, stmts string, err error) {
	if parsesAsDecls(code) {
		return code, "", nil
	}
	_, _, err = parseStmts(code)
	if err == nil {
		return "", code, nil
	}
	lines := strings.SplitAfter(code, "\n")
	for k := len(lines) - 1; k >= 1; k-- {
		d, s := strings.Join(lines[:k], ""), strings.Join(lines[k:], "")
		if parsesAsDecls(d) {
			if _, _, e := parseStmts(s); e == nil {
				return d, s, nil
			}
		}
	}
	return "", "", fmt.Errorf("neither declarations nor statements: %v", err)
}

// wrapStmts turns statements into a function body that uses every variable
// declared at its top level, and returns the function.
func wrapStmts(code string) (string, error) {
	if strings.TrimSpace(code) == "" {
		return "", nil
	}
	fn, fset, err := parseStmts(code)
	if err != nil {
		return "", err
	}
	returnsValue := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returnsValue = returnsValue || len(x.Results) > 0
		}
		return true
	})
	type insert struct {
		at   int
		text string
	}
	var ins []insert
	for _, st := range fn.Body.List {
		var names []string
		switch x := st.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, e := range x.Lhs {
					if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
						names = append(names, id.Name)
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					for _, id := range spec.(*ast.ValueSpec).Names {
						if id.Name != "_" {
							names = append(names, id.Name)
						}
					}
				}
			}
		}
		if len(names) > 0 {
			at := fset.Position(st.End()).Offset - len(stmtPrefix)
			ins = append(ins, insert{at, "; _ = " + strings.Join(names, "; _ = ")})
		}
	}
	for i := len(ins) - 1; i >= 0; i-- {
		code = code[:ins[i].at] + ins[i].text + code[ins[i].at:]
	}
	sig, tail := "func _() {\n", "}\n"
	if returnsValue {
		sig = "func _() error {\n"
		if _, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt); !ok {
			tail = "return nil\n}\n"
		}
	}
	return sig + code + tail, nil
}

// wrapFragment fills u with a fragment wrapped by the package's rules.
func (r *resolver) wrapFragment(u *unit, name string) error {
	decls, stmts, err := splitFragment(u.snip.Code)
	if err != nil {
		return err
	}
	body, err := wrapStmts(stmts)
	if err != nil {
		return err
	}
	src := decls + "\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", "package "+name+"\n\n"+src, 0)
	if err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				declared[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					declared[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						declared[n.Name] = true
					}
				}
			}
		}
	}
	sel := map[string]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok {
				if sel[id.Name] == nil {
					sel[id.Name] = map[string]bool{}
				}
				sel[id.Name][s.Sel.Name] = true
			}
		}
		return true
	})
	imports := map[string]bool{}
	freeImports := map[string]bool{}
	var free []string
	seen := map[string]bool{}
	for _, id := range f.Unresolved {
		n := id.Name
		if seen[n] || declared[n] || types.Universe.Lookup(n) != nil {
			continue
		}
		seen[n] = true
		if fn, ok := freeNames[n]; ok {
			free = append(free, fn.decl)
			for _, p := range fn.imports {
				if rest, ok := strings.CutPrefix(p, "./"); ok {
					p = r.core + "/" + rest
				}
				freeImports[p] = true
			}
			continue
		}
		p, err := r.importPath(n, sel[n])
		if err != nil {
			return err
		}
		if p == "" {
			return fmt.Errorf("undeclared name %s: declare it in the block or add it to freeNames", n)
		}
		imports[p] = true
	}
	u.files["snippet.go"] = "package " + name + "\n\n" + importDecl(imports) + src
	if len(free) > 0 {
		slices.Sort(free)
		u.files["free.go"] = "package " + name + "\n\n" + importDecl(freeImports) +
			strings.Join(free, "\n\n") + "\n"
	}
	u.imports = slices.Sorted(maps.Keys(imports))
	for p := range freeImports {
		u.imports = append(u.imports, p)
	}
	return nil
}

// importDecl returns an import declaration for paths, or "" for none.
func importDecl(paths map[string]bool) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("import (\n")
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		fmt.Fprintf(&b, "\t%q\n", p)
	}
	b.WriteString(")\n\n")
	return b.String()
}

// Check compiles, with go vet, the Go blocks of Files that belong to module
// (Core, OrxOtel or OrxPg), from root, the repository root. It skips t in
// short mode, when the go command is not on PATH, and when root lacks one
// of Files (a module tested outside a repository checkout). It fails t when
// a block of the module does not compile, when the module has no blocks, or
// (for Core) when a block cannot be wrapped.
func Check(t *testing.T, root, module string) {
	t.Helper()
	if testing.Short() {
		t.Skip("compiling the documentation's Go blocks is skipped in short mode")
	}
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go command not on PATH")
	}
	for _, file := range Files {
		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			t.Skipf("%s not found under %s: not a repository checkout", file, root)
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	core, goLine := readGoMod(t, filepath.Join(root, "go.mod"))
	r, err := newResolver(root, core)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var units []unit
	for _, file := range Files {
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range Extract(file, string(b)) {
			name := fmt.Sprintf("s%03d", len(units)+1)
			u, err := r.prepare(s, name)
			if err == nil {
				var m string
				m, err = u.module(core)
				if err == nil && m != module {
					continue
				}
			}
			if err != nil {
				if module == Core {
					t.Errorf("%s: %v", s, err)
				}
				continue
			}
			units = append(units, u)
		}
	}
	if len(units) == 0 {
		t.Fatalf("no Go blocks of %v belong to module %q", Files, module)
	}
	for i, u := range units {
		pd := filepath.Join(dir, fmt.Sprintf("s%03d", i+1))
		if err := os.MkdirAll(pd, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, src := range u.files {
			if err := os.WriteFile(filepath.Join(pd, name), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	modRoot := root
	gomod := "module snippetcheck\n\n" + goLine + "\n\n"
	deps := map[string]string{core: root}
	switch module {
	case Core:
		deps[core+"/orxdocker"] = filepath.Join(root, "orxdocker")
	default:
		modRoot = filepath.Join(root, module)
		deps[core+"/"+module] = modRoot
	}
	// The scratch module requires what the tested module requires, so the
	// go command loads the same pruned module graph the tested module's own
	// build already put in the module cache; without them it would resolve
	// the replaced modules' graphs afresh and need go.mod files that a cache
	// filled by go test ./... does not hold.
	for _, req := range readRequires(t, filepath.Join(modRoot, "go.mod")) {
		if _, ok := deps[req.path]; !ok {
			gomod += fmt.Sprintf("require %s %s\n", req.path, req.version)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(deps)) {
		gomod += fmt.Sprintf("require %s v0.0.0\nreplace %s => %s\n", p, p, filepath.ToSlash(deps[p]))
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(modRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), goCmd, "vet", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		reportFailures(t, units, out)
		t.Fatalf("go vet: %v", err)
	}
	t.Logf("compiled %d Go blocks", len(units))
}

// failedPkg matches the package directory at the start of a go vet message.
var failedPkg = regexp.MustCompile(`(?m)(?:^|[\s/])(s\d{3})[/\\]`)

// reportFailures maps go vet output back to the Markdown blocks it came
// from, and logs the generated files of each block that failed.
func reportFailures(t *testing.T, units []unit, out []byte) {
	t.Helper()
	t.Logf("go vet output:\n%s", out)
	failed := map[string]bool{}
	for _, m := range failedPkg.FindAllSubmatch(out, -1) {
		failed[string(m[1])] = true
	}
	for i, u := range units {
		if !failed[fmt.Sprintf("s%03d", i+1)] {
			continue
		}
		var b bytes.Buffer
		for _, name := range slices.Sorted(maps.Keys(u.files)) {
			fmt.Fprintf(&b, "--- s%03d/%s\n", i+1, name)
			for j, line := range strings.Split(u.files[name], "\n") {
				fmt.Fprintf(&b, "%4d  %s\n", j+1, line)
			}
		}
		t.Errorf("%s does not compile (package s%03d):\n%s", u.snip, i+1, b.String())
	}
}

// requirement is one module path and version from a go.mod require line.
type requirement struct {
	path, version string
}

// readRequires returns the requirements of a go.mod file, from single
// require lines and require blocks alike.
func readRequires(t *testing.T, path string) []requirement {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var reqs []requirement
	inBlock := false
	for line := range strings.Lines(string(b)) {
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case !inBlock:
			rest, ok := strings.CutPrefix(line, "require ")
			if !ok {
				continue
			}
			line = strings.TrimSpace(rest)
		}
		if f := strings.Fields(line); len(f) == 2 {
			reqs = append(reqs, requirement{path: f[0], version: f[1]})
		}
	}
	return reqs
}

// readGoMod returns the module path and the go line of a go.mod file.
func readGoMod(t *testing.T, path string) (module, goLine string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if p, ok := strings.CutPrefix(line, "module "); ok {
			module = strings.TrimSpace(p)
		}
		if strings.HasPrefix(line, "go ") {
			goLine = line
		}
	}
	if module == "" || goLine == "" {
		t.Fatalf("%s: no module or go line", path)
	}
	return module, goLine
}
