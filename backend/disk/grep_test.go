package disk_test

import (
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
	ctx := t.Context()
	withRg, root := newDisk(t, disk.Config{})
	pureGo := openDisk(t, root, disk.Config{DisableRipgrep: true})
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
		"digits.txt":            "\u0663 arabic-indic three\n",
		"badutf8.txt":           "hello \xff\n",
		"huge.txt":              "hello\n" + strings.Repeat("x", backend.MaxGrepFileBytes),
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

	tests := []struct {
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
		{"hello", backend.GrepOptions{Mode: backend.GrepContent, Path: "/bin.dat"}},
		{`\d`, backend.GrepOptions{Mode: backend.GrepContent}},
		{"hello", backend.GrepOptions{Mode: backend.GrepFiles, Glob: "*.{go,md}"}},
		{"hello", backend.GrepOptions{Mode: backend.GrepFiles, Glob: "[ab].go"}},
		{"^$", backend.GrepOptions{Mode: backend.GrepContent}},
		{"nomatchanywhere", backend.GrepOptions{Mode: backend.GrepContent}},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("%s/%s/%+v", tt.pattern, tt.opts.Mode, tt.opts)
		t.Run(name, func(t *testing.T) {
			want, err := pureGo.Grep(ctx, tt.pattern, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := withRg.Grep(ctx, tt.pattern, tt.opts)
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
	ctx := t.Context()
	b, root := newDisk(t, disk.Config{DisableRipgrep: true})
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

func TestDiskGrepSemantics(t *testing.T) {
	ctx := t.Context()
	for _, rg := range []bool{true, false} {
		b, root := newDisk(t, disk.Config{DisableRipgrep: !rg})
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("bad.txt", "hello \xff\n")
		write("bin.dat", "hello\n\x00x")
		write("digits.txt", "\u0663\n")
		write("huge.txt", "hello\n"+strings.Repeat("x", backend.MaxGrepFileBytes))
		write("ok.txt", "hello\n")
		for _, tt := range []struct{ pattern, path string }{
			{"hello", ""}, {"hello", "/bin.dat"}, {"hello", "/bad.txt"}, {"hello", "/huge.txt"}, {`\d`, ""},
		} {
			got, err := b.Grep(ctx, tt.pattern, backend.GrepOptions{Mode: backend.GrepFiles, Path: tt.path})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tt.path == "" && tt.pattern == "hello" {
				// only ok.txt: binary, invalid UTF-8 and oversized files are
				// skipped
				want = 1
			}
			if len(got) != want {
				t.Errorf("rg=%v Grep(%q, %q) = %+v, want %d result(s)", rg, tt.pattern, tt.path, got, want)
			}
		}
	}
}

func TestDiskGrepFallsBackWhenRipgrepRejectsPattern(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	b, root := newDisk(t, disk.Config{})
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("a.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// RE2 accepts \Q...\E quoting; Rust's regex engine rejects it.
	got, err := b.Grep(t.Context(), `\Qa.\E`, backend.GrepOptions{Mode: backend.GrepFiles})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v; want fallback to the Go walker", got, err)
	}
}
