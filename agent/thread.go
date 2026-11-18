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

var (
	// ErrThreadBusy is the Final.Err of a Thread run rejected because
	// another run of the same Thread handle is in progress (ReasonBusy).
	ErrThreadBusy = errors.New("agent: thread is already running")
	// ErrThreadExists is returned by Thread.Fork when the new id is the
	// thread's own or already has a checkpoint.
	ErrThreadExists = errors.New("agent: thread already exists")
)

// Thread is a conversation with an agent that keeps its own State: each run
// continues where the last one ended. With Config.Checkpointer set, every
// run is saved under the thread's id, and Agent.Thread picks the
// conversation up again, in this process or another; without it the thread
// lives in memory only.
//
// A Thread handle runs one run at a time and is safe for concurrent use.
// The guarantee is per handle: two handles opened on the same id, in one
// process or several, know nothing of each other and would overwrite each
// other's checkpoints, so open one handle per conversation. Between runs,
// Messages, State and Pending describe the outcome of the last one; while a
// run is in progress they describe the State it started from.
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

// Messages returns a deep copy of the thread's conversation: changing it
// never touches the thread.
func (t *Thread) Messages() []llm.Message {
	t.mu.Lock()
	msgs := t.state.Messages()
	t.mu.Unlock()
	return copyMessages(msgs)
}

// copyMessages deep-copies msgs through JSON, the representation a
// checkpoint keeps, so no block content is shared with the original.
func copyMessages(msgs []llm.Message) []llm.Message {
	s := state.New()
	s.SetMessages(msgs)
	return s.Clone().Messages()
}

// State returns an independent copy of the thread's State.
func (t *Thread) State() *state.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.Clone()
}

// Fork returns a new thread newID that starts with a copy of this thread's
// State, workspace snapshot included, and continues independently. With
// Config.Checkpointer set, the copy is saved under newID first. Fork never
// overwrites a conversation: it fails with ErrThreadExists when newID is
// this thread's id or already has a checkpoint.
func (t *Thread) Fork(ctx context.Context, newID string) (*Thread, error) {
	if err := checkpoint.ValidThreadID(newID); err != nil {
		return nil, fmt.Errorf("agent: fork: %w", err)
	}
	if newID == t.id {
		return nil, fmt.Errorf("agent: fork %q: %w", newID, ErrThreadExists)
	}
	s := t.State()
	if cp := t.agent.cfg.Checkpointer; cp != nil {
		if err := saveNew(ctx, cp, newID, s); err != nil {
			return nil, fmt.Errorf("agent: fork %q: %w", newID, err)
		}
	}
	return &Thread{id: newID, agent: t.agent, state: s}, nil
}

// saveNew saves s as the first checkpoint of thread id, failing with
// ErrThreadExists when the thread already has one. The check and the save
// are not atomic; Checkpointer offers no create-only save.
func saveNew(ctx context.Context, cp checkpoint.Checkpointer, id string, s *state.State) error {
	_, err := cp.Load(ctx, id)
	switch {
	case err == nil:
		return ErrThreadExists
	case !errors.Is(err, checkpoint.ErrNotFound):
		return fmt.Errorf("check for an existing checkpoint: %w", err)
	}
	if err := cp.Save(ctx, id, s); err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	return nil
}

// stream runs start on a copy of the thread's State and adopts a copy of
// the State the run ends with. The run works on a copy so that Messages and State keep
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
				// A copy, so a consumer changing Final.State cannot rewrite
				// the thread's history.
				s = ev.Final.State.Clone()
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
