package state

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
)

type todo struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

func appendSlices(parent, child []string) []string {
	return append(slices.Clone(parent), child...)
}

func TestKeyGetSetTyped(t *testing.T) {
	count := NewKey[int]("count")
	s := New()

	if _, ok := count.Get(s); ok {
		t.Fatal("Get on unset key reported ok")
	}
	count.Set(s, 7)
	if got, ok := count.Get(s); !ok || got != 7 {
		t.Fatalf("Get = %d, %v; want 7, true", got, ok)
	}
	count.Delete(s)
	if _, ok := count.Get(s); ok {
		t.Fatal("Get after Delete reported ok")
	}
}

func TestNewKeyEmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewKey with empty name did not panic")
		}
	}()
	NewKey[int]("")
}

func TestStateJSONRoundTripLazyDecode(t *testing.T) {
	todos := NewKey[[]todo]("todos")
	s := New()
	s.AppendMessages(llm.UserText("hello"))
	todos.Set(s, []todo{{Title: "write tests", Done: true}})

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	restored := New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, ok := todos.Get(restored)
	if !ok || len(got) != 1 || got[0].Title != "write tests" || !got[0].Done {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if msgs := restored.Messages(); len(msgs) != 1 || msgs[0].Text() != "hello" {
		t.Fatalf("Messages = %+v", msgs)
	}
	if _, stillRaw := restored.raw["todos"]; stillRaw {
		t.Fatal("raw entry should move to decoded values after Get")
	}
}

func TestUnknownKeyPreserved(t *testing.T) {
	in := `{"version":1,"messages":[],"values":{"mystery":{"a":[1,2,3]}}}`
	s := New()
	if err := json.Unmarshal([]byte(in), s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != in {
		t.Fatalf("round trip changed data:\n got %s\nwant %s", out, in)
	}
}

func TestUnmarshalRejectsUnknownVersion(t *testing.T) {
	for _, in := range []string{
		`{"version":2,"messages":[],"values":{}}`,
		`{"messages":[],"values":{}}`,
	} {
		err := json.Unmarshal([]byte(in), New())
		if err == nil || !strings.Contains(err.Error(), "version") {
			t.Fatalf("Unmarshal(%s) error = %v; want version error", in, err)
		}
	}
}

func TestUnmarshalResetsState(t *testing.T) {
	name := NewKey[string]("name")
	s := New()
	s.AppendMessages(llm.UserText("old"))
	name.Set(s, "old")

	if err := json.Unmarshal([]byte(`{"version":1,"messages":[],"values":{}}`), s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := name.Get(s); ok || len(s.Messages()) != 0 {
		t.Fatal("Unmarshal did not reset previous contents")
	}
}

func TestMarshalDeterministic(t *testing.T) {
	s := New()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		NewKey[int](n).Set(s, 1)
	}
	first, _ := json.Marshal(s)
	for range 20 {
		again, _ := json.Marshal(s)
		if string(again) != string(first) {
			t.Fatalf("output not stable:\n%s\n%s", first, again)
		}
	}
	if !strings.Contains(string(first), `"alpha":1,"mid":1,"zeta":1`) {
		t.Fatalf("names not sorted: %s", first)
	}
}

func TestGetDecodeFailureKeepsRaw(t *testing.T) {
	count := NewKey[int]("count")
	s := New()
	if err := json.Unmarshal([]byte(`{"version":1,"messages":[],"values":{"count":"not a number"}}`), s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, ok := count.Get(s); ok || got != 0 {
		t.Fatalf("Get = %d, %v; want 0, false", got, ok)
	}
	out, _ := json.Marshal(s)
	if !strings.Contains(string(out), `"count":"not a number"`) {
		t.Fatalf("raw data lost: %s", out)
	}
}

func TestChildForSkipsPrivate(t *testing.T) {
	shared := NewKey[string]("shared")
	secret := NewKey[string]("secret", Private[string]())
	s := New()
	s.AppendMessages(llm.UserText("parent only"))
	shared.Set(s, "visible")
	secret.Set(s, "hidden")

	child := s.ChildFor()

	if got, ok := shared.Get(child); !ok || got != "visible" {
		t.Fatalf("shared in child = %q, %v", got, ok)
	}
	if _, ok := secret.Get(child); ok {
		t.Fatal("private key leaked into child")
	}
	if len(child.Messages()) != 0 {
		t.Fatal("child must start without messages")
	}
}

func TestChildForCopiesUntouchedRawAndIsIndependent(t *testing.T) {
	todos := NewKey[[]todo]("todos")
	s := New()
	if err := json.Unmarshal([]byte(`{"version":1,"messages":[],"values":{"todos":[{"title":"a","done":false}],"other":1}}`), s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	child := s.ChildFor()
	todos.Set(child, nil)

	if got, ok := todos.Get(s); !ok || len(got) != 1 {
		t.Fatalf("parent changed by child write: %+v, %v", got, ok)
	}
	if _, ok := s.ChildFor().raw["other"]; !ok {
		t.Fatal("unregistered raw key not copied to child")
	}
}

func TestMergeChildCustomMerge(t *testing.T) {
	notes := NewKey[[]string]("notes", MergeWith(appendSlices))
	s := New()
	notes.Set(s, []string{"parent"})

	child := s.ChildFor()
	notes.Update(child, func(n []string) []string { return append(slices.Clone(n), "child") })
	s.MergeChild(child)

	// The child started from a copy of the parent's notes, so the merge
	// function sees parent=[parent] and child=[parent child].
	got, _ := notes.Get(s)
	if want := []string{"parent", "parent", "child"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v; want %v", got, want)
	}
}

func TestMergeChildDefaultChildWins(t *testing.T) {
	name := NewKey[string]("name")
	s := New()
	name.Set(s, "parent")
	child := s.ChildFor()
	name.Set(child, "child")
	NewKey[int]("extra").Set(child, 5)

	s.MergeChild(child)

	if got, _ := name.Get(s); got != "child" {
		t.Fatalf("name = %q; want child", got)
	}
	if got, ok := NewKey[int]("extra").Get(s); !ok || got != 5 {
		t.Fatalf("extra = %d, %v; want 5, true", got, ok)
	}
}

func TestMergeChildSkipsPrivate(t *testing.T) {
	secret := NewKey[string]("secret", Private[string]())
	s := New()
	secret.Set(s, "parent")
	child := New()
	secret.Set(child, "child")

	s.MergeChild(child)

	if got, _ := secret.Get(s); got != "parent" {
		t.Fatalf("private key merged back: %q", got)
	}
}

func TestMergeChildDecodesRawOnBothSides(t *testing.T) {
	notes := NewKey[[]string]("notes", MergeWith(appendSlices))
	s := New()
	if err := json.Unmarshal([]byte(`{"version":1,"messages":[],"values":{"notes":["p"]}}`), s); err != nil {
		t.Fatalf("Unmarshal parent: %v", err)
	}
	child := New()
	if err := json.Unmarshal([]byte(`{"version":1,"messages":[],"values":{"notes":["c"]}}`), child); err != nil {
		t.Fatalf("Unmarshal child: %v", err)
	}

	s.MergeChild(child)

	if got, _ := notes.Get(s); !slices.Equal(got, []string{"p", "c"}) {
		t.Fatalf("merged = %v", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	todos := NewKey[[]todo]("todos")
	s := New()
	s.AppendMessages(llm.UserText("one"))
	todos.Set(s, []todo{{Title: "a"}})

	c := s.Clone()
	todos.Update(c, func(l []todo) []todo { l[0].Title = "changed"; return l })
	c.AppendMessages(llm.UserText("two"))

	if got, _ := todos.Get(s); got[0].Title != "a" {
		t.Fatalf("original mutated through clone: %+v", got)
	}
	if len(s.Messages()) != 1 || len(c.Messages()) != 2 {
		t.Fatal("messages not independent")
	}
}

func TestMessagesReturnsCopy(t *testing.T) {
	s := New()
	s.SetMessages([]llm.Message{llm.UserText("a")})
	got := s.Messages()
	got[0] = llm.UserText("changed")
	if s.Messages()[0].Text() != "a" {
		t.Fatal("Messages exposed internal slice")
	}
}

func TestUpdateConcurrent(t *testing.T) {
	counter := NewKey[int]("counter")
	s := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter.Update(s, func(n int) int { return n + 1 })
		}()
	}
	wg.Wait()
	if got, _ := counter.Get(s); got != 100 {
		t.Fatalf("counter = %d; want 100", got)
	}
}

func TestPrivateKeySurvivesCheckpointResume(t *testing.T) {
	secret := NewKey[string]("resume-secret", Private[string]())
	original := New()
	secret.Set(original, "hidden")
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	restored := New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	child := restored.ChildFor() // secret never touched on restored

	if _, leaked := child.raw["resume-secret"]; leaked {
		t.Fatal("private key copied to child after resume")
	}
}

func TestMergeChildSkipsChildOnlyPrivate(t *testing.T) {
	secret := NewKey[string]("child-only-secret", Private[string]())
	parent := New()
	child := New()
	secret.Set(child, "child")

	parent.MergeChild(child)

	if _, ok := secret.Get(parent); ok {
		t.Fatal("child-only private value merged into parent")
	}
}

func TestRedeclareKey(t *testing.T) {
	NewKey[int]("redeclared")
	NewKey[int]("redeclared") // identical: fine

	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("different type", func() { NewKey[string]("redeclared") })
	mustPanic("different privacy", func() { NewKey[int]("redeclared", Private[int]()) })
}

func TestSetUnencodableValuePanics(t *testing.T) {
	type holder struct{ C chan int }
	key := NewKey[holder]("unencodable")
	s := New()

	assertPanics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		f()
	}
	assertPanics("Set", func() { key.Set(s, holder{C: make(chan int)}) })
	assertPanics("Update", func() { key.Update(s, func(holder) holder { return holder{C: make(chan int)} }) })
	if _, ok := key.Get(s); ok {
		t.Fatal("rejected value was stored")
	}
}
