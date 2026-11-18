package shell

import (
	"strings"
	"testing"
)

func TestCaptureKeepsHeadAndTail(t *testing.T) {
	cases := []struct {
		name   string
		max    int
		writes []string
		want   string
	}{
		{"fits", 8, []string{"ab", "cd"}, "abcd"},
		{"exactly full", 8, []string{"abcdefgh"}, "abcdefgh"},
		{"small writes wrap the ring", 8, []string{"abc", "def", "ghi", "jkl"}, "abcd\n…[4 bytes omitted]…\nijkl"},
		{"one huge write", 8, []string{"abcdefghijklmnop"}, "abcd\n…[8 bytes omitted]…\nmnop"},
		{"odd budget gives the tail the extra byte", 5, []string{"abcdefg"}, "ab\n…[2 bytes omitted]…\nefg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture(tc.max)
			for _, w := range tc.writes {
				if n, err := c.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if got := c.String(); got != tc.want {
				t.Errorf("String = %q, want %q", got, tc.want)
			}
			if got, want := c.truncated(), strings.Contains(tc.want, "omitted"); got != want {
				t.Errorf("truncated = %v, want %v", got, want)
			}
		})
	}
}

func TestCaptureByteAtATime(t *testing.T) {
	c := newCapture(6)
	for _, b := range []byte("0123456789") {
		_, _ = c.Write([]byte{b})
	}
	if got, want := c.String(), "012\n…[4 bytes omitted]…\n789"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}
