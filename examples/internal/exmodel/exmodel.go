// Package exmodel lets the example programs run offline by default and
// against a real model on request.
//
// Without ORX_MODEL every example replays a short script, so it runs
// anywhere with no network and no API key. With ORX_MODEL set to a model
// spec such as anthropic:claude-sonnet-4-5 or openai:gpt-4o, the same code
// talks to that model, reading its API key from ANTHROPIC_API_KEY or
// OPENAI_API_KEY as the orx CLI does.
package exmodel

import (
	"os"

	"github.com/aswin-kevin/orchestrix/internal/termsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// ModelEnv names the environment variable holding the model spec, the same
// one orx reads.
const ModelEnv = "ORX_MODEL"

// Live reports whether ORX_MODEL is set.
func Live() bool { return os.Getenv(ModelEnv) != "" }

// Model returns the ORX_MODEL spec string when live, else a scripted provider
// replaying turns. Either one can be passed to orchestrix.New.
func Model(turns ...llmtest.Turn) any {
	if Live() {
		return os.Getenv(ModelEnv)
	}
	return llmtest.New(turns...)
}

// Sub returns nil when live (a subagent then inherits the parent's model),
// else a scripted provider for a subagent's own turns.
func Sub(turns ...llmtest.Turn) llm.Provider {
	if Live() {
		return nil
	}
	return llmtest.New(turns...)
}

// Safe makes model text safe to print: control characters other than \n and
// \t, invisible format characters and invalid UTF-8 are shown escaped, so the
// text cannot inject terminal escape sequences.
func Safe(s string) string { return termsafe.String(s) }
