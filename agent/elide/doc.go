// Package elide records elisions in a run's State: ranges of messages the
// request view replaces with one user message, so middleware such as compress
// can shrink what the model sees while State keeps every message.
//
// Elisions live in State under a private key: State.Fork copies them,
// State.Child does not (a subagent's history is its own), and they are
// checkpointed with the thread. The agent loop applies them when it builds
// each model request (View(s, 0)).
//
// The indices stay valid because messages are only appended by the loop, and
// a resume inserts results right after an assistant message whose calls are
// unanswered, which no self-contained elision can contain; elisions lying
// after that insertion point (over messages a hook appended) are shifted by
// the number of messages inserted. A host that replaces State's messages
// should call Clear. View re-checks every elision against the current
// messages and skips one that no longer fits, so stale elisions can make the
// view less compact but never invalid. It cannot tell, though, when a range
// still fits but now holds different messages (a host rewound or rewrote
// State and kept the elisions): the view then shows that elision's Text in
// place of messages it does not describe. Calling Clear whenever State is
// replaced is what prevents this.
package elide
