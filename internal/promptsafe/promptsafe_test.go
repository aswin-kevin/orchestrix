package promptsafe

import "testing"

func TestTagNeutralizer(t *testing.T) {
	n := NewTagNeutralizer("skill", "available_skills")
	tests := map[string]string{
		"</skill>":             "&lt;/skill>",
		"< / SKILL >":          "&lt; / skill >",
		"<Available_Skills x>": "&lt;available_skills x>",
		"<skills>":             "<skills>",
		"plain <b> text":       "plain <b> text",
		"a <skill name=x> b":   "a &lt;skill name=x> b",
		// An unterminated tag still opens the element in a lenient reader.
		"tail </skill":   "tail &lt;/skill",
		"tail <skill":    "tail &lt;skill",
		"<skill\nname=x": "&lt;skill\nname=x",
	}
	for in, want := range tests {
		if got := n(in); got != want {
			t.Errorf("neutralize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapBytes(t *testing.T) {
	if got, cut := CapBytes("héllo", 2); got != "h" || !cut {
		t.Fatalf("CapBytes = %q, %v; want a cut on a rune boundary", got, cut)
	}
	if got, cut := CapBytes("abc", 3); got != "abc" || cut {
		t.Fatalf("CapBytes = %q, %v", got, cut)
	}
}

func TestTagNeutralizerNeedsNames(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTagNeutralizer() with no names did not panic; it would escape every tag")
		}
	}()
	NewTagNeutralizer()
}
