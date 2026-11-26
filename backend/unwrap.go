package backend

import (
	"context"
	"io"
)

// as returns the first of b and the backends it wraps (through
// Unwrap() Backend) that implements T: the outermost implementer wins. A nil
// b yields false.
func as[T any](b Backend) (T, bool) {
	for b != nil {
		if v, ok := b.(T); ok {
			return v, true
		}
		u, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			break
		}
		b = u.Unwrap()
	}
	var zero T
	return zero, false
}

// AsExecutor returns the Executor of b or of a backend it wraps (see
// Backend); ok is false when none implements it or b is nil.
func AsExecutor(b Backend) (Executor, bool) { return as[Executor](b) }

// AsStarter returns the Starter of b or of a backend it wraps (see Backend);
// ok is false when none implements it or b is nil.
func AsStarter(b Backend) (Starter, bool) { return as[Starter](b) }

// AsSnapshotter returns the Snapshotter of b or of a backend it wraps (see
// Backend); ok is false when none implements it or b is nil.
func AsSnapshotter(b Backend) (Snapshotter, bool) { return as[Snapshotter](b) }

// AsMultiEditor returns the MultiEditor of b or of a backend it wraps (see
// Backend); ok is false when none implements it or b is nil.
func AsMultiEditor(b Backend) (MultiEditor, bool) { return as[MultiEditor](b) }

// AsCloser returns how to release b: the Closer of b or of a backend it wraps
// (see Backend), or, for one that implements only io.Closer, an adapter whose
// Close(ctx) calls Close() and so is not bounded by ctx. The outermost of
// either kind wins. ok is false when none implements either or b is nil.
func AsCloser(b Backend) (Closer, bool) {
	for b != nil {
		switch c := b.(type) {
		case Closer:
			return c, true
		case io.Closer:
			return ioCloser{c}, true
		}
		u, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			break
		}
		b = u.Unwrap()
	}
	return nil, false
}

// ioCloser adapts an io.Closer to Closer.
type ioCloser struct{ c io.Closer }

// Close implements Closer; ctx is ignored.
func (a ioCloser) Close(context.Context) error { return a.c.Close() }
