// Package filesystem provides the agent's workspace tools (ls, read_file,
// write_file, edit_file, multi_edit, delete, glob, grep and, when the backend
// can run commands, execute) as a middleware over a pluggable backend.
//
// The middleware also enforces read-before-edit: a file that already exists
// can only be changed after the agent has read it, and only while it has not
// changed since. This stops the model from overwriting content it never saw.
package filesystem
