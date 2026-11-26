package orxdocker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

func execOpts(timeout time.Duration) backend.ExecOptions {
	return backend.ExecOptions{Timeout: timeout}
}

func frame(stream byte, payload string) []byte {
	hdr := make([]byte, 8, 8+len(payload))
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr, payload...)
}

func TestDemux(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(streamStdout, "out1 "))
	in.Write(frame(streamStderr, "err "))
	in.Write(frame(streamStdout, ""))
	in.Write(frame(streamStdout, "out2"))
	var out bytes.Buffer
	if err := demux(&in, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out1 err out2" {
		t.Fatalf("demux = %q", out.String())
	}
}

func TestDemuxRejectsBrokenStreams(t *testing.T) {
	short := frame(streamStdout, "abcdef")[:10]
	if err := demux(bytes.NewReader(short), &bytes.Buffer{}); err == nil {
		t.Error("truncated frame accepted")
	}
	if err := demux(strings.NewReader("plain text, not frames"), &bytes.Buffer{}); err == nil {
		t.Error("raw stream accepted")
	}
}

func TestDemuxCapturesBounded(t *testing.T) {
	var in bytes.Buffer
	for range 100 {
		in.Write(frame(streamStdout, strings.Repeat("x", 1000)))
	}
	c := backend.NewOutputCapture(1000)
	if err := demux(&in, c); err != nil {
		t.Fatal(err)
	}
	if !c.Truncated() || len(c.String()) > 1100 {
		t.Fatalf("capture kept %d bytes, truncated %v", len(c.String()), c.Truncated())
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"alpine":                       {"alpine", "latest"},
		"alpine:3.22":                  {"alpine", "3.22"},
		"localhost:5000/img":           {"localhost:5000/img", "latest"},
		"localhost:5000/img:v1":        {"localhost:5000/img", "v1"},
		"alpine@sha256:abc":            {"alpine", "sha256:abc"},
		"ghcr.io/org/tools:2026.1-rc1": {"ghcr.io/org/tools", "2026.1-rc1"},
	} {
		name, tag := splitRef(ref)
		if name != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q, %q, want %q", ref, name, tag, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	got, err := mergeEnv(map[string]string{"A": "1", "SHARED": "cfg"}, map[string]string{"SHARED": "call"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "SHARED=call"}; !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
	got, err = mergeEnv(map[string]string{"C": "x=y", "B": ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"B=", "C=x=y"}; !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
	for _, bad := range []map[string]string{{"": "x"}, {"A=B": "x"}, {"A\x00B": "x"}} {
		if _, err := mergeEnv(bad); err == nil || !strings.Contains(err.Error(), "env key") {
			t.Errorf("mergeEnv(%q) = %v, want an env key error", bad, err)
		}
	}
}

func TestDefaultsAreHardened(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	o, err := Options{}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg := o.containerSpec()
	h := cfg.HostConfig
	switch {
	case cfg.User != DefaultUser:
		t.Errorf("user = %q", cfg.User)
	case !h.ReadonlyRootfs:
		t.Error("root file system is writable")
	case !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) != 0:
		t.Errorf("caps: drop %v add %v", h.CapDrop, h.CapAdd)
	case !slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}):
		t.Errorf("security options = %v", h.SecurityOpt)
	case h.PidsLimit != DefaultPidsLimit || h.Memory != DefaultMemoryBytes || h.MemorySwap != h.Memory || h.NanoCPUs != DefaultNanoCPUs:
		t.Errorf("limits: pids %d memory %d swap %d cpu %d", h.PidsLimit, h.Memory, h.MemorySwap, h.NanoCPUs)
	case h.NetworkMode != "none":
		t.Errorf("network = %q", h.NetworkMode)
	case len(cfg.Env) != 0:
		t.Errorf("container env = %q", cfg.Env)
	case !h.Init:
		t.Error("no init")
	case len(h.Tmpfs) != 2 || !strings.Contains(h.Tmpfs["/workspace"], "uid=65534") || !strings.Contains(h.Tmpfs["/tmp"], "mode=1777"):
		t.Errorf("tmpfs = %v", h.Tmpfs)
	case cfg.Labels[LabelManaged] != "true" || cfg.Labels[LabelScope] != DefaultScope || cfg.Labels[LabelConfig] == "":
		t.Errorf("labels = %v", cfg.Labels)
	}
	if o.Socket != DefaultSocket || o.Image != DefaultImage || o.IdleTTL != DefaultIdleTTL {
		t.Errorf("defaults = %+v", o)
	}
}

