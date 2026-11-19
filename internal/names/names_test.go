package names

import "testing"

func TestValidate(t *testing.T) {
	for _, ok := range []string{"read", "mcp__srv__tool", "a.b:c-d_e", "T1"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "write (/x)", "a b", "é", "a/b"} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil, want error", bad)
		}
	}
}

func TestSanitizeAndMCPPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"get.file/v2": "get_file_v2",
		"ok_Name-9":   "ok_Name-9",
		"é b":         "__b",
		"":            "",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MCPPrefix("my.srv"); got != "mcp__my_srv__" {
		t.Errorf("MCPPrefix(my.srv) = %q", got)
	}
}
