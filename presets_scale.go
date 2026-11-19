package orchestrix

import (
	"errors"

	"github.com/aswin-kevin/orchestrix/middleware/async"
)

// WithAsync adds background subagents (start_task and friends). The host
// keeps m and calls m.Shutdown when done. A nil m makes New fail.
func WithAsync(m *async.Middleware) Option {
	return func(s *settings) {
		if m == nil {
			s.errs = append(s.errs, errors.New("WithAsync: the middleware must not be nil"))
			return
		}
		s.async = m
	}
}
