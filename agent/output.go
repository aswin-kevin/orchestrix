package agent

import (
	"encoding/json"
	"errors"
	"fmt"
)

// OutputRetries is how many times a run sends an answer that fails
// Config.OutputSchema back to the model for correction before it gives up.
const OutputRetries = 2

// OutputFormatPrompt introduces the JSON schema the final answer of a run
// with a Config.OutputSchema must satisfy; the compact schema follows it in
// the same system segment.
const OutputFormatPrompt = "When you finish, reply with only a JSON object, with no other text, that is valid against this JSON schema:\n"

// InvalidOutputPrompt asks the model to correct an answer that failed
// Config.OutputSchema; %v is the validation error.
const InvalidOutputPrompt = "Your answer is not valid: %v. Reply with only the JSON object."

// ErrInvalidOutput reports a final answer that still did not match
// Config.OutputSchema once OutputRetries corrections were spent.
var ErrInvalidOutput = errors.New("agent: the final answer does not match Config.OutputSchema")

// outputPrompt returns the system segment that asks for an answer valid
// against schema: OutputFormatPrompt followed by the compact schema. It
// returns "" for a nil schema and fails when the schema's top-level "type"
// is not "object" or the schema cannot be encoded as JSON.
func outputPrompt(schema map[string]any) (string, error) {
	if schema == nil {
		return "", nil
	}
	if schema["type"] != "object" {
		return "", errors.New(`agent: Config.OutputSchema must have "type": "object"`)
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("agent: encode Config.OutputSchema: %w", err)
	}
	return OutputFormatPrompt + string(b), nil
}
