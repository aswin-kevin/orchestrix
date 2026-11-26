package composite

import (
	"context"
	"errors"
	"reflect"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Close releases def and then each route's backend in the order given to
// New, each through backend.AsCloser (a backend without a Closer is
// skipped). A Closer reached from more than one place (the same comparable
// value, including through a wrapper's Unwrap) is closed once. The errors
// are joined. The first call does the work, passing its ctx to each
// Closer; later and concurrent calls wait for it to finish, or for their
// own ctx to end (returning ctx.Err() while the work goes on), and return
// its result.
func (c *composite) Close(ctx context.Context) error {
	c.closeMu.Lock()
	if c.closing {
		c.closeMu.Unlock()
		select {
		case <-c.closeDone:
			return c.closeErr
		default:
		}
		select {
		case <-c.closeDone:
			return c.closeErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.closing = true
	c.closeDone = make(chan struct{})
	c.closeMu.Unlock()
	c.closeErr = c.closeAll(ctx)
	close(c.closeDone)
	return c.closeErr
}

// closeAll closes every distinct Closer of c.owned.
func (c *composite) closeAll(ctx context.Context) error {
	var errs []error
	seen := map[any]bool{}
	for _, b := range c.owned {
		cl, ok := backend.AsCloser(b)
		if !ok {
			continue
		}
		// A Closer whose dynamic type is not comparable (it holds a func,
		// map or slice) would panic as a map key; it is closed each time.
		if reflect.ValueOf(cl).Comparable() {
			if seen[cl] {
				continue
			}
			seen[cl] = true
		}
		if err := cl.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
