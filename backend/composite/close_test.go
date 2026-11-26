package composite_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/composite"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
)

// ctxCloser is a backend with Close(ctx); it counts calls and returns err.
type ctxCloser struct {
	backend.Backend
	calls atomic.Int32
	order *[]string
	name  string
	err   error
}

func (c *ctxCloser) Close(context.Context) error {
	c.calls.Add(1)
	if c.order != nil {
		*c.order = append(*c.order, c.name)
	}
	return c.err
}

// ioCloserBackend is a backend with only io.Closer's Close() error.
type ioCloserBackend struct {
	backend.Backend
	calls atomic.Int32
	order *[]string
	name  string
}

func (c *ioCloserBackend) Close() error {
	c.calls.Add(1)
	if c.order != nil {
		*c.order = append(*c.order, c.name)
	}
	return nil
}

// closeOf returns c's Close through backend.AsCloser.
func closeOf(t *testing.T, c backend.Backend) func(context.Context) error {
	t.Helper()
	cl, ok := backend.AsCloser(c)
	if !ok {
		t.Fatal("composite does not implement backend.Closer")
	}
	return cl.Close
}

func TestCloseClosesDefaultAndRoutes(t *testing.T) {
	ctx := t.Context()
	var order []string
	def := &ctxCloser{Backend: testutil.Memory(t), order: &order, name: "def"}
	a := &ioCloserBackend{Backend: testutil.Memory(t), order: &order, name: "a"}
	z := &ctxCloser{Backend: testutil.Memory(t), order: &order, name: "z"}
	// Routes are closed in the order given to New, not by prefix length.
	c := mustNew(t, def,
		composite.Route{Prefix: "/z", Backend: z},
		composite.Route{Prefix: "/a/long/prefix", Backend: a},
		composite.Route{Prefix: "/plain", Backend: testutil.Memory(t)})
	closeFn := closeOf(t, c)
	if err := closeFn(ctx); err != nil {
		t.Fatal(err)
	}
	if def.calls.Load() != 1 || a.calls.Load() != 1 || z.calls.Load() != 1 {
		t.Fatalf("calls def=%d a=%d z=%d, want 1 each", def.calls.Load(), a.calls.Load(), z.calls.Load())
	}
	if want := []string{"def", "z", "a"}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("close order = %q, want %q", order, want)
	}
	if err := closeFn(ctx); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if def.calls.Load() != 1 || a.calls.Load() != 1 || z.calls.Load() != 1 {
		t.Fatal("second Close closed a backend again")
	}
}

func TestCloseEveryVariant(t *testing.T) {
	ctx := t.Context()
	testutil.RequireUnixShell(t)
	// Each capability variant must have Close: memory (Snapshotter),
	// execOnly (Executor), a shell (Executor+Starter) and a plain backend.
	sh, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for name, def := range map[string]backend.Backend{
		"snap":      testutil.Memory(t),
		"exec":      execOnly{testutil.Memory(t)},
		"execStart": sh,
		"none":      noMulti{testutil.Memory(t)},
	} {
		t.Run(name, func(t *testing.T) {
			r := &ctxCloser{Backend: testutil.Memory(t)}
			if err := closeOf(t, mustNew(t, def, composite.Route{Prefix: "/m", Backend: r}))(ctx); err != nil {
				t.Fatal(err)
			}
			if r.calls.Load() != 1 {
				t.Fatalf("route closed %d times, want 1", r.calls.Load())
			}
		})
	}
}

func TestCloseJoinsErrors(t *testing.T) {
	ctx := t.Context()
	errA, errB := errors.New("a failed"), errors.New("b failed")
	def := &ctxCloser{Backend: testutil.Memory(t), err: errA}
	route := &ctxCloser{Backend: testutil.Memory(t), err: errB}
	c := mustNew(t, def, composite.Route{Prefix: "/m", Backend: route})
	closeFn := closeOf(t, c)
	err := closeFn(ctx)
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Close = %v, want both errors", err)
	}
	if def.calls.Load() != 1 || route.calls.Load() != 1 {
		t.Fatalf("calls def=%d route=%d, want 1 each", def.calls.Load(), route.calls.Load())
	}
	if again := closeFn(ctx); !errors.Is(again, errA) || !errors.Is(again, errB) {
		t.Fatalf("second Close = %v, want the first result", again)
	}
}

