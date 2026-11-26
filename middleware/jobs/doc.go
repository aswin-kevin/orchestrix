// Package jobs runs shell commands as background jobs of a thread. With it,
// the filesystem middleware's execute tool accepts run_in_background (see
// filesystem.Config.Background), and the model gets tools to read a job's
// output (check_job), cancel it (cancel_job), list jobs (list_jobs), wait for
// one to finish (sleep) and watch its output for a pattern (monitor).
//
// When a job ends, the next turn of the thread starts with a user message
// announcing it, so the model learns of it without polling. Jobs belong to a
// thread, not to a run: a top-level run may end while its jobs keep running,
// and a later run on the thread picks them up. A nested (subagent) run's jobs
// are stopped when it ends. Close stops every job; call it, or Close the
// agent, before the program exits, since jobs are processes of the host.
//
// Job records live in the run's State (see Records); a job recorded as running
// that the middleware does not know, because the State was saved by an
// earlier process, is reported as lost. A run only reads, announces and
// stops the jobs of its own thread.
package jobs
