// Package shell provides a disk backend that can also run commands on the
// host.
//
// Every command runs in a fresh "/bin/sh -c" from the workspace root (or
// ExecOptions.Dir), so `cd` and `export` do not carry over between commands;
// chain steps with `&&`. On Unix each command leads its own process group:
// when the shell exits, times out or is stopped, the whole group is killed
// (SIGTERM, then SIGKILL after DefaultKillGrace), so nothing a command spawns
// inside the group outlives it. Long-lived work belongs in background jobs
// (Starter), not in `cmd &`.
//
// This is cleanup, not containment: a process that leaves the group (setsid,
// a daemonizing double fork, a new session) is neither tracked nor killed and
// keeps running after the command ends. Execute waits at most
// DefaultKillGrace for such a process to release the output pipe, then
// returns without it. Use an OS-level sandbox when commands must not escape.
//
// Commands get no stdin, a small allowlisted environment (InheritedEnv) plus
// the variables of NonInteractiveEnv and SafeGitEnv (the host environment,
// which may hold API keys, is not inherited unless Config.InheritEnv is
// set), and output capped at a head and a tail so memory stays bounded.
// Those three functions return copies: changing what they return does not
// change what commands get.
//
// Commands need Shell (/bin/sh), so they run on Unix only. On Windows the
// file operations work, but every command fails to start; use a disk
// backend there, or run commands in a container (orxdocker). The memory
// limit is enforced on Linux only.
package shell
