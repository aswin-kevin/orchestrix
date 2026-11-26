package llm

import "context"

// handoffKey keys the hand-off test a fallback provider puts in the context of
// a call to an entry that another entry follows.
type handoffKey struct{}

// withHandoff returns ctx telling the retry loop (Transport.Do) that a
// fallback provider will move to its next entry on any error for which should
// reports true, so such an error is returned at once instead of being retried
// here.
func withHandoff(ctx context.Context, should func(error) bool) context.Context {
	return context.WithValue(ctx, handoffKey{}, should)
}

// FallbackPending reports whether ctx belongs to a call that a fallback
// provider (NewFallback) made to an entry another entry follows, and that
// provider will switch to the next entry on err. An adapter that retries
// errors itself should return such an error at once instead of retrying it;
// Transport.Do already does.
func FallbackPending(ctx context.Context, err error) bool {
	should, _ := ctx.Value(handoffKey{}).(func(error) bool)
	return should != nil && should(err)
}
