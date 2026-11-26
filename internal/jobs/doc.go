// Package jobs is a thread-scoped registry of background work. Each thread
// numbers its jobs prefix+N, with numbers that never repeat even when the
// thread's entry was released and the process restarted (the caller passes the
// ids it recorded). A thread has a running limit and a bounded history of
// finished jobs that forgets announced jobs first; the registry offers a
// change signal, release, stop-and-forget, and a bounded Close.
//
// It is generic over the job handle, so shell tasks and background subagents
// share it. All Manager methods are safe for concurrent use.
package jobs
