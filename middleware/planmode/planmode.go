package planmode

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// The names of the tools the middleware offers.
const (
	ToolEnter = "enter_plan_mode"
	ToolExit  = "exit_plan_mode"
)

// The questions put to the human.
const (
	// approverReason is the reason an Approver is given.
	approverReason = "Approve this plan and leave plan mode?"
	// interruptReason is the reason of the interrupt raised without an
	// Approver.
	interruptReason = "Approve the plan to leave plan mode"
)

// Limits on the plan text.
const (
	// maxPlanBytes is the largest plan exit_plan_mode accepts.
	maxPlanBytes = 64 << 10
	// maxSubjectBytes is how much of the plan the permission request's
	// Subject (shown to a human Approver) carries before it is cut. It
	// equals maxPlanBytes, so every plan that is accepted reaches the
	// Approver whole; the cut only guards inputs that bypass checkPlan.
	maxSubjectBytes = maxPlanBytes
)

// TruncationMarker ends the Subject of an exit_plan_mode permission request
// whose plan was cut, so an Approver can tell it has not seen the whole plan.
const TruncationMarker = "\n... [plan truncated]"

// previousKey holds the mode a run was in when it entered plan mode. It is
// bookkeeping of the run that owns it, so forks do not inherit it: a forked
// thread that is still in plan mode has no recorded mode, and approving its
// plan restores Config.ApproveMode, else permission.ModeDefault. It never
// escalates beyond what the host configured.
var previousKey = state.NewKey[permission.Mode]("planmode.previous", state.RunLocal[permission.Mode]())

// planKey holds the last approved plan. It is private to the run.
var planKey = state.NewKey[string]("planmode.plan", state.Private[string]())

// Plan returns the last approved plan of the run, and false when none was
// approved yet.
func Plan(s *state.State) (string, bool) { return planKey.Get(s) }

// Name is the name of the planmode middleware.
const Name = "planmode"

// Config configures the planmode middleware.
type Config struct {
	// ApproveMode is the permission mode after the plan is approved. Empty
	// means the mode recorded when plan mode was entered, unless none is
	// recorded or it is plan (the run started in plan mode, or is a fork
	// of a run that entered it), in which case permission.ModeDefault.
	ApproveMode permission.Mode
	// DisableEnter offers only exit_plan_mode, for hosts that start runs in
	// plan mode. False offers both tools.
	DisableEnter bool
	// Prompt is the system prompt segment shown while plan mode is active.
	// nil uses DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string
}

// EnterInput is the input of enter_plan_mode.
type EnterInput struct {
	// Reason says why planning first helps.
	Reason string `json:"reason,omitempty" desc:"Why planning first helps."`
}

// ExitInput is the input of exit_plan_mode.
type ExitInput struct {
	// Plan is the implementation plan in markdown: steps, files to change
	// and how to verify.
	Plan string `json:"plan" desc:"The implementation plan in markdown: steps, files to change, how to verify."`
}

// Middleware is the planmode middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	cfg    Config
	prompt string
}

