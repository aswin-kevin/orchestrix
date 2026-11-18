package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// ErrThreadBusy is the Final.Err of a Thread run rejected because another
// run of the thread is in progress (ReasonBusy).
var ErrThreadBusy = errors.New("agent: thread is already running")

// Thread is a conversation with an agent that keeps its own State: each run
// continues where the last one ended. With Config.Checkpointer set, every
// run is saved under the thread's id, and Agent.Thread picks the
// conversation up again, in this process or another; without it the thread
// lives in memory only.
//
// A Thread runs one run at a time and is safe for concurrent use. Between
// runs, Messages, State and Pending describe the outcome of the last one;
// while a run is in progress they describe the State it started from.
type Thread struct {
	id    string
	agent *Agent

	// mu guards state and running.
	mu    sync.Mutex
	state *state.State
	// running is set while a run of the thread streams.
	running bool
}

// Thread opens the conversation id: the latest checkpoint of the thread when
// Config.Checkpointer holds one, else a fresh State. id must pass
// checkpoint.ValidThreadID; ctx bounds the load only.
func (a *Agent) Thread(ctx context.Context, id string) (*Thread, error) {
	if err := checkpoint.ValidThreadID(id); err != nil {
		return nil, fmt.Errorf("agent: thread: %w", err)
	}
	s, err := a.loadThread(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Thread{id: id, agent: a, state: s}, nil
}

// loadThread returns the latest saved State of thread id, or a fresh one
// when there is none or no Checkpointer is configured.
func (a *Agent) loadThread(ctx context.Context, id string) (*state.State, error) {
	if a.cfg.Checkpointer == nil {
		return state.New(), nil
	}
	s, err := a.cfg.Checkpointer.Load(ctx, id)
	switch {
	case errors.Is(err, checkpoint.ErrNotFound):
		return state.New(), nil
	case err != nil:
		return nil, fmt.Errorf("agent: thread %q: load checkpoint: %w", id, err)
	}
	return s, nil
}

// ID returns the thread's id.
func (t *Thread) ID() string {
	return t.id
}

// Prompt sends text as a user message; see Send.
func (t *Thread) Prompt(ctx context.Context, text string) iter.Seq2[Event, error] {
	return t.Send(ctx, llm.UserText(text))
}

// Send starts a run of the thread with msgs appended to its conversation,
// streaming its events as Agent.Run does. The thread adopts the run's
// Final.State. While another run of the thread is in progress the stream is
// a single KindFinal with ReasonBusy.
func (t *Thread) Send(ctx context.Context, msgs ...llm.Message) iter.Seq2[Event, error] {
	return t.stream(func(s *state.State) iter.Seq2[Event, error] {
		return t.agent.Run(ctx, Input{ThreadID: t.id, Messages: msgs, State: s})
	})
}

// Resume finishes the thread's interrupted run with decisions, as
// Agent.ResumeWith does, and adopts its Final.State. While another run of
// the thread is in progress the stream is a single KindFinal with
// ReasonBusy.
func (t *Thread) Resume(ctx context.Context, decisions []Decision) iter.Seq2[Event, error] {
	return t.stream(func(s *state.State) iter.Seq2[Event, error] {
		return t.agent.ResumeWith(ctx, Input{ThreadID: t.id, State: s}, decisions)
	})
}

// Pending returns the actions the thread's interrupted run awaits; see the
// package-level Pending.
func (t *Thread) Pending() (*Interrupt, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Pending(t.state)
}

// Messages returns a copy of the thread's conversation.
func (t *Thread) Messages() []llm.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.Clone().Messages()
}

// State returns an independent copy of the thread's State.
func (t *Thread) State() *state.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.Clone()
}

// Fork returns a new thread newID that starts with a copy of this thread's
// State, workspace snapshot included, and continues independently. With
// Config.Checkpointer set, the copy is saved under newID first.
func (t *Thread) Fork(ctx context.Context, newID string) (*Thread, error) {
	if err := checkpoint.ValidThreadID(newID); err != nil {
		return nil, fmt.Errorf("agent: fork: %w", err)
	}
	s := t.State()
	if cp := t.agent.cfg.Checkpointer; cp != nil {
		if err := cp.Save(ctx, newID, s); err != nil {
			return nil, fmt.Errorf("agent: fork %q: save checkpoint: %w", newID, err)
		}
	}
	return &Thread{id: newID, agent: t.agent, state: s}, nil
}

// stream runs start on a copy of the thread's State and adopts the State the
// run ends with. The run works on a copy so that Messages and State keep
// describing a finished run while it is in progress.
func (t *Thread) stream(start func(*state.State) iter.Seq2[Event, error]) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		s, ok := t.acquire()
		if !ok {
			yield(t.busyEvent(), ErrThreadBusy)
			return
		}
		// Deferred so that a consumer breaking out early, which never sees
		// KindFinal, still frees the thread; the run's State then holds
		// whatever progress it made, as its checkpoint does.
		defer func() { t.release(s) }()
		for ev, err := range start(s) {
			if ev.Kind == KindFinal && ev.Final.State != nil {
				s = ev.Final.State
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// acquire marks the thread running and returns a copy of its State to run
// on; it reports false when a run is already in progress.
func (t *Thread) acquire() (*state.State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return nil, false
	}
	t.running = true
	return t.state.Clone(), true
}

// release adopts s as the thread's State and marks the thread idle.
func (t *Thread) release(s *state.State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
	t.running = false
}

// busyEvent is the lone KindFinal of a run rejected with ReasonBusy.
func (t *Thread) busyEvent() Event {
	final := &Final{Reason: ReasonBusy, Err: ErrThreadBusy}
	return Event{Kind: KindFinal, Agent: t.agent.cfg.Name, Final: final}
}
