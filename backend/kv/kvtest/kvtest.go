package kvtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/kv"
)

// Options tunes RunWith for a store whose behaviour the default checks
// would reject wrongly.
type Options struct {
	// ModTimeTolerance is how far apart the ModTimes that List, Get and Stat
	// report for one key may be, and, with a floor of one minute, how old a
	// fresh Put's ModTime may be. Zero means one second. Raise it for a store
	// whose clock is not the test machine's, such as a database server's.
	// A negative value fails the test.
	ModTimeTolerance time.Duration
}

// Run executes the conformance suite with the default Options. It is
// RunWith(t, newStore, Options{}).
func Run(t *testing.T, newStore func(t *testing.T) kv.Store) {
	t.Helper()
	RunWith(t, newStore, Options{})
}

// RunWith executes the conformance suite. newStore must return an empty Store
// isolated from other calls (for example backed by t.TempDir(), or a fresh
// schema or tenant). When the Store implements kv.Stater, Stat is checked
// against Get and List. Keys stay within kv.MaxKeyBytes.
func RunWith(t *testing.T, newStore func(t *testing.T) kv.Store, opts Options) {
	t.Helper()
	if opts.ModTimeTolerance < 0 {
		t.Fatal("kvtest: ModTimeTolerance is negative")
	}
	tol := opts.ModTimeTolerance
	if tol == 0 {
		tol = time.Second
	}
	recent := max(time.Minute, tol)
	tests := []struct {
		name string
		fn   func(*testing.T, kv.Store)
	}{
		{"RoundTrip", roundTrip},
		{"MissingKey", missingKey},
		{"Overwrite", overwrite},
		{"DeleteRemoves", deleteRemoves},
		{"EmptyAndBinaryValues", emptyAndBinary},
		{"NamespaceIsolation", namespaceIsolation},
		{"ListPrefix", listPrefix},
		{"ListPrefixIsLiteral", listPrefixLiteral},
		{"ListEmpty", listEmpty},
		{"AwkwardKeys", awkwardKeys},
		{"ValuesAreCopies", valuesAreCopies},
		{"ModTime", func(t *testing.T, s kv.Store) { modTime(t, s, tol, recent) }},
		{"StatAgrees", func(t *testing.T, s kv.Store) { statAgrees(t, s, tol) }},
		{"ConcurrentPutGet", concurrentPutGet},
		{"CancelledContext", cancelledContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, newStore(t)) })
	}
}

const ns = "kvtest"

func mustPut(t *testing.T, s kv.Store, namespace, key, value string) {
	t.Helper()
	ctx := t.Context()
	if err := s.Put(ctx, namespace, key, []byte(value)); err != nil {
		t.Fatalf("Put(%q, %q): %v", namespace, key, err)
	}
}

func mustGet(t *testing.T, s kv.Store, namespace, key string) kv.Item {
	t.Helper()
	ctx := t.Context()
	item, err := s.Get(ctx, namespace, key)
	if err != nil {
		t.Fatalf("Get(%q, %q): %v", namespace, key, err)
	}
	return item
}

func keysOf(t *testing.T, s kv.Store, namespace, prefix string) []string {
	t.Helper()
	ctx := t.Context()
	infos, err := s.List(ctx, namespace, prefix)
	if err != nil {
		t.Fatalf("List(%q, %q): %v", namespace, prefix, err)
	}
	keys := make([]string, 0, len(infos))
	for _, in := range infos {
		keys = append(keys, in.Key)
	}
	sort.Strings(keys)
	return keys
}

func roundTrip(t *testing.T, s kv.Store) {
	mustPut(t, s, ns, "/a.txt", "hello")
	if got := mustGet(t, s, ns, "/a.txt"); string(got.Value) != "hello" {
		t.Fatalf("Get = %q, want hello", got.Value)
	}
}

