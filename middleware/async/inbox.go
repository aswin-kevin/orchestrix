package async

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// UpdateInput is update_task's input.
type UpdateInput struct {
	// TaskID is the task to send the message to.
	TaskID string `json:"task_id" desc:"The task to send the message to."`
	// Message is the new instructions or information for the running task.
	Message string `json:"message" desc:"New instructions or information for the running task."`
}

// ErrFinished is the error of a message sent to a task that already ended.
var ErrFinished = errors.New("async: task already finished")

// MaxUpdateBytes caps, in bytes, a message update_task sends.
const MaxUpdateBytes = 8_000

// inboxName is the name of the middleware that delivers messages to the
// local runner's tasks.
const inboxName = "async.inbox"

// inbox holds the messages sent to one running task that its run has not
// read yet. Once closed it refuses messages with ErrFinished.
type inbox struct {
	mu      sync.Mutex
	pending []string
	closed  bool
}

// send queues msg; ErrFinished once the inbox is closed.
func (b *inbox) send(msg string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrFinished
	}
	b.pending = append(b.pending, msg)
	return nil
}

// drain takes every queued message, oldest first.
func (b *inbox) drain() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := b.pending
	b.pending = nil
	return msgs
}

// close refuses further messages and returns how many were never read.
func (b *inbox) close() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	n := len(b.pending)
	b.pending = nil
	return n
}

// inboxes is the middleware that hands a task's messages to its run. The
// local runner registers one inbox per task under the task's thread id and
// passes this middleware to every child it builds (subagents.NewLauncher).
// A child's own children inherit it, and the Launcher keeps that one value
// rather than adding it twice. A run reads only the inbox of its own thread
// (agent.RunInfo.ThreadID), so a subagent of a task never takes the task's
// messages.
type inboxes struct {
	mu       sync.Mutex
	byThread map[string]*inbox
}

var (
	_ agent.BeforeModel = (*inboxes)(nil)
	_ agent.BeforeStop  = (*inboxes)(nil)
)

func newInboxes() *inboxes { return &inboxes{byThread: make(map[string]*inbox)} }

// Name implements agent.Middleware.
func (x *inboxes) Name() string { return inboxName }

// open registers a new inbox for the task run on threadID.
func (x *inboxes) open(threadID string) (*inbox, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.byThread[threadID]; ok {
		return nil, fmt.Errorf("a task already runs on thread %q", threadID)
	}
	b := &inbox{}
	x.byThread[threadID] = b
	return b, nil
}

// remove closes and forgets the inbox of threadID, returning how many of its
// messages were never read.
func (x *inboxes) remove(threadID string) int {
	x.mu.Lock()
	b := x.byThread[threadID]
	delete(x.byThread, threadID)
	x.mu.Unlock()
	if b == nil {
		return 0
	}
	return b.close()
}

// take drains the inbox of the run ctx belongs to; nil when it has none.
func (x *inboxes) take(ctx context.Context) []string {
	x.mu.Lock()
	b := x.byThread[runThread(ctx)]
	x.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.drain()
}

// BeforeModel implements agent.BeforeModel: each message sent to the task
// since its last turn becomes an injected user message.
func (x *inboxes) BeforeModel(ctx context.Context, s *state.State) error {
	for _, msg := range x.take(ctx) {
		m := llm.UserText(updateText(msg))
		m.Meta = map[string]string{agent.InjectedMetaKey: "async"}
		s.AppendMessages(m)
	}
	return nil
}

// BeforeStop implements agent.BeforeStop: a task about to end with unread
// messages takes another turn, with them as the prompt.
func (x *inboxes) BeforeStop(ctx context.Context, _ *state.State, _ *llm.Response) (string, error) {
	msgs := x.take(ctx)
	texts := make([]string, len(msgs))
	for i, msg := range msgs {
		texts[i] = updateText(msg)
	}
	return strings.Join(texts, "\n"), nil
}

// updateText frames a message sent to a task.
func updateText(msg string) string { return frame(tagUpdate, neutralize(msg)) }

// withLateUpdates adds to out's Error how many messages arrived after the
// task's run read its inbox for the last time.
func withLateUpdates(out Outcome, n int) Outcome {
	if n == 0 {
		return out
	}
	note := fmt.Sprintf("%d update(s) arrived after the task finished", n)
	if out.Error != "" {
		note = out.Error + "\n" + note
	}
	out.Error = note
	return out
}
