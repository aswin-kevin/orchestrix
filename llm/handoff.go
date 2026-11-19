package llm

import "context"

// handoffKey keys the hand-off test a fallback provider puts in the context of
// a call to an entry that another entry follows.
type handoffKey struct{}

// withHandoff returns ctx telling the retry loop (Config.do) that a fallback
// provider will move to its next entry on any error for which should reports
// true, so such an error is returned at once instead of being retried here.
func withHandoff(ctx context.Context, should func(error) bool) context.Context {
	return context.WithValue(ctx, handoffKey{}, should)
}

// handsOff reports whether err should be returned without retrying because
// a fallback provider will try another entry for it.
func handsOff(ctx context.Context, err error) bool {
	should, _ := ctx.Value(handoffKey{}).(func(error) bool)
	return should != nil && should(err)
}
