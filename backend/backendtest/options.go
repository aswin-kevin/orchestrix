package backendtest

import (
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

// resolutionSlack is added to Options.ModTimeResolution when spacing writes,
// so two writes never land in the same timestamp tick.
const resolutionSlack = 50 * time.Millisecond

// Options adapts the conformance suite to a backend's limitations. The zero
// value runs the suite exactly as Run does.
type Options struct {
	// ModTimeResolution is the granularity of the modification times the
	// backend reports (time.Second for one that reads whole-second mtimes).
	// Tests that need strictly ordered mtimes space their writes by at least
	// this plus a small slack. 0 means the backend keeps fine-grained times.
	ModTimeResolution time.Duration
}

// RunWith executes the conformance suite like Run, adjusted by opts.
func RunWith(t *testing.T, newBackend func(t *testing.T) backend.Backend, opts Options) {
	t.Helper()
	run(t, newBackend, opts.writeGap())
}

// writeGap is how long to wait between writes whose mtimes must differ.
func (o Options) writeGap() time.Duration {
	if o.ModTimeResolution <= 0 {
		return mtimeGap
	}
	return max(mtimeGap, o.ModTimeResolution+resolutionSlack)
}
