// Package file provides a checkpoint.Checkpointer that stores each thread as
// an append-only JSONL file.
//
// # Layout
//
// A thread lives in <dir>/<h>.jsonl where h is the first 32 hex characters of
// sha256(threadID). The name never derives from the raw id, so ids that differ
// only by case cannot collide on case-insensitive file systems and long or
// path-like ids cannot overflow NAME_MAX or escape dir. Every record repeats
// the real thread id, which is what List reports.
//
// Each line is one JSON object, either a full snapshot of the state or a
// delta against the previous save (the common message prefix to keep, the
// messages to append, changed values, deleted value names). The first save
// and every SnapshotEvery-th delta after a snapshot write a snapshot, which
// bounds replay work.
//
// The file does not grow forever: a Save that would take it past
// compactAfter records instead replaces it, atomically, with a single
// snapshot of the new state. The snapshot continues the version sequence,
// so versions issued before the compaction stay stale.
//
// # Durability
//
// A record is written with a single Write on an O_APPEND handle and, unless
// Config.DisableSync is set, fsynced before Save returns; a thread's first
// save also fsyncs the directory so the new file's entry survives a crash.
// After Save returns nil the checkpoint survives process and OS crashes
// (subject to the storage device honoring fsync). Windows has no directory
// fsync, so there the directory step is skipped: the default sync covers the
// records but not a new or compacted file's directory entry, which is as
// durable as the file system makes it on its own. A crash mid-write can leave
// a torn final line, one without its newline: readers ignore it, and the next
// Save truncates it away before appending. A newline-terminated line that does
// not parse, even the last one, cannot come from a crash and is reported as
// corruption. A compaction writes a temporary file in the same directory,
// fsyncs it and renames it over the thread file, so a reader sees either the
// old file or the new one, never a mix.
//
// # Versions
//
// Every record carries the thread's generation, a random token chosen when
// the thread file is started, and its sequence number; together they form
// the checkpoint.Version. A thread deleted and saved again gets a new
// generation, so versions never repeat. Files written before versions
// existed have no generation; their version is ":<seq>" until the thread is
// recreated.
//
// # Concurrency
//
// A Checkpointer serializes all operations with a mutex, so the version
// check and the append of a Save are atomic among the callers of one
// Checkpointer, and its leases (checkpoint.Leaser) hold among them.
// Several Checkpointers, in one or several processes, must not write the
// same directory at once; that is unsupported, and neither versions nor
// leases protect against it. Use a shared store such as a database for
// several processes. Edits made behind a Checkpointer's back are only
// noticed when they change a thread file's size, in which case the file is
// replayed again.
package file
