package orxdocker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// Unlimited, given to Config.PidsLimit, MemoryBytes, NanoCPUs or IdleTTL,
// removes that limit (an opt-out). It is the only negative value Config
// accepts.
const Unlimited = -1

// Defaults for Config.
const (
	// DefaultImage is the image containers run when Config.Image is empty.
	// Its busybox userland has everything orxdocker and backend/sandbox
	// need (see Config.Image).
	DefaultImage = "alpine:3.22"
	// DefaultSocket is the Docker daemon's Unix socket.
	DefaultSocket = "/var/run/docker.sock"
	// DefaultAPIVersion is the Docker Engine API version requested; Docker
	// 25 and later serve it.
	DefaultAPIVersion = "1.44"
	// DefaultUser runs commands as nobody.
	DefaultUser = "65534:65534"
	// DefaultWorkDir is the writable workspace directory, a tmpfs.
	DefaultWorkDir = "/workspace"
	// DefaultWorkspaceBytes sizes the workspace tmpfs.
	DefaultWorkspaceBytes = 512 << 20
	// DefaultTmpBytes sizes the /tmp tmpfs.
	DefaultTmpBytes = 64 << 20
	// DefaultPidsLimit caps the processes of a container.
	DefaultPidsLimit = 256
	// DefaultMemoryBytes caps a container's memory, tmpfs content
	// included; swap is disabled.
	DefaultMemoryBytes = 1 << 30
	// DefaultNanoCPUs caps a container at one CPU.
	DefaultNanoCPUs = 1_000_000_000
	// DefaultTimeout bounds a command whose ExecOptions.Timeout is 0.
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxOutputBytes caps captured output; the first half and the
	// last half are kept.
	DefaultMaxOutputBytes = 256 << 10
	// DefaultIdleTTL removes a thread's container after this long without
	// commands.
	DefaultIdleTTL = 30 * time.Minute
	// DefaultMaxThreads caps the executors a Manager keeps.
	DefaultMaxThreads = 1024
	// DefaultScope is the value of LabelScope when Config.Scope is empty.
	DefaultScope = "default"
)

