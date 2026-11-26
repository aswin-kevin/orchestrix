package permission

import "testing"

func TestStricter(t *testing.T) {
	for _, tt := range []struct {
		a, b, want Mode
	}{
		{ModePlan, ModeBypass, ModePlan},
		{ModeBypass, ModeDontAsk, ModeDontAsk},
		{ModeAcceptEdits, ModeDontAsk, ModeDontAsk},
		{ModeDontAsk, ModeAuto, ModeDontAsk},
		{ModeAuto, ModeDefault, ModeAuto},
		{ModeDefault, ModeAcceptEdits, ModeDefault},
		{ModeAcceptEdits, ModeBypass, ModeAcceptEdits},
		{ModeBypass, ModeBypass, ModeBypass},
		// Unset means ModeDefault.
		{"", ModeBypass, ModeDefault},
		{ModeAuto, "", ModeAuto},
		{"", "", ModeDefault},
		// An unknown mode fails closed.
		{"nope", ModeBypass, ModePlan},
		{ModeDefault, "nope", ModePlan},
	} {
		if got := Stricter(tt.a, tt.b); got != tt.want {
			t.Errorf("Stricter(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
		if got := Stricter(tt.b, tt.a); got != tt.want {
			t.Errorf("Stricter(%q, %q) = %q, want %q", tt.b, tt.a, got, tt.want)
		}
	}
}
