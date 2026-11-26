package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/state"
)

// workspaceKey holds the snapshot of a run's backend (backend.Snapshotter)
// when Config.SyncWorkspace is set. It is run-local (state.RunLocal): neither
// a subagent nor a fork inherits its parent's workspace through State.
var workspaceKey = state.NewKey[json.RawMessage]("agent.workspace", state.RunLocal[json.RawMessage]())

// A persisted run saves at the five points Config.Checkpointer lists. The
// first save follows the BeforeAgent hooks, so the input is stored only once
// they have accepted (or rewritten) it, with the orphans of an earlier crash
// resolved when the orphan pass runs (see OrphanResolver). A crash between
// the save after a reply and the save after its results loses that turn's
// results; OrphanResolver covers them. The workspace is captured (Snapshot)
// only on the saves that follow tool calls (tool results, interrupt, end of
// run), because only tools change files; this spares a full backend capture
// on every save. Once the workspace is captured, every save still writes its
// value, the latest capture, along with the rest of State.
//
// Saves run under a context that is not cancelled with the run, so a
// cancelled run still records how far it got and its reason stays the
// cancellation, not a failed save.

// storeFor returns the checkpointer a run of in saves to, or nil when the run
// is not persisted.
func (a *Agent) storeFor(in Input) checkpoint.Checkpointer {
	if in.ThreadID == "" || in.Ephemeral {
		return nil
	}
	return a.cfg.Checkpointer
}

// workspaceFor returns the backend whose content a run of in keeps in State,
// or nil when the workspace is not synced. A backend passed in Input belongs
// to the caller (a parent run, typically), so it is never synced.
func (a *Agent) workspaceFor(in Input, be backend.Backend) backend.Snapshotter {
	if !a.cfg.SyncWorkspace || in.Backend != nil {
		return nil
	}
	snapshotter, _ := backend.AsSnapshotter(be)
	return snapshotter
}

// restoreWorkspace fills an empty backend from the snapshot in State. A
// backend that holds anything is live and authoritative, so it is never
// overwritten. On failure the run stops syncing, so the stored snapshot is
// not replaced by a capture of the unrestored backend.
func (r *run) restoreWorkspace() error {
	if r.workspace == nil {
		return nil
	}
	snapshot, ok := workspaceKey.Get(r.state)
	if !ok {
		return nil
	}
	err := r.restoreInto(snapshot)
	if err != nil {
		r.workspace = nil
	}
	return err
}

// restoreInto restores snapshot into the run's backend when it is empty.
func (r *run) restoreInto(snapshot json.RawMessage) error {
	return restoreEmpty(r.ctx, r.backend, r.workspace, snapshot)
}

// restoreEmpty restores snapshot into be, through its Snapshotter ws, when
// be is empty.
func restoreEmpty(ctx context.Context, be backend.Backend, ws backend.Snapshotter, snapshot json.RawMessage) error {
	entries, err := be.Ls(ctx, "/")
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return fmt.Errorf("agent: restore workspace: list backend: %w", err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := ws.Restore(context.WithoutCancel(ctx), snapshot); err != nil {
		return fmt.Errorf("agent: restore workspace: %w", err)
	}
	return nil
}

// ThreadBackend returns the backend a run of threadID resolves for itself,
// with no Input.Backend: BackendFactory(threadID), else Config.Backend; nil
// when the agent has neither. When the workspace is synced
// (Config.SyncWorkspace) and s holds a snapshot of it, the snapshot is
// first restored into that backend if it is empty, exactly as a run
// starting from s would do. Code that works on a thread's backend outside
// its runs, such as a background task a host resumes after a restart, uses
// it so that its files add to the thread's saved ones: a later run finds
// the backend filled, so it skips the restore and captures what it holds,
// which must then include the saved files. s may be nil (nothing is
// restored). It fails when the restore fails. It must not be called while a
// run of threadID is live: that run restores and captures the same backend
// itself.
func (a *Agent) ThreadBackend(ctx context.Context, threadID string, s *state.State) (backend.Backend, error) {
	be := a.backendFor(Input{ThreadID: threadID})
	if be == nil || s == nil {
		return be, nil
	}
	ws := a.workspaceFor(Input{ThreadID: threadID}, be)
	if ws == nil {
		return be, nil
	}
	snapshot, ok := workspaceKey.Get(s)
	if !ok {
		return be, nil
	}
	if err := restoreEmpty(ctx, be, ws, snapshot); err != nil {
		return nil, err
	}
	return be, nil
}

// snapshotWorkspace captures the backend's content into State.
func (r *run) snapshotWorkspace() error {
	if r.workspace == nil {
		return nil
	}
	snapshot, err := r.workspace.Snapshot(context.WithoutCancel(r.ctx))
	if err != nil {
		return fmt.Errorf("agent: snapshot workspace: %w", err)
	}
	workspaceKey.Set(r.state, snapshot)
	return nil
}

// checkpoint saves State mid-run; withWorkspace first captures the
// workspace into it.
func (r *run) checkpoint(withWorkspace bool) error {
	if r.store == nil {
		return nil
	}
	if withWorkspace {
		if err := r.snapshotWorkspace(); err != nil {
			return err
		}
	}
	return r.save()
}

// save stores State in the run's checkpointer, if it has one, over the
// version the run holds. After a conflict the thread belongs to another
// writer, so the run stops saving: the conflict is reported once.
func (r *run) save() error {
	if r.store == nil {
		return nil
	}
	if err := r.lease.lost(); err != nil {
		r.store = nil
		r.logger.Warn("agent: checkpoint save skipped", "err", err)
		return fmt.Errorf("agent: checkpoint: %w", err)
	}
	v, err := r.store.Save(context.WithoutCancel(r.ctx), r.threadID, r.state, r.version)
	if err != nil {
		if errors.Is(err, checkpoint.ErrConflict) {
			r.store = nil
		}
		r.logger.Warn("agent: checkpoint save failed", "err", err)
		return fmt.Errorf("agent: checkpoint: %w", err)
	}
	r.version = v
	return nil
}

// checkpointFailure is the Final for a run a failed save ended.
func (r *run) checkpointFailure(err error) *Final {
	return r.finish(ReasonCheckpointError, err)
}

// finalCheckpoint captures the workspace and saves State at the end of the
// run, and folds a failure into final: a run that had otherwise succeeded
// ends with ReasonCheckpointError; one that had already failed keeps its
// reason and gains the error. The workspace is captured even without a
// Checkpointer, so Final.State carries it to whoever keeps the State.
func (r *run) finalCheckpoint(final *Final) {
	err := r.snapshotWorkspace()
	if err == nil {
		err = r.save()
	}
	if err == nil {
		return
	}
	if final.Err == nil {
		final.Reason = ReasonCheckpointError
		final.Output = nil
	}
	final.Err = errors.Join(final.Err, err)
}