func TestOptOuts(t *testing.T) {
	o, err := Options{
		Network: "scoped", PidsLimit: -1, MemoryBytes: -1, NanoCPUs: -1, WritableRootfs: true,
		AllowNewPrivileges: true, CapAdd: []string{"NET_RAW"}, User: "tool", Socket: "/s",
	}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	h := o.containerSpec().HostConfig
	if h.NetworkMode != "scoped" || h.PidsLimit != -1 || h.Memory != 0 || h.NanoCPUs != 0 ||
		h.ReadonlyRootfs || len(h.SecurityOpt) != 0 || !slices.Equal(h.CapAdd, []string{"NET_RAW"}) {
		t.Fatalf("host config = %+v", h)
	}
	if strings.Contains(h.Tmpfs["/workspace"], "uid=") {
		t.Errorf("non-numeric user got a workspace uid: %q", h.Tmpfs["/workspace"])
	}
	base, _ := Options{Socket: "/s"}.withDefaults()
	if base.containerSpec().Labels[LabelConfig] == o.containerSpec().Labels[LabelConfig] {
		t.Error("configuration hash ignores the configuration")
	}
}

func TestOptionsValidation(t *testing.T) {
	for name, o := range map[string]Options{
		"relative workdir": {WorkDir: "work", Socket: "/s"},
		"root workdir":     {WorkDir: "/", Socket: "/s"},
		"tmp workdir":      {WorkDir: "/tmp", Socket: "/s"},
		"unclean workdir":  {WorkDir: "/a/../b", Socket: "/s"},
		"comma workdir":    {WorkDir: "/a,b", Socket: "/s"},
		"bad scope":        {Scope: "a b", Socket: "/s"},
		"bad env":          {Env: map[string]string{"A=B": "x"}, Socket: "/s"},
		"negative op":      {OpTimeout: -time.Second, Socket: "/s"},
	} {
		if _, err := NewManager(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2375")
	if _, err := NewManager(Options{}); err == nil {
		t.Error("tcp DOCKER_HOST accepted")
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	o, err := Options{}.withDefaults()
	if err != nil || o.Socket != "/run/user/1000/docker.sock" {
		t.Errorf("unix DOCKER_HOST: socket %q, %v", o.Socket, err)
	}
}

func TestManagerWithoutDaemon(t *testing.T) {
	m, err := NewManager(Options{Socket: t.TempDir() + "/missing.sock", IdleTTL: -1})
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend("t1") != m.Backend("t1") {
		t.Error("Backend is not stable per thread")
	}
	if err := m.Ping(t.Context()); err == nil {
		t.Error("Ping succeeded without a daemon")
	}
	if _, err := m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Second)); err == nil {
		t.Error("Execute succeeded without a daemon")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Second)); err == nil {
		t.Error("Execute succeeded after Close")
	}
}

func TestExecutorRejectsEmptyThread(t *testing.T) {
	// The socket does not exist: any engine call would fail with a dial
	// error, so ErrNoThread proves the check comes first.
	m, err := NewManager(Options{Socket: t.TempDir() + "/missing.sock", IdleTTL: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.Executor("").Execute(t.Context(), "true", execOpts(time.Second))
	if !errors.Is(err, ErrNoThread) {
		t.Fatalf("Execute with an empty thread id = %v, want ErrNoThread", err)
	}
	if _, err := m.Executor("").ContainerID(t.Context()); !errors.Is(err, ErrNoThread) {
		t.Fatalf("ContainerID with an empty thread id = %v, want ErrNoThread", err)
	}
}
