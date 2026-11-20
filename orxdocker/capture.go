package orxdocker

import (
	"fmt"
	"sync"
)

// capture is an io.Writer that keeps a bounded view of a command's output: the
// first half of its budget verbatim and a ring buffer holding the most recent
// bytes. Memory stays bounded however much the command prints, and the start
// (usually the command's intent) and the end (usually the error) survive. It is
// safe for concurrent Write and String.
type capture struct {
	mu      sync.Mutex
	headCap int
	head    []byte
	// ring is allocated on first overflow of head, so short outputs never pay
	// for the whole budget.
	ring      []byte
	ringCap   int
	ringStart int // index of the oldest byte in ring
	ringLen   int
	total     int64 // bytes written, including dropped ones
}

func newCapture(maxBytes int) *capture {
	headCap := maxBytes / 2
	return &capture{headCap: headCap, ringCap: maxBytes - headCap}
}

// Write records p; it never fails.
func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	rest := p
	if room := c.headCap - len(c.head); room > 0 {
		n := min(room, len(rest))
		c.head = append(c.head, rest[:n]...)
		rest = rest[n:]
	}
	c.writeRing(rest)
	return len(p), nil
}

// writeRing appends p to the ring, overwriting the oldest bytes when full.
func (c *capture) writeRing(p []byte) {
	if len(p) == 0 || c.ringCap == 0 {
		return
	}
	if c.ring == nil {
		c.ring = make([]byte, c.ringCap)
	}
	if len(p) >= c.ringCap {
		copy(c.ring, p[len(p)-c.ringCap:])
		c.ringStart, c.ringLen = 0, c.ringCap
		return
	}
	end := (c.ringStart + c.ringLen) % c.ringCap
	n := copy(c.ring[end:], p)
	copy(c.ring, p[n:])
	c.ringLen += len(p)
	if overflow := c.ringLen - c.ringCap; overflow > 0 {
		c.ringStart = (c.ringStart + overflow) % c.ringCap
		c.ringLen = c.ringCap
	}
}

// String returns the kept output, with a marker naming the dropped byte count
// between head and tail when anything was dropped.
func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, 0, len(c.head)+c.ringLen+64)
	out = append(out, c.head...)
	if omitted := c.omitted(); omitted > 0 {
		out = fmt.Appendf(out, "\n…[%d bytes omitted]…\n", omitted)
	}
	first := c.ring[c.ringStart:min(c.ringStart+c.ringLen, c.ringCap)]
	out = append(out, first...)
	out = append(out, c.ring[:c.ringLen-len(first)]...)
	return string(out)
}

// truncated reports whether any output was dropped.
func (c *capture) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.omitted() > 0
}

func (c *capture) omitted() int64 {
	return c.total - int64(len(c.head)) - int64(c.ringLen)
}
