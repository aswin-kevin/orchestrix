// Package guard blocks a run's input and its tool calls when a decision model
// says they break a rule. Each Check is a yes/no question; a probability of
// yes at or above its threshold blocks.
//
// What is checked: Input checks see the joined text of the run's user input,
// in BeforeAgent, and only in top-level runs (agent.RunInfo.Depth 0) that
// carry input, so a resumed run is not checked again. A blocked input ends
// the run with agent.ReasonHookStopped and an error naming the check.
// ToolCall checks see the tool's name and JSON arguments of every call, at
// any depth. The arguments are decoded and encoded again first, so string
// escapes such as \u0072 cannot hide what the tool will receive; arguments
// that are not valid JSON fail every check. A blocked call is not run and
// gets an error result naming the check, so the model can adapt. All checks for one target go in one Decide
// call. Text longer than Config.MaxInputBytes is never sent to the decider,
// nor checked by a prefix: every check of that target fails, as if the
// decider had erred, so by default the input or call is blocked. Raise
// MaxInputBytes for tools whose arguments are large. Non-text input blocks
// (images, documents) are not checked, and input with no text is not checked.
//
// The guard is a safety control, so it fails closed: when the decider
// returns an error, refuses a check or leaves one unanswered, the input or
// call is blocked. Config.FailOpen lets it through instead. Either way the
// failure is recorded in the verdict.
//
// The tool checks run as an agent.ToolWrapper, after permission rules and
// human approval, so they check every call that is about to run, approved
// ones too. A subagent whose spec has its own Middleware list does not
// inherit the guard; add it to that list to guard the subagent.
//
// Verdicts are kept per run in State (see Verdicts) and cleared when a run
// starts, including a resumed one. The decider's calls are its own: their
// tokens are not in the run's Final.Usage and do not count towards
// agent.Config.TokenBudget, and each call is bounded by Config.Timeout.
package guard
