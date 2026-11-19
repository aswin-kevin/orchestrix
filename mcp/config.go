package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
)

// configFile is the JSON shape LoadConfig and ParseConfig read.
type configFile struct {
	MCPServers map[string]configServer `json:"mcpServers"`
}

// configServer is one entry of configFile.MCPServers.
type configServer struct {
	Command            string            `json:"command"`
	Args               []string          `json:"args"`
	Env                map[string]string `json:"env"`
	Cwd                string            `json:"cwd"`
	InheritEnv         bool              `json:"inheritEnv"`
	URL                string            `json:"url"`
	Headers            map[string]string `json:"headers"`
	BearerToken        string            `json:"bearerToken"`
	Tools              []string          `json:"tools"`
	Defer              bool              `json:"defer"`
	Gated              bool              `json:"gated"`
	Optional           bool              `json:"optional"`
	TrustReadOnlyHints bool              `json:"trustReadOnlyHints"`
	IgnoreInstructions bool              `json:"ignoreInstructions"`
	Disabled           bool              `json:"disabled"`
}

// envReference matches a ${VAR} reference in a configuration string.
var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadConfig reads the server configuration file at path; see ParseConfig.
func LoadConfig(path string) ([]ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: config: %w", err)
	}
	servers, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%w (in %s)", err, path)
	}
	return servers, nil
}

// ParseConfig parses a server configuration:
//
//	{"mcpServers": {"<name>": {"command": "...", "args": [...], "env": {...},
//	  "cwd": "...", "inheritEnv": false, "url": "...", "headers": {...},
//	  "bearerToken": "...", "tools": [...], "defer": false, "gated": false,
//	  "optional": false, "trustReadOnlyHints": false,
//	  "ignoreInstructions": false, "disabled": false}}}
//
// Each server sets exactly one of command and url; unknown fields are
// errors; disabled servers are skipped. Servers are checked and returned in
// name order, so the first error reported is always the same.
//
// Every ${VAR} in a string value is replaced by the host process's
// environment variable VAR when the configuration is parsed, and a variable
// that is not set is an error. Whoever writes the file can therefore pull
// any of the host's environment variables, secrets included, into a
// command's arguments or environment, or into headers sent to a URL: load
// configuration only from trusted files.
func ParseConfig(data []byte) ([]ServerConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file configFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("mcp: config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("mcp: config: unexpected data after the configuration object")
	}
	var servers []ServerConfig
	for _, name := range slices.Sorted(maps.Keys(file.MCPServers)) {
		entry := file.MCPServers[name]
		if entry.Disabled {
			continue
		}
		cfg, err := entry.serverConfig(name)
		if err != nil {
			return nil, fmt.Errorf("mcp: config: server %q: %w", name, err)
		}
		servers = append(servers, cfg)
	}
	if err := validateServers(servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// serverConfig turns the entry into the ServerConfig of the server named
// name, expanding ${VAR} references.
func (e configServer) serverConfig(name string) (ServerConfig, error) {
	if (e.Command == "") == (e.URL == "") {
		return ServerConfig{}, errors.New("set exactly one of command and url")
	}
	x := expander{}
	cfg := ServerConfig{
		Name:               name,
		Command:            x.string(e.Command),
		Args:               x.strings(e.Args),
		Env:                x.values(e.Env),
		Dir:                x.string(e.Cwd),
		InheritEnv:         e.InheritEnv,
		URL:                x.string(e.URL),
		Headers:            x.values(e.Headers),
		BearerToken:        x.string(e.BearerToken),
		Tools:              x.strings(e.Tools),
		Defer:              e.Defer,
		Gated:              e.Gated,
		Optional:           e.Optional,
		TrustReadOnlyHints: e.TrustReadOnlyHints,
		IgnoreInstructions: e.IgnoreInstructions,
	}
	if x.missing != "" {
		return ServerConfig{}, fmt.Errorf("environment variable %s is not set", x.missing)
	}
	return cfg, nil
}

// expander replaces ${VAR} references with the host's environment, noting
// the first variable that is not set.
type expander struct{ missing string }

func (x *expander) string(s string) string {
	return envReference.ReplaceAllStringFunc(s, func(ref string) string {
		name := envReference.FindStringSubmatch(ref)[1]
		value, ok := os.LookupEnv(name)
		if !ok && x.missing == "" {
			x.missing = name
		}
		return value
	})
}

func (x *expander) strings(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = x.string(s)
	}
	return out
}

func (x *expander) values(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = x.string(v)
	}
	return out
}
