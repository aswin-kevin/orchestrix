// Package askuser lets the agent put structured multiple-choice questions to
// the human. The ask_user tool is answered either by a synchronous Asker
// (Config.Asker) or, without one, by pausing the run with an interrupt that
// the host answers with Respond (the answers) or Reject (declined).
//
// Questions, options and headers are text written by the model. They are
// shown to the human as data: control characters other than newline are
// stripped and every part is length-capped before it is rendered or handed to
// a callback.
//
// ask_user is read-only, so it is available in plan mode and, unlike a tool
// that needs approval, in dontAsk mode too: dontAsk governs permission
// prompts, not questions the model asks.
package askuser
