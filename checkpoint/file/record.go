package file

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Record kinds.
const (
	kindSnapshot = "snapshot"
	kindDelta    = "delta"
)

// record is one JSONL line. Snapshot lines carry State; delta lines carry the
// remaining fields. Thread is repeated in every record because the file name
// is only a hash of the id.
type record struct {
	Seq    int       `json:"seq"`
	Time   time.Time `json:"time"`
	Thread string    `json:"thread"`
	Kind   string    `json:"kind"`

	State json.RawMessage `json:"state,omitempty"`

	// Keep is the length of the unchanged message prefix; zero (omitted)
	// means every previous message is replaced.
	Keep     int                        `json:"keep,omitempty"`
	Messages []json.RawMessage          `json:"messages,omitempty"`
	Values   map[string]json.RawMessage `json:"values,omitempty"`
	Deleted  []string                   `json:"deleted,omitempty"`
}

// image is a serialized state split into the parts a delta can address. It
// mirrors the state package's JSON layout without decoding messages or values.
type image struct {
	Version  int                        `json:"version"`
	Messages []json.RawMessage          `json:"messages"`
	Values   map[string]json.RawMessage `json:"values"`
}

// parseImage decodes state JSON (as produced by State.MarshalJSON).
func parseImage(data []byte) (*image, error) {
	var img image
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, fmt.Errorf("decode state image: %w", err)
	}
	if img.Values == nil {
		img.Values = map[string]json.RawMessage{}
	}
	return &img, nil
}

// encode returns the image as state JSON.
func (img *image) encode() ([]byte, error) {
	out := *img
	if out.Messages == nil {
		out.Messages = []json.RawMessage{}
	}
	return json.Marshal(out)
}

// snapshotRecord builds a full-state record for img.
func snapshotRecord(seq int, now time.Time, thread string, img *image) (record, error) {
	data, err := img.encode()
	if err != nil {
		return record{}, err
	}
	return record{Seq: seq, Time: now, Thread: thread, Kind: kindSnapshot, State: data}, nil
}

// deltaRecord builds the record that turns prev into next.
func deltaRecord(seq int, now time.Time, thread string, prev, next *image) record {
	keep := commonPrefix(prev.Messages, next.Messages)
	rec := record{
		Seq: seq, Time: now, Thread: thread, Kind: kindDelta,
		Keep:     keep,
		Messages: next.Messages[keep:],
	}
	for name, raw := range next.Values {
		if old, ok := prev.Values[name]; !ok || !bytes.Equal(old, raw) {
			if rec.Values == nil {
				rec.Values = map[string]json.RawMessage{}
			}
			rec.Values[name] = raw
		}
	}
	for name := range prev.Values {
		if _, ok := next.Values[name]; !ok {
			rec.Deleted = append(rec.Deleted, name)
		}
	}
	slices.Sort(rec.Deleted)
	return rec
}

// commonPrefix returns how many leading messages a and b share byte for byte.
func commonPrefix(a, b []json.RawMessage) int {
	n := 0
	for n < len(a) && n < len(b) && bytes.Equal(a[n], b[n]) {
		n++
	}
	return n
}

// apply returns the image that results from replaying rec onto img. For a
// snapshot img is ignored; for a delta img must not be nil. The result does
// not alias img's maps or message slice header.
func (rec record) apply(img *image) (*image, error) {
	switch rec.Kind {
	case kindSnapshot:
		return parseImage(rec.State)
	case kindDelta:
		return rec.applyDelta(img)
	default:
		return nil, fmt.Errorf("unknown record kind %q", rec.Kind)
	}
}

func (rec record) applyDelta(prev *image) (*image, error) {
	if prev == nil {
		return nil, fmt.Errorf("delta seq %d has no snapshot before it", rec.Seq)
	}
	if rec.Keep < 0 || rec.Keep > len(prev.Messages) {
		return nil, fmt.Errorf("delta seq %d keeps %d of %d messages", rec.Seq, rec.Keep, len(prev.Messages))
	}
	next := &image{
		Version:  prev.Version,
		Messages: append(slices.Clone(prev.Messages[:rec.Keep]), rec.Messages...),
		Values:   make(map[string]json.RawMessage, len(prev.Values)+len(rec.Values)),
	}
	for name, raw := range prev.Values {
		next.Values[name] = raw
	}
	for name, raw := range rec.Values {
		next.Values[name] = raw
	}
	for _, name := range rec.Deleted {
		delete(next.Values, name)
	}
	return next, nil
}
