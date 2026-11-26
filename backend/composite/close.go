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
// are joined. The first call does the work; later and concurrent calls wait
// for it and return its result.
func (c *composite) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { c.closeErr = c.closeAll(ctx) })
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
