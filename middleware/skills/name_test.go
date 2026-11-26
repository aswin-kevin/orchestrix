package skills

import "testing"

// The skills middleware is what subagents look up by Name and query
// through Definer.
var _ Definer = (*Middleware)(nil)

func TestName(t *testing.T) {
	m, err := New(Config{Skills: []Skill{{Name: "pdf", Description: "PDFs."}}})
	if err != nil {
		t.Fatal(err)
	}
	if Name != "skills" || m.Name() != Name {
		t.Fatalf("Name = %q, m.Name() = %q", Name, m.Name())
	}
	if !m.Has("pdf") || m.Has("ghost") {
		t.Fatal("Has does not report the defined skills")
	}
}
