// Package askuser lets the agent put structured multiple-choice questions to
// the human. The ask_user tool is answered either by a synchronous callback
// (Config.Ask) or, without one, by pausing the run with an interrupt that the
// host answers with Respond (the answers) or Reject (declined).
//
// Questions, options and headers are text written by the model. They are
// shown to the human as data: control characters other than newline are
// stripped and every part is length-capped before it is rendered or handed to
// a callback.
//
// ask_user is read-only, so it is available in plan mode and, unlike a tool
// that needs approval, in dontAsk mode too: dontAsk governs permission
// prompts, not questions the model asks.
package askuser

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ToolAskUser is the name of the tool the middleware offers.
const ToolAskUser = "ask_user"

// Limits on what the model may ask. The counts and MaxHeaderChars are the
// documented shape of the tool input; the Max...Runes caps bound the text a
// human is shown.
const (
	// MaxQuestions is the most questions one call may carry.
	MaxQuestions = 4
	// MinOptions is the fewest options a question needs.
	MinOptions = 2
	// MaxOptions is the most options a question may offer.
	MaxOptions = 4
	// MaxHeaderChars is the longest header, in runes.
	MaxHeaderChars = 12

	// MaxQuestionRunes is the longest question text, in runes.
	MaxQuestionRunes = 1000
	// MaxLabelRunes is the longest option label, in runes.
	MaxLabelRunes = 200
	// MaxDescriptionRunes is the longest option description, in runes.
	MaxDescriptionRunes = 500
	// MaxAnswerRunes is the longest free-text answer kept, in runes; a
	// longer one is cut.
	MaxAnswerRunes = 8000
)

// Fixed texts of the tool's results.
const (
	declinedText = "The user declined to answer."
	answersTitle = "User answers:"
)

// Option is one choice of a question.
type Option struct {
	Label       string `json:"label" desc:"Short choice text."`
	Description string `json:"description,omitempty" desc:"What choosing it means."`
}

// Question is one multiple-choice question.
type Question struct {
	Question    string   `json:"question" desc:"The full question."`
	Header      string   `json:"header,omitempty" desc:"Very short label (max 12 chars)."`
	Options     []Option `json:"options" desc:"2-4 choices; the user may also answer in their own words."`
	MultiSelect bool     `json:"multi_select,omitempty" desc:"Allow several choices."`
}

// Input is the input of ask_user.
type Input struct {
	Questions []Question `json:"questions" desc:"1-4 questions."`
}

// Answer is the human's answer to one question.
type Answer struct {
	// Question is the text of the question answered. Empty means the
	// question at the same position.
	Question string `json:"question"`
	// Selected holds the labels of the chosen options. For a question that
	// is not MultiSelect only the first label counts; the rest are reported
	// as other text. Repeated labels count once.
	Selected []string `json:"selected,omitempty"`
	// Other is the answer in the human's own words.
	Other string `json:"other,omitempty"`
}

// Config configures the askuser middleware.
type Config struct {
	// Ask answers the questions synchronously. It returns one Answer per
	// question, in order. nil pauses the run with an interrupt instead,
	// which the host answers with Respond or Reject.
	//
	// A subagent cannot pause for a human, so ask_user called at a depth
	// above zero (inside a subagent) without Ask returns an error result and
	// raises no interrupt: the agent that spawns subagents must supply Ask
	// for them to be able to ask.
	//
	// Parallel subagents may call ask_user at the same time, so Ask must be
	// safe for concurrent use and should serialise its prompts to the human
	// itself.
	Ask func(ctx context.Context, questions []Question) ([]Answer, error)
	// Prompt is the system prompt segment describing the tool. nil uses
	// DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string
}

// middleware is the askuser middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type middleware struct {
	cfg    Config
	prompt string
}

// New returns the askuser middleware.
func New(cfg Config) agent.Middleware {
	m := &middleware{cfg: cfg, prompt: DefaultPrompt}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	return m
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "askuser" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool {
	return []tool.Tool{tool.New(ToolAskUser,
		"Ask the user one to four multiple-choice questions when a decision is theirs to make. Each question needs 2-4 options; the user may also answer in their own words.",
		m.ask,
		// Asking changes nothing, so as a read-only tool it needs no
		// approval and works in plan and dontAsk modes: dontAsk governs
		// permission prompts, not questions the model asks. It is not
		// concurrency-safe: two questions at once would confuse the human.
		tool.WithReadOnly())}
}

// WrapModel implements agent.ModelWrapper. It appends the prompt after the
// host's system segments, outside the cached prefix.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	return next(ctx, req)
}

