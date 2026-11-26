package tool_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/tool"
)

// greetInput is the input struct of the greet tool.
type greetInput struct {
	Name string `json:"name" desc:"Who to greet."`
}

// ExampleNew builds a typed tool, prints its schema and calls it once.
func ExampleNew() {
	greet := tool.New("greet", "Greets someone by name.",
		func(ctx context.Context, in greetInput, c *tool.Call) (tool.Result, error) {
			return tool.Text("Hello, " + in.Name + "!"), nil
		})

	schema, err := json.Marshal(greet.Schema())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(greet.Name())
	fmt.Println(string(schema))

	res, err := greet.Call(context.Background(), json.RawMessage(`{"name":"Ada"}`), nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(res.Flatten())
	// Output:
	// greet
	// {"additionalProperties":false,"properties":{"name":{"description":"Who to greet.","type":"string"}},"required":["name"],"type":"object"}
	// Hello, Ada!
}
