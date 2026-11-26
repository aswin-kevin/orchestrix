package state

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/aswin-kevin/orchestrix/llm"
)

// formatVersion is the only serialized layout this package reads and writes.
const formatVersion = 1

// State is a concurrency-safe bag of messages and named values. Create one
// with New; the zero value is not usable. Methods must not be called on a nil
// *State.
//
// Key options (private, merge) live in the package-level key registry, not
// in the State, so they survive a round trip through JSON.
//
// Every value is held in exactly one of two forms: decoded (values) or still
// serialized (raw, awaiting its first Key access).
type State struct {
	mu       sync.Mutex
	messages []llm.Message
	values   map[string]any
	raw      map[string]json.RawMessage
}

// New returns an empty State.
func New() *State {
	return &State{
		values: make(map[string]any),
		raw:    make(map[string]json.RawMessage),
	}
}

// Messages returns a copy of the message slice. The messages' inner content
// is shared with the State and must be treated as read-only.
func (s *State) Messages() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Message(nil), s.messages...)
}

// SetMessages replaces the messages with a copy of m.
func (s *State) SetMessages(m []llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append([]llm.Message(nil), m...)
}

// AppendMessages adds m to the end of the conversation.
func (s *State) AppendMessages(m ...llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m...)
}

// Clone returns an independent deep copy: messages and values are copied
// through JSON, so later changes to either State never show in the other.
func (s *State) Clone() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := s.copyLocked(func(string) bool { return true })
	clone.messages = deepCopyMessages(s.messages)
	return clone
}

// Fork returns a State for a run that continues this one's conversation
// separately: an independent deep copy, like Clone, without the values of
// RunLocal keys. Keys the State has never seen are copied.
func (s *State) Fork() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	fork := s.copyLocked(func(name string) bool { return !lookupKey(name).runLocal })
	fork.messages = deepCopyMessages(s.messages)
	return fork
}

// Child returns a State for a subagent: it carries a deep copy of every
// non-private value and no messages. Keys the State has never seen are
// treated as non-private.
func (s *State) Child() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyLocked(func(name string) bool { return !lookupKey(name).private })
}

// copyLocked deep-copies the entries accepted by include into a new State. Copies stay serialized until first read.
// s.mu must be held.
func (s *State) copyLocked(include func(name string) bool) *State {
	c := New()
	for name := range s.values {
		if !include(name) {
			continue
		}
		c.raw[name] = s.mustSerializedLocked(name)
	}
	for name, raw := range s.raw {
		if include(name) {
			c.raw[name] = append(json.RawMessage(nil), raw...)
		}
	}
	return c
}

// serializedLocked returns a fresh JSON encoding of the named entry, decoded
// or not. s.mu must be held.
func (s *State) serializedLocked(name string) (json.RawMessage, error) {
	if raw, ok := s.raw[name]; ok {
		return append(json.RawMessage(nil), raw...), nil
	}
	raw, err := json.Marshal(s.values[name])
	if err != nil {
		return nil, fmt.Errorf("state: encode key %q: %w", name, err)
	}
	return raw, nil
}

// mustSerializedLocked is serializedLocked for copy paths that cannot return
// an error. Key.Set rejects unencodable values, so a failure here means a
// caller mutated a stored value in place against the read-only rule.
// s.mu must be held.
func (s *State) mustSerializedLocked(name string) json.RawMessage {
	raw, err := s.serializedLocked(name)
	if err != nil {
		panic(fmt.Sprintf("%v (a stored value was mutated into an unencodable form)", err))
	}
	return raw
}

// MergeChild folds a child's values back into s. For each key the child
// holds: private keys are skipped; a key declared with a MergeWith function gets merge(parentValue, childValue); every other key takes the child's
// value. A key whose merge inputs cannot be decoded is left unchanged. The
// child's value already contains what Child copied from s, which MergeWith
// funcs must account for. Merge
// functions run while s is locked and must not use any State. The child is
// not modified.
func (s *State) MergeChild(child *State) {
	incoming := child.snapshot()

	s.mu.Lock()
	defer s.mu.Unlock()
	for name, raw := range incoming {
		meta := lookupKey(name)
		switch {
		case meta.private:
			continue
		case meta.merge == nil:
			s.raw[name] = raw
			delete(s.values, name)
		default:
			s.mergeLocked(name, meta, raw)
		}
	}
}

// snapshot returns the serialized form of every entry.
func (s *State) snapshot() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]json.RawMessage, len(s.values)+len(s.raw))
	for name := range s.values {
		out[name] = s.mustSerializedLocked(name)
	}
	for name, raw := range s.raw {
		out[name] = append(json.RawMessage(nil), raw...)
	}
	return out
}

// mergeLocked applies a key's merge function to the parent value and the
// child's serialized value. s.mu must be held.
func (s *State) mergeLocked(name string, meta keyMeta, childRaw json.RawMessage) {
	child, err := meta.decode(childRaw)
	if err != nil {
		return
	}
	parent, ok := s.values[name]
	if !ok {
		raw, hasRaw := s.raw[name]
		if !hasRaw {
			s.values[name] = child
			return
		}
		if parent, err = meta.decode(raw); err != nil {
			return
		}
	}
	s.values[name] = meta.merge(parent, child)
	delete(s.raw, name)
}

// stateJSON is the serialized layout of a State.
type stateJSON struct {
	Version  int                        `json:"version"`
	Messages []llm.Message              `json:"messages"`
	Values   map[string]json.RawMessage `json:"values"`
}

// MarshalJSON encodes the State as {"version":1,"messages":[...],"values":{...}}.
// Decoded values are encoded with encoding/json; values never read since
// loading are emitted verbatim. Value names are sorted, so equal states
// produce identical bytes.
func (s *State) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := stateJSON{
		Version:  formatVersion,
		Messages: s.messages,
		Values:   make(map[string]json.RawMessage, len(s.values)+len(s.raw)),
	}
	if out.Messages == nil {
		out.Messages = []llm.Message{}
	}
	for name := range s.values {
		raw, err := s.serializedLocked(name)
		if err != nil {
			return nil, err
		}
		out.Values[name] = raw
	}
	for name, raw := range s.raw {
		out.Values[name] = raw
	}
	return json.Marshal(out)
}

// UnmarshalJSON replaces the State's entire contents with the decoded data.
// Values stay serialized until a Key reads them, so entries unknown to this
// process survive a later MarshalJSON. It fails on a missing or unsupported version.
func (s *State) UnmarshalJSON(data []byte) error {
	var in stateJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("state: decode: %w", err)
	}
	if in.Version != formatVersion {
		return fmt.Errorf("state: unsupported version %d (only %d is supported)", in.Version, formatVersion)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = in.Messages
	s.values = make(map[string]any)
	s.raw = make(map[string]json.RawMessage, len(in.Values))
	for name, raw := range in.Values {
		s.raw[name] = raw
	}
	return nil
}

// deepCopyMessages copies messages through JSON. A message that cannot be
// encoded falls back to a shallow copy of the slice.
func deepCopyMessages(m []llm.Message) []llm.Message {
	if m == nil {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return append([]llm.Message(nil), m...)
	}
	var out []llm.Message
	if err := json.Unmarshal(data, &out); err != nil {
		return append([]llm.Message(nil), m...)
	}
	return out
}

// Compile-time checks that State implements the json interfaces.
var (
	_ json.Marshaler   = (*State)(nil)
	_ json.Unmarshaler = (*State)(nil)
)
