package orxotel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// TestOwnKeysCoverSpanKeys checks that every span attribute key declared in
// attrs.go (each key* constant outside the fallback event's block, whose
// keys go on an event) is in ownKeys, so Config.Attributes cannot replace
// a key added later.
func TestOwnKeysCoverSpanKeys(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "attrs.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var names []string
		values := map[string]string{}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				names = append(names, n.Name)
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
					values[n.Name], _ = strconv.Unquote(lit.Value)
				}
			}
		}
		if slices.Contains(names, "eventFallback") {
			continue // the fallback event's own keys
		}
		for _, n := range names {
			if strings.HasPrefix(n, "key") {
				keys = append(keys, n)
				if !ownKeys[attribute.Key(values[n])] {
					t.Errorf("%s (%q) is not in ownKeys", n, values[n])
				}
			}
		}
	}
	if len(keys) != len(ownKeys) {
		t.Errorf("attrs.go declares %d span keys, ownKeys has %d", len(keys), len(ownKeys))
	}
}
