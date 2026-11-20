package kvtest_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/backend/kv/kvtest"
)

func TestMemoryStore(t *testing.T) {
	kvtest.Run(t, func(*testing.T) kv.Store { return kv.NewMemoryStore() })
}

func TestDirStore(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.Store {
		s, err := kv.NewDirStore(t.TempDir())
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
	kvtest.Run(t, func(*testing.T) kv.Store { return getOnly{kv.NewMemoryStore()} })
}

func TestBackendWithoutStater(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, err := kv.New(getOnly{kv.NewMemoryStore()}, "ns")
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}
