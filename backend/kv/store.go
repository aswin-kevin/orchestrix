// Package kv provides a backend.Backend over a pluggable namespaced key/value
// Store, for state that must outlive one thread: several Backend values (or
// processes, with DirStore) share a Store and each owns one namespace.
package kv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	// MaxKeyBytes is the longest key (a cleaned backend path) DirStore accepts.
	MaxKeyBytes = 180
	// MaxNamespaceBytes is the longest namespace New and DirStore accept.
	MaxNamespaceBytes = 128
)

// ErrKeyTooLong means a key is longer than the store can hold. DirStore
// returns it (wrapped) for keys over MaxKeyBytes; MemoryStore has no limit.
var ErrKeyTooLong = errors.New("kv: key too long")

// Store is a namespaced key/value store; keys are cleaned absolute backend
// paths. Implementations are safe for concurrent use.
type Store interface {
	// Get fails with an error wrapping backend.ErrNotFound for a missing key.
	Get(ctx context.Context, namespace, key string) (Item, error)
	// Put stores a copy of value, stamping ModTime with the store's clock.
	Put(ctx context.Context, namespace, key string, value []byte) error
	// Delete removes key; a missing key is not an error.
	Delete(ctx context.Context, namespace, key string) error
	// List returns every key of namespace starting with prefix, in any order.
	List(ctx context.Context, namespace, prefix string) ([]ItemInfo, error)
}

// Item is a stored value. Value is the caller's own copy.
type Item struct {
	Value   []byte    // file content
	ModTime time.Time // when the value was last put
}

// ItemInfo describes one stored key without its value.
type ItemInfo struct {
	Key     string    // the full key
	Size    int64     // value length in bytes
	ModTime time.Time // when the value was last put
}

type storeOptions struct {
	now func() time.Time
}

// StoreOption configures a MemoryStore or a DirStore.
type StoreOption func(*storeOptions)

// WithClock sets the clock that stamps modification times; the default is
// time.Now.
func WithClock(now func() time.Time) StoreOption {
	return func(o *storeOptions) { o.now = now }
}

func newStoreOptions(opts []StoreOption) storeOptions {
	o := storeOptions{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// stater is implemented by the stores in this package so a Backend can test
// existence and size without reading a value; other stores fall back to Get.
type stater interface {
	stat(ctx context.Context, namespace, key string) (ItemInfo, error)
}

// checkNamespace rejects a namespace no store can hold safely.
func checkNamespace(namespace string) error {
	switch {
	case namespace == "":
		return errors.New("kv: empty namespace")
	case len(namespace) > MaxNamespaceBytes:
		return fmt.Errorf("kv: namespace longer than %d bytes", MaxNamespaceBytes)
	case strings.ContainsRune(namespace, 0):
		return errors.New("kv: namespace contains NUL byte")
	}
	return nil
}

type entry struct {
	value   []byte
	modTime time.Time
}

// MemoryStore is a Store held in memory. The zero value is not usable; create
// one with NewMemoryStore.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]map[string]entry
	now  func() time.Time
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore(opts ...StoreOption) *MemoryStore {
	return &MemoryStore{data: map[string]map[string]entry{}, now: newStoreOptions(opts).now}
}

// Get returns a copy of the value stored under key.
func (s *MemoryStore) Get(ctx context.Context, namespace, key string) (Item, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[namespace][key]
	if !ok {
		return Item{}, backend.ErrNotFound
	}
	return Item{Value: slices.Clone(e.value), ModTime: e.modTime}, nil
}

// stat returns key's metadata without copying its value.
func (s *MemoryStore) stat(ctx context.Context, namespace, key string) (ItemInfo, error) {
	if err := ctx.Err(); err != nil {
		return ItemInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[namespace][key]
	if !ok {
		return ItemInfo{}, backend.ErrNotFound
	}
	return ItemInfo{Key: key, Size: int64(len(e.value)), ModTime: e.modTime}, nil
}

// Put stores a copy of value under key.
func (s *MemoryStore) Put(ctx context.Context, namespace, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ns := s.data[namespace]
	if ns == nil {
		ns = map[string]entry{}
		s.data[namespace] = ns
	}
	ns[key] = entry{value: slices.Clone(value), modTime: s.now()}
	return nil
}

// Delete removes key; a missing key is not an error.
func (s *MemoryStore) Delete(ctx context.Context, namespace, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data[namespace], key)
	return nil
}

// List returns every key of namespace starting with prefix, in no
// particular order.
func (s *MemoryStore) List(ctx context.Context, namespace, prefix string) ([]ItemInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ItemInfo
	for key, e := range maps.All(s.data[namespace]) {
		if strings.HasPrefix(key, prefix) {
			out = append(out, ItemInfo{Key: key, Size: int64(len(e.value)), ModTime: e.modTime})
		}
	}
	return out, nil
}
