// Package hooks runs host Go callbacks at the lifecycle points of a run:
// before and after tool calls, when a prompt is submitted, when a session
// starts and ends, when the model is about to stop, and when a subagent is
// launched. A hook can block, rewrite a tool's input, inject context into the
// conversation and keep a run going past its natural end. Hooks also fire
// inside subagent runs and resumed runs (see Config).
//
// Hooks are trusted host code: unlike the model, they may route a tool call
// to a place the permission Ask would have questioned (a Deny rule still
// refuses it).
package hooks