// Config configures a Manager. The zero value is ready to use and gives
// every container the hardened defaults: a non-root user, a read-only root
// file system with only the workspace and /tmp writable (both tmpfs), all
// capabilities dropped, no-new-privileges, Docker's default seccomp profile,
// process, memory and CPU limits, no network, and no host environment. The
// fields that weaken them are opt-outs and say so.
//
// A zero numeric field means its default. A negative one makes NewManager
// fail, except Unlimited in the fields whose doc names it.
type Config struct {
	// Image is the container image; "" means DefaultImage. It needs a POSIX
	// /bin/sh with setsid, sleep, kill, cat, rm and mkdir, plus what
	// backend/sandbox lists (wc, tail -c, base64 -d, find -H, date -r).
	// Busybox (alpine, busybox:musl) and Debian or Ubuntu images qualify.
	// It is pulled on first use when the daemon lacks it.
	Image string
	// Socket is the Docker daemon's Unix socket. "" uses DOCKER_HOST when it
	// is a unix:// URL, else DefaultSocket. Set at most one of Socket and
	// Dial.
	Socket string
	// Dial, when set, opens every connection to the daemon instead of the
	// Unix socket, for instance to reach it over TCP with TLS or through an
	// SSH tunnel; the requests then go to the placeholder host "docker".
	// DOCKER_HOST is not read. nil uses Socket.
	Dial func(ctx context.Context) (net.Conn, error)
	// APIVersion is the Engine API version, such as "1.44"; "" means
	// DefaultAPIVersion.
	APIVersion string
	// Scope is stored in LabelScope. Reap only touches containers of its
	// Manager's scope, so processes sharing a daemon should use distinct
	// scopes. "" means DefaultScope.
	Scope string
	// User is the "uid:gid" (or user name) commands run as; "" means
	// DefaultUser. A numeric uid also owns the workspace.
	User string
	// WorkDir is the workspace: an absolute path, mounted as a writable
	// tmpfs and used as the sandbox root and default working directory. ""
	// means DefaultWorkDir.
	WorkDir string
	// WorkspaceBytes and TmpBytes size the workspace and /tmp tmpfs mounts;
	// 0 means DefaultWorkspaceBytes and DefaultTmpBytes. Their content
	// counts against MemoryBytes.
	WorkspaceBytes, TmpBytes int64
	// PidsLimit caps the container's processes; 0 means DefaultPidsLimit
	// and Unlimited removes the limit (opt-out).
	PidsLimit int64
	// MemoryBytes caps the container's memory; 0 means DefaultMemoryBytes
	// and Unlimited removes the limit (opt-out).
	MemoryBytes int64
	// NanoCPUs caps CPU in billionths of a CPU; 0 means DefaultNanoCPUs and
	// Unlimited removes the limit (opt-out).
	NanoCPUs int64
	// Network is the name of a Docker network to join instead of having no
	// network at all (opt-out). orxdocker cannot filter traffic from inside
	// the container: blocking the cloud metadata address and anything else
	// out of scope is the network's job, for instance an internal network
	// whose only way out is a filtering proxy.
	Network string
	// Env is added to every command's environment; ExecOptions.Env wins
	// over it. A key must be non-empty and hold no "=" or NUL. The host
	// environment is never passed.
	Env map[string]string
	// Timeout applies to commands whose ExecOptions.Timeout is 0; 0 means
	// DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes applies to commands whose ExecOptions.MaxOutputBytes is
	// 0; 0 means DefaultMaxOutputBytes.
	MaxOutputBytes int
	// IdleTTL removes a thread's container once it has run no command for
	// this long and none is running; 0 means DefaultIdleTTL and Unlimited
	// keeps containers until Delete or Close.
	IdleTTL time.Duration
	// MaxThreads caps the thread executors a Manager keeps; 0 means
	// DefaultMaxThreads. Creating one more evicts the least recently used
	// executor that runs no command and removes its container (its
	// workspace is lost; a later command of that thread starts a new one).
	// When every executor is running a command the cap is exceeded rather
	// than waiting, and the count stays above it until later creations
	// find idle executors to evict.
	MaxThreads int
	// OpTimeout bounds each command a sandbox file operation sends; 0 means
	// sandbox.DefaultOpTimeout.
	OpTimeout time.Duration
	// Labels are added to every container. LabelManaged
	// ("dev.orchestrix.orxdocker") and keys under it
	// ("dev.orchestrix.orxdocker.*") are reserved for orxdocker's own labels
	// and rejected.
	Labels map[string]string

	// WritableRootfs leaves the root file system writable (opt-out).
	WritableRootfs bool
	// AllowNewPrivileges drops the no-new-privileges flag (opt-out).
	AllowNewPrivileges bool
	// CapAdd adds capabilities back after all are dropped (opt-out).
	CapAdd []string
	// SecurityOpt adds Docker security options, for instance
	// "seccomp=<profile JSON>" for a stricter profile.
	// "seccomp=unconfined" is an opt-out.
	SecurityOpt []string
	// DisableInit runs without Docker's init process as PID 1. The init
	// reaps orphaned processes; leave it on unless the daemon has no init
	// binary.
	DisableInit bool
}

// scopePattern restricts scopes to label-friendly values.
var scopePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// withDefaults validates c and fills in its defaults.
func (c Config) withDefaults() (Config, error) {
	if err := c.validate(); err != nil {
		return c, err
	}
	return c.applyDefaults(), nil
}

// dockerHostSocket returns the socket named by DOCKER_HOST, or DefaultSocket
// when it is unset.
func dockerHostSocket() (string, error) {
	h := os.Getenv("DOCKER_HOST")
	if h == "" {
		return DefaultSocket, nil
	}
	sock, ok := strings.CutPrefix(h, "unix://")
	if !ok {
		return "", fmt.Errorf("orxdocker: DOCKER_HOST %q is not a unix:// URL; set Config.Socket or Config.Dial", h)
	}
	return sock, nil
}

