// Package memory puts standing instructions into the system prompt: files
// such as AGENTS.md that the host names, read from the run's backend once per
// run and appended after the host's own system segments.
//
// # Trust
//
// A file's content goes into the system prompt as written, so whoever can
// write a configured file can steer the agent. The default prompt also invites
// the model to save durable notes into these files with edit_file: anything
// the model read during a run, including untrusted web pages or tool output,
// can thereby become standing instructions in later runs. The files are
// named by the host, so they are read directly from the backend and are not
// subject to permission path rules; only list files whose writers you trust,
// and use Config.Prompt to drop the invitation if the agent should not edit
// them. The only transformation that protects the framing is that a literal
// opening or closing tag of the prompt's own elements (<memory>,
// <memory_index>) inside a file is neutralized, so it can neither end its
// element nor open a fake one.
//
// The structured store (Config.Store) raises the same concern: record_memory
// persists whatever the model passes it, so content the model read from an
// untrusted page or tool result can be recalled in later runs, and its
// descriptions reach the system prompt through the index. Treat recorded
// entries as untrusted data, not instructions: keep DisableIndexInPrompt set
// where that matters, and deny the record_memory tool or, for a FileStore,
// write access to its directory where the agent must not remember.
// recall_memory honors read deny rules per entry of a FileStore, but the
// index in the system prompt lists the name and description of every entry
// regardless of them, so keep secrets out of names and descriptions or set
// DisableIndexInPrompt. The instruction files above are not filtered either.
// A Store that does not implement Pather is not path-checked at all: only
// rules on the tools themselves reach it.
package memory
