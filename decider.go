package orchestrix

import "github.com/aswin-kevin/orchestrix/llm"

// NewDecider returns a decision model for spec, which looks like
// "format:model" (for example "systemone:<model>"). The API key and base URL
// come from the format's environment variables; build an llm.Config and call
// llm.NewDecider for more control.
func NewDecider(spec string) (llm.Decider, error) {
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		return nil, err
	}
	return llm.NewDecider(cfg)
}
