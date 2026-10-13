package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func officialChoiceResponse(model, choice string) []byte {
	return []byte(`{
		"model": "` + model + `",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "` + choice + `",
				"probabilities": {
					"hold": 0.1,
					"enter": 0.85,
					"exit": 0.05
				},
				"confidence": 0.8
			}
		}
	}`)
}

func TestParseTypeSafeResponseReadsChoiceAnswer(t *testing.T) {
	raw := officialChoiceResponse("jev-1.13.0", "enter")
	answer, err := ParseTypeSafeResponse(raw, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if answer.Decision != DecisionEnter || answer.ModelActual != "jev-1.13.0" {
		t.Fatalf("%+v", answer)
	}
	if !answer.EnterProbability.Equal(decimal.RequireFromString("0.85")) {
		t.Fatalf("enter=%s", answer.EnterProbability)
	}
	if !answer.ExitProbability.Equal(decimal.RequireFromString("0.05")) {
		t.Fatalf("exit=%s", answer.ExitProbability)
	}
	if !answer.Confidence.Equal(decimal.RequireFromString("0.8")) {
		t.Fatalf("confidence=%s", answer.Confidence)
	}
	if answer.ExpectedNetReturn24h != nil || answer.Rationale != "" {
		t.Fatalf("local fields must stay empty after parse: %+v", answer)
	}
}

func TestParseTypeSafeResponseRejectsRootChoice(t *testing.T) {
	raw := []byte(`{"model":"x","choice":"enter","decision":"enter"}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err == nil {
		t.Fatal("root decision must not be accepted")
	}
}

func TestParseTypeSafeResponseRejectsInvalidChoice(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "scale",
				"probabilities": {"hold": 0.1, "enter": 0.85, "exit": 0.05},
				"confidence": 0.8
			}
		}
	}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err == nil {
		t.Fatal("invalid choice was accepted")
	}
}

func TestParseTypeSafeResponseRejectsWrongType(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "noul",
				"choice": "enter",
				"probabilities": {"hold": 0.1, "enter": 0.85, "exit": 0.05},
				"confidence": 0.8
			}
		}
	}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err == nil {
		t.Fatal("non-choice type was accepted")
	}
}

func TestParseTypeSafeResponseRejectsMissingProbability(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "enter",
				"probabilities": {"hold": 0.1, "enter": 0.9},
				"confidence": 0.8
			}
		}
	}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err == nil {
		t.Fatal("missing exit probability was accepted")
	}
}

func TestParseTypeSafeResponseRejectsOutOfRangeProbability(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "enter",
				"probabilities": {"hold": 0.1, "enter": 1.2, "exit": -0.3},
				"confidence": 0.8
			}
		}
	}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err == nil {
		t.Fatal("out-of-range probability was accepted")
	}
}

func TestParseTypeSafeResponseRejectsProbabilitySum(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "enter",
				"probabilities": {"hold": 0.2, "enter": 0.85, "exit": 0.05},
				"confidence": 0.8
			}
		}
	}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err == nil {
		t.Fatal("probability sum outside tolerance was accepted")
	}
}

func TestParseTypeSafeResponseAcceptsProbabilitySumWithinTolerance(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "hold",
				"probabilities": {"hold": 0.333333, "enter": 0.333333, "exit": 0.333334},
				"confidence": 0.5
			}
		}
	}`)
	if _, err := ParseTypeSafeResponse(raw, "x"); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) Do(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestTypeSafeClientSendsOfficialPayload(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.TypeSafeModel = "jev-latest"
	var captured map[string]any
	client := NewTypeSafeClient(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &captured); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(officialChoiceResponse("jev-1.13.0", "hold")))),
			Header:     make(http.Header),
		}, nil
	}))
	if _, err := client.Decide(context.Background(), DecisionRequest{Prompt: "state-body"}); err != nil {
		t.Fatal(err)
	}
	if captured["state"] != "state-body" || captured["model"] != "jev-latest" {
		t.Fatalf("payload=%v", captured)
	}
	questions, ok := captured["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions must be an object: %T", captured["questions"])
	}
	decision, ok := questions["decision"].(map[string]any)
	if !ok {
		t.Fatalf("questions.decision missing: %v", questions)
	}
	if decision["type"] != "choice" || decision["instructions"] != decisionQuestionInstructions {
		t.Fatalf("decision=%v", decision)
	}
	criteria, ok := decision["criteria"].(map[string]any)
	if !ok {
		t.Fatalf("criteria=%v", decision["criteria"])
	}
	if criteria["hold"] != decisionHoldCriteria ||
		criteria["enter"] != decisionEnterCriteria ||
		criteria["exit"] != decisionExitCriteria {
		t.Fatalf("criteria=%v", criteria)
	}
}

func TestTypeSafeClientStopsAfter401(t *testing.T) {
	calls := 0
	client := NewTypeSafeClient(testJevConfig(t), roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
			Header:     make(http.Header),
		}, nil
	}))
	if _, err := client.Decide(context.Background(), DecisionRequest{Prompt: "p"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err=%v", err)
	}
	if _, err := client.Decide(context.Background(), DecisionRequest{Prompt: "p"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("second err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestLocalExpectedNetReturnUsesCandidateField(t *testing.T) {
	got := localExpectedNetReturn24h(Candidate{
		ExpectedNetReturn24h: decimal.RequireFromString("0.0042"),
		FundingReturn24h:     decimal.RequireFromString("0.002"),
	}, decimal.NewFromInt(12))
	if !got.Equal(decimal.RequireFromString("0.0042")) {
		t.Fatalf("got=%s", got)
	}
}

func TestFormatDecisionRationaleKeepsSmallProbabilities(t *testing.T) {
	got := formatDecisionRationale(ModelAnswer{
		Decision:         DecisionHold,
		EnterProbability: decimal.RequireFromString("0.004"),
		ExitProbability:  decimal.RequireFromString("0.05"),
		Confidence:       decimal.RequireFromString("0.8"),
	})
	if !strings.HasPrefix(got, "effective_choice=hold ") {
		t.Fatalf("rationale=%s", got)
	}
	if !strings.Contains(got, "p_enter=0.004") || strings.Contains(got, "p_enter=0.00 ") {
		t.Fatalf("small probability was truncated: %s", got)
	}
}
