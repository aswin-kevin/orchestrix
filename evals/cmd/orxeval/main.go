// Command orxeval runs the eval scenarios against a live model and compares
// the pass rates with a stored baseline.
//
//	ORX_TEST_MODEL=openai:<model> ORX_TEST_BASE_URL=<url> ORX_TEST_API_KEY=<key> \
//	  go run ./cmd/orxeval -n 5 -out results.json -baseline baselines/openai-compatible.json
//
// The model, endpoint and key come from the same variables as the root
// module's live tests (the -model and -base-url flags override the first
// two); the key is read from the environment only. The exit status is 1 when
// a scenario's pass rate falls below its baseline minus -tolerance, when any
// trial broke a harness invariant, or when the run was cut short; 2 on a
// usage error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"time"

	"github.com/aswin-kevin/orchestrix/evals"
	"github.com/aswin-kevin/orchestrix/llm"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		model         = flag.String("model", os.Getenv("ORX_TEST_MODEL"), "model spec, as for the live tests (default $ORX_TEST_MODEL)")
		baseURL       = flag.String("base-url", os.Getenv("ORX_TEST_BASE_URL"), "endpoint base URL (default $ORX_TEST_BASE_URL)")
		trials        = flag.Int("n", 5, "trials per scenario")
		filter        = flag.String("run", "", "run only scenarios whose name matches this regular expression")
		out           = flag.String("out", "eval-results.json", "results file to write")
		baseline      = flag.String("baseline", "", "baseline file to compare against; empty skips the comparison")
		writeBaseline = flag.String("write-baseline", "", "write the pass rates of this run to this baseline file")
		tolerance     = flag.Float64("tolerance", 0.2, "how far a pass rate may fall below its baseline")
		maxTokens     = flag.Int("max-tokens", 4096, "output token cap of each model response")
		maxTotal      = flag.Int("max-total-tokens", 3_000_000, "stop once the trials used this many tokens in all; 0 for no limit")
		timeout       = flag.Duration("timeout", 45*time.Minute, "bound on the whole run")
		list          = flag.Bool("list", false, "list the scenarios and exit")
	)
	flag.Parse()

	scenarios := evals.Scenarios()
	if *list {
		for _, sc := range scenarios {
			fmt.Printf("%-24s %s\n", sc.Name, sc.Description)
		}
		return 0
	}
	if *model == "" {
		fmt.Fprintln(os.Stderr, "orxeval: no model: set ORX_TEST_MODEL or -model")
		return 2
	}
	format, err := configureModel(*model, *baseURL, os.Getenv("ORX_TEST_API_KEY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "orxeval:", evals.Scrub(err.Error()))
		return 2
	}
	var re *regexp.Regexp
	if *filter != "" {
		if re, err = regexp.Compile(*filter); err != nil {
			fmt.Fprintln(os.Stderr, "orxeval: -run:", err)
			return 2
		}
	}
	var base evals.Baseline
	if *baseline != "" {
		if base, err = evals.ReadBaseline(*baseline); err != nil {
			fmt.Fprintln(os.Stderr, "orxeval:", evals.Scrub(err.Error()))
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	res := evals.RunSuite(ctx, evals.Env{Model: *model, MaxTokens: *maxTokens}, scenarios, evals.Options{
		Trials:         *trials,
		Filter:         re,
		MaxTotalTokens: *maxTotal,
		Progress: func(name string, i int, tr evals.Trial) {
			verdict := "pass"
			switch {
			case tr.Violation != "":
				verdict = "VIOLATION: " + tr.Violation
			case tr.Failure != "":
				verdict = "fail: " + tr.Failure
			case tr.Error != "":
				verdict = "error: " + tr.Error
			}
			fmt.Fprintf(os.Stderr, "%s #%d: %s (%s, %d turns, %d tokens, %.1fs)\n",
				name, i+1, verdict, tr.Reason, tr.Turns, tr.InputTokens+tr.OutputTokens, float64(tr.WallMS)/1000)
		},
	})
	res.Format = format
	if err := evals.WriteJSON(*out, res); err != nil {
		fmt.Fprintln(os.Stderr, "orxeval:", evals.Scrub(err.Error()))
		return 1
	}
	fmt.Print(evals.Summary(res))
	fmt.Println("results written to", *out)
	if *writeBaseline != "" {
		if err := evals.WriteJSON(*writeBaseline, evals.BaselineOf(res)); err != nil {
			fmt.Fprintln(os.Stderr, "orxeval:", evals.Scrub(err.Error()))
			return 1
		}
		fmt.Println("baseline written to", *writeBaseline)
	}

	problems := evals.Compare(res, base, *tolerance, re != nil)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "FAIL", p)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

// configureModel checks spec and points its format's environment variables
// at baseURL and key, as the live tests do, so the agents built from the
// spec string pick them up. It returns the spec's format.
func configureModel(spec, baseURL, key string) (string, error) {
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		return "", err
	}
	prefix := "OPENAI"
	if cfg.Format == llm.FormatAnthropic {
		prefix = "ANTHROPIC"
	}
	if baseURL != "" {
		if err := os.Setenv(prefix+"_BASE_URL", baseURL); err != nil {
			return "", err
		}
	}
	if key != "" {
		if err := os.Setenv(prefix+"_API_KEY", key); err != nil {
			return "", err
		}
	}
	// Parse again so the check sees the variables just set.
	if cfg, err = llm.ParseModel(spec); err != nil {
		return "", err
	}
	if _, err := llm.New(cfg); err != nil {
		return "", err
	}
	return string(cfg.Format), nil
}
