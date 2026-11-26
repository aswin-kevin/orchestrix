package backend_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
)

func TestOutputCaptureKeepsHeadAndTail(t *testing.T) {
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
		{"spec example", 10, []string{"0123456789abcdefghij"}, "01234\n…[10 bytes omitted]…\nfghij"},
		{"under budget", 10, []string{"abc"}, "abc"},
		{"zero keeps nothing", 0, []string{"abc"}, "\n…[3 bytes omitted]…\n"},
		{"negative keeps nothing", -5, []string{"abc"}, "\n…[3 bytes omitted]…\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := backend.NewOutputCapture(tc.max)
			for _, w := range tc.writes {
				if n, err := c.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if got := c.String(); got != tc.want {
				t.Errorf("String = %q, want %q", got, tc.want)
			}
			trunc := strings.Contains(tc.want, "omitted")
			if c.Truncated() != trunc {
				t.Errorf("Truncated = %v, want %v", c.Truncated(), trunc)
			}
			if (c.Omitted() > 0) != trunc {
				t.Errorf("Omitted = %d, want >0 = %v", c.Omitted(), trunc)
			}
		})
	}
}

func TestOutputCaptureOmitted(t *testing.T) {
	c := backend.NewOutputCapture(10)
	_, _ = c.Write([]byte("0123456789abcdefghij"))
	if got := c.Omitted(); got != 10 {
		t.Errorf("Omitted = %d, want 10", got)
	}
	u := backend.NewOutputCapture(10)
	_, _ = u.Write([]byte("abc"))
	if u.Omitted() != 0 || u.Truncated() {
		t.Errorf("under budget: Omitted = %d, Truncated = %v", u.Omitted(), u.Truncated())
	}
}

func TestOutputCaptureByteAtATime(t *testing.T) {
	c := backend.NewOutputCapture(10)
	for _, b := range []byte("0123456789abcdefghij") {
		_, _ = c.Write([]byte{b})
	}
	if got, want := c.String(), "01234\n…[10 bytes omitted]…\nfghij"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	d := backend.NewOutputCapture(6)
	for _, b := range []byte("0123456789") {
		_, _ = d.Write([]byte{b})
	}
	if got, want := d.String(), "012\n…[4 bytes omitted]…\n789"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestOutputCaptureConcurrent(t *testing.T) {
	c := backend.NewOutputCapture(256)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				_, _ = c.Write([]byte("x"))
				_ = c.String()
			}
		})
	}
	wg.Wait()
	if got := c.Omitted(); got != 8000-256 {
		t.Errorf("Omitted = %d, want %d", got, 8000-256)
	}
}
