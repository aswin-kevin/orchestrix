package kvtest_test

import (
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/backend/kv/kvtest"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
)

func TestMemoryStore(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.Store { return testutil.MemoryStore(t) })
}

func TestDirStore(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.Store {
		s, err := kv.NewDirStore(t.TempDir(), kv.DirStoreConfig{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// getOnly hides the Stater of the store it wraps, so Backend takes its
// fallback path through Get.
type getOnly struct{ kv.Store }

func TestStoreWithoutStater(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.Store { return getOnly{testutil.MemoryStore(t)} })
}

func TestBackendWithoutStater(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, err := kv.New(getOnly{testutil.MemoryStore(t)}, "ns")
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

// TestRunWithSkewedClock runs the suite over a store whose clock is 90
// seconds behind the test machine's; the tolerance admits it.
func TestRunWithSkewedClock(t *testing.T) {
	kvtest.RunWith(t, func(t *testing.T) kv.Store {
		s, err := kv.NewMemoryStore(kv.MemoryStoreConfig{
			Clock: func() time.Time { return time.Now().Add(-90 * time.Second) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}, kvtest.Options{ModTimeTolerance: 2 * time.Minute})
}