// Questions decodes the questions of a pending ask_user request, for a host
// to display. The text is cleaned like the interrupt's Reason.
func Questions(req agent.ActionRequest) ([]Question, error) {
	if req.Tool != ToolAskUser {
		return nil, fmt.Errorf("askuser: request is for %q, not %s", req.Tool, ToolAskUser)
	}
	var in Input
	if err := json.Unmarshal(req.Input, &in); err != nil {
		return nil, fmt.Errorf("askuser: decode questions: %w", err)
	}
	return clean(in.Questions), nil
}

// answersMessage is the JSON a Respond decision carries.
type answersMessage struct {
	Answers []Answer `json:"answers"`
}

// Respond builds the decision that answers the pending ask_user call
// toolUseID with answers. Its Message is the JSON {"answers":[...]}.
//
// A Respond message is read as structured answers only when it is a JSON
// object with a non-empty "answers" array. Any other message is the human's
// free-text reply, shown to the model once; a blank one counts as declining.
func Respond(toolUseID string, answers []Answer) agent.Decision {
	msg, err := json.Marshal(answersMessage{Answers: answers})
	if err != nil { // plain strings cannot fail to marshal
		panic(fmt.Sprintf("askuser: marshal answers: %v", err))
	}
	return agent.Decision{ToolUseID: toolUseID, Type: agent.Respond, Message: string(msg)}
}

func (m *middleware) ask(ctx context.Context, in Input, c *tool.Call) (tool.Result, error) {
	qs := clean(in.Questions)
	if err := validate(qs); err != nil {
		return tool.Errorf("invalid questions: %v", err), nil
	}
	if r := c.Resume; r != nil {
		return resumed(qs, r), nil
	}
	if m.cfg.Ask != nil {
		answers, err := m.cfg.Ask(ctx, qs)
		if err != nil {
			return tool.Errorf("could not ask the user: %v", err), nil
		}
		return tool.Text(formatAnswers(qs, answers)), nil
	}
	if info, _ := agent.Info(ctx); info.Depth > 0 {
		return tool.Errorf("ask_user cannot pause a subagent for the user's answer: the agent that spawned this one must supply Config.Ask. Decide yourself or report the open question in your result."), nil
	}
	return tool.Result{}, &agent.Interrupt{
		Requests: []agent.ActionRequest{{
			Reason:  render(qs),
			Allowed: []agent.DecisionType{agent.Respond, agent.Reject},
		}},
		ReinvokeOnReject: true,
	}
}

// resumed turns the human's decision into the tool result.
func resumed(qs []Question, r *tool.ResumeValue) tool.Result {
	switch r.Type {
	case tool.ResumeRespond:
		if strings.TrimSpace(r.Message) == "" {
			return tool.Text(declinedText)
		}
		if answers, ok := structuredAnswers(r.Message); ok {
			return tool.Text(formatAnswers(qs, answers))
		}
		return tool.Text(answersTitle + "\n" + inline(r.Message, MaxAnswerRunes))
	case tool.ResumeReject:
		if r.Message == "" {
			return tool.Text(declinedText)
		}
		return tool.Text(declinedText + " " + inline(r.Message, MaxAnswerRunes))
	default:
		// Unreachable: the interrupt allows only respond and reject.
		return tool.Errorf("unexpected decision %q for ask_user", r.Type)
	}
}

// structuredAnswers reads msg as {"answers":[...]} with at least one answer.
func structuredAnswers(msg string) ([]Answer, bool) {
	var parsed answersMessage
	if err := json.Unmarshal([]byte(msg), &parsed); err != nil || len(parsed.Answers) == 0 {
		return nil, false
	}
	return parsed.Answers, true
}

// noAnswer stands for a question that got no answer.
const noAnswer = "(no answer)"

// formatAnswers renders answers for the model, whoever produced them. One
// answer belongs to each question by position and extras are dropped; a
// question without an answer reads "(no answer)". All text is cleaned and
// capped, and its newlines are indented so no answer can start a line that
// looks like another bullet or the title.
func formatAnswers(qs []Question, answers []Answer) string {
	var sb strings.Builder
	sb.WriteString(answersTitle)
	for i, q := range qs {
		var a Answer
		if i < len(answers) {
			a = answers[i]
		}
		text := inline(a.Question, MaxQuestionRunes)
		if text == "" {
			text = inline(q.Question, MaxQuestionRunes)
		}
		var selected, other []string
		seen := make(map[string]bool, len(a.Selected))
		for _, label := range a.Selected {
			label = inline(label, MaxLabelRunes)
			if seen[label] {
				continue
			}
			seen[label] = true
			if hasOption(q, label) && (q.MultiSelect || len(selected) == 0) {
				selected = append(selected, label)
			} else if label != "" {
				other = append(other, label)
			}
		}
		if o := inline(a.Other, MaxAnswerRunes); o != "" {
			other = append(other, o)
		}
		fmt.Fprintf(&sb, "\n- %s: ", text)
		switch {
		case len(selected) == 0 && len(other) == 0:
			sb.WriteString(noAnswer)
		default:
			sb.WriteString(strings.Join(selected, ", "))
			if len(other) > 0 {
				fmt.Fprintf(&sb, "; other: %s", truncateRunes(strings.Join(other, ", "), MaxAnswerRunes))
			}
		}
	}
	return sb.String()
}

