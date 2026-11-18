package state

import (
	"encoding/json"
	"fmt"
)

// keyMeta is the type-erased description of a Key that a State remembers so
// that ChildFor and MergeChild can honor the key's options even when they
// only hold untyped values.
type keyMeta struct {
	private bool
	// decode turns serialized JSON into the key's concrete type T.
	decode func(json.RawMessage) (any, error)
	// merge combines parent and child values; nil means "child wins".
	merge func(parent, child any) any
}

// Key is a typed handle to one named entry in a State. Declare keys once, as
// package-level variables, and use them to read and write state:
//
//	var Todos = state.NewKey[[]Todo]("todos")
//
// The value stored under a key must be JSON-serializable: it is encoded when
// the State is marshaled, cloned or handed to a child.
type Key[T any] struct {
	name    string
	private bool
	merge   func(parent, child T) T
}

// KeyOption configures a Key at construction time.
type KeyOption[T any] func(*Key[T])

// Private marks a key as private: ChildFor does not copy it to children and
// MergeChild never merges it back into the parent.
func Private[T any]() KeyOption[T] {
	return func(k *Key[T]) { k.private = true }
}

// MergeWith sets how MergeChild combines the parent's and the child's values
// for this key. Without it the child's value replaces the parent's. The
// function runs while the parent State is locked and must not use any State.
func MergeWith[T any](f func(parent, child T) T) KeyOption[T] {
	return func(k *Key[T]) { k.merge = f }
}

// NewKey declares a key with the given name. It panics if name is empty,
// because an unnamed key can never round-trip through JSON and is always a
// programming error.
func NewKey[T any](name string, opts ...KeyOption[T]) Key[T] {
	if name == "" {
		panic("state: NewKey requires a non-empty name")
	}
	k := Key[T]{name: name}
	for _, opt := range opts {
		opt(&k)
	}
	return k
}

// Name returns the key's name, which is its field name in serialized state.
func (k Key[T]) Name() string { return k.name }

// Get returns the value stored under the key. The second result is false when
// the key is unset, when the stored value has a different type, or when
// serialized data for the key cannot be decoded into T; in the last case the
// serialized data is kept so nothing is lost.
//
// Get returns the stored value itself, not a copy. Treat slices, maps and
// pointers obtained from it as read-only; to change them, build a modified
// copy and store it with Set or Update.
func (k Key[T]) Get(s *State) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.register(k.name, k.meta())
	return k.load(s)
}

// Set stores v under the key, replacing any previous value.
func (k Key[T]) Set(s *State, v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.register(k.name, k.meta())
	k.store(s, v)
}

// Update atomically replaces the stored value with f applied to it. f receives
// the zero value when the key is unset or its serialized data cannot be
// decoded (that data is then overwritten). f runs while the State is locked,
// so it must not call any method of the same State.
func (k Key[T]) Update(s *State, f func(T) T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.register(k.name, k.meta())
	current, _ := k.load(s)
	k.store(s, f(current))
}

// Delete removes the key's value, whether decoded or still serialized.
func (k Key[T]) Delete(s *State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.register(k.name, k.meta())
	delete(s.values, k.name)
	delete(s.raw, k.name)
}

// load reads the value with s.mu held, decoding serialized data on first use.
func (k Key[T]) load(s *State) (T, bool) {
	var zero T
	if v, ok := s.values[k.name]; ok {
		typed, ok := v.(T)
		if !ok {
			return zero, false
		}
		return typed, true
	}
	raw, ok := s.raw[k.name]
	if !ok {
		return zero, false
	}
	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return zero, false
	}
	s.values[k.name] = decoded
	delete(s.raw, k.name)
	return decoded, true
}

// store writes the value with s.mu held.
func (k Key[T]) store(s *State, v T) {
	s.values[k.name] = v
	delete(s.raw, k.name)
}

// meta builds the type-erased description registered on a State.
func (k Key[T]) meta() keyMeta {
	m := keyMeta{
		private: k.private,
		decode: func(raw json.RawMessage) (any, error) {
			var v T
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("state: decode key %q: %w", k.name, err)
			}
			return v, nil
		},
	}
	if k.merge != nil {
		m.merge = func(parent, child any) any {
			p, pok := parent.(T)
			c, cok := child.(T)
			if !pok || !cok {
				return child
			}
			return k.merge(p, c)
		}
	}
	return m
}
