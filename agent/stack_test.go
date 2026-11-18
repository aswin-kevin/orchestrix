package agent

import (
	"reflect"
	"strings"
	"testing"
)

// named is a middleware with a configurable name and an id to tell
// same-named instances apart.
type named struct {
	name string
	id   int
}

func (n named) Name() string { return n.name }

func names(mws []Middleware) []string {
	out := make([]string, len(mws))
	for i, m := range mws {
		out[i] = m.Name()
	}
	return out
}

func TestComposeReplaceInPlace(t *testing.T) {
	base := []Middleware{named{"a", 0}, named{"b", 0}, named{"c", 0}}
	user := []Middleware{named{"x", 1}, named{"b", 1}}

	got, err := Compose(base, user, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c", "x"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("names = %v, want %v", names(got), want)
	}
	if got[1].(named).id != 1 {
		t.Fatal("user middleware did not replace base \"b\"")
	}
	if base[1].(named).id != 0 || len(base) != 3 {
		t.Fatal("Compose mutated its base input")
	}
}

func TestComposeExclude(t *testing.T) {
	base := []Middleware{named{"a", 0}, named{"b", 0}, named{"c", 0}}

	got, err := Compose(base, nil, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("names = %v, want %v", names(got), want)
	}
}

func TestComposeErrors(t *testing.T) {
	base := []Middleware{named{"a", 0}}
	tests := []struct {
		name    string
		user    []Middleware
		exclude []string
		wantErr string
	}{
		{"unknown exclude", nil, []string{"nope"}, `"nope"`},
		{"duplicate user names", []Middleware{named{"x", 1}, named{"x", 2}}, nil, "duplicate"},
		{"nil user middleware", []Middleware{nil}, nil, "nil"},
		{"empty user name", []Middleware{named{"", 1}}, nil, "empty name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compose(base, tc.user, tc.exclude)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestComposeNilBaseEntry(t *testing.T) {
	if _, err := Compose([]Middleware{nil}, nil, nil); err == nil {
		t.Fatal("expected error for nil base middleware")
	}
}