// inline cleans s, caps it at n runes and indents its newlines.
func inline(s string, n int) string {
	return strings.ReplaceAll(truncateRunes(stripControl(s), n), "\n", "\n    ")
}

func hasOption(q Question, label string) bool {
	return slices.ContainsFunc(q.Options, func(o Option) bool { return inline(o.Label, MaxLabelRunes) == label })
}

// clean returns a copy of qs with every text stripped of control characters
// other than newline.
func clean(qs []Question) []Question {
	out := make([]Question, len(qs))
	for i, q := range qs {
		out[i] = Question{
			Question:    stripControl(q.Question),
			Header:      stripControl(q.Header),
			MultiSelect: q.MultiSelect,
			Options:     make([]Option, len(q.Options)),
		}
		for j, o := range q.Options {
			out[i].Options[j] = Option{Label: stripControl(o.Label), Description: stripControl(o.Description)}
		}
	}
	return out
}

// stripControl drops every control character except newline, which also
// removes the escape character that starts terminal control sequences. The
// Unicode line and paragraph separators count as newlines.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\u2028' || r == '\u2029' {
			return '\n'
		}
		if r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// validate checks the shape and size limits of already cleaned questions.
func validate(qs []Question) error {
	if len(qs) < 1 || len(qs) > MaxQuestions {
		return fmt.Errorf("give 1-%d questions, got %d", MaxQuestions, len(qs))
	}
	for i, q := range qs {
		n := i + 1
		switch {
		case strings.TrimSpace(q.Question) == "":
			return fmt.Errorf("question %d has no text", n)
		case utf8.RuneCountInString(q.Question) > MaxQuestionRunes:
			return fmt.Errorf("question %d is longer than %d characters", n, MaxQuestionRunes)
		case utf8.RuneCountInString(q.Header) > MaxHeaderChars:
			return fmt.Errorf("question %d: header is longer than %d characters", n, MaxHeaderChars)
		case len(q.Options) < MinOptions || len(q.Options) > MaxOptions:
			return fmt.Errorf("question %d needs %d-%d options, got %d", n, MinOptions, MaxOptions, len(q.Options))
		}
		if err := validateOptions(n, q.Options); err != nil {
			return err
		}
	}
	return nil
}

func validateOptions(n int, opts []Option) error {
	seen := make(map[string]bool, len(opts))
	for _, o := range opts {
		switch {
		case strings.TrimSpace(o.Label) == "":
			return fmt.Errorf("question %d has an option with no label", n)
		case utf8.RuneCountInString(o.Label) > MaxLabelRunes:
			return fmt.Errorf("question %d: an option label is longer than %d characters", n, MaxLabelRunes)
		case utf8.RuneCountInString(o.Description) > MaxDescriptionRunes:
			return fmt.Errorf("question %d: an option description is longer than %d characters", n, MaxDescriptionRunes)
		case seen[o.Label]:
			return fmt.Errorf("question %d repeats the option label %q", n, o.Label)
		}
		seen[o.Label] = true
	}
	return nil
}

// render writes cleaned questions as text with numbered options, for the
// interrupt's Reason.
func render(qs []Question) string {
	var sb strings.Builder
	sb.WriteString("The agent asks:")
	for i, q := range qs {
		sb.WriteString("\n\n")
		fmt.Fprintf(&sb, "%d. ", i+1)
		if q.Header != "" {
			fmt.Fprintf(&sb, "[%s] ", q.Header)
		}
		sb.WriteString(q.Question)
		if q.MultiSelect {
			sb.WriteString(" (choose any number)")
		}
		for j, o := range q.Options {
			fmt.Fprintf(&sb, "\n   %d) %s", j+1, o.Label)
			if o.Description != "" {
				fmt.Fprintf(&sb, " - %s", o.Description)
			}
		}
	}
	sb.WriteString("\n\nAnswer with the options you choose, or in your own words.")
	return sb.String()
}
