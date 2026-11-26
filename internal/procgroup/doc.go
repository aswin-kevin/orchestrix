// Package procgroup runs a child process as the leader of its own process
// group, so that everything the child spawns can be signalled and reaped
// together. It is shared by the shell backend and the MCP stdio transport.
//
// On platforms without POSIX process groups (Windows) it is best-effort: only
// the started process itself is killed, so processes it spawned may outlive
// it, and Alive is always false.
package procgroup