// New returns the planmode middleware.
func New(cfg Config) (*Middleware, error) {
	m := &Middleware{cfg: cfg, prompt: DefaultPrompt}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Required implements agent.Required: plan mode is a safety check, so a
// MiddlewareExcluder may not remove it.
func (m *Middleware) Required() bool { return true }

// Tools implements agent.ToolProvider.
func (m *Middleware) Tools() []tool.Tool {
	exit := tool.New(ToolExit,
		"Submit your plan for the user's approval and leave plan mode. Call it once you have investigated and have a concrete plan.",
		m.exit,
		// Both tools change only the run's own state, so they count as
		// read-only and work in plan and dontAsk modes. They are not
		// concurrency-safe: the mode they set must apply to the calls that
		// follow in the same turn.
		tool.WithReadOnly(),
		// The subject lets a human Approver see what it approves; rules
		// can match it too.
		tool.WithSubject(planSubject))
	if m.cfg.DisableEnter {
		return []tool.Tool{exit}
	}
	enter := tool.New(ToolEnter,
		"Switch to plan mode: explore read-only and write a plan before changing anything. Use it for large or risky tasks.",
		enter, tool.WithReadOnly())
	return []tool.Tool{enter, exit}
}

// Active reports whether the run with State s is in plan mode: its effective
// permission mode is permission.ModePlan.
func Active(ctx context.Context, s *state.State) bool {
	if info, ok := agent.Info(ctx); ok {
		return info.Agent.PermissionMode(s) == permission.ModePlan
	}
	mode, _ := agent.PermissionModeOf(s)
	return mode == permission.ModePlan
}

// modeOf returns the effective mode of the run of ctx, ModeDefault when it
// is unset or ctx belongs to no run.
func modeOf(ctx context.Context, s *state.State) permission.Mode {
	var mode permission.Mode
	if info, ok := agent.Info(ctx); ok {
		mode = info.Agent.PermissionMode(s)
	} else {
		mode, _ = agent.PermissionModeOf(s)
	}
	if mode == "" {
		return permission.ModeDefault
	}
	return mode
}

// planSubject renders the plan of an exit_plan_mode input for an Approver,
// cut to maxSubjectBytes. The plan is model-written text: it is shown as
// data and never interpreted.
func planSubject(raw json.RawMessage) string {
	var in ExitInput
	if json.Unmarshal(raw, &in) != nil || len(in.Plan) <= maxSubjectBytes {
		return in.Plan
	}
	return strings.ToValidUTF8(in.Plan[:maxSubjectBytes], "") + TruncationMarker
}

func enter(ctx context.Context, _ EnterInput, c *tool.Call) (tool.Result, error) {
	if info, _ := agent.Info(ctx); info.Depth > 0 {
		return tool.Text("Plan mode is for the top-level agent; a subagent cannot enter it. Continue with your task."), nil
	}
	if Active(ctx, c.State) {
		return tool.Text("Already in plan mode."), nil
	}
	previousKey.Set(c.State, modeOf(ctx, c.State))
	if err := agent.SetPermissionMode(c.State, permission.ModePlan); err != nil {
		return tool.Result{}, err
	}
	return tool.Text("Plan mode is on. Explore with read-only tools, then call exit_plan_mode with your plan for the user to approve."), nil
}

func (m *Middleware) exit(ctx context.Context, in ExitInput, c *tool.Call) (tool.Result, error) {
	info, _ := agent.Info(ctx)
	switch {
	case info.Depth > 0:
		return tool.Errorf("exit_plan_mode is not available to a subagent: only the top-level agent can leave plan mode"), nil
	case !Active(ctx, c.State):
		return tool.Errorf("not in plan mode"), nil
	}
	if res := checkPlan(in.Plan); res != nil {
		return *res, nil
	}
	if r := c.Resume; r != nil {
		switch r.Type {
		case tool.ResumeApprove, tool.ResumeEdit: // the loop already applied an edit to in
			return m.approve(c, in.Plan)
		default:
			return refuse(r.Message), nil
		}
	}
	if approver := approverOf(info); approver != nil {
		return m.ask(ctx, info.Agent, approver, in, c)
	}
	return tool.Result{}, &agent.Interrupt{
		Requests: []agent.ActionRequest{{
			Reason:  interruptReason,
			Allowed: []agent.DecisionType{agent.Approve, agent.Edit, agent.Reject, agent.Respond},
		}},
		ReinvokeOnReject: true,
	}
}

// checkPlan returns the result that sends the model back to fix plan, or nil
// when plan is acceptable. An empty plan is an error; an oversize one is
// ordinary feedback.
func checkPlan(plan string) *tool.Result {
	var res tool.Result
	switch {
	case strings.TrimSpace(plan) == "":
		res = tool.Errorf("plan is empty: write the implementation plan and call exit_plan_mode again")
	case len(plan) > maxPlanBytes:
		res = tool.Text(fmt.Sprintf("The plan is too long (%d bytes, limit %d). Shorten it and call exit_plan_mode again.", len(plan), maxPlanBytes))
	default:
		return nil
	}
	return &res
}

// ask puts the plan to the run's Approver. The request forbids remembering
// an answer, so an "always allow" can never turn into silent approvals, and
// an UpdatedInput in the answer replaces the plan, like an Edit decision.
func (m *Middleware) ask(ctx context.Context, a *agent.Agent, approver permission.Approver, in ExitInput, c *tool.Call) (tool.Result, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return tool.Result{}, err
	}
	req := a.PermissionRequest(ctx, &agent.ToolCall{ID: c.ToolUseID, Name: ToolExit, Input: input, State: c.State})
	req.DisableRemember = true
	d := approver(ctx, req, approverReason)
	if d.Behavior != permission.Allow {
		return refuse(d.Message), nil
	}
	plan := in.Plan
	if len(d.UpdatedInput) > 0 {
		var edited ExitInput
		if err := json.Unmarshal(d.UpdatedInput, &edited); err != nil {
			return tool.Errorf("the approver's edited plan is invalid: %v", err), nil
		}
		if res := checkPlan(edited.Plan); res != nil {
			return *res, nil
		}
		plan = edited.Plan
	}
	return m.approve(c, plan)
}

func approverOf(info agent.RunInfo) permission.Approver {
	if info.Agent == nil {
		return nil
	}
	return info.Agent.Config().Approver
}

// approve leaves plan mode for the mode the config and the entry record
// select, and remembers plan as the approved one.
func (m *Middleware) approve(c *tool.Call, plan string) (tool.Result, error) {
	mode := m.cfg.ApproveMode
	if mode == "" {
		mode, _ = previousKey.Get(c.State)
		if mode == "" || mode == permission.ModePlan {
			mode = permission.ModeDefault
		}
	}
	if err := agent.SetPermissionMode(c.State, mode); err != nil {
		return tool.Errorf("cannot leave plan mode: %v", err), nil
	}
	planKey.Set(c.State, plan)
	previousKey.Delete(c.State)
	return tool.Text(fmt.Sprintf("The user approved the plan. Permission mode is now %s. Implement it.\n\nApproved plan:\n%s", mode, plan)), nil
}

// refuse keeps plan mode and passes the human's feedback to the model. It is
// not an error: revising the plan is the expected next step.
func refuse(msg string) tool.Result {
	if msg == "" {
		msg = "(none)"
	}
	return tool.Text(fmt.Sprintf("The user did not approve the plan. Feedback: %s. Stay in plan mode, revise the plan and call exit_plan_mode again.", msg))
}

// WrapModel implements agent.ModelWrapper. While plan mode is active it
// appends the prompt after the host's system segments, outside the cached
// prefix.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" && Active(ctx, req.State) {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	return next(ctx, req)
}
