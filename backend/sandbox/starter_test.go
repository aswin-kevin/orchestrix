package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
	"github.com/aswin-kevin/orchestrix/backend/shell"
)

// fakeStarter records the commands and options it is given and starts
// nothing.
type fakeStarter struct {
	cmds []string
	opts []backend.ExecOptions
}

func (f *fakeStarter) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{}, errors.New("fakeStarter: Execute not expected")
}

func (f *fakeStarter) Start(_ context.Context, cmd string, opts backend.ExecOptions) (backend.Process, error) {
	f.cmds = append(f.cmds, cmd)
	f.opts = append(f.opts, opts)
	return nil, nil
}

func TestNewDoesNotClaimStarter(t *testing.T) {
	b, err := sandbox.New(&fakeStarter{}, sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(b).(backend.Starter); ok {
		t.Fatal("sandbox.New's Backend claims backend.Starter")
	}
}

func TestNewStarterValidates(t *testing.T) {
	if _, err := sandbox.NewStarter(nil, sandbox.Options{}); err == nil {
		t.Error("nil executor accepted")
	}
	if _, err := sandbox.NewStarter(&fakeStarter{}, sandbox.Options{Root: "rel"}); err == nil {
		t.Error("relative root accepted")
	}
}

func TestStartPrefixesQuotedCd(t *testing.T) {
	f := &fakeStarter{}
	b, err := sandbox.NewStarter(f, sandbox.Options{Root: "/work space"})
	if err != nil {
		t.Fatal(err)
	}
	in := backend.ExecOptions{Dir: "/sub/../it's", Timeout: time.Minute, Env: []string{"A=1"}, MaxOutputBytes: 7}
	if _, err := b.Start(ctx, "make", in); err != nil {
		t.Fatal(err)
	}
	want := "cd '/work space/it'\\''s' || exit\nmake"
	if len(f.cmds) != 1 || f.cmds[0] != want {
		t.Fatalf("command = %q, want %q", f.cmds, want)
	}
	got := f.opts[0]
	if got.Dir != "" || got.Timeout != time.Minute || len(got.Env) != 1 || got.Env[0] != "A=1" || got.MaxOutputBytes != 7 {
		t.Fatalf("options = %+v", got)
	}
	if _, err := b.Start(ctx, "make", backend.ExecOptions{Dir: "a\x00b"}); !errors.Is(err, backend.ErrInvalidPath) || len(f.cmds) != 1 {
		t.Fatalf("NUL in Dir: err = %v, commands %d", err, len(f.cmds))
	}
}

func TestStartOverShell(t *testing.T) {
	skipWithoutSh(t)
	dir := t.TempDir()
	sh, err := shell.New(dir, shell.Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := sandbox.NewStarter(sh, sandbox.Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Write(ctx, "/sub/f.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	p, err := b.Start(ctx, "cat f.txt", backend.ExecOptions{Dir: "/sub"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("command did not finish")
	}
	res, ok := p.Result()
	if !ok || res.ExitCode != 0 || strings.TrimSpace(res.Output) != "hello" {
		t.Fatalf("result = %+v, %v", res, ok)
	}

	// A missing directory stops the command before it runs.
	p, err = b.Start(ctx, "echo ran", backend.ExecOptions{Dir: "/missing"})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if res, _ := p.Result(); res.ExitCode == 0 || strings.Contains(res.Output, "ran") {
		t.Fatalf("command ran outside its directory: %+v", res)
	}
}
