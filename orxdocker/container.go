package orxdocker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Labels set on every container a Manager creates, so Reap (or an operator
// with docker ps --filter label=...) can find them.
const (
	// LabelManaged marks a container as created by orxdocker; its value is
	// "true".
	LabelManaged = "dev.orchestrix.orxdocker"
	// LabelScope holds Config.Scope.
	LabelScope = "dev.orchestrix.orxdocker.scope"
	// LabelThread holds the thread id the container serves.
	LabelThread = "dev.orchestrix.orxdocker.thread"
	// LabelThreadHash holds the hex SHA-256 of the thread id, which is safe
	// to use in a label filter whatever the id contains.
	LabelThreadHash = "dev.orchestrix.orxdocker.thread-sha256"
	// LabelCreated holds the creation time in RFC 3339 format (UTC).
	LabelCreated = "dev.orchestrix.orxdocker.created-at"
	// LabelConfig holds a hash of the container configuration; a running
	// container is reused for its thread only when the hash matches.
	LabelConfig = "dev.orchestrix.orxdocker.config"
)

// keepAlive is the container's main process: it does nothing until the
// container is removed. Commands run beside it as execs.
const keepAlive = "while :; do sleep 3600; done"

// containerConfig is the body of POST /containers/create (the fields
// orxdocker sets).
type containerConfig struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	User       string            `json:"User"`
	WorkingDir string            `json:"WorkingDir"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	Init           bool              `json:"Init"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop"`
	CapAdd         []string          `json:"CapAdd,omitempty"`
	SecurityOpt    []string          `json:"SecurityOpt,omitempty"`
	PidsLimit      int64             `json:"PidsLimit,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	NanoCPUs       int64             `json:"NanoCpus,omitempty"`
	NetworkMode    string            `json:"NetworkMode"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	IpcMode        string            `json:"IpcMode"`
	RestartPolicy  map[string]string `json:"RestartPolicy"`
}

// containerSpec builds the creation request for a thread's container from
// a validated Config. Labels other than the thread's and the creation time
// are part of the configuration hash.
func (o *Config) containerSpec() containerConfig {
	uid, gid, _ := strings.Cut(o.User, ":")
	if gid == "" {
		gid = uid
	}
	var secOpt []string
	if !o.AllowNewPrivileges {
		secOpt = append(secOpt, "no-new-privileges")
	}
	secOpt = append(secOpt, o.SecurityOpt...)
	workspaceOpts := "rw,nosuid,nodev,size=" + strconv.FormatInt(o.WorkspaceBytes, 10) + ",mode=0755"
	if isNumeric(uid) && isNumeric(gid) {
		workspaceOpts += ",uid=" + uid + ",gid=" + gid
	}
	labels := map[string]string{LabelManaged: "true", LabelScope: o.Scope}
	for k, v := range o.Labels {
		labels[k] = v
	}
	cfg := containerConfig{
		Image:      o.Image,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{keepAlive},
		User:       o.User,
		WorkingDir: o.WorkDir,
		Env:        []string{},
		Labels:     labels,
		HostConfig: hostConfig{
			Init:           !o.DisableInit,
			ReadonlyRootfs: !o.WritableRootfs,
			CapDrop:        []string{"ALL"},
			CapAdd:         o.CapAdd,
			SecurityOpt:    secOpt,
			PidsLimit:      o.PidsLimit,
			Memory:         o.MemoryBytes,
			MemorySwap:     o.MemoryBytes,
			NanoCPUs:       o.NanoCPUs,
			NetworkMode:    o.network(),
			Tmpfs: map[string]string{
				"/tmp":    "rw,nosuid,nodev,size=" + strconv.FormatInt(o.TmpBytes, 10) + ",mode=1777",
				o.WorkDir: workspaceOpts,
			},
			IpcMode:       "private",
			RestartPolicy: map[string]string{"Name": "no"},
		},
	}
	if o.PidsLimit == Unlimited {
		cfg.HostConfig.PidsLimit = -1 // the Engine API's "no limit"
	}
	if o.MemoryBytes == Unlimited {
		cfg.HostConfig.Memory, cfg.HostConfig.MemorySwap = 0, 0
	}
	if o.NanoCPUs == Unlimited {
		cfg.HostConfig.NanoCPUs = 0
	}
	data, _ := json.Marshal(cfg) // plain strings and maps: cannot fail
	sum := sha256.Sum256(data)
	cfg.Labels[LabelConfig] = hex.EncodeToString(sum[:8])
	return cfg
}

// isNumeric reports whether s is a non-empty decimal number.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

// threadHash is the value of LabelThreadHash for a thread id.
func threadHash(threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	return hex.EncodeToString(sum[:])
}

// containerSummary is one entry of GET /containers/json.
type containerSummary struct {
	ID      string            `json:"Id"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
}

// listContainers lists the containers (running or not) carrying every
// label filter ("key" or "key=value").
func (c *client) listContainers(ctx context.Context, labels ...string) ([]containerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": labels}) // strings: cannot fail
	q := url.Values{"all": {"true"}, "filters": {string(filters)}}
	var out []containerSummary
	if err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// createContainer creates and starts a container for threadID, pulling the
// image first when the daemon does not have it. A container that fails to
// start is removed.
func (c *client) createContainer(ctx context.Context, spec containerConfig, threadID string) (string, error) {
	if err := c.ensureImage(ctx, spec.Image); err != nil {
		return "", err
	}
	spec.Labels = cloneLabels(spec.Labels)
	spec.Labels[LabelThread] = threadID
	spec.Labels[LabelThreadHash] = threadHash(threadID)
	spec.Labels[LabelCreated] = time.Now().UTC().Format(time.RFC3339)
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", nil, spec, &created); err != nil {
		return "", err
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, nil); err != nil {
		_ = c.removeContainer(context.WithoutCancel(ctx), created.ID)
		return "", err
	}
	return created.ID, nil
}

func cloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+3)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// removeContainer force-removes a container and its anonymous volumes; one
// already gone is not an error.
func (c *client) removeContainer(ctx context.Context, id string) error {
	q := url.Values{"force": {"true"}, "v": {"true"}}
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
	if err != nil && !isStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

// ensureImage pulls ref unless the daemon already has it.
func (c *client) ensureImage(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if err == nil || !isStatus(err, http.StatusNotFound) {
		return err
	}
	name, tag := splitRef(ref)
	body, err := c.stream(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	// The pull reports progress, and failure, as a stream of JSON objects.
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Error != "" {
			return fmt.Errorf("orxdocker: pulling %s: %s", ref, msg.Error)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("orxdocker: pulling %s: %w", ref, err)
	}
	return nil
}

// splitRef splits an image reference into the fromImage and tag parameters
// of a pull. The tag is never empty: an empty tag would pull every tag.
func splitRef(ref string) (name, tag string) {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}
