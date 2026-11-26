package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func init() {
	RegisterDecider(FormatOpenAIDecisions, func(cfg Config) (Decider, error) {
		d, err := newOpenAIDecisions(cfg)
		if err != nil {
			return nil, err
		}
		return d, nil
	})
}

// OpenAI wire type names.
const (
	openAIPredicate = "predicate"
	openAIChoice    = "choice"
	openAIScore     = "score"
	openAIRefusal   = "refusal"
)

// openAIDecisions speaks the OpenAI Decisions API. It is built from the
// published API reference and has not yet been verified against a live
// endpoint.
type openAIDecisions struct {
	cfg Config
	t   *Transport
}

// newOpenAIDecisions returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newOpenAIDecisions(cfg Config) (*openAIDecisions, error) {
	t, err := NewTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &openAIDecisions{cfg: t.Config(), t: t}, nil
}

// Wire types.

type openAIDecisionsRequest struct {
	Model string `json:"model"`
	// string, or []openAIInputMessage
	Input     any                      `json:"input"`
	Questions []openAIDecisionQuestion `json:"questions"`
}

type openAIInputMessage struct {
	Role    string            `json:"role"`
	Content []openAIInputPart `json:"content"`
}

type openAIInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type openAIDecisionQuestion struct {
	Type         string                 `json:"type"`
	Name         string                 `json:"name"`
	Instructions string                 `json:"instructions"`
	Choices      []openAIDecisionChoice `json:"choices,omitempty"`
	Levels       []openAIDecisionLevel  `json:"levels,omitempty"`
}

type openAIDecisionChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type openAIDecisionLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type openAIDecisionsReply struct {
	Model   string                 `json:"model"`
	Answers []openAIDecisionAnswer `json:"answers"`
	Usage   struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type openAIDecisionAnswer struct {
	Type          string  `json:"type"`
	Name          string  `json:"name"`
	Probability   float64 `json:"probability"`
	Choice        string  `json:"choice"`
	Score         float64 `json:"score"`
	Confidence    float64 `json:"confidence"`
	Probabilities []struct {
		// Value is a string or boolean for a choice and a level index for
		// a score.
		Value       json.RawMessage `json:"value"`
		Label       string          `json:"label"`
		Probability float64         `json:"probability"`
	} `json:"probabilities"`
}

// Decide implements Decider.
func (o *openAIDecisions) Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	wire, err := o.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	target, err := endpoint(o.cfg.BaseURL, "decisions")
	if err != nil {
		return nil, err
	}
	resp, err := postDecision(ctx, o.t, target, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reply openAIDecisionsReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("llm: decode openai-decisions reply: %w", err)
	}
	out := reply.toResponse(req.Questions)
	if err := out.checkAnswered(req.Questions); err != nil {
		return nil, err
	}
	return out, nil
}

func (o *openAIDecisions) buildRequest(req DecisionRequest) (openAIDecisionsRequest, error) {
	input, err := openAIInput(req.Input)
	if err != nil {
		return openAIDecisionsRequest{}, err
	}
	wire := openAIDecisionsRequest{
		Model:     firstNonEmpty(req.Model, o.cfg.Model),
		Input:     input,
		Questions: make([]openAIDecisionQuestion, len(req.Questions)),
	}
	for i, q := range req.Questions {
		wire.Questions[i] = toOpenAIDecisionQuestion(q)
	}
	return wire, nil
}

