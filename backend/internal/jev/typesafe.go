package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

const (
	decisionQuestionInstructions = "Choose the current paper-trading arb action."
	decisionHoldCriteria         = "Keep the current state when evidence is insufficient or uncertain."
	decisionEnterCriteria        = "Enter only with no open position, fresh data, and positive expected net return."
	decisionExitCriteria         = "Exit when the edge disappears, reverses, or an exit condition is reached."

	probabilitySumTolerance = "0.000001"
)

type DecisionClient interface {
	Decide(context.Context, DecisionRequest) (ModelAnswer, error)
}

type DecisionRequest struct {
	Prompt     string
	AllowEnter bool
	ForceExit  bool
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type TypeSafeClient struct {
	baseURL                 string
	apiKey                  string
	model                   string
	httpClient              HTTPDoer
	abort401                atomic.Bool
	probabilitySumTolerance decimal.Decimal
}

func NewTypeSafeClient(cfg config.Jev, client HTTPDoer) *TypeSafeClient {
	if client == nil {
		client = &http.Client{Timeout: cfg.TypeSafeTimeout}
	}
	tolerance := cfg.ProbabilitySumTolerance
	if !tolerance.IsPositive() {
		tolerance = decimal.RequireFromString("0.02")
	}
	return &TypeSafeClient{
		baseURL:                 strings.TrimRight(strings.TrimSpace(cfg.TypeSafeBaseURL), "/"),
		apiKey:                  strings.TrimSpace(cfg.TypeSafeAPIKey),
		model:                   strings.TrimSpace(cfg.TypeSafeModel),
		httpClient:              client,
		probabilitySumTolerance: tolerance,
	}
}

func (c *TypeSafeClient) ResetCycle() {
	if c != nil {
		c.abort401.Store(false)
	}
}

func (c *TypeSafeClient) Aborted() bool {
	return c != nil && c.abort401.Load()
}

func (c *TypeSafeClient) Decide(ctx context.Context, request DecisionRequest) (ModelAnswer, error) {
	if c == nil || c.apiKey == "" {
		return ModelAnswer{}, fmt.Errorf("%w: typesafe api key missing", ErrUnavailable)
	}
	if c.abort401.Load() {
		return ModelAnswer{}, ErrUnauthorized
	}
	payload := map[string]any{
		"state": request.Prompt,
		"model": c.model,
		"questions": map[string]any{
			"decision": map[string]any{
				"type":         "choice",
				"instructions": decisionQuestionInstructions,
				"criteria": map[string]string{
					DecisionHold:  decisionHoldCriteria,
					DecisionEnter: decisionEnterCriteria,
					DecisionExit:  decisionExitCriteria,
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ModelAnswer{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return ModelAnswer{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return ModelAnswer{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return ModelAnswer{}, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		c.abort401.Store(true)
		return ModelAnswer{}, ErrUnauthorized
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ModelAnswer{}, fmt.Errorf("typesafe status %d", response.StatusCode)
	}
	answer, err := parseTypeSafeResponse(raw, c.model, c.probabilitySumTolerance)
	if err != nil {
		return ModelAnswer{Raw: FailureRawResponse("parse_error", excerptBytes(raw)), ParseErrorType: "parse_error"}, err
	}
	if request.ForceExit && answer.Decision != DecisionExit {
		return ModelAnswer{}, fmt.Errorf("force exit required")
	}
	if !request.AllowEnter && answer.Decision == DecisionEnter {
		answer.Decision = DecisionHold
	}
	return answer, nil
}

type typeSafeEnvelope struct {
	Model   string          `json:"model"`
	Answers json.RawMessage `json:"answers"`
}

type typeSafeAnswers struct {
	Decision *typeSafeChoiceAnswer `json:"decision"`
}

type typeSafeChoiceAnswer struct {
	Type          string          `json:"type"`
	Choice        string          `json:"choice"`
	Probabilities json.RawMessage `json:"probabilities"`
	Confidence    json.RawMessage `json:"confidence"`
}

type typeSafeProbabilities struct {
	Hold  json.RawMessage `json:"hold"`
	Enter json.RawMessage `json:"enter"`
	Exit  json.RawMessage `json:"exit"`
}

func ParseTypeSafeResponse(raw []byte, fallbackModel string) (ModelAnswer, error) {
	return parseTypeSafeResponse(raw, fallbackModel, decimal.RequireFromString("0.02"))
}

func parseTypeSafeResponse(raw []byte, fallbackModel string, tolerance decimal.Decimal) (ModelAnswer, error) {
	var envelope typeSafeEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ModelAnswer{}, fmt.Errorf("decode typesafe envelope: %w", err)
	}
	if len(envelope.Answers) == 0 || string(envelope.Answers) == "null" {
		return ModelAnswer{}, fmt.Errorf("typesafe answers missing")
	}
	var answers typeSafeAnswers
	if err := json.Unmarshal(envelope.Answers, &answers); err != nil {
		return ModelAnswer{}, fmt.Errorf("decode typesafe answers: %w", err)
	}
	if answers.Decision == nil {
		return ModelAnswer{}, fmt.Errorf("answers.decision missing")
	}
	if strings.TrimSpace(answers.Decision.Type) != "choice" {
		return ModelAnswer{}, fmt.Errorf("answers.decision.type must be choice")
	}
	decision := strings.ToLower(strings.TrimSpace(answers.Decision.Choice))
	if decision != DecisionHold && decision != DecisionEnter && decision != DecisionExit {
		return ModelAnswer{}, fmt.Errorf("answers.decision.choice must be hold, enter, or exit")
	}
	if len(answers.Decision.Probabilities) == 0 || string(answers.Decision.Probabilities) == "null" {
		return ModelAnswer{}, fmt.Errorf("answers.decision.probabilities missing")
	}
	var probabilities typeSafeProbabilities
	if err := json.Unmarshal(answers.Decision.Probabilities, &probabilities); err != nil {
		return ModelAnswer{}, fmt.Errorf("decode answers.decision.probabilities: %w", err)
	}
	holdProb, err := parseUnitInterval(probabilities.Hold, "answers.decision.probabilities.hold")
	if err != nil {
		return ModelAnswer{}, err
	}
	enterProb, err := parseUnitInterval(probabilities.Enter, "answers.decision.probabilities.enter")
	if err != nil {
		return ModelAnswer{}, err
	}
	exitProb, err := parseUnitInterval(probabilities.Exit, "answers.decision.probabilities.exit")
	if err != nil {
		return ModelAnswer{}, err
	}
	holdProb, enterProb, exitProb, err = normalizeProbabilities(holdProb, enterProb, exitProb, tolerance)
	if err != nil {
		return ModelAnswer{}, err
	}
	confidence, err := parseUnitInterval(answers.Decision.Confidence, "answers.decision.confidence")
	if err != nil {
		return ModelAnswer{}, err
	}
	model := strings.TrimSpace(envelope.Model)
	if model == "" {
		model = fallbackModel
	}
	return ModelAnswer{
		Decision:         decision,
		EnterProbability: enterProb,
		ExitProbability:  exitProb,
		Confidence:       confidence,
		ModelActual:      model,
		Raw:              append([]byte(nil), raw...),
	}, nil
}

func parseRequiredDecimal(raw json.RawMessage, field string) (decimal.Decimal, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return decimal.Zero, fmt.Errorf("%s is required", field)
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		parsed, err := decimal.NewFromString(asNumber.String())
		if err != nil {
			return decimal.Zero, fmt.Errorf("%s is invalid", field)
		}
		return parsed, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return decimal.Zero, fmt.Errorf("%s is invalid", field)
	}
	parsed, err := decimal.NewFromString(strings.TrimSpace(asString))
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s is invalid", field)
	}
	return parsed, nil
}

func parseUnitInterval(raw json.RawMessage, field string) (decimal.Decimal, error) {
	parsed, err := parseRequiredDecimal(raw, field)
	if err != nil {
		return decimal.Zero, err
	}
	if parsed.IsNegative() || parsed.GreaterThan(decimal.NewFromInt(1)) {
		return decimal.Zero, fmt.Errorf("%s must be in [0,1]", field)
	}
	return parsed, nil
}

func normalizeProbabilities(hold, enter, exit, tolerance decimal.Decimal) (decimal.Decimal, decimal.Decimal, decimal.Decimal, error) {
	if !tolerance.IsPositive() {
		tolerance = decimal.RequireFromString(probabilitySumTolerance)
	}
	sum := hold.Add(enter).Add(exit)
	diff := sum.Sub(decimal.NewFromInt(1)).Abs()
	if diff.GreaterThan(tolerance) {
		return decimal.Zero, decimal.Zero, decimal.Zero, fmt.Errorf("answers.decision.probabilities must sum to 1")
	}
	if !sum.IsPositive() {
		return decimal.Zero, decimal.Zero, decimal.Zero, fmt.Errorf("answers.decision.probabilities must sum to 1")
	}
	return hold.Div(sum), enter.Div(sum), exit.Div(sum), nil
}

func FailureRawResponse(parseErrorType, excerpt string) []byte {
	payload := map[string]any{
		"truncated":        true,
		"parse_error_type": parseErrorType,
		"excerpt":          excerpt,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"truncated":true,"parse_error_type":"marshal_failed","excerpt":""}`)
	}
	return raw
}

func SanitizeJSONBytes(raw []byte) []byte {
	if json.Valid(raw) {
		return append([]byte(nil), raw...)
	}
	return FailureRawResponse("invalid_json", excerptBytes(raw))
}

func excerptBytes(raw []byte) string {
	const max = 512
	text := strings.ToValidUTF8(string(raw), "")
	if len(text) > max {
		text = text[:max]
	}
	return text
}

func localExpectedNetReturn24h(candidate Candidate, costBPS decimal.Decimal) decimal.Decimal {
	return candidate.ExpectedNetReturn24h
}

func formatDecisionRationale(answer ModelAnswer) string {
	return fmt.Sprintf(
		"effective_choice=%s p_enter=%s p_exit=%s confidence=%s",
		answer.Decision,
		answer.EnterProbability.String(),
		answer.ExitProbability.String(),
		answer.Confidence.String(),
	)
}
