// Package elision holds the elisions of a run's State: ranges of messages the
// request view replaces with one user message. Package agent applies them
// when it builds a model request and shifts them when a resume inserts
// messages; agent/elide is the public API over them.
package elision
