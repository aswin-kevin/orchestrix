// Package hitl adds human-in-the-loop approval to chosen tools, configured
// per tool name or name pattern: which decisions a human may answer with,
// and an optional predicate narrowing which calls need one.
//
// It sits on top of the permission pipeline and does not replace it: a
// configured call pauses even in bypass mode, but a deny rule still refuses
// it first. A call a human already approved in the permission step is not
// asked about again when its InterruptConfig allows Approve; when it does
// not, the approval does not count: the call is rejected when only Reject is
// allowed, and otherwise asked about again so that the human gives a decision
// hitl allows (an Edit or a Respond). An Approver can only allow, perhaps
// with a rewrite, or deny, so when the agent has one and Edit is not allowed
// either, the call is rejected without asking it. The pause itself is the
// agent's ordinary interrupt: the host answers it with Agent.Resume.
package hitl
