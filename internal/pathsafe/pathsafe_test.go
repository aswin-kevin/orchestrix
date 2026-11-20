package pathsafe

import (
	"regexp"
	"strings"
	"testing"
)

func TestSegmentKeepsSafeIDs(t *testing.T) {
	for _, id := range []string{"call_1", "t1", "a-b_9"} {
		if got := Segment(id); got != id {
			t.Errorf("Segment(%q) = %q, want it unchanged", id, got)
		}
	}
}

func TestSegmentSanitizesAndDisambiguates(t *testing.T) {
	got := Segment("../a b/c")
	if !regexp.MustCompile(`^___a_b_c-[0-9a-f]{8}$`).MatchString(got) {
		t.Fatalf("Segment(../a b/c) = %q, want ___a_b_c-<8 hex>", got)
	}
	if Segment("a/b") == Segment("a_b") {
		t.Fatalf("a/b and a_b map to the same segment %q", Segment("a/b"))
	}
	if first, second := Segment("a/b"), Segment("a/b"); first != second {
		t.Fatal("Segment is not deterministic")
	}
}

func TestSegmentEmptyID(t *testing.T) {
	got := Segment("")
	if !regexp.MustCompile(`^` + emptyPlaceholder + `-[0-9a-f]{8}$`).MatchString(got) {
		t.Fatalf("Segment(\"\") = %q, want %s-<8 hex>", got, emptyPlaceholder)
	}
	if got == Segment(emptyPlaceholder) {
		t.Fatalf("empty id collides with the literal placeholder %q", emptyPlaceholder)
	}
}

func TestSegmentCaseOnlyIDsDistinct(t *testing.T) {
	upper, lower := Segment("Thread"), Segment("thread")
	if upper == lower {
		t.Fatalf("Thread and thread map to the same segment %q", upper)
	}
	if !regexp.MustCompile(`^thread-[0-9a-f]{8}$`).MatchString(upper) {
		t.Fatalf("Segment(Thread) = %q, want thread-<8 hex>", upper)
	}
}

func TestSegmentLongIDBounded(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := Segment(long)
	if len(got) != maxSafeChars+1+hashChars || !strings.HasPrefix(got, strings.Repeat("a", maxSafeChars)+"-") {
		t.Fatalf("Segment(500 x a) = %q (%d chars), want 64 chars + -<8 hex>", got, len(got))
	}
	if got == Segment(long+"a") {
		t.Fatal("long ids sharing a prefix map to the same segment")
	}
	if exact := strings.Repeat("a", maxSafeChars); Segment(exact) != exact {
		t.Fatal("an id of exactly the cap must stay unchanged")
	}
}
