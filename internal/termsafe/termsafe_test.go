package termsafe

import "testing"

func TestString(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain text", "ls -la", "ls -la"},
		{"newline and tab kept", "a\n\tb", "a\n\tb"},
		{"escape sequence", "\x1b[31mred", `\x1b[31mred`},
		{"carriage return", "safe\rrm -rf /", `safe\rrm -rf /`},
		{"bell and delete", "a\x07b\x7f", `a\x07b\x7f`},
		{"C1 control", "a\u009b31m", `a\u009b31m`},
		{"bidi override", "a\u202eb", `a\u202eb`},
		{"invalid UTF-8", "a\x9b31mb", `a\x9b31mb`},
		{"raw C1 byte", "\x85", `\x85`},
		{"cut multi-byte sequence", "a\xe2\x80", `a\xe2\x80`},
		{"arabic letter mark", "a\u061cb", `a\u061cb`},
		{"zero-width space", "a\u200bb", `a\u200bb`},
		{"zero-width joiner", "a\u200db", `a\u200db`},
		{"word joiner", "a\u2060b", `a\u2060b`},
		{"byte order mark", "\ufeffa", `\ufeffa`},
		{"unicode text kept", "héllo ✓", "héllo ✓"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.in); got != tc.want {
				t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
