package file

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// replayed is the outcome of reading one thread file.
type replayed struct {
	img     *image    // state after the last good record
	seq     int       // seq of the last good record
	updated time.Time // time of the last good record
	deltas  int       // delta records since the last snapshot
	goodEnd int64     // byte offset just past the last good record
	size    int64     // file size; larger than goodEnd means a torn tail
}

// readRecords parses data into records. A final line that lacks its newline
// or does not parse is a torn write and is dropped; an unparsable line
// anywhere earlier is corruption and an error. goodEnd is the offset after
// the last record kept.
func readRecords(data []byte) (recs []record, goodEnd int64, err error) {
	for pos := 0; pos < len(data); {
		nl := bytes.IndexByte(data[pos:], '\n')
		if nl < 0 {
			break // unterminated tail
		}
		end := pos + nl + 1
		var rec record
		if jsonErr := json.Unmarshal(data[pos:pos+nl], &rec); jsonErr != nil {
			if end == len(data) {
				break // torn final line
			}
			return nil, 0, fmt.Errorf("corrupt record at byte %d: %w", pos, jsonErr)
		}
		recs = append(recs, rec)
		pos, goodEnd = end, int64(end)
	}
	return recs, goodEnd, nil
}

// replayFile reads path and rebuilds the thread's latest image, starting from
// its last snapshot. It returns os.ErrNotExist (wrapped) for a missing file
// and also for one without any good record.
func replayFile(path, thread string) (*replayed, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	recs, goodEnd, err := readRecords(data)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("no complete record: %w", os.ErrNotExist)
	}
	for i, rec := range recs {
		if rec.Thread != thread {
			return nil, fmt.Errorf("record %d belongs to thread %q, want %q", i, rec.Thread, thread)
		}
	}
	return foldFromLastSnapshot(recs, goodEnd, int64(len(data)))
}

func foldFromLastSnapshot(recs []record, goodEnd, size int64) (*replayed, error) {
	start := len(recs) - 1
	for start > 0 && recs[start].Kind != kindSnapshot {
		start--
	}
	out := &replayed{goodEnd: goodEnd, size: size}
	for _, rec := range recs[start:] {
		img, err := rec.apply(out.img)
		if err != nil {
			return nil, err
		}
		if rec.Kind == kindDelta {
			out.deltas++
		}
		out.img, out.seq, out.updated = img, rec.Seq, rec.Time
	}
	return out, nil
}

// lastRecord returns the final good record of path without replaying it; List
// uses it for Updated and Saves. Missing or record-less files report
// os.ErrNotExist.
func lastRecord(path string) (record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return record{}, err
	}
	recs, _, err := readRecords(data)
	if err != nil {
		return record{}, err
	}
	if len(recs) == 0 {
		return record{}, fmt.Errorf("no complete record: %w", os.ErrNotExist)
	}
	return recs[len(recs)-1], nil
}
