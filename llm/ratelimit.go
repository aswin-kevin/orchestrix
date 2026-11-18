package llm

import (
	"context"
	"sync"
	"time"
)

// RateLimit caps how many requests are started. Zero fields are unlimited.
type RateLimit struct {
	// PerSecond is the maximum requests per second.
	PerSecond int
	// PerMinute is the maximum requests per minute.
	PerMinute int
}

// bucket is a token bucket that may go negative: a caller reserves its token
// immediately and is told how long to wait for it to become valid.
type bucket struct {
	capacity float64
	perToken time.Duration
	tokens   float64
	last     time.Time
}

func newBucket(count int, window time.Duration) *bucket {
	return &bucket{
		capacity: float64(count),
		perToken: window / time.Duration(count),
		tokens:   float64(count),
	}
}

// reserve takes one token and returns how long the caller must wait before
// using it.
func (b *bucket) reserve(now time.Time) time.Duration {
	if b.last.IsZero() {
		b.last = now // first use: the bucket starts full
	}
	b.tokens = min(b.capacity, b.tokens+float64(now.Sub(b.last))/float64(b.perToken))
	b.last = now
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens * float64(b.perToken))
}

// limiter enforces a RateLimit across concurrent callers.
type limiter struct {
	mu      sync.Mutex
	buckets []*bucket

	// now and sleep are fields so tests can run on a fake clock.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// newLimiter returns nil when rl imposes no limit.
func newLimiter(rl *RateLimit) *limiter {
	if rl == nil || (rl.PerSecond <= 0 && rl.PerMinute <= 0) {
		return nil
	}
	l := &limiter{now: time.Now, sleep: sleepContext}
	if rl.PerSecond > 0 {
		l.buckets = append(l.buckets, newBucket(rl.PerSecond, time.Second))
	}
	if rl.PerMinute > 0 {
		l.buckets = append(l.buckets, newBucket(rl.PerMinute, time.Minute))
	}
	return l
}

// acquire blocks until a request may start or ctx ends. A nil limiter never
// blocks. A token reserved by a caller that is then cancelled is not
// returned, which only makes the limiter slightly conservative.
func (l *limiter) acquire(ctx context.Context) error {
	if l == nil {
		return ctx.Err()
	}
	return l.sleep(ctx, l.reserve())
}

func (l *limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	for _, b := range l.buckets {
		wait = max(wait, b.reserve(now))
	}
	return wait
}

// sleepContext waits for d or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
