package tool

import (
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/internal/names"
	"github.com/aswin-kevin/orchestrix/llm"
)

// Registry holds tools in registration order, which is also the order they are
// presented to the model. It is populated before the agent runs and is not safe
// for concurrent Add.
type Registry struct {
	tools  []Tool
	byName map[string]Tool
}

// NewRegistry returns a registry holding tools, or an error for the first
// invalid or duplicate one.
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool)}
	for _, t := range tools {
		if err := r.Add(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Add registers t. It fails for a nil tool, an illegal name or a name that is
// already registered.
func (r *Registry) Add(t Tool) error {
	if t == nil {
		return errors.New("tool registry: nil tool")
	}
	name := t.Name()
	if err := names.Validate(name); err != nil {
		return fmt.Errorf("tool registry: %w", err)
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("tool registry: duplicate tool %q", name)
	}
	r.byName[name] = t
	r.tools = append(r.tools, t)
	return nil
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Tools returns the registered tools in registration order.
func (r *Registry) Tools() []Tool {
	return append([]Tool(nil), r.tools...)
}

// Schemas returns the model-facing description of every tool, in order.
func (r *Registry) Schemas() []llm.ToolSchema {
	schemas := make([]llm.ToolSchema, len(r.tools))
	for i, t := range r.tools {
		schemas[i] = llm.ToolSchema{Name: t.Name(), Description: t.Description(), InputSchema: t.Schema()}
	}
	return schemas
}
