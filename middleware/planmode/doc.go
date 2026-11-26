// Package planmode lets the agent plan before it acts. enter_plan_mode
// switches the run to the read-only plan permission mode, and exit_plan_mode
// submits a plan for approval and, once approved, restores the previous mode.
//
// The approval is an ordinary human decision: the run's Approver answers it
// when the agent has one, otherwise the run pauses with an interrupt that
// offers approve, edit, reject and respond. A rejection or a response keeps
// plan mode and hands the feedback to the model, so it can revise the plan.
//
// The plan is text written by the model. It is shown to the Approver or host
// as data (the tool input) and never interpreted: the permission request's
// Subject is the plan text itself (a cut plan ends with TruncationMarker).
//
// # Asking twice
//
// exit_plan_mode asks for its own approval, with DisableRemember set. Do not
// also put it under an approval elsewhere: an "ask" permission rule on it, or
// hitl's InterruptOn, adds a second, loop-level prompt before the tool runs,
// and that request does not carry DisableRemember, so an Approver may answer
// it from an "always allow" it remembered. That is safe, since the tool's own
// prompt still decides, but the human is asked twice for one plan.
package planmode
