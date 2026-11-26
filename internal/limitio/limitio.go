package limitio

import (
	"io"
	"math"
)

// ReadAll reads r up to limit bytes; limit must be >= 0, and a negative limit
// counts as 0. over reports that r held more than limit bytes; b then holds
// the first limit bytes. It reads at most limit+1 bytes, so a stream that
// grows without end is still bounded.
func ReadAll(r io.Reader, limit int64) (b []byte, over bool, err error) {
	limit = max(limit, 0)
	if limit == math.MaxInt64 {
		// limit+1 would overflow; no stream is longer than the limit.
		b, err = io.ReadAll(r)
	} else {
		b, err = io.ReadAll(io.LimitReader(r, limit+1))
	}
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}
