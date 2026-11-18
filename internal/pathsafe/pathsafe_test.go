package pathsafe

import (
	"regexp"
	"testing"
)

func TestSegmentKeepsSafeIDs(t *testing.T) {
	for _, id := range []string{"call_1", "t1", "A-b_9"} {
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
	if Segment("a/b") != Segment("a/b") {
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
