// Package orxdocker runs an Orchestrix agent's tools in Docker containers,
// one per thread. It is the recommended backend when tools run untrusted
// commands or touch untrusted targets.
//
// A Manager creates each thread's container on first use and gives the
// agent a sandboxed backend for it:
//
//	m, err := orxdocker.NewManager(orxdocker.Config{})
//	...
//	defer m.Close(context.Background())
//	tm, err := jobs.New(jobs.Config{})
//	...
//	a, err := orchestrix.New("anthropic:claude-sonnet-4-5",
//		orchestrix.WithBackendFactory(m.Backend),
//		orchestrix.WithJobs(tm), // background jobs run in the container too
//	)
//	...
//	defer a.Close(context.Background()) // stops the jobs too
//
// A Manager is an agent.Closer. Agent.Close does not close what a backend
// factory draws from, so it closes a Manager only when the host passes
// m.Close in agent.Config.OnClose; otherwise the host closes it, as any
// Closer, once the agent is done. Close(ctx) is bounded by ctx, and only
// its first call does the work.
//
// Manager.Backend returns a sandbox.StarterBackend (see backend/sandbox):
// file tools and execute work in the container's workspace, and background
// jobs work with WithJobs. Containers are removed after Config.IdleTTL
// without commands, by Manager.Delete when a thread is deleted, and by
// Manager.Close; Manager.Reap clears containers left by a crashed process.
// A Manager keeps at most Config.MaxThreads executors, evicting the least
// recently used idle one (and its container) beyond that.
// Every container carries labels (LabelManaged, LabelScope, LabelThread,
// LabelCreated) for that and for operators.
//
// # Isolation
//
// Containers get hardened settings by default; every Config field that
// weakens them is an explicit opt-out:
//
//   - commands run as nobody (65534:65534), not root
//   - the root file system is read-only; only the workspace
//     (Config.WorkDir, default /workspace) and /tmp are writable, both
//     size-limited tmpfs mounts
//   - all capabilities are dropped, no-new-privileges is set, and Docker's
//     default seccomp profile applies
//   - process, memory (swap disabled) and CPU limits; Unlimited removes one
//   - Docker's init process runs as PID 1 and reaps orphaned processes
//     (Config.DisableInit turns it off)
//   - no network at all (network mode "none"); Config.Network joins a named
//     network instead, and filtering that network's traffic, the cloud
//     metadata address included, is then the network's job
//   - no host environment: a command sees the image's environment,
//     Config.Env and ExecOptions.Env only, the latter winning on a shared
//     key
//
// The workspace is a tmpfs: it lives as long as the container, counts
// against the memory limit, and is gone when the container is removed.
// orxdocker does not implement backend.Snapshotter, so checkpoints do not
// capture workspace files.
//
// # Commands, timeouts and output
//
// Docker cannot stop an exec: cancelling the request leaves the process
// running in the container. So each command runs under a small sh wrapper
// that starts it with setsid in its own process group and records the group
// id in /tmp. A timeout, Process.Stop or a cancelled Execute context sends
// SIGTERM to that group through a second exec, then SIGKILL after
// backend.DefaultKillGrace. A backstop process inside the container kills
// the group a few seconds after the timeout even if the host process died.
// The pid file and the backstop run as the command's user, so a hostile
// command can defeat them. Execute and timeouts stay bounded all the same:
// when a stop does not end the command, or it still runs 10 seconds after
// its timeout, the container is force-removed (ending the thread's other
// commands and its workspace) and the thread's next command starts with a
// new one; as a best effort, so is a timed-out or stopped command whose
// wrapper died of a signal before any signal reached its group.
// When a command's shell exits, whatever it left running in its group is
// killed too. A command can still leave processes running in its thread's
// container, by detaching them or by killing its wrapper before its
// timeout. They stay confined by the container's hardening (no network,
// dropped capabilities, resource limits) and end when the container is
// removed: Manager.Delete, Manager.ReapIdle, the idle TTL or Manager.Close. Output (stdout and stderr interleaved) is capped while it
// streams at ExecOptions.MaxOutputBytes, keeping the first and last halves.
//
// The daemon is reached with the standard library only, over its Unix
// socket (Config.Socket, else a unix:// DOCKER_HOST, else DefaultSocket) or
// over the connections Config.Dial opens, for instance through an SSH
// tunnel or a TLS connection to a remote daemon. Without Dial, Windows
// hosts are not supported at run time.
package orxdocker
