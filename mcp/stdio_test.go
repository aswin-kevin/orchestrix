package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperEnv names the mode in which the test binary serves a test server
// over stdin/stdout instead of running tests. Stdio tests spawn only this
// binary.
const helperEnv = "ORX_MCP_HELPER"

// testCloseGrace keeps Close fast in tests whose server exits on stdin EOF.
const testCloseGrace = 300 * time.Millisecond

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runHelper(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelper serves the test server in the given mode:
//   - serve: plain server
//   - noisy: prints a non-JSON line to stdout first
//   - crash: tool "crash" writes "boom" to stderr and exits
//   - spawn: tool "spawn" starts a sleeping grandchild and returns its pid;
//     tool "quit" exits
//   - sleep: only sleeps (the grandchild of spawn)
//
// Modes that need platform-specific calls (signals, process groups) live in
// platformHelpers; they run before serving and keep the helper running after
// stdin EOF.
func runHelper(mode string) {
	if mode == "sleep" {
		time.Sleep(time.Hour)
		return
	}
	s := newTestServer(nil)
	s.AddTool(ToolInfo{Name: "pid"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return TextResult(strconv.Itoa(os.Getpid())), nil
	})
	s.AddTool(ToolInfo{Name: "env"}, func(_ context.Context, args json.RawMessage) (*CallToolResult, error) {
		var in struct{ Name string }
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		return TextResult(os.Getenv(in.Name)), nil
	})
	platform := platformHelpers[mode]
	switch mode {
	case "noisy":
		fmt.Println("server starting up, please wait")
	case "crash":
		s.AddTool(ToolInfo{Name: "crash"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
			fmt.Fprint(os.Stderr, "boom")
			os.Exit(3)
			return nil, nil
		})
	case "spawn":
		s.AddTool(ToolInfo{Name: "spawn"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), helperEnv+"=sleep")
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return TextResult(strconv.Itoa(cmd.Process.Pid)), nil
		})
		s.AddTool(ToolInfo{Name: "quit"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
			os.Exit(0)
			return nil, nil
		})
	default:
		if platform != nil {
			platform()
		}
	}
	_ = s.ServeStdio(context.Background(), os.Stdin, os.Stdout)
	if platform != nil {
		time.Sleep(time.Hour)
	}
}

// platformHelpers holds helper modes that need platform-specific calls,
// registered by the platform's test files.
var platformHelpers = map[string]func(){}

