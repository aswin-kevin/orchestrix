package backend_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
)

// wrap is a decorator over inner that forwards the Backend methods and
// exposes inner through Unwrap.
type wrap struct {
	backend.Backend
	inner backend.Backend
}

func (w wrap) Unwrap() backend.Backend { return w.inner }

func newWrap(inner backend.Backend) wrap { return wrap{Backend: inner, inner: inner} }

// capable is a memory backend that also executes, starts and snapshots,
// recording what was called.
type capable struct {
	*memory.Backend
	calls []string
}

func (c *capable) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	c.calls = append(c.calls, "execute")
	return backend.ExecResult{Output: "inner"}, nil
}

func (c *capable) Start(context.Context, string, backend.ExecOptions) (backend.Process, error) {
	c.calls = append(c.calls, "start")
	return nil, nil
}

func (c *capable) Snapshot(ctx context.Context) (json.RawMessage, error) {
	c.calls = append(c.calls, "snapshot")
	return c.Backend.Snapshot(ctx)
}

func (c *capable) Restore(ctx context.Context, data json.RawMessage) error {
	c.calls = append(c.calls, "restore")
	return c.Backend.Restore(ctx, data)
}

func TestAsWalksUnwrap(t *testing.T) {
	inner := &capable{Backend: memory.New()}
	b := newWrap(newWrap(inner))
	ctx := t.Context()

	e, ok := backend.AsExecutor(b)
	if !ok {
		t.Fatal("AsExecutor: not found through Unwrap")
	}
	if res, err := e.Execute(ctx, "x", backend.ExecOptions{}); err != nil || res.Output != "inner" {
		t.Fatalf("Execute = %+v, %v; want the inner executor", res, err)
	}
	s, ok := backend.AsStarter(b)
	if !ok {
		t.Fatal("AsStarter: not found through Unwrap")
	}
	if _, err := s.Start(ctx, "x", backend.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	sn, ok := backend.AsSnapshotter(b)
	if !ok {
		t.Fatal("AsSnapshotter: not found through Unwrap")
	}
	data, err := sn.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sn.Restore(ctx, data); err != nil {
		t.Fatal(err)
	}
	want := []string{"execute", "start", "snapshot", "restore"}
	if len(inner.calls) != len(want) {
		t.Fatalf("inner calls = %v, want %v", inner.calls, want)
	}
	for i := range want {
		if inner.calls[i] != want[i] {
			t.Fatalf("inner calls = %v, want %v", inner.calls, want)
		}
	}
	if _, ok := backend.AsMultiEditor(b); !ok {
		t.Fatal("AsMultiEditor: memory's MultiEdit not found through Unwrap")
	}
}

// errWrapper is the sentinel of refusingWrap's own Execute.
var errWrapper = errors.New("wrapper refused")

// refusingWrap restricts the executor of the backend it wraps.
type refusingWrap struct{ wrap }

func (refusingWrap) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{}, errWrapper
}

func TestAsExecutorPrefersWrapper(t *testing.T) {
	b := refusingWrap{newWrap(&capable{Backend: memory.New()})}
	e, ok := backend.AsExecutor(b)
	if !ok {
		t.Fatal("AsExecutor: not found")
	}
	if _, err := e.Execute(t.Context(), "x", backend.ExecOptions{}); !errors.Is(err, errWrapper) {
		t.Fatalf("Execute = %v, want the wrapper's %v", err, errWrapper)
	}
}

func TestAsWithoutCapability(t *testing.T) {
	if _, ok := backend.AsExecutor(memory.New()); ok {
		t.Error("AsExecutor(memory) = ok, want false")
	}
	if _, ok := backend.AsExecutor(newWrap(memory.New())); ok {
		t.Error("AsExecutor(wrapped memory) = ok, want false")
	}
	if _, ok := backend.AsExecutor(nil); ok {
		t.Error("AsExecutor(nil) = ok, want false")
	}
	if _, ok := backend.AsStarter(nil); ok {
		t.Error("AsStarter(nil) = ok, want false")
	}
	if _, ok := backend.AsSnapshotter(nil); ok {
		t.Error("AsSnapshotter(nil) = ok, want false")
	}
	if _, ok := backend.AsMultiEditor(nil); ok {
		t.Error("AsMultiEditor(nil) = ok, want false")
	}
	if _, ok := backend.AsCloser(nil); ok {
		t.Error("AsCloser(nil) = ok, want false")
	}
	if _, ok := backend.AsCloser(memory.New()); ok {
		t.Error("AsCloser(memory) = ok, want false")
	}
}

// ioClosing implements io.Closer only.
type ioClosing struct {
	*memory.Backend
	n int
}

func (c *ioClosing) Close() error {
	c.n++
	return nil
}

// ctxKey keys the value ctxClosing looks for.
type ctxKey struct{}

// ctxClosing implements backend.Closer and records the ctx it got.
type ctxClosing struct {
	*memory.Backend
	got []any
}

func (c *ctxClosing) Close(ctx context.Context) error {
	c.got = append(c.got, ctx.Value(ctxKey{}))
	return nil
}

func TestAsCloserAdaptsIOCloser(t *testing.T) {
	ioc := &ioClosing{Backend: memory.New()}
	c, ok := backend.AsCloser(newWrap(ioc))
	if !ok {
		t.Fatal("AsCloser(io.Closer) = false, want an adapter")
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ioc.n != 1 {
		t.Fatalf("io.Closer called %d times, want 1", ioc.n)
	}

	cc := &ctxClosing{Backend: memory.New()}
	c, ok = backend.AsCloser(cc)
	if !ok {
		t.Fatal("AsCloser(Closer) = false")
	}
	ctx := context.WithValue(t.Context(), ctxKey{}, "mine")
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cc.got) != 1 || cc.got[0] != "mine" {
		t.Fatalf("Close got ctx values %v, want one call with the caller's ctx", cc.got)
	}
}
