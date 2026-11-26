package jobs

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/state"
)

func TestToolDescriptionsNameBackgroundShellJobs(t *testing.T) {
	// The model also has the task tool, which starts subagents; these tools
	// must not be mistaken for it.
	for _, tl := range mustNew(t, Config{}).Tools() {
		if !strings.Contains(tl.Description(), "background shell job") {
			t.Errorf("%s description %q does not say it works on background shell jobs", tl.Name(), tl.Description())
		}
	}
}

func TestName(t *testing.T) {
	if Name != "jobs" || mustNew(t, Config{}).Name() != Name {
		t.Fatalf("Name = %q, middleware name = %q", Name, mustNew(t, Config{}).Name())
	}
}

func TestToolNames(t *testing.T) {
	var got []string
	for _, tl := range mustNew(t, Config{}).Tools() {
		got = append(got, tl.Name())
	}
	want := []string{"check_job", "cancel_job", "list_jobs", "sleep", "monitor"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if ToolCheck != want[0] || ToolCancel != want[1] || ToolList != want[2] || ToolSleep != want[3] || ToolMonitor != want[4] {
		t.Fatal("the Tool constants do not match the tool names")
	}
}

func TestNewRejectsNegative(t *testing.T) {
	for name, cfg := range map[string]Config{
		"MaxRunning":  {MaxRunning: -1},
		"JobTimeout":  {JobTimeout: -time.Second},
		"MaxFinished": {MaxFinished: -1},
	} {
		m, err := New(cfg)
		if err == nil || m != nil || !strings.Contains(err.Error(), "jobs: "+name+" is negative") {
			t.Errorf("New with negative %s = %v, %v", name, m, err)
		}
	}
	if _, err := New(Config{MaxRunning: 1, JobTimeout: time.Second, MaxFinished: 1}); err != nil {
		t.Fatalf("New with positive values: %v", err)
	}
}

func TestRecordsAccessor(t *testing.T) {
	s := state.New()
	if got := Records(s); got != nil {
		t.Fatalf("Records of an empty State = %+v, want nil", got)
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []Record{
		{ID: "bg_1", ThreadID: "t1", Command: "make", Status: StatusCompleted, Started: started, Ended: started.Add(time.Second), Notified: true, OutputTail: "ok"},
		{ID: "bg_2", ThreadID: "t2", Command: "sleep 30", Status: StatusRunning, Started: started},
	}
	recordsKey.Set(s, want)
	got := Records(s)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Records = %+v, want %+v", got, want)
	}
	got[0].Status = StatusLost
	if again := Records(s); again[0].Status != StatusCompleted {
		t.Fatal("changing the returned records changed the State")
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"jobs.records"`) || !strings.Contains(string(data), `"status":"completed"`) {
		t.Fatalf("State JSON lacks the jobs.records key: %s", data)
	}
}
