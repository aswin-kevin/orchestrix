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
)

// neutralize escapes every tag in model-derived text that could open or end
// one of the middleware's frames.
var neutralize = promptsafe.NewTagNeutralizer(tagNotification, tagResult, tagProgress, tagDescription)

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
		fmt.Fprintf(&b, "\n%s\nUse %s for the full result.", frame(tagResult, excerpt(r.Result, DefaultMaxResultChars)), ToolCheck)
	}
	fmt.Fprintf(&b, "</%s>", tagNotification)
	msg := llm.UserText(b.String())
	msg.Meta = map[string]string{NotificationMetaKey: r.ID, agent.InjectedMetaKey: "async"}
	return msg
}

// outcomeLine says how the task of an ended record ended, as a sentence
// that follows the task's name; the error is excerpted and neutralized.
func outcomeLine(r TaskRecord) string {
	switch r.Status {
	case StatusLost:
		return lostNote
	case StatusCompleted, StatusCancelled:
		return r.Status + "."
	}
	status := strings.ReplaceAll(r.Status, "_", " ")
	if r.Error == "" {
		return status + "."
	}
	return fmt.Sprintf("%s: %s.", status, excerpt(r.Error, DefaultMaxResultChars))
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
		return fmt.Sprintf("%s: %s after %s: %s", head, r.Status, elapsed(r.Started, r.Ended), excerpt(r.Error, DefaultMaxResultChars))
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
	parts := []string{summary(r, now), frame(tagDescription, excerpt(r.Description, descriptionPreviewChars))}
	if r.Result != "" {
		parts = append(parts, frame(tagResult, excerpt(r.Result, DefaultMaxResultChars)))
	}
	return strings.Join(parts, "\n")
}
