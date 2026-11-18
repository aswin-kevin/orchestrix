package frontmatter

import (
	"reflect"
	"strings"
	"testing"
)

type doc struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
}

func TestParseFrontmatter(t *testing.T) {
	var d doc
	body, ok, err := Parse([]byte("---\nname: demo\ncount: 3\n---\nhello\nworld\n"), &d)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if d != (doc{Name: "demo", Count: 3}) {
		t.Fatalf("decoded %+v", d)
	}
	if body != "hello\nworld\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestNoFrontmatter(t *testing.T) {
	for _, in := range []string{"just text\n", "", "--- not a fence\nx\n---\n", "---\nname: x\nno closing fence\n"} {
		d := doc{Name: "keep"}
		body, ok, err := Parse([]byte(in), &d)
		if err != nil || ok || body != in || d.Name != "keep" {
			t.Fatalf("Parse(%q) = %q, %v, %v, d=%+v; want untouched", in, body, ok, err, d)
		}
	}
}

func TestEmptyFrontmatter(t *testing.T) {
	var d doc
	body, ok, err := Parse([]byte("---\n---\nbody"), &d)
	if err != nil || !ok || body != "body" {
		t.Fatalf("body=%q ok=%v err=%v", body, ok, err)
	}
}

func TestCRLFAndBOM(t *testing.T) {
	var d doc
	body, ok, err := Parse([]byte("\ufeff---\r\nname: demo\r\ncount: 2\r\n---\r\nline1\r\nline2"), &d)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if d != (doc{Name: "demo", Count: 2}) {
		t.Fatalf("decoded %+v", d)
	}
	if body != "line1\r\nline2" {
		t.Fatalf("body = %q", body)
	}
}

func TestMalformedYAML(t *testing.T) {
	var d doc
	_, ok, err := Parse([]byte("---\nname: [unclosed\n---\nbody"), &d)
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v, want an error", ok, err)
	}
}

func TestRenderRoundTrip(t *testing.T) {
	out, err := Render(doc{Name: "demo", Count: 7}, "body\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "---\n") || !strings.HasSuffix(string(out), "---\nbody\n") {
		t.Fatalf("rendered %q", out)
	}
	var d doc
	body, ok, err := Parse(out, &d)
	if err != nil || !ok || body != "body\n" || d != (doc{Name: "demo", Count: 7}) {
		t.Fatalf("round trip: %+v %q %v %v", d, body, ok, err)
	}
}

func TestStringList(t *testing.T) {
	type fm struct {
		Tools StringList `yaml:"tools"`
	}
	cases := map[string]struct {
		in   string
		want StringList
	}{
		"space separated": {"tools: read_file  grep   glob", StringList{"read_file", "grep", "glob"}},
		"yaml list":       {"tools: [read_file, grep]", StringList{"read_file", "grep"}},
		"block list":      {"tools:\n  - a\n  - b", StringList{"a", "b"}},
		"empty string":    {`tools: ""`, nil},
		"absent":          {"other: 1", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var f fm
			if _, ok, err := Parse([]byte("---\n"+tc.in+"\n---\n"), &f); err != nil || !ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if !reflect.DeepEqual(f.Tools, tc.want) && len(f.Tools)+len(tc.want) != 0 {
				t.Fatalf("Tools = %#v, want %#v", f.Tools, tc.want)
			}
		})
	}
}

func TestStringListRejectsMapping(t *testing.T) {
	var f struct {
		Tools StringList `yaml:"tools"`
	}
	if _, _, err := Parse([]byte("---\ntools: {a: b}\n---\n"), &f); err == nil {
		t.Fatal("a mapping is neither a string nor a list")
	}
}

func TestMetadata(t *testing.T) {
	var f struct {
		Meta Metadata `yaml:"metadata"`
	}
	if _, _, err := Parse([]byte("---\nmetadata:\n  author: me\n  version: 2\n---\n"), &f); err != nil {
		t.Fatal(err)
	}
	if f.Meta["author"] != "me" || f.Meta["version"] != "2" {
		t.Fatalf("Meta = %v", f.Meta)
	}
}

func TestBlockOverMaxBytesRejected(t *testing.T) {
	in := "---\nname: " + strings.Repeat("x", MaxBytes) + "\n---\nbody"
	var d doc
	if _, ok, err := Parse([]byte(in), &d); err == nil || ok {
		t.Fatalf("ok=%v err=%v, want an error for an oversized block", ok, err)
	}
}

func TestStringListTrimsSequenceElements(t *testing.T) {
	var f struct {
		Tools StringList `yaml:"tools"`
	}
	if _, _, err := Parse([]byte("---\ntools: [\" a \", b ]\n---\n"), &f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Tools, StringList{"a", "b"}) {
		t.Fatalf("Tools = %#v", f.Tools)
	}
}
