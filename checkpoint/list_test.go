package checkpoint_test

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

func ids(infos []checkpoint.ThreadInfo) []string {
	out := make([]string, len(infos))
	for i, in := range infos {
		out[i] = in.ID
	}
	return out
}

// walk pages through threads with the given limit and returns each page's ids
// and the Next cursor of the last page.
func walk(t *testing.T, threads []checkpoint.ThreadInfo, limit int) ([][]string, string) {
	t.Helper()
	var pages [][]string
	cursor := ""
	for range 100 {
		p, err := checkpoint.Paginate(threads, checkpoint.ListOptions{Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, ids(p.Threads))
		if p.Next == "" {
			return pages, p.Next
		}
		cursor = p.Next
	}
	t.Fatal("paging did not terminate")
	return nil, ""
}

func TestPaginate(t *testing.T) {
	base := time.Unix(1000, 0)
	var threads []checkpoint.ThreadInfo
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		threads = append(threads, checkpoint.ThreadInfo{ID: id, Updated: base.Add(time.Duration(i) * time.Second)})
	}
	slices.Reverse(threads[1:3]) // input order must not matter
	pages, next := walk(t, threads, 2)
	want := [][]string{{"e", "d"}, {"c", "b"}, {"a"}}
	if !slices.EqualFunc(pages, want, slices.Equal) || next != "" {
		t.Fatalf("pages = %v next %q, want %v", pages, next, want)
	}
}

func TestPaginateTies(t *testing.T) {
	at := time.Unix(1000, 5)
	threads := []checkpoint.ThreadInfo{{ID: "b", Updated: at}, {ID: "a", Updated: at}, {ID: "c", Updated: at}}
	pages, _ := walk(t, threads, 1)
	want := [][]string{{"a"}, {"b"}, {"c"}}
	if !slices.EqualFunc(pages, want, slices.Equal) {
		t.Fatalf("pages = %v, want %v", pages, want)
	}
}

func TestPaginateDefaultLimit(t *testing.T) {
	var threads []checkpoint.ThreadInfo
	for i := range checkpoint.DefaultListLimit + 5 {
		threads = append(threads, checkpoint.ThreadInfo{ID: string(rune('A' + i)), Updated: time.Unix(int64(i), 0)})
	}
	p, err := checkpoint.Paginate(threads, checkpoint.ListOptions{})
	if err != nil || len(p.Threads) != checkpoint.DefaultListLimit || p.Next == "" {
		t.Fatalf("Paginate = %d threads, next %q, %v", len(p.Threads), p.Next, err)
	}
}

func TestPaginateDoesNotModifyInput(t *testing.T) {
	threads := []checkpoint.ThreadInfo{{ID: "a", Updated: time.Unix(1, 0)}, {ID: "b", Updated: time.Unix(2, 0)}}
	if _, err := checkpoint.Paginate(threads, checkpoint.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if threads[0].ID != "a" {
		t.Fatal("Paginate sorted its input")
	}
}

func TestListCursorRoundTrip(t *testing.T) {
	in := checkpoint.ThreadInfo{ID: "x\x01/é", Updated: time.Unix(1700000000, 123456789)}
	updated, id, err := checkpoint.ParseListCursor(checkpoint.ListCursor(in))
	if err != nil {
		t.Fatal(err)
	}
	if id != in.ID || !updated.Equal(in.Updated) {
		t.Fatalf("round trip = %v %q, want %v %q", updated, id, in.Updated, in.ID)
	}
	for _, bad := range []string{"!!", "bm8tbnVsCg"} {
		if _, _, err := checkpoint.ParseListCursor(bad); !errors.Is(err, checkpoint.ErrInvalidCursor) {
			t.Errorf("ParseListCursor(%q) = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

func TestPaginateInvalidCursor(t *testing.T) {
	_, err := checkpoint.Paginate(nil, checkpoint.ListOptions{Cursor: "!!"})
	if !errors.Is(err, checkpoint.ErrInvalidCursor) {
		t.Fatalf("err = %v, want ErrInvalidCursor", err)
	}
}

func TestPaginateNegativeLimit(t *testing.T) {
	if _, err := checkpoint.Paginate(nil, checkpoint.ListOptions{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestPaginateEmptyIsNonNil(t *testing.T) {
	for name, threads := range map[string][]checkpoint.ThreadInfo{"nil": nil, "empty": {}} {
		p, err := checkpoint.Paginate(threads, checkpoint.ListOptions{})
		if err != nil || p.Threads == nil || len(p.Threads) != 0 || p.Next != "" {
			t.Errorf("%s: Paginate = %#v, %v, want non-nil empty Threads", name, p, err)
		}
	}
}

func TestListOptionsPageLimit(t *testing.T) {
	for _, tc := range []struct {
		limit, want int
	}{
		{0, checkpoint.DefaultListLimit},
		{1, 1},
		{7, 7},
		{math.MaxInt, math.MaxInt},
	} {
		got, err := checkpoint.ListOptions{Limit: tc.limit}.PageLimit()
		if err != nil || got != tc.want {
			t.Errorf("PageLimit(%d) = %d, %v, want %d", tc.limit, got, err, tc.want)
		}
	}
	for _, limit := range []int{-1, math.MinInt} {
		if _, err := (checkpoint.ListOptions{Limit: limit}).PageLimit(); err == nil || !strings.Contains(err.Error(), "checkpoint: Limit is negative") {
			t.Errorf("PageLimit(%d) error = %v, want a negative-limit error", limit, err)
		}
	}
}