func missingKey(t *testing.T, s kv.Store) {
	ctx := t.Context()
	if _, err := s.Get(ctx, ns, "/missing"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Get missing: got %v, want backend.ErrNotFound", err)
	}
	if err := s.Delete(ctx, ns, "/missing"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	if st, ok := s.(kv.Stater); ok {
		if _, err := st.Stat(ctx, ns, "/missing"); !errors.Is(err, backend.ErrNotFound) {
			t.Fatalf("Stat missing: got %v, want backend.ErrNotFound", err)
		}
	}
}

func overwrite(t *testing.T, s kv.Store) {
	ctx := t.Context()
	mustPut(t, s, ns, "/f", "a much longer first value")
	mustPut(t, s, ns, "/f", "short")
	if got := mustGet(t, s, ns, "/f"); string(got.Value) != "short" {
		t.Fatalf("Get after overwrite = %q", got.Value)
	}
	infos, err := s.List(ctx, ns, "/")
	if err != nil || len(infos) != 1 || infos[0].Size != 5 {
		t.Fatalf("List after overwrite = %+v, %v; want one key of size 5", infos, err)
	}
}

func deleteRemoves(t *testing.T, s kv.Store) {
	ctx := t.Context()
	mustPut(t, s, ns, "/d", "x")
	mustPut(t, s, ns, "/keep", "y")
	if err := s.Delete(ctx, ns, "/d"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, ns, "/d"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if keys := keysOf(t, s, ns, "/"); !slices.Equal(keys, []string{"/keep"}) {
		t.Fatalf("List after Delete = %q", keys)
	}
}

func emptyAndBinary(t *testing.T, s kv.Store) {
	ctx := t.Context()
	binary := []byte{0, 1, 2, 0xff, 0xfe, '\n', 0, 'z'}
	if err := s.Put(ctx, ns, "/bin", binary); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, ns, "/empty", nil); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, ns, "/bin"); !bytes.Equal(got.Value, binary) {
		t.Fatalf("binary value = %v, want %v", got.Value, binary)
	}
	if got := mustGet(t, s, ns, "/empty"); len(got.Value) != 0 {
		t.Fatalf("empty value = %v", got.Value)
	}
	sizes := map[string]int64{}
	infos, err := s.List(ctx, ns, "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range infos {
		sizes[in.Key] = in.Size
	}
	if sizes["/bin"] != int64(len(binary)) || sizes["/empty"] != 0 || len(sizes) != 2 {
		t.Fatalf("List sizes = %v", sizes)
	}
}

func namespaceIsolation(t *testing.T, s kv.Store) {
	ctx := t.Context()
	mustPut(t, s, "one", "/k", "1")
	mustPut(t, s, "two", "/k", "2")
	mustPut(t, s, "one-more", "/other", "3")
	if got := mustGet(t, s, "one", "/k"); string(got.Value) != "1" {
		t.Fatalf("namespace one = %q", got.Value)
	}
	if got := mustGet(t, s, "two", "/k"); string(got.Value) != "2" {
		t.Fatalf("namespace two = %q", got.Value)
	}
	if keys := keysOf(t, s, "one", ""); !slices.Equal(keys, []string{"/k"}) {
		t.Fatalf("List(one) = %q, want only its own key", keys)
	}
	if err := s.Delete(ctx, "one", "/k"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, "two", "/k"); string(got.Value) != "2" {
		t.Fatalf("Delete in one namespace reached another: %q", got.Value)
	}
}

func listPrefix(t *testing.T, s kv.Store) {
	for _, k := range []string{"/dir/a", "/dir/b/c", "/dir2/x", "/di", "/other"} {
		mustPut(t, s, ns, k, k)
	}
	tests := map[string][]string{
		"/dir/": {"/dir/a", "/dir/b/c"},
		"/dir":  {"/dir/a", "/dir/b/c", "/dir2/x"},
		"/di":   {"/di", "/dir/a", "/dir/b/c", "/dir2/x"},
		"":      {"/di", "/dir/a", "/dir/b/c", "/dir2/x", "/other"},
		"/zzz":  {},
	}
	for prefix, want := range tests {
		if got := keysOf(t, s, ns, prefix); !slices.Equal(got, want) {
			t.Errorf("List(%q) = %q, want %q", prefix, got, want)
		}
	}
}

