package composite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Capability mixins. They hold the composite by name (not by embedding) so a
// variant can combine several without ambiguous Backend methods.

type execer struct{ c *composite }

func (e execer) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	if err := e.c.checkDir(opts.Dir); err != nil {
		return backend.ExecResult{}, err
	}
	return e.c.exec.Execute(ctx, cmd, opts)
}

type starter struct{ c *composite }

func (s starter) Start(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.Process, error) {
	if err := s.c.checkDir(opts.Dir); err != nil {
		return nil, err
	}
	return s.c.start.Start(ctx, cmd, opts)
}

type snapper struct{ c *composite }

func (s snapper) Snapshot(ctx context.Context) (json.RawMessage, error) {
	return s.c.snap.Snapshot(ctx)
}

func (s snapper) Restore(ctx context.Context, data json.RawMessage) error {
	return s.c.snap.Restore(ctx, data)
}

// checkDir rejects a working directory inside a mount: commands run in the
// default backend, which cannot see mounted content.
func (c *composite) checkDir(dir string) error {
	if dir == "" {
		return nil
	}
	p, err := backend.Clean(dir)
	if err != nil {
		return err
	}
	if m, _, ok := c.resolve(p); ok {
		return fmt.Errorf("composite: exec dir %s is inside mount %s: %w", p, m.prefix, errors.ErrUnsupported)
	}
	return nil
}

type (
	withExec struct {
		*composite
		execer
	}
	withExecStart struct {
		*composite
		execer
		starter
	}
	withSnap struct {
		*composite
		snapper
	}
	withExecSnap struct {
		*composite
		execer
		snapper
	}
	withExecStartSnap struct {
		*composite
		execer
		starter
		snapper
	}
)

// withCapabilities returns the variant of c that exposes what its default
// backend can do.
func withCapabilities(c *composite) backend.Backend {
	var exec, start, snap bool
	c.exec, exec = backend.AsExecutor(c.def)
	c.start, start = backend.AsStarter(c.def)
	c.snap, snap = backend.AsSnapshotter(c.def)
	e, s, sn := execer{c}, starter{c}, snapper{c}
	switch {
	case exec && start && snap:
		return withExecStartSnap{c, e, s, sn}
	case exec && start:
		return withExecStart{c, e, s}
	case exec && snap:
		return withExecSnap{c, e, sn}
	case exec:
		return withExec{c, e}
	case snap:
		return withSnap{c, sn}
	}
	return c
}
