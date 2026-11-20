package agent

import (
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// The tool set: the static tools (Config.Tools and every ToolProvider,
// checked once by New) plus the tools the DynamicToolProviders offer right
// now. A run takes one snapshot per model turn, so the schemas the model is
// shown and the tools its calls resolve to are the same set. A call of a
// tool.Dispatcher is resolved to its target here, before admission, so the
// permission pipeline, ToolWrappers, events and interrupts see the target.

// Tools returns the tools the agent's next turn offers: the static ones in
// registration order, then the dynamic ones in stack order. It calls every
// DynamicToolProvider and builds a new set each time; inside a run,
// RunInfo.Tools returns the turn's snapshot instead. During settlement a run
// still withholds the disabled ones from its model.
func (a *Agent) Tools() []tool.Tool {
	return a.toolSet(nil).Tools()
}

// Tool returns the tool named name among those the next turn offers (see
// Tools): a static tool first, else a dynamic one.
func (a *Agent) Tool(name string) (tool.Tool, bool) {
	if t, ok := a.registry.Get(name); ok {
		return t, true
	}
	return a.toolSet(nil).Get(name)
}

// skippedTool logs a dynamic tool the run's snapshot left out, once per name.
func (r *run) skippedTool(t tool.Tool, err error) { r.skips.skipped(r.logger, t, err) }

// toolSet returns a snapshot of the tool set. skipped, when set, is told of
// each dynamic tool the set refused; Agent.Tools and Agent.Tool, which run
// outside any run, pass nil and log nothing. Without dynamic providers it
// is the registry itself, which never changes after New.
func (a *Agent) toolSet(skipped func(tool.Tool, error)) *tool.Registry {
	if len(a.dynamic) == 0 {
		return a.registry
	}
	// The static tools were validated by New, so this cannot fail.
	set, _ := tool.NewRegistry(a.registry.Tools()...)
	for _, p := range a.dynamic {
		for _, t := range p.DynamicTools() {
			// Add refuses a nil tool, an invalid name and a name already
			// taken: such a tool is skipped, as DynamicToolProvider says.
			if err := set.Add(t); err != nil && skipped != nil {
				skipped(t, err)
			}
		}
	}
	return set
}

// dynamicProviders returns the DynamicToolProviders of stack, in stack order.
func dynamicProviders(stack []Middleware) []DynamicToolProvider {
	var providers []DynamicToolProvider
	for _, mw := range stack {
		if p, ok := mw.(DynamicToolProvider); ok {
			providers = append(providers, p)
		}
	}
	return providers
}

// refreshTools takes the tool snapshot of the next turn. A panicking
// DynamicToolProvider fails the run like a failing hook.
func (r *run) refreshTools() error {
	err := guard(func() error {
		r.tools.Store(r.agent.toolSet(r.skippedTool))
		return nil
	})
	if err != nil {
		return fmt.Errorf("agent: dynamic tools: %w", err)
	}
	return nil
}

// turnTools returns the tool set the current turn's calls resolve against.
// A resumed run finishes its interrupted turn before its first model call,
// so that turn takes its snapshot here.
func (r *run) turnTools() *tool.Registry {
	if set := r.tools.Load(); set != nil {
		return set
	}
	r.tools.CompareAndSwap(nil, r.agent.toolSet(r.skippedTool))
	return r.tools.Load()
}

// Tools returns the tools of the run's current turn: the snapshot its
// request's schemas were built from and its calls resolve against, taken
// once per turn (see Agent.Tools). Unlike Agent.Tools it calls no
// DynamicToolProvider, so middleware should prefer it inside a run. Before
// the first snapshot it is Agent.Tools; for the zero RunInfo it is nil.
func (i RunInfo) Tools() []tool.Tool {
	if set := i.toolSet(); set != nil {
		return set.Tools()
	}
	return nil
}

// Tool returns the tool named name among RunInfo.Tools.
func (i RunInfo) Tool(name string) (tool.Tool, bool) {
	if set := i.toolSet(); set != nil {
		return set.Get(name)
	}
	return nil, false
}

// toolSet returns the run's current snapshot, else the agent's tool set.
func (i RunInfo) toolSet() *tool.Registry {
	if i.run != nil {
		if set := i.run.tools.Load(); set != nil {
			return set
		}
	}
	if i.Agent != nil {
		if i.run != nil {
			return i.Agent.toolSet(i.run.skippedTool)
		}
		return i.Agent.toolSet(nil)
	}
	return nil
}

// prepare resolves the tool named by use against the turn's tool set and
// checks its input. A call of a tool.Dispatcher becomes a call of the tool
// it names (see dispatch).
//
// via is empty for a call the model made. It is set when an earlier run of
// this turn dispatched the call (see restoreCalls): use then names the
// target, which is prepared again directly, keeping via.
func (r *run) prepare(use llm.Block, via string) (c *toolCall) {
	c = &toolCall{id: use.ID, name: use.Name, input: use.Input, via: via}
	defer func() {
		if v := recover(); v != nil {
			c.blocked = panicResult(c.name, v)
		}
	}()

	t, blocked := r.resolve(use.Name)
	if blocked != nil {
		c.blocked = blocked
		return c
	}
	c.tool = t
	input, err := parseToolInput(use.Input)
	if err != nil {
		c.blocked = errorResult("invalid JSON arguments: %v. Re-issue the call with valid JSON.", err)
		return c
	}
	c.input = input
	if c.blocked = validateInput(t, input); c.blocked != nil {
		return c
	}
	if d, ok := t.(tool.Dispatcher); ok {
		if via != "" {
			c.blocked = notDispatchable(use.Name, via)
			return c
		}
		if c.blocked = r.dispatch(c, d); c.blocked != nil {
			return c
		}
	}
	c.concurrent = c.tool.ConcurrencySafe(c.input)
	return c
}

// resolve returns the tool of the turn named name, or the error result of a
// call that names a tool it may not call: one disabled during settlement,
// an unknown one, or one the run's State makes unavailable
// (tool.Availability). Both direct calls and dispatch targets resolve here,
// so an unavailable tool is refused before any approval is asked for.
func (r *run) resolve(name string) (tool.Tool, *tool.Result) {
	// The model may still call a tool hidden from it during settlement.
	if r.disabled(name) {
		return nil, errorResult("tool %s is not available while the run is wrapping up", name)
	}
	t, ok := r.turnTools().Get(name)
	if !ok {
		return nil, errorResult("unknown tool %q", name)
	}
	if a, ok := t.(tool.Availability); ok {
		if err := a.Available(r.state); err != nil {
			return nil, errorResult("%v", err)
		}
	}
	return t, nil
}

// dispatch turns c, a call of the dispatcher d with valid input, into a call
// of the tool d names, with the input d returns, so that everything after
// preparation sees the target. The target must be a tool of the turn that
// may be called now, must not itself be a dispatcher, and its input must be
// valid JSON that passes its schema, exactly as for a direct call. It
// returns the error result of a dispatch that fails.
func (r *run) dispatch(c *toolCall, d tool.Dispatcher) *tool.Result {
	name, input, err := d.Dispatch(c.input)
	if err != nil {
		return errorResult("%v", err)
	}
	target, blocked := r.resolve(name)
	if blocked != nil {
		return blocked
	}
	if _, ok := target.(tool.Dispatcher); ok {
		return notDispatchable(name, c.name)
	}
	if !json.Valid(input) {
		return errorResult("invalid input for %s: not valid JSON", name)
	}
	if res := validateInput(target, input); res != nil {
		return res
	}
	c.via, c.dispatcher, c.dispatchInput = c.name, c.tool, c.input
	c.name, c.tool, c.input = name, target, input
	return nil
}

// notDispatchable answers a dispatch whose target is itself a dispatcher.
func notDispatchable(target, via string) *tool.Result {
	return errorResult("tool %s cannot be called through %s", target, via)
}
