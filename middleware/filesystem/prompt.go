package filesystem

// DefaultPrompt is the system prompt segment that explains the workspace
// tools. It is neutral guidance only; the host supplies any persona.
const DefaultPrompt = `You can inspect and change files in a workspace with the filesystem tools.

- Paths are absolute and start at the workspace root "/".
- Read a file with read_file before you edit or overwrite it; edits to a file you have not read, or that changed since you read it, are rejected.
- Prefer edit_file or multi_edit over rewriting a whole file with write_file.
- Use glob to find files by name and grep to search file contents before reading many files.
- Long files are returned in pages; use offset and limit to read further.`
