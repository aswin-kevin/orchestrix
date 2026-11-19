package async

import (
	"fmt"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
)

// Frame tags the middleware wraps model-derived text in.
const (
	tagNotification = "task-notification"
	tagResult       = "task-result"
	tagProgress     = "task-progress"
	tagDescription  = "task-description"
	tagUpdate       = "task-update"
	tagRunning      = "background_tasks"
)

// neutralize escapes every tag in model-derived text that could open or end
// one of the middleware's frames.
var neutralize = promptsafe.NewTagNeutralizer(tagNotification, tagResult, tagProgress, tagDescription, tagUpdate, tagRunning)

// truncatedNote ends an excerpt that was cut.
const truncatedNote = "\n[truncated]"

// lostNote says what a lost task means and what to do about it.
const lostNote = "was lost: the process restarted before it finished. Start it again if it is still needed."

// excerpt returns text cut to limit bytes and neutralized, with a note when
// it was cut.
func excerpt(text string, limit int) string {
	cut, truncated := promptsafe.CapBytes(text, limit)
	cut = neutralize(cut)
	if truncated {
		cut += truncatedNote
	}
	return cut
}

// frame wraps body, already neutralized, in tag.
func frame(tag, body string) string {
	return fmt.Sprintf("<%s>\n%s\n</%s>", tag, body, tag)
}

// notification is the message announcing that the task of r ended: its
// status, then for a task with an answer an excerpt of it.
func notification(r TaskRecord) llm.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "<%s>Background task %s (%s) %s", tagNotification, r.ID, r.SubagentType, outcomeLine(r))
	if r.Result != "" {
		fmt.Fprintf(&b, "\n%s\nUse %s for the full result.", frame(tagResult, excerpt(r.Result, DefaultMaxResultBytes)), ToolCheck)
	}
	fmt.Fprintf(&b, "</%s>", tagNotification)
	msg := llm.UserText(b.String())
	msg.Meta = map[string]string{NotificationMetaKey: r.ID, agent.InjectedMetaKey: "async"}
	return msg
}

// outcomeLine says how the task of an ended record ended, as a sentence
// that follows the task's name; the error is excerpted and neutralized. A
// completed or cancelled task may carry an error too, such as updates that
// arrived after it finished, which is added as a note.
func outcomeLine(r TaskRecord) string {
	if r.Status == StatusLost {
		return lostNote
	}
	status := strings.ReplaceAll(r.Status, "_", " ")
	switch {
	case r.Error == "":
		return status + "."
	case r.Status == StatusCompleted || r.Status == StatusCancelled:
		return fmt.Sprintf("%s (note: %s).", status, excerpt(r.Error, DefaultMaxResultBytes))
	}
	return fmt.Sprintf("%s: %s.", status, excerpt(r.Error, DefaultMaxResultBytes))
}

// summary renders a task's one-line status: running for how long, or how it
// ended after how long.
func summary(r TaskRecord, now time.Time) string {
	head := fmt.Sprintf("Task %s (%s)", r.ID, r.SubagentType)
	switch {
	case r.Status == StatusRunning:
		return fmt.Sprintf("%s: %s for %s.", head, StatusRunning, elapsed(r.Started, now))
	case r.Status == StatusLost:
		return head + " " + lostNote
	case r.Error != "":
		return fmt.Sprintf("%s: %s after %s: %s", head, r.Status, elapsed(r.Started, r.Ended), excerpt(r.Error, DefaultMaxResultBytes))
	}
	return fmt.Sprintf("%s: %s after %s.", head, r.Status, elapsed(r.Started, r.Ended))
}

// elapsed renders the time from start to end, in whole seconds.
func elapsed(start, end time.Time) time.Duration {
	return end.Sub(start).Round(time.Second)
}

// progressText renders a running task's recent output.
func progressText(progress string) string {
	if progress == "" {
		return "(no output yet)"
	}
	return frame(tagProgress, neutralize(progress))
}

// listEntry renders one task for list_tasks: its summary, its description
// cut short and, once it ended, an excerpt of its result.
func listEntry(r TaskRecord, now time.Time) string {
	parts := []string{summary(r, now), frame(tagDescription, excerpt(r.Description, descriptionPreviewBytes))}
	if r.Result != "" {
		parts = append(parts, frame(tagResult, excerpt(r.Result, DefaultMaxResultBytes)))
	}
	return strings.Join(parts, "\n")
}

// runningList renders the tasks of the running records for the system
// prompt, one line each with the description cut short to one line; ""
// without any.
func runningList(records []TaskRecord, now time.Time) string {
	var b strings.Builder
	for _, r := range records {
		if r.Status != StatusRunning {
			continue
		}
		if b.Len() == 0 {
			fmt.Fprintf(&b, "<%s>", tagRunning)
		}
		fmt.Fprintf(&b, "\n- %s (%s, %s %s): %s", r.ID, r.SubagentType, StatusRunning, runningFor(now.Sub(r.Started)), oneLine(r.Description))
	}
	if b.Len() == 0 {
		return ""
	}
	fmt.Fprintf(&b, "\n</%s>", tagRunning)
	return b.String()
}

// runningFor renders how long a task runs, in whole minutes, so the system
// prompt changes at most once a minute.
func runningFor(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return strings.TrimSuffix(d.Truncate(time.Minute).String(), "0s")
}

// oneLine is the start of a description on a single line, neutralized.
func oneLine(desc string) string {
	cut, truncated := promptsafe.CapBytes(desc, descriptionPreviewBytes)
	cut = neutralize(strings.Join(strings.Fields(cut), " "))
	if truncated {
		cut += "..."
	}
	return cut
}
