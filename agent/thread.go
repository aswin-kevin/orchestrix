package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

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
// A Thread handle runs one run at a time and is safe for concurrent use;
// a second run of the same handle ends at once with ReasonBusy. Handles on
// the same id, in one process or several, cannot overwrite each other's
// checkpoints: a handle remembers the checkpoint version it last loaded or
// saved, and a run whose save finds the thread changed by another writer
// ends with ReasonCheckpointError wrapping checkpoint.ErrConflict. Such a
// run started from an outdated version ends before its first model call;
// one that loses the race mid-run ends at its next save, after its tools
// ran. When the Checkpointer implements checkpoint.Leaser, runs of the same
// thread also never overlap: a run started while another holds the
// thread's lease ends at once with ReasonBusy. After a conflict or a busy
// lease the handle keeps its State; open the thread again (Agent.Thread)
// to continue from what the other writer saved.
//
// Between runs, Messages, State and Pending describe the outcome of the
// last one; while a run is in progress they describe the State it started
// from.
//
// Create one with Agent.Thread; the zero value is not usable.
type Thread struct {
	id    string
	agent *Agent

	// mu guards state, version and running.
	mu    sync.Mutex
	state *state.State
	// version is the checkpoint version state was loaded or saved at.
	version checkpoint.Version
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
	s, v, err := a.loadThread(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Thread{id: id, agent: a, state: s, version: v}, nil
}

// loadThread returns the latest saved State of thread id and its version,
// or a fresh State and the zero Version when there is none or no
// Checkpointer is configured.
func (a *Agent) loadThread(ctx context.Context, id string) (*state.State, checkpoint.Version, error) {
	if a.cfg.Checkpointer == nil {
		return state.New(), "", nil
	}
	s, v, err := a.cfg.Checkpointer.Load(ctx, id)
	switch {
	case errors.Is(err, checkpoint.ErrNotFound):
		return state.New(), "", nil
	case err != nil:
		return nil, "", fmt.Errorf("agent: thread %q: load checkpoint: %w", id, err)
	}
	return s, v, nil
}

// ID returns the thread's id.
func (t *Thread) ID() string {
	return t.id
}

// Version returns the checkpoint version of the thread's State: the one
// it was loaded at or its last run saved, the zero Version when it was never
// saved.
func (t *Thread) Version() checkpoint.Version {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version
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
	return t.stream(func(in Input) iter.Seq2[Event, error] {
		in.Messages = msgs
		return t.agent.Run(ctx, in)
	})
}

// Resume finishes the thread's interrupted run with decisions, as
// Agent.ResumeWith does, and adopts its Final.State. While another run of
// the thread is in progress the stream is a single KindFinal with
// ReasonBusy.
func (t *Thread) Resume(ctx context.Context, decisions []Decision) iter.Seq2[Event, error] {
	return t.stream(func(in Input) iter.Seq2[Event, error] {
		return t.agent.ResumeWith(ctx, in, decisions)
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
	var v checkpoint.Version
	if cp := t.agent.cfg.Checkpointer; cp != nil {
		var err error
		if v, err = saveNew(ctx, cp, newID, s); err != nil {
			return nil, fmt.Errorf("agent: fork %q: %w", newID, err)
		}
	}
	return &Thread{id: newID, agent: t.agent, state: s, version: v}, nil
}

// saveNew saves s as the first checkpoint of thread id, failing with
// ErrThreadExists when the thread already has one. The save is create-only
// (the zero expected Version), so the check is atomic.
func saveNew(ctx context.Context, cp checkpoint.Checkpointer, id string, s *state.State) (checkpoint.Version, error) {
	v, err := cp.Save(ctx, id, s, "")
	switch {
	case errors.Is(err, checkpoint.ErrConflict):
		return "", ErrThreadExists
	case err != nil:
		return "", fmt.Errorf("save checkpoint: %w", err)
	}
	return v, nil
}

// stream runs start on a copy of the thread's State and adopts a copy of
// the State the run ends with, and its version. The run works on a copy so
// that Messages and State keep describing a finished run while it is in
// progress. The run reports its end through Input.onEnd, so a consumer
// breaking out early, which never sees KindFinal, still leaves the thread
// with the progress the run made and the version it last saved. A run that
// lost the thread to another writer (a conflict or a lease held elsewhere),
// or that never started (an invalid resume), is not adopted: the handle
// keeps what it had.
func (t *Thread) stream(start func(Input) iter.Seq2[Event, error]) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		s, v, ok := t.acquire()
		if !ok {
			yield(t.busyEvent(), ErrThreadBusy)
			return
		}
		// onEnd runs on the run's goroutine before its event stream closes,
		// and the stream's iterator returns only once it has closed, so the
		// deferred release sees what onEnd set.
		onEnd := func(final *Final) {
			if final.State == nil || lostThread(final.Err) {
				return
			}
			// A copy, taken before the consumer gets Final.State, so a
			// consumer changing it cannot rewrite the thread's history.
			s, v = final.State.Clone(), final.Version
		}
		// Deferred so that a consumer breaking out early still frees the
		// thread.
		defer func() { t.release(s, v) }()
		for ev, err := range start(Input{ThreadID: t.id, State: s.Clone(), Version: v, onEnd: onEnd}) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// lostThread reports whether err says another writer owns the thread.
func lostThread(err error) bool {
	return errors.Is(err, checkpoint.ErrConflict) || errors.Is(err, checkpoint.ErrLeaseHeld)
}

// acquire marks the thread running and returns a copy of its State to run
// on, with its version; it reports false when a run is already in progress.
func (t *Thread) acquire() (*state.State, checkpoint.Version, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return nil, "", false
	}
	t.running = true
	return t.state.Clone(), t.version, true
}

// release adopts s and v as the thread's State and version and marks the
// thread idle.
func (t *Thread) release(s *state.State, v checkpoint.Version) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
	t.version = v
	t.running = false
}

// busyEvent is the lone KindFinal of a run rejected with ReasonBusy.
func (t *Thread) busyEvent() Event {
	final := &Final{Reason: ReasonBusy, Err: ErrThreadBusy}
	return Event{Kind: KindFinal, Agent: t.agent.cfg.Name, Time: time.Now(), Final: final}
}
