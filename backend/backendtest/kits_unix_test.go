//go:build unix

package backendtest

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
)

func realShell(t *testing.T) *shell.Backend {
	t.Helper()
	b, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// execFn adapts a function to backend.Executor.
type execFn func(ctx context.Context, cmd string, o backend.ExecOptions) (backend.ExecResult, error)

func (f execFn) Execute(ctx context.Context, cmd string, o backend.ExecOptions) (backend.ExecResult, error) {
	return f(ctx, cmd, o)
}

// execMutant wraps the shell, letting mut change the call or its result.
func execMutant(mut func(inner backend.Executor, ctx context.Context, cmd string, o backend.ExecOptions) (backend.ExecResult, error)) func(*testing.T) {
	return func(t *testing.T) {
		RunExecutor(t, func(t *testing.T) backend.Executor {
			inner := realShell(t)
			return execFn(func(ctx context.Context, cmd string, o backend.ExecOptions) (backend.ExecResult, error) {
				return mut(inner, ctx, cmd, o)
			})
		})
	}
}

// procMut wraps a process; nil fields fall through to the real one.
type procMut struct {
	backend.Process
	result func(backend.ExecResult, bool) (backend.ExecResult, bool)
	output func(string) string
	stop   func(ctx context.Context, p backend.Process) error
}

func (p *procMut) Result() (backend.ExecResult, bool) {
	r, ok := p.Process.Result()
	if p.result != nil {
		return p.result(r, ok)
	}
	return r, ok
}

func (p *procMut) Output() string {
	if p.output != nil {
		return p.output(p.Process.Output())
	}
	return p.Process.Output()
}

func (p *procMut) Stop(ctx context.Context) error {
	if p.stop != nil {
		return p.stop(ctx, p.Process)
	}
	return p.Process.Stop(ctx)
}

type startFn func(ctx context.Context, cmd string, o backend.ExecOptions) (backend.Process, error)

func (f startFn) Start(ctx context.Context, cmd string, o backend.ExecOptions) (backend.Process, error) {
	return f(ctx, cmd, o)
}

// startMutant wraps every process the shell starts with wrap.
func startMutant(wrap func(ctx context.Context, p backend.Process) backend.Process) func(*testing.T) {
	return func(t *testing.T) {
		RunStarter(t, func(t *testing.T) backend.Starter {
			inner := realShell(t)
			return startFn(func(ctx context.Context, cmd string, o backend.ExecOptions) (backend.Process, error) {
				p, err := inner.Start(ctx, cmd, o)
				if err != nil {
					return nil, err
				}
				return wrap(ctx, p), nil
			})
		})
	}
}

// snapMut breaks one behaviour of a memory backend's snapshots.
type snapMut struct {
	*memory.Backend
	snap    func(ctx context.Context, m *memory.Backend) (json.RawMessage, error)
	restore func(ctx context.Context, m *memory.Backend, data json.RawMessage) error
}

func (s snapMut) Snapshot(ctx context.Context) (json.RawMessage, error) {
	return s.snap(ctx, s.Backend)
}

func (s snapMut) Restore(ctx context.Context, d json.RawMessage) error {
	return s.restore(ctx, s.Backend, d)
}

func snapMutant(snap func(context.Context, *memory.Backend) (json.RawMessage, error), restore func(context.Context, *memory.Backend, json.RawMessage) error) func(*testing.T) {
	if snap == nil {
		snap = func(ctx context.Context, m *memory.Backend) (json.RawMessage, error) { return m.Snapshot(ctx) }
	}
	if restore == nil {
		restore = func(ctx context.Context, m *memory.Backend, d json.RawMessage) error { return m.Restore(ctx, d) }
	}
	return func(t *testing.T) {
		RunSnapshotter(t, func(*testing.T) backend.Backend {
			return snapMut{Backend: testutil.Memory(t), snap: snap, restore: restore}
		})
	}
}

func init() {
	mutants["exec/ExitCode"] = execMutant(func(in backend.Executor, ctx context.Context, c string, o backend.ExecOptions) (backend.ExecResult, error) {
		r, err := in.Execute(ctx, c, o)
		r.ExitCode = 0
		return r, err
	})
	mutants["exec/Env"] = execMutant(func(in backend.Executor, ctx context.Context, c string, o backend.ExecOptions) (backend.ExecResult, error) {
		o.Env = nil
		return in.Execute(ctx, c, o)
	})
	mutants["exec/Timeout"] = execMutant(func(in backend.Executor, ctx context.Context, c string, o backend.ExecOptions) (backend.ExecResult, error) {
		r, err := in.Execute(ctx, c, o)
		r.TimedOut = false
		return r, err
	})
	mutants["exec/Truncate"] = execMutant(func(in backend.Executor, ctx context.Context, c string, o backend.ExecOptions) (backend.ExecResult, error) {
		o.MaxOutputBytes = 0
		return in.Execute(ctx, c, o)
	})
	mutants["exec/Cancel"] = execMutant(func(in backend.Executor, ctx context.Context, c string, o backend.ExecOptions) (backend.ExecResult, error) {
		r, err := in.Execute(ctx, c, o)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		return r, err
	})

	mutants["start/ResultBeforeDone"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		return &procMut{Process: p, result: func(r backend.ExecResult, _ bool) (backend.ExecResult, bool) {
			return r, true
		}}
	})
	mutants["start/Completes"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		return &procMut{Process: p, result: func(r backend.ExecResult, ok bool) (backend.ExecResult, bool) {
			r.Output = ""
			return r, ok
		}}
	})
	mutants["start/NotTiedToStartCtx"] = startMutant(func(ctx context.Context, p backend.Process) backend.Process {
		go func() {
			select {
			case <-ctx.Done():
				_ = p.Stop(context.WithoutCancel(ctx))
			case <-p.Done():
			}
		}()
		return p
	})
	mutants["start/OutputWhileRunning"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		return &procMut{Process: p, output: func(s string) string {
			select {
			case <-p.Done():
				return s
			default:
				return ""
			}
		}}
	})
	// Stop does nothing on its first call (the kit's cleanup calls it again).
	mutants["start/Stop"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		var calls atomic.Int32
		return &procMut{Process: p, stop: func(ctx context.Context, p backend.Process) error {
			if calls.Add(1) == 1 {
				return nil
			}
			return p.Stop(ctx)
		}}
	})
	mutants["start/StopExitCode"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		return &procMut{Process: p, result: func(r backend.ExecResult, ok bool) (backend.ExecResult, bool) {
			if r.ExitCode == -1 {
				r.ExitCode = 143
			}
			return r, ok
		}}
	})
	// A second Stop fails.
	mutants["start/StopIdempotent"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		var calls atomic.Int32
		return &procMut{Process: p, stop: func(ctx context.Context, p backend.Process) error {
			err := p.Stop(ctx)
			if calls.Add(1) > 1 && err == nil {
				err = errors.New("already stopped")
			}
			return err
		}}
	})
	mutants["start/Timeout"] = startMutant(func(_ context.Context, p backend.Process) backend.Process {
		return &procMut{Process: p, result: func(r backend.ExecResult, ok bool) (backend.ExecResult, bool) {
			r.TimedOut = false
			return r, ok
		}}
	})

	mutants["snap/RoundTrip"] = snapMutant(func(context.Context, *memory.Backend) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}, nil)
	mutants["snap/RestoreMerges"] = snapMutant(nil, func(ctx context.Context, m *memory.Backend, d json.RawMessage) error {
		old, err := m.Read(ctx, "/x", 0, 0)
		if err := m.Restore(ctx, d); err != nil {
			return err
		}
		if err == nil && len(old.Lines) > 0 {
			return m.Write(ctx, "/x", []byte("x\n"))
		}
		return nil
	})
	mutants["snap/CancelledSnapshot"] = snapMutant(func(_ context.Context, m *memory.Backend) (json.RawMessage, error) {
		return m.Snapshot(context.Background())
	}, nil)
	mutants["snap/CancelledRestore"] = snapMutant(nil, func(_ context.Context, m *memory.Backend, d json.RawMessage) error {
		return m.Restore(context.Background(), d)
	})
	mutants["snap/BadDataAccepted"] = snapMutant(nil, func(ctx context.Context, m *memory.Backend, d json.RawMessage) error {
		_ = m.Restore(ctx, d)
		return nil
	})
	mutants["snap/BadDataClobbers"] = snapMutant(nil, func(ctx context.Context, m *memory.Backend, d json.RawMessage) error {
		err := m.Restore(ctx, d)
		if err != nil {
			_ = m.Delete(ctx, "/keep.txt")
		}
		return err
	})
	for k, v := range map[string]string{
		"exec/Echo": "Echo", "start/StopExitCode": "Stop",
		"snap/RestoreMerges": "RestoreReplaces", "snap/CancelledSnapshot": "Cancelled",
		"snap/CancelledRestore": "Cancelled", "snap/BadDataAccepted": "BadData",
		"snap/BadDataClobbers": "BadData",
	} {
		flagged[k] = v
	}
}
