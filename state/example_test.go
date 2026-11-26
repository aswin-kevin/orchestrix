package state_test

import (
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/state"
)

// counterKey is namespaced with the package name so keys of different
// libraries cannot collide.
var counterKey = state.NewKey[int]("example.counter")

// ExampleNewKey declares a typed key and shows its value surviving a JSON
// round trip of the State.
func ExampleNewKey() {
	s := state.New()
	counterKey.Set(s, 42)
	v, _ := counterKey.Get(s)
	fmt.Println(v)

	data, err := json.Marshal(s)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		fmt.Println("error:", err)
		return
	}
	v, ok := counterKey.Get(restored)
	fmt.Println(v, ok)
	// Output:
	// 42
	// 42 true
}