// validate rejects an invalid c. It looks at the fields as set, before any
// default: every default is valid.
func (c Config) validate() error {
	switch {
	case c.Dial != nil && c.Socket != "":
		return errors.New("orxdocker: set at most one of Socket and Dial")
	case c.Dial == nil && c.Socket == "":
		if _, err := dockerHostSocket(); err != nil {
			return err
		}
	}
	if c.Scope != "" && !scopePattern.MatchString(c.Scope) {
		return fmt.Errorf("orxdocker: scope %q must be 1-63 letters, digits, '_', '.' or '-'", c.Scope)
	}
	if err := c.validateWorkDir(); err != nil {
		return err
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	if _, err := mergeEnv(c.Env); err != nil {
		return err
	}
	for k := range c.Labels {
		if k == LabelManaged || strings.HasPrefix(k, LabelManaged+".") {
			return fmt.Errorf("orxdocker: label %q is reserved", k)
		}
	}
	return nil
}

// validateWorkDir rejects an unusable WorkDir; "" is the default.
func (c Config) validateWorkDir() error {
	w := c.WorkDir
	if w == "" {
		return nil
	}
	if !path.IsAbs(w) || path.Clean(w) != w || w == "/" || w == "/tmp" {
		return fmt.Errorf("orxdocker: workdir %q must be an absolute, clean path other than / and /tmp", w)
	}
	if strings.ContainsAny(w, ",:") {
		return errors.New("orxdocker: workdir must not contain ',' or ':'")
	}
	return nil
}

// validateLimits rejects negative numeric fields. A zero value means the
// default; only Unlimited, where a field's doc names it, may be negative.
// OpTimeout's 0 is left to sandbox.Config.
func (c Config) validateLimits() error {
	for _, f := range []struct {
		name      string
		v         int64
		unlimited bool
	}{
		{"PidsLimit", c.PidsLimit, true},
		{"MemoryBytes", c.MemoryBytes, true},
		{"NanoCPUs", c.NanoCPUs, true},
		{"IdleTTL", int64(c.IdleTTL), true},
		{"WorkspaceBytes", c.WorkspaceBytes, false},
		{"TmpBytes", c.TmpBytes, false},
		{"Timeout", int64(c.Timeout), false},
		{"MaxOutputBytes", int64(c.MaxOutputBytes), false},
		{"MaxThreads", int64(c.MaxThreads), false},
		{"OpTimeout", int64(c.OpTimeout), false},
	} {
		if f.v < 0 && !(f.unlimited && f.v == Unlimited) {
			return fmt.Errorf("orxdocker: %s is negative", f.name)
		}
	}
	return nil
}

// applyDefaults fills in the zero fields of a validated c. Unlimited is
// negative, so it survives.
func (c Config) applyDefaults() Config {
	c.Image = cmp.Or(c.Image, DefaultImage)
	if c.Dial == nil && c.Socket == "" {
		c.Socket, _ = dockerHostSocket() // validated
	}
	c.APIVersion = cmp.Or(c.APIVersion, DefaultAPIVersion)
	c.Scope = cmp.Or(c.Scope, DefaultScope)
	c.User = cmp.Or(c.User, DefaultUser)
	c.WorkDir = cmp.Or(c.WorkDir, DefaultWorkDir)
	c.PidsLimit = cmp.Or(c.PidsLimit, DefaultPidsLimit)
	c.MemoryBytes = cmp.Or(c.MemoryBytes, DefaultMemoryBytes)
	c.NanoCPUs = cmp.Or(c.NanoCPUs, DefaultNanoCPUs)
	c.IdleTTL = cmp.Or(c.IdleTTL, DefaultIdleTTL)
	c.WorkspaceBytes = cmp.Or(c.WorkspaceBytes, DefaultWorkspaceBytes)
	c.TmpBytes = cmp.Or(c.TmpBytes, DefaultTmpBytes)
	c.Timeout = cmp.Or(c.Timeout, DefaultTimeout)
	c.MaxOutputBytes = cmp.Or(c.MaxOutputBytes, DefaultMaxOutputBytes)
	c.MaxThreads = cmp.Or(c.MaxThreads, DefaultMaxThreads)
	return c
}

// network is the container's network mode.
func (c *Config) network() string {
	if c.Network == "" {
		return "none"
	}
	return c.Network
}

// positiveOr returns v when it is positive, else fallback.
func positiveOr[T int | int64 | time.Duration](v, fallback T) T {
	if v > 0 {
		return v
	}
	return fallback
}
