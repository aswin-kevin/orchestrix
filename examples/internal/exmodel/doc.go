// Package exmodel lets the example programs run offline by default and
// against a real model on request.
//
// Without ORX_MODEL every example replays a short script, so it runs
// anywhere with no network and no API key. With ORX_MODEL set to a model
// spec such as anthropic:claude-sonnet-4-5 or openai:gpt-4o, the same code
// talks to that model, reading its API key from ANTHROPIC_API_KEY or
// OPENAI_API_KEY as the orx CLI does.
//
// Copying an example out of this repository: this package and the termsafe
// package it uses are internal, so they cannot be imported from another
// module. Replace exmodel.Model(...), exmodel.New(model, ...) and
// exmodel.Coordinator(model, ...) with
// orchestrix.New("anthropic:claude-sonnet-4-5", ...) or
// orchestrix.NewWithProvider(provider, ...), drop exmodel.Sub and
// exmodel.SubHeld (a subagent without a Provider uses its parent's model),
// drop exmodel.WaitTasks and exmodel.Check (they only keep the examples
// predictable), and replace exmodel.Safe with your own escaping of untrusted
// text before printing it.
package exmodel
