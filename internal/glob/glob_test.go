package glob

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"literal equal", "/a/b", "/a/b", true},
		{"literal differs", "/a/b", "/a/c", false},
		{"star within segment", "/a/*.go", "/a/main.go", true},
		{"star does not cross slash", "/a/*", "/a/b/c", false},
		{"star matches empty", "/a/*x", "/a/x", true},
		{"star in middle of segment", "/a/f*o", "/a/foo", true},
		{"question matches one char", "/a/?.go", "/a/b.go", true},
		{"question needs a char", "/a/?", "/a/", false},
		{"question does not match slash", "/a?b", "/a/b", false},
		{"double star at end matches the dir itself", "/a/**", "/a", true},
		{"double star at end matches child", "/a/**", "/a/b", true},
		{"double star at end matches deep", "/a/**", "/a/b/c/d", true},
		{"double star at end rejects sibling prefix", "/a/**", "/ab/c", false},
		{"double star in middle matches zero segments", "/a/**/c", "/a/c", true},
		{"double star in middle matches many", "/a/**/c", "/a/x/y/c", true},
		{"double star in middle needs tail", "/a/**/c", "/a/x/y", false},
		{"double star at start", "**/c.go", "/a/b/c.go", true},
		{"double star at start relative", "**/c.go", "c.go", true},
		{"double star alone matches everything", "/**", "/a/b", true},
		{"double star is whole segment only", "/a/b**", "/a/b/c", false},
		{"double star glued to text acts as star", "/a/b**", "/a/bcd", true},
		{"two double stars", "/**/x/**", "/a/x/b", true},
		{"paths are cleaned", "/a/b", "/a/./c/../b", true},
		{"traversal cannot escape rule", "/secrets/**", "/secrets/../etc/passwd", false},
		{"traversal into rule is caught", "/secrets/**", "/tmp/../secrets/key", true},
		{"pattern is cleaned", "/a//b/", "/a/b", true},
		{"trailing slash on path", "/a/b", "/a/b/", true},
		{"relative vs absolute differ", "a/b", "/a/b", false},
		{"unicode question", "/a/?", "/a/é", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(tt.pattern, tt.path); got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}
