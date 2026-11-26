package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/backend"
)

// StarterBackend is a Backend whose executor can also start background
// commands. It adds backend.Starter, so it works with the jobs middleware
// (orchestrix.WithJobs). Build one with NewStarter.
type StarterBackend struct {
	// Backend is the sandbox backend whose file operations and Execute
	// StarterBackend provides.
	*Backend
	start backend.Starter
}

var (
	_ backend.Backend     = (*StarterBackend)(nil)
	_ backend.MultiEditor = (*StarterBackend)(nil)
	_ backend.Executor    = (*StarterBackend)(nil)
	_ backend.Starter     = (*StarterBackend)(nil)
)

// NewStarter is New for an executor that also implements backend.Starter.
// The Backend New returns never implements backend.Starter, so a type check
// for it tells the truth about what the executor can do. NewStarter fails
// like New.
func NewStarter(exec backend.ExecStarter, cfg Config) (*StarterBackend, error) {
	if exec == nil {
		return nil, errors.New("sandbox: nil executor")
	}
	b, err := New(exec, cfg)
	if err != nil {
		return nil, err
	}
	return &StarterBackend{Backend: b, start: exec}, nil
}

// Start starts cmd in Root (or Root+opts.Dir) through the executor's Start,
// entering the directory with the same quoted cd as Execute; if the cd fails,
// the command exits without running. opts.Timeout, Env and MaxOutputBytes
// pass through unchanged.
func (b *StarterBackend) Start(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.Process, error) {
	cmd, opts, err := b.prepare(cmd, opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: start: %w", err)
	}
	return b.start.Start(ctx, cmd, opts)
}
