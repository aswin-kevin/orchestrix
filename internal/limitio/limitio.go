// Package limitio reads untrusted input up to a size limit.
package limitio

import (
	"io"
)

// ReadAll reads r up to limit bytes. over reports that r held more than
// limit bytes; b then holds the first limit bytes. It reads at most limit+1
// bytes, so a stream that grows without end is still bounded.
func ReadAll(r io.Reader, limit int64) (b []byte, over bool, err error) {
	b, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}
