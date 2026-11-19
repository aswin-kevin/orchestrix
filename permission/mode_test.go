package permission

import "testing"

func TestStricter(t *testing.T) {
	for _, tc := range []struct {
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
		if got := Stricter(tc.a, tc.b); got != tc.want {
			t.Errorf("Stricter(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
		if got := Stricter(tc.b, tc.a); got != tc.want {
			t.Errorf("Stricter(%q, %q) = %q, want %q", tc.b, tc.a, got, tc.want)
		}
	}
}
