package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("ORX_TEST_TOKEN", "s3cret")
	t.Setenv("ORX_TEST_DIR", "/work")
	data := []byte(`{"mcpServers": {
		"web": {"url": "https://h/mcp", "headers": {"X-Key": "k-${ORX_TEST_TOKEN}"}, "bearerToken": "${ORX_TEST_TOKEN}",
			"tools": ["read_*"], "gated": true, "optional": true},
		"files": {"command": "srv", "args": ["--root", "${ORX_TEST_DIR}"], "env": {"TOKEN": "${ORX_TEST_TOKEN}"},
			"cwd": "${ORX_TEST_DIR}", "inheritEnv": true, "defer": true, "trustReadOnlyHints": true},
		"off": {"command": "nope", "disabled": true}
	}}`)
	got, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []ServerConfig{
		{Name: "files", Command: "srv", Args: []string{"--root", "/work"}, Env: map[string]string{"TOKEN": "s3cret"},
			Dir: "/work", InheritEnv: true, Defer: true, TrustReadOnlyHints: true},
		{Name: "web", URL: "https://h/mcp", Headers: map[string]string{"X-Key": "k-s3cret"}, BearerToken: "s3cret",
			Tools: []string{"read_*"}, Gated: true, Optional: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseConfig =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name, data, wantErr string
	}{
		{"unknown field", `{"mcpServers": {"a": {"command": "x", "comand": "y"}}}`, "comand"},
		{"unknown top-level field", `{"servers": {}}`, "servers"},
		{"command and url", `{"mcpServers": {"a": {"command": "x", "url": "http://h"}}}`, "a"},
		{"neither command nor url", `{"mcpServers": {"a": {"args": ["x"]}}}`, "a"},
		{"missing variable", `{"mcpServers": {"a": {"command": "${ORX_TEST_MISSING_VAR}"}}}`, "ORX_TEST_MISSING_VAR"},
		{"invalid name", `{"mcpServers": {"a__b": {"command": "x"}}}`, "a__b"},
		{"not json", `{"mcpServers": `, "mcp"},
		{"trailing data", `{"mcpServers": {}} {}`, "mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseConfigLeavesOtherDollarsAlone(t *testing.T) {
	t.Setenv("HOME_ORX", "x")
	got, err := ParseConfig([]byte(`{"mcpServers": {"a": {"command": "x", "args": ["$HOME_ORX", "$${HOME_ORX"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"$HOME_ORX", "$${HOME_ORX"}; !reflect.DeepEqual(got[0].Args, want) {
		t.Fatalf("args = %q, want %q unchanged", got[0].Args, want)
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {"a": {"command": "x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" || got[0].Command != "x" {
		t.Fatalf("LoadConfig = %+v", got)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadConfig of a missing file succeeded")
	}
}
