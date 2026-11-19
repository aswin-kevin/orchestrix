package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func init() {
	RegisterDecider(FormatSystemOne, func(cfg Config) (Decider, error) {
		d, err := newSystemOne(cfg)
		if err != nil {
			return nil, err
		}
		return d, nil
	})
}

// System One wire type names.
const (
	systemOneYesNo  = "noul"
	systemOneChoice = "choice"
	systemOneScore  = "score"
)

// systemOne speaks the System One decision API.
type systemOne struct {
	cfg *Config
}

// newSystemOne returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newSystemOne(cfg Config) (*systemOne, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("llm: systemone needs a BaseURL (or SYSTEMONE_BASE_URL)")
	}
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	return &systemOne{cfg: &cfg}, nil
}

// Wire types.

type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     string                       `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type systemOneReply struct {
	Model   string                     `json:"model"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type systemOneAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Decide implements Decider.
func (s *systemOne) Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	wire, err := s.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	resp, err := postDecision(ctx, s.cfg, s.url(), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reply systemOneReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("llm: decode systemone reply: %w", err)
	}
	out, err := reply.toResponse(req.Questions)
	if err != nil {
		return nil, err
	}
	if err := out.checkAnswered(req.Questions); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *systemOne) url() string {
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/systemone"
	}
	return base + "/v1/systemone"
}

func (s *systemOne) buildRequest(req DecisionRequest) (systemOneRequest, error) {
	texts := make([]string, 0, len(req.Input))
	for _, b := range req.Input {
		if b.Type != BlockText {
			return systemOneRequest{}, fmt.Errorf("llm: systemone accepts text input only (got %s)", b.Type)
		}
		texts = append(texts, b.Text)
	}
	wire := systemOneRequest{
		Model:     firstNonEmpty(req.Model, s.cfg.Model),
		State:     strings.Join(texts, "\n\n"),
		Questions: make(map[string]systemOneQuestion, len(req.Questions)),
	}
	for _, q := range req.Questions {
		wire.Questions[q.Name] = toSystemOneQuestion(q)
	}
	return wire, nil
}

func toSystemOneQuestion(q Question) systemOneQuestion {
	wq := systemOneQuestion{Instructions: q.Instructions}
	switch q.Type {
	case YesNo:
		wq.Type = systemOneYesNo
	case Choice:
		wq.Type = systemOneChoice
		// An option without a description is sent as null (a bare label).
		criteria := make(map[string]*string, len(q.Options))
		for _, o := range q.Options {
			if o.Description != "" {
				criteria[o.Value] = &o.Description
			} else {
				criteria[o.Value] = nil
			}
		}
		wq.Criteria = criteria
	case Score:
		wq.Type = systemOneScore
		// Levels are plain strings, lowest first; answers refer to them by
		// index, which toResponse maps back to Option.Value.
		levels := make([]string, len(q.Options))
		for i, o := range q.Options {
			levels[i] = o.Value
			if o.Description != "" {
				levels[i] += ": " + o.Description
			}
		}
		wq.Criteria = levels
	}
	return wq
}

// toResponse maps the reply onto the questions. Answers of an unknown type
// are dropped, which checkAnswered then reports as missing.
func (r systemOneReply) toResponse(qs []Question) (*DecisionResponse, error) {
	out := &DecisionResponse{
		Model:   r.Model,
		Answers: make(map[string]Answer, len(qs)),
		Usage:   Usage{Input: r.Usage.InputTokens, Output: r.Usage.OutputTokens},
	}
	for _, q := range qs {
		a, ok := r.Answers[q.Name]
		if !ok {
			continue
		}
		switch a.Type {
		case systemOneYesNo:
			out.Answers[q.Name] = Answer{Type: YesNo, Yes: a.Noul}
		case systemOneChoice:
			out.Answers[q.Name] = Answer{Type: Choice, Choice: a.Choice, Probabilities: a.Probabilities, Confidence: a.Confidence}
		case systemOneScore:
			probs, err := rekeyLevels(q, a.Probabilities)
			if err != nil {
				return nil, err
			}
			out.Answers[q.Name] = Answer{Type: Score, Score: a.Score, Probabilities: probs, Confidence: a.Confidence}
		}
	}
	return out, nil
}

// rekeyLevels replaces the level indexes System One keys score
// probabilities by with the question's option values.
func rekeyLevels(q Question, probs map[string]float64) (map[string]float64, error) {
	if probs == nil {
		return nil, nil
	}
	out := make(map[string]float64, len(probs))
	for k, p := range probs {
		i, err := strconv.Atoi(k)
		if err != nil || i < 0 || i >= len(q.Options) {
			return nil, fmt.Errorf("llm: systemone score key %q for question %q is not a level index", k, q.Name)
		}
		out[q.Options[i].Value] = p
	}
	return out, nil
}
