// Command orx is a small demo of the Orchestrix SDK: a coding-agent CLI that
// runs one prompt (-p) or an interactive session, streams the reply, and asks
// before running tools that need approval. It exits with status 1 when a
// prompt does not complete (a model error, a limit such as -max-turns, or
// Ctrl-C) and 2 for usage errors.
package main
