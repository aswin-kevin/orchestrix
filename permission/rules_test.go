package permission

import "testing"

func TestParseRule(t *testing.T) {
	tests := []struct {
		in      string
		want    Rule
		wantErr bool
	}{
		{in: "execute", want: Rule{Tool: "execute"}},
		{in: "  execute  ", want: Rule{Tool: "execute"}},
		{in: "execute(git:*)", want: Rule{Tool: "execute", Pattern: "git:*"}},
		{in: "write(/secrets/**)", want: Rule{Pattern: "/secrets/**", Op: OpWrite}},
		{in: "read(/etc/**)", want: Rule{Pattern: "/etc/**", Op: OpRead}},
		{in: "read", want: Rule{Tool: "read"}},
		{in: "Read(/x)", want: Rule{Tool: "Read", Pattern: "/x"}},
		{in: "fetch(https://a.com/*)", want: Rule{Tool: "fetch", Pattern: "https://a.com/*"}},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "write(", wantErr: true},
		{in: "execute(git", wantErr: true},
		{in: "execute)", wantErr: true},
		{in: "execute()", wantErr: true},
		{in: "write()", wantErr: true},
		{in: "(x)", wantErr: true},
		{in: "execute(a)b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRule(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRule(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseRule(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestToolPatternPrefix(t *testing.T) {
	rule := mustRule(t, "execute(git:*)")
	tests := []struct {
		subject string
		want    bool
	}{
		{"git", true},
		{"git status", true},
		{"git commit -m x", true},
		{"gitx", false},
		{"gitx status", false},
		{"echo git", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			got := rule.Matches(Request{Tool: "execute", Subject: tt.subject})
			if got != tt.want {
				t.Errorf("Matches(subject %q) = %v, want %v", tt.subject, got, tt.want)
			}
		})
	}
}

func TestToolRuleMatching(t *testing.T) {
	tests := []struct {
		name string
		rule string
		req  Request
		want bool
	}{
		{"bare name matches any input", "execute", Request{Tool: "execute", Subject: "rm -rf /"}, true},
		{"name is case sensitive", "execute", Request{Tool: "Execute"}, false},
		{"name must be exact", "exec", Request{Tool: "execute"}, false},
		{"wildcard crosses slash", "fetch(https://a.com/*)", Request{Tool: "fetch", Subject: "https://a.com/x/y"}, true},
		{"wildcard mismatch", "fetch(https://a.com/*)", Request{Tool: "fetch", Subject: "https://b.com/x"}, false},
		{"exact pattern", "execute(ls)", Request{Tool: "execute", Subject: "ls"}, true},
		{"exact pattern is not a prefix", "execute(ls)", Request{Tool: "execute", Subject: "ls -la"}, false},
		{"pattern for other tool", "execute(ls)", Request{Tool: "fetch", Subject: "ls"}, false},
		{"tool rule ignores paths", "execute", Request{Tool: "other", Paths: []PathAccess{{"/a", OpRead}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRule(t, tt.rule).Matches(tt.req); got != tt.want {
				t.Errorf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPathRuleMatching(t *testing.T) {
	tests := []struct {
		name  string
		rule  string
		paths []PathAccess
		want  bool
	}{
		{"write rule matches write", "write(/secrets/**)", []PathAccess{{"/secrets/a/b", OpWrite}}, true},
		{"write rule ignores read", "write(/secrets/**)", []PathAccess{{"/secrets/a/b", OpRead}}, false},
		{"read rule ignores write", "read(/etc/**)", []PathAccess{{"/etc/passwd", OpWrite}}, false},
		{"any path may match", "write(/secrets/**)", []PathAccess{{"/tmp/x", OpWrite}, {"/secrets/k", OpWrite}}, true},
		{"op and path must match on the same access", "write(/secrets/**)", []PathAccess{{"/tmp/x", OpWrite}, {"/secrets/k", OpRead}}, false},
		{"no paths", "write(/secrets/**)", nil, false},
		{"access path is cleaned", "write(/secrets/**)", []PathAccess{{"/tmp/../secrets/k", OpWrite}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustRule(t, tt.rule).Matches(Request{Tool: "write_file", Paths: tt.paths})
			if got != tt.want {
				t.Errorf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustRule(t *testing.T, s string) Rule {
	t.Helper()
	r, err := ParseRule(s)
	if err != nil {
		t.Fatalf("ParseRule(%q): %v", s, err)
	}
	return r
}
