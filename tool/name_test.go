package tool

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"read_file", false},
		{"", true},
		{"a b", true},
		{"mcp:x.y-z", false},
	}
	for _, tt := range tests {
		err := ValidName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidName(%q) = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "tool: invalid name") {
			t.Errorf("ValidName(%q) error %q lacks the tool: invalid name prefix", tt.name, err)
		}
	}
}