// startHelper starts the test binary as a stdio server in mode; cfg's
// Command, Args and the mode variable are filled in.
func startHelper(t *testing.T, mode string, cfg StdioConfig) Transport {
	t.Helper()
	cfg.Command = os.Args[0]
	cfg.Args = []string{"-test.run=^$"}
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env[helperEnv] = mode
	// A race-enabled binary otherwise sleeps a second at exit, past the grace.
	cfg.Env["GORACE"] = "atexit_sleep_ms=0"
	if cfg.CloseGrace == 0 {
		cfg.CloseGrace = testCloseGrace
	}
	tr, err := StartStdio(cfg)
	if err != nil {
		t.Fatalf("StartStdio: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close(context.Background()) })
	return tr
}

// clientFor connects a client over tr, closed when the test ends.
func clientFor(t *testing.T, tr Transport) *Client {
	t.Helper()
	c, err := NewClient(context.Background(), tr, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// connectHelper starts a helper server and connects a client to it.
func connectHelper(t *testing.T, mode string, cfg StdioConfig) *Client {
	t.Helper()
	return clientFor(t, startHelper(t, mode, cfg))
}

// callText calls tool and returns its text result.
func callText(t *testing.T, c *Client, tool, args string) string {
	t.Helper()
	res, err := c.CallTool(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	return resultText(t, res)
}

func TestStdioHelperServer(t *testing.T) {
	c := connectHelper(t, "serve", StdioConfig{})
	if c.ServerInfo().Name != "test-server" {
		t.Errorf("ServerInfo = %+v", c.ServerInfo())
	}
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) < 4 {
		t.Fatalf("ListTools = %d tools, %v", len(tools), err)
	}
	if got := callText(t, c, "echo", `{"text":"over stdio"}`); got != "over stdio" {
		t.Errorf("echo = %q", got)
	}
	contents, err := c.ReadResource(context.Background(), "file:///readme")
	if err != nil || len(contents) != 1 || contents[0].Text != "hello" {
		t.Errorf("ReadResource = %+v, %v", contents, err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestStdioSkipsNonJSONLines(t *testing.T) {
	c := connectHelper(t, "noisy", StdioConfig{})
	if got := callText(t, c, "echo", `{"text":"after noise"}`); got != "after noise" {
		t.Errorf("echo = %q", got)
	}
}

func TestStdioEnvironment(t *testing.T) {
	t.Setenv("ORX_MCP_TEST_SECRET", "s3cret")
	c := connectHelper(t, "serve", StdioConfig{Env: map[string]string{"ORX_MCP_TEST_EXTRA": "extra"}})
	if got := callText(t, c, "env", `{"name":"ORX_MCP_TEST_SECRET"}`); got != "" {
		t.Errorf("host variable outside the allowlist leaked: %q", got)
	}
	if got := callText(t, c, "env", `{"name":"ORX_MCP_TEST_EXTRA"}`); got != "extra" {
		t.Errorf("configured variable = %q", got)
	}
	if got := callText(t, c, "env", `{"name":"PATH"}`); got != os.Getenv("PATH") {
		t.Errorf("allowlisted PATH = %q", got)
	}

	inherit := connectHelper(t, "serve", StdioConfig{InheritEnv: true})
	if got := callText(t, inherit, "env", `{"name":"ORX_MCP_TEST_SECRET"}`); got != "s3cret" {
		t.Errorf("InheritEnv: host variable = %q", got)
	}
}

func TestServerCrashFailsPending(t *testing.T) {
	c := connectHelper(t, "crash", StdioConfig{})
	_, err := c.CallTool(context.Background(), "crash", nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the stderr tail", err)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed after the server crashed")
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Errorf("Err = %v, want ErrClosed", c.Err())
	}
}

func TestOversizedMessageEndsTransport(t *testing.T) {
	c := connectHelper(t, "serve", StdioConfig{MaxMessageBytes: 1024})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "big", nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the transport to end", err)
	}
	if !errors.Is(err, ErrClosed) || !strings.Contains(err.Error(), "1024") {
		t.Errorf("err = %v, want ErrClosed naming the 1024-byte limit", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("oversized message took %v to fail", elapsed)
	}
}

func TestStartStdioErrors(t *testing.T) {
	if _, err := StartStdio(StdioConfig{}); err == nil {
		t.Error("empty command accepted")
	}
	if _, err := StartStdio(StdioConfig{Command: t.TempDir() + "/missing"}); err == nil {
		t.Error("missing executable accepted")
	}
	for _, tt := range []struct {
		name string
		cfg  StdioConfig
		want string
	}{
		{"negative MaxMessageBytes", StdioConfig{MaxMessageBytes: -1}, "mcp: MaxMessageBytes is negative"},
		{"negative CloseGrace", StdioConfig{CloseGrace: -time.Second}, "mcp: CloseGrace is negative"},
		{"empty env key", StdioConfig{Env: map[string]string{"": "x"}}, `mcp: env key "" is invalid`},
		{"env key with =", StdioConfig{Env: map[string]string{"A=B": "x"}}, `mcp: env key "A=B" is invalid`},
		{"env key with NUL", StdioConfig{Env: map[string]string{"A\x00": "x"}}, `mcp: env key "A\x00" is invalid`},
	} {
		tt.cfg.Command = t.TempDir() + "/missing" // the check comes before the start
		if _, err := StartStdio(tt.cfg); err == nil || err.Error() != tt.want {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	inproc, err := NewClient(context.Background(), InProcess(newTestServer(nil)), ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tr := startHelper(t, "serve", StdioConfig{})
	stdio, err := NewClient(context.Background(), tr, ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Client{inproc, stdio} {
		if got := callText(t, c, "echo", `{"text":"x"}`); got != "x" {
			t.Fatalf("echo = %q", got)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
