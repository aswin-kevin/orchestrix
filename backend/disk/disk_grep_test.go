package disk_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/disk"
)

func TestDiskRipgrepParity(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	ctx := context.Background()
	withRg, root := newDisk(t, disk.WithRipgrep(true))
	pureGo, err := disk.New(root, disk.WithRipgrep(false))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.go":                  "package a\nfunc Hello() {}\nfunc hello2() {}\n",
		"b.txt":                 "Hello world\r\nhello again\r\n\r\nno newline at end hello",
		"src/deep/c.go":         "package c\nvar hello = `x`\n",
		"src/z.md":              "# Hello\n",
		".hidden/h.txt":         "hello hidden\n",
		".git/config":           "hello git\n",
		"node_modules/m/i.js":   "hello module\n",
		"a.dir/x.txt":           "hello dir\n",
		"unicode.txt":           "héllo wörld Hello\n",
		"bin.dat":               "hello\x00binary",
		"src/hello-with-dash/p": "hello\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		pattern string
		opts    backend.GrepOptions
	}{
		{"hello", backend.GrepOptions{Mode: backend.GrepContent}},
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, IgnoreCase: true}},
		{"hello", backend.GrepOptions{Mode: backend.GrepFiles}},
		{"hello", backend.GrepOptions{Mode: backend.GrepCount, IgnoreCase: true}},
		{"func \\w+\\(", backend.GrepOptions{Mode: backend.GrepContent}},
		{"`x`", backend.GrepOptions{Mode: backend.GrepContent, Literal: true}},
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, Glob: "*.go"}},
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, Glob: "src/**/*.go"}},
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, Path: "/src"}},
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, Path: "/b.txt"}},
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, MaxResults: 2}},
		{"hello", backend.GrepOptions{Mode: backend.GrepFiles, MaxResults: 3}},
		{"^$", backend.GrepOptions{Mode: backend.GrepContent}},
		{"nomatchanywhere", backend.GrepOptions{Mode: backend.GrepContent}},
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s/%s/%+v", c.pattern, c.opts.Mode, c.opts)
		t.Run(name, func(t *testing.T) {
			want, err := pureGo.Grep(ctx, c.pattern, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := withRg.Grep(ctx, c.pattern, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
				t.Fatalf("ripgrep and pure-Go results differ\nrg:   %+v\ngo:   %+v", got, want)
			}
		})
	}
}

func TestDiskGrepSkipsGitAndNodeModules(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t, disk.WithRipgrep(false))
	for _, name := range []string{".git/config", "node_modules/x/i.js", "ok.txt"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Grep(ctx, "needle", backend.GrepOptions{Mode: backend.GrepFiles})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].Path, "/ok.txt") {
		t.Fatalf("got %+v, want only /ok.txt", got)
	}
}