func TestCloseSharedBackendOnce(t *testing.T) {
	ctx := t.Context()
	shared := &ctxCloser{Backend: testutil.Memory(t)}
	c := mustNew(t, shared,
		composite.Route{Prefix: "/m", Backend: shared},
		// Wrapped, it is still the same Closer.
		composite.Route{Prefix: "/n", Backend: unwrapping{shared}})
	if err := closeOf(t, c)(ctx); err != nil {
		t.Fatal(err)
	}
	if n := shared.calls.Load(); n != 1 {
		t.Fatalf("shared backend closed %d times, want 1", n)
	}
}

// funcCloser is a closer whose dynamic type is not comparable.
type funcCloser struct {
	backend.Backend
	close func() error
}

func (f funcCloser) Close(context.Context) error { return f.close() }

func TestCloseUncomparableBackend(t *testing.T) {
	ctx := t.Context()
	var calls atomic.Int32
	nc := funcCloser{Backend: testutil.Memory(t), close: func() error { calls.Add(1); return nil }}
	c := mustNew(t, nc, composite.Route{Prefix: "/m", Backend: nc})
	if err := closeOf(t, c)(ctx); err != nil {
		t.Fatal(err)
	}
	// Not comparable, so not deduplicated: each appearance is closed.
	if n := calls.Load(); n != 2 {
		t.Fatalf("closed %d times, want 2", n)
	}
}

func TestCloseConcurrent(t *testing.T) {
	ctx := t.Context()
	def := &ctxCloser{Backend: testutil.Memory(t), err: errors.New("boom")}
	closeFn := closeOf(t, mustNew(t, def))
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			errs[i] = closeFn(ctx)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if !errors.Is(err, def.err) {
			t.Fatalf("Close = %v, want %v", err, def.err)
		}
	}
	if n := def.calls.Load(); n != 1 {
		t.Fatalf("closed %d times, want 1", n)
	}
}

// blockingCloser is a backend whose Close signals entered, then blocks
// until release is closed.
type blockingCloser struct {
	backend.Backend
	entered chan struct{}
	release chan struct{}
}

func (c *blockingCloser) Close(context.Context) error {
	close(c.entered)
	<-c.release
	return nil
}

// A Close whose ctx ends while the first Close is still working returns
// ctx.Err() at once; the work goes on, and a later Close gets its result.
func TestCloseHonoursEachCallersCtx(t *testing.T) {
	def := &blockingCloser{Backend: testutil.Memory(t), entered: make(chan struct{}), release: make(chan struct{})}
	closeFn := closeOf(t, mustNew(t, def))
	first := make(chan error, 1)
	go func() { first <- closeFn(t.Context()) }()
	<-def.entered

	expired, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() { second <- closeFn(expired) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second Close = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		close(def.release)
		t.Fatal("second Close with an expired ctx waited for the first")
	}
	select {
	case err := <-first:
		t.Fatalf("first Close returned %v before its work finished", err)
	default:
	}
	close(def.release)
	if err := <-first; err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := closeFn(expired); err != nil {
		t.Fatalf("Close after the work finished = %v, want its result", err)
	}
}

func TestCompositeSnapshotterConformance(t *testing.T) {
	backendtest.RunSnapshotter(t, func(t *testing.T) backend.Backend {
		return mustNew(t, testutil.Memory(t))
	})
}

// panicCloser is a backend whose Close panics.
type panicCloser struct{ backend.Backend }

func (panicCloser) Close(context.Context) error { panic("close failed") }

// A Closer that panics must not leave later Close callers waiting forever.
func TestCloseAfterPanickingCloser(t *testing.T) {
	closeFn := closeOf(t, mustNew(t, panicCloser{Backend: testutil.Memory(t)}))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("first Close did not panic")
			}
		}()
		_ = closeFn(t.Context())
	}()
	second := make(chan error, 1)
	go func() { second <- closeFn(t.Context()) }()
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("second Close = nil, want an error reporting the failed close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Close blocked after the first Close panicked")
	}
}