// listPrefixLiteral catches stores that match prefixes with a pattern
// language (SQL LIKE, globs, regular expressions).
func listPrefixLiteral(t *testing.T, s kv.Store) {
	for _, k := range []string{"/a%b", "/axb", "/a_c", "/ayc", "/a*d", "/a.e", "/aze", `/a\f`, "/a[g]"} {
		mustPut(t, s, ns, k, "v")
	}
	for prefix, want := range map[string][]string{
		"/a%":  {"/a%b"},
		"/a_":  {"/a_c"},
		"/a*":  {"/a*d"},
		"/a.":  {"/a.e"},
		`/a\`:  {`/a\f`},
		"/a[":  {"/a[g]"},
		"/a[g": {"/a[g]"},
	} {
		if got := keysOf(t, s, ns, prefix); !slices.Equal(got, want) {
			t.Errorf("List(%q) = %q, want %q: prefixes must match literally", prefix, got, want)
		}
	}
}

func listEmpty(t *testing.T, s kv.Store) {
	ctx := t.Context()
	infos, err := s.List(ctx, "never-used", "/")
	if err != nil || len(infos) != 0 {
		t.Fatalf("List on an empty namespace = %v, %v", infos, err)
	}
}

func awkwardKeys(t *testing.T, s kv.Store) {
	keys := []string{
		"/with space.txt", "/ünï/çødé", "/UPPER", "/upper", "/a/../b", "/'quote\"",
		"/" + strings.Repeat("x", kv.MaxKeyBytes-1),
	}
	for i, k := range keys {
		mustPut(t, s, ns, k, fmt.Sprint(i))
	}
	for i, k := range keys {
		if got := mustGet(t, s, ns, k); string(got.Value) != fmt.Sprint(i) {
			t.Errorf("Get(%.20q) = %q, want %d", k, got.Value, i)
		}
	}
	got := keysOf(t, s, ns, "")
	want := slices.Clone(keys)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("List = %q, want %q", got, want)
	}
}

func valuesAreCopies(t *testing.T, s kv.Store) {
	ctx := t.Context()
	value := []byte("original")
	if err := s.Put(ctx, ns, "/c", value); err != nil {
		t.Fatal(err)
	}
	copy(value, "MUTATED!")
	got := mustGet(t, s, ns, "/c")
	if string(got.Value) != "original" {
		t.Fatalf("Put kept the caller's slice: %q", got.Value)
	}
	copy(got.Value, "MUTATED!")
	if again := mustGet(t, s, ns, "/c"); string(again.Value) != "original" {
		t.Fatalf("Get returned the store's own slice: %q", again.Value)
	}
}

func modTime(t *testing.T, s kv.Store, tol, recent time.Duration) {
	ctx := t.Context()
	before := time.Now().Add(-recent)
	mustPut(t, s, ns, "/m", "v")
	item := mustGet(t, s, ns, "/m")
	if item.ModTime.IsZero() || item.ModTime.Before(before) {
		t.Fatalf("ModTime = %v, want a recent time", item.ModTime)
	}
	infos, err := s.List(ctx, ns, "/m")
	if err != nil || len(infos) != 1 {
		t.Fatalf("List = %v, %v", infos, err)
	}
	if d := infos[0].ModTime.Sub(item.ModTime); d < -tol || d > tol {
		t.Fatalf("List ModTime %v disagrees with Get ModTime %v", infos[0].ModTime, item.ModTime)
	}
}

func statAgrees(t *testing.T, s kv.Store, tol time.Duration) {
	ctx := t.Context()
	st, ok := s.(kv.Stater)
	if !ok {
		t.Skip("store does not implement kv.Stater")
	}
	mustPut(t, s, ns, "/s/file", "twelve bytes")
	info, err := st.Stat(ctx, ns, "/s/file")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	item := mustGet(t, s, ns, "/s/file")
	if info.Key != "/s/file" || info.Size != int64(len(item.Value)) {
		t.Fatalf("Stat = %+v, want key /s/file of size %d", info, len(item.Value))
	}
	if d := info.ModTime.Sub(item.ModTime); d < -tol || d > tol {
		t.Fatalf("Stat ModTime %v disagrees with Get ModTime %v", info.ModTime, item.ModTime)
	}
	if _, err := st.Stat(ctx, "other", "/s/file"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Stat in another namespace: got %v, want backend.ErrNotFound", err)
	}
	if _, err := st.Stat(ctx, ns, "/s"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Stat of a key prefix: got %v, want backend.ErrNotFound", err)
	}
}

func concurrentPutGet(t *testing.T, s kv.Store) {
	ctx := t.Context()
	const workers, each = 8, 20
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				own := fmt.Sprintf("/w%d/%d", w, i)
				if err := s.Put(ctx, ns, own, []byte(own)); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if err := s.Put(ctx, ns, "/shared", []byte(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					t.Errorf("Put shared: %v", err)
					return
				}
				item, err := s.Get(ctx, ns, own)
				if err != nil || string(item.Value) != own {
					t.Errorf("Get(%s) = %q, %v", own, item.Value, err)
					return
				}
				if _, err := s.Get(ctx, ns, "/shared"); err != nil {
					t.Errorf("Get shared: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if n := len(keysOf(t, s, ns, "/w")); n != workers*each {
		t.Fatalf("List found %d keys, want %d", n, workers*each)
	}
	got := string(mustGet(t, s, ns, "/shared").Value)
	if !strings.HasPrefix(got, "w") || !strings.Contains(got, "-") {
		t.Fatalf("shared value %q is not one of the written values", got)
	}
}

func cancelledContext(t *testing.T, s kv.Store) {
	ctx := t.Context()
	mustPut(t, s, ns, "/k", "v")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Put(cctx, ns, "/k", []byte("new")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put: got %v, want context.Canceled", err)
	}
	if _, err := s.Get(cctx, ns, "/k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get: got %v, want context.Canceled", err)
	}
	if _, err := s.List(cctx, ns, "/"); !errors.Is(err, context.Canceled) {
		t.Errorf("List: got %v, want context.Canceled", err)
	}
	if err := s.Delete(cctx, ns, "/k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete: got %v, want context.Canceled", err)
	}
	if st, ok := s.(kv.Stater); ok {
		if _, err := st.Stat(cctx, ns, "/k"); !errors.Is(err, context.Canceled) {
			t.Errorf("Stat: got %v, want context.Canceled", err)
		}
	}
	if got := mustGet(t, s, ns, "/k"); string(got.Value) != "v" {
		t.Fatalf("a cancelled Put changed the value: %q", got.Value)
	}
}