// openAIInput returns the input as a plain string when it is text only, and
// as one user message of typed parts otherwise.
func openAIInput(blocks []Block) (any, error) {
	parts := make([]openAIInputPart, 0, len(blocks))
	texts := make([]string, 0, len(blocks))
	textOnly := true
	for _, b := range blocks {
		switch b.Type {
		case BlockText:
			texts = append(texts, b.Text)
			parts = append(parts, openAIInputPart{Type: "input_text", Text: b.Text})
		case BlockImage:
			url, err := decisionImageURL(b)
			if err != nil {
				return nil, err
			}
			textOnly = false
			parts = append(parts, openAIInputPart{Type: "input_image", ImageURL: url})
		case BlockDocument:
			return nil, fmt.Errorf("llm: openai-decisions does not accept document input")
		default:
			return nil, fmt.Errorf("llm: openai-decisions does not accept %s input", b.Type)
		}
	}
	if textOnly {
		return strings.Join(texts, "\n\n"), nil
	}
	return []openAIInputMessage{{Role: "user", Content: parts}}, nil
}

func decisionImageURL(b Block) (string, error) {
	switch {
	case b.Source == nil:
		return "", fmt.Errorf("llm: openai-decisions image block has no source")
	case b.Source.Type == "base64":
		return "data:" + b.Source.MediaType + ";base64," + b.Source.Data, nil
	case b.Source.Type == "url":
		return b.Source.URL, nil
	}
	return "", fmt.Errorf("llm: openai-decisions image source type %q is not base64 or url", b.Source.Type)
}

func toOpenAIDecisionQuestion(q Question) openAIDecisionQuestion {
	wq := openAIDecisionQuestion{Name: q.Name, Instructions: q.Instructions}
	switch q.Type {
	case QuestionYesNo:
		wq.Type = openAIPredicate
	case QuestionChoice:
		wq.Type = openAIChoice
		for _, o := range q.Choices {
			wq.Choices = append(wq.Choices, openAIDecisionChoice(o))
		}
	case QuestionScore:
		wq.Type = openAIScore
		for _, o := range q.Choices {
			wq.Levels = append(wq.Levels, openAIDecisionLevel{Label: o.Value, Description: o.Description})
		}
	}
	return wq
}

// toResponse maps the reply onto the questions by name. Answers of an
// unknown type, or for unknown names, are dropped; checkAnswered then reports
// any question left without one.
func (r openAIDecisionsReply) toResponse(qs []Question) *DecisionResponse {
	out := &DecisionResponse{
		Model:   r.Model,
		Answers: make(map[string]Answer, len(qs)),
		Usage: Usage{
			Input:      r.Usage.InputTokens,
			Output:     r.Usage.OutputTokens,
			CacheRead:  r.Usage.InputTokensDetails.CachedTokens,
			CacheWrite: r.Usage.InputTokensDetails.CacheWriteTokens,
		},
	}
	byName := make(map[string]Question, len(qs))
	for _, q := range qs {
		byName[q.Name] = q
	}
	for _, a := range r.Answers {
		q, ok := byName[a.Name]
		if !ok {
			continue
		}
		switch a.Type {
		case openAIPredicate:
			out.Answers[q.Name] = Answer{Type: QuestionYesNo, Yes: a.Probability}
		case openAIChoice:
			out.Answers[q.Name] = Answer{Type: QuestionChoice, Choice: a.Choice, Probabilities: a.probabilities(false), Confidence: a.Confidence}
		case openAIScore:
			out.Answers[q.Name] = Answer{Type: QuestionScore, Score: a.Score, Probabilities: a.probabilities(true), Confidence: a.Confidence}
		case openAIRefusal:
			out.Answers[q.Name] = Answer{Type: q.Type, Refused: true}
		}
	}
	return out
}

// probabilities maps the answer's probabilities by choice value, or by label
// for scores (the request sends each level's Choice.Value as its label).
func (a openAIDecisionAnswer) probabilities(byLabel bool) map[string]float64 {
	if a.Probabilities == nil {
		return nil
	}
	m := make(map[string]float64, len(a.Probabilities))
	for _, p := range a.Probabilities {
		key := p.Label
		if !byLabel {
			key = choiceValue(p.Value)
		}
		m[key] = p.Probability
	}
	return m
}

// choiceValue returns a choice value as text: a JSON string unquoted, any
// other literal (a boolean) as written.
func choiceValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
