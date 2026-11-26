package llm

import (
	"context"
	"fmt"
	"iter"
)

// Provider is a model backend. Implementations translate a Request to a
// vendor API and the reply back into the provider-neutral types.
//
// An implementation keeps these rules:
//   - Complete returns what Collect would assemble from Stream for the same
//     request.
//   - A stream that ends without an error yields at least one event.
//   - Errors drive retry, fallback and context-overflow handling: return
//     errors that IsRetryable and IsContextOverflow classify correctly (an
//     *APIError, or an error with a Retryable() bool or ContextOverflow()
//     bool method).
//   - Usage reported by successive events of one response follows
//     Usage.MergeEcho.
//   - Capabilities such as token counting arrive as optional interfaces,
//     never as methods on Provider.
//   - HTTP adapters are built on Transport.
type Provider interface {
	// Stream sends req and yields incremental events as they arrive. A
	// non-nil error ends the sequence.
	Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error]

	// Complete sends req and returns the whole reply once it is finished.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Collect drains p.Stream and assembles the events into a Response. Providers
// can implement Complete with it when they only support streaming.
func Collect(ctx context.Context, p Provider, req Request) (*Response, error) {
	var acc Accumulator
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return nil, err
		}
		if err := acc.Add(ev); err != nil {
			return nil, fmt.Errorf("collect stream: %w", err)
		}
	}
	return acc.Response()
}
