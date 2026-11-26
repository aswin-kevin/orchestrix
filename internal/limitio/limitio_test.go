package limitio_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/aswin-kevin/orchestrix/internal/limitio"
)

func TestReadAll(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		limit    int64
		wantBody string
		wantOver bool
	}{
		{"empty", "", 4, "", false},
		{"below limit", "abc", 4, "abc", false},
		{"exactly at limit", "abcd", 4, "abcd", false},
		{"limit plus one", "abcde", 4, "abcd", true},
		{"far over limit", strings.Repeat("x", 100), 4, "xxxx", true},
		{"limit zero, empty", "", 0, "", false},
		{"limit zero, one byte", "a", 0, "", true},
		{"max limit", "abcde", math.MaxInt64, "abcde", false},
		{"negative limit, empty", "", -5, "", false},
		{"negative limit, one byte", "a", -5, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, over, err := limitio.ReadAll(strings.NewReader(tt.input), tt.limit)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(b) != tt.wantBody || over != tt.wantOver {
				t.Errorf("ReadAll = (%q, %v), want (%q, %v)", b, over, tt.wantBody, tt.wantOver)
			}
		})
	}
}

func TestReadAllError(t *testing.T) {
	boom := errors.New("boom")
	b, over, err := limitio.ReadAll(iotest.ErrReader(boom), 4)
	if !errors.Is(err, boom) || b != nil || over {
		t.Errorf("ReadAll = (%q, %v, %v), want (nil, false, boom)", b, over, err)
	}
}
