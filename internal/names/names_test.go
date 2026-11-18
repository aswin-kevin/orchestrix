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
