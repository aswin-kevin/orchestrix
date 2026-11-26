package summarize

import (
	"context"
	"errors"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
)

// SummaryRequest is what a Summarizer summarizes.
type SummaryRequest struct {
	// Messages are the messages being summarized, oldest first: the State
	// messages between the previous boundary and the kept tail. They are
	// shared with State and must not be modified.
	Messages []llm.Message
	// Previous is the summary of an earlier compaction, which the new
	// summary replaces together with Messages; "" when none.
	Previous string
	// Transcript is Messages rendered as by Transcript, each block cut to
	// DefaultMaxBlockChars. A span whose rendering exceeds about 400,000
	// bytes keeps only its newest messages that fit, under a first line
	// saying how many were left out, so a summarizer request stays bounded.
	Transcript string
}

// Summarizer writes the summary that replaces the older part of a
// conversation in the request view.
//
// The middleware calls Summarize once per compaction, during a model call of
// the run (the automatic trigger, a compact_conversation request, or the
// retry after the provider rejected a request as too long). ctx is that
// model call's context, so agent.Info reports the run. Calls within one run
// are sequential, but runs sharing the middleware call it concurrently, so
// an implementation must be safe for concurrent use.
//
// The returned summary replaces both req.Previous and req.Messages; it is
// trimmed, and a blank one counts as a failure. An error fails the
// compaction, not the turn: the request goes out uncompacted, the error is
// recorded in Boundary.LastError, and it counts towards Config.MaxFailures.
// On the too-long retry, the provider's original error is returned instead.
type Summarizer interface {
	Summarize(ctx context.Context, req SummaryRequest) (string, error)
}

// SummarizerFunc adapts a function to the Summarizer interface.
type SummarizerFunc func(ctx context.Context, req SummaryRequest) (string, error)

// Summarize implements Summarizer by calling f.
func (f SummarizerFunc) Summarize(ctx context.Context, req SummaryRequest) (string, error) {
	return f(ctx, req)
}

// ModelSummarizer is the default Summarizer: one model call with Prompt as
// the system prompt and the previous summary and the transcript as the one
// user message, with a reply budget of DefaultSummaryMaxTokens. It is safe
// for concurrent use when Provider is.
type ModelSummarizer struct {
	// Provider makes the call; nil means the provider of the run's agent
	// (agent.Info), which ctx must then belong to.
	Provider llm.Provider
	// Model is the model asked; "" means the run agent's Config.Model when
	// Provider is nil, and the provider's default model otherwise. The
	// summarize middleware's own default differs: with Config.Summarizer
	// nil it builds a ModelSummarizer per request whose Provider and Model
	// default to the request's (Config.Provider, Config.Model), so a
	// ModelSummarizer set as Config.Summarizer with a Provider but no Model
	// asks that provider's default model, not the request's.
	Model string
	// Prompt is the whole system prompt; "" means DefaultSummaryPrompt.
	Prompt string
}

// Summarize implements Summarizer. It returns the reply's text, trimmed, and
// the provider's error unchanged.
func (s *ModelSummarizer) Summarize(ctx context.Context, req SummaryRequest) (string, error) {
	provider, model := s.Provider, s.Model
	if provider == nil {
		info, ok := agent.Info(ctx)
		if !ok || info.Agent == nil {
			return "", errors.New("summarize: ModelSummarizer has no Provider and ctx belongs to no agent run")
		}
		cfg := info.Agent.Config()
		provider = cfg.Provider
		if model == "" {
			model = cfg.Model
		}
	}
	prompt := s.Prompt
	if prompt == "" {
		prompt = DefaultSummaryPrompt
	}
	resp, err := llm.Collect(ctx, provider, llm.Request{
		Model:     model,
		System:    []string{prompt},
		Messages:  []llm.Message{llm.UserText("Previous summary:\n" + req.Previous + "\n\nNew conversation:\n" + req.Transcript)},
		MaxTokens: DefaultSummaryMaxTokens,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.Text()), nil
}
