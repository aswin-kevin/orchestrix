// Command orxeval runs the eval scenarios against a live model and compares
// the pass rates with a stored baseline.
//
//	ORX_TEST_MODEL=openai:<model> ORX_TEST_BASE_URL=<url> \
//	  ORX_TEST_API_KEY=<key> go run ./cmd/orxeval -n 5 -out results.json \
//	  -baseline baselines/openai-compatible.json
//
// The model, endpoint and key come from the same variables as the root
// module's live tests (the -model and -base-url flags override the first
// two); the key is read from the environment only. The exit status is 1 when
// a scenario's pass rate falls below its baseline minus -tolerance, when any
// trial broke a harness invariant, or when the run was cut short; 2 on a
// usage error.
package main
