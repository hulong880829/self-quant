package jev

import (
	"sort"
	"strings"
	"time"

	"selfquant/backend/internal/config"
)

func RankEnterDecisions(items []Decision) []Decision {
	ranked := append([]Decision(nil), items...)
	sort.SliceStable(ranked, func(i, j int) bool {
		ni := decimalOrZero(ranked[i].ExpectedNetReturn24h)
		nj := decimalOrZero(ranked[j].ExpectedNetReturn24h)
		if !ni.Equal(nj) {
			return ni.GreaterThan(nj)
		}
		ci := decimalOrZero(ranked[i].BasisForecastConfidence)
		cj := decimalOrZero(ranked[j].BasisForecastConfidence)
		if !ci.Equal(cj) {
			return ci.GreaterThan(cj)
		}
		pi := decimalOrZero(ranked[i].BasisReturnP10)
		pj := decimalOrZero(ranked[j].BasisReturnP10)
		if !pi.Equal(pj) {
			return pi.GreaterThan(pj)
		}
		if !ranked[i].EnterProbability.Equal(ranked[j].EnterProbability) {
			return ranked[i].EnterProbability.GreaterThan(ranked[j].EnterProbability)
		}
		if !ranked[i].Confidence.Equal(ranked[j].Confidence) {
			return ranked[i].Confidence.GreaterThan(ranked[j].Confidence)
		}
		return ranked[i].CandidateKey < ranked[j].CandidateKey
	})
	return ranked
}

func AllocateEnters(cfg config.Jev, open []Position, enters []Decision, justExitedBases map[string]struct{}) []Decision {
	openBases := map[string]struct{}{}
	openCount := 0
	for _, position := range open {
		if position.Status != PositionOpen {
			continue
		}
		openCount++
		openBases[strings.ToUpper(position.BaseAsset)] = struct{}{}
	}
	ranked := RankEnterDecisions(enters)
	reserved := map[string]struct{}{}
	out := make([]Decision, 0, len(ranked))
	for _, decision := range ranked {
		if decision.PolicyStatus == PolicyRejected {
			out = append(out, decision)
			continue
		}
		base := strings.ToUpper(strings.TrimSpace(decision.BaseAsset))
		reject := decision.Decision != DecisionEnter
		if !reject {
			if base == "" {
				reject = true
			}
			if _, exists := openBases[base]; exists {
				reject = true
			}
			if _, exists := justExitedBases[base]; exists {
				reject = true
			}
			if _, exists := reserved[base]; exists {
				reject = true
			}
			if openCount >= cfg.MaxOpenPositions {
				reject = true
			}
		}
		if reject {
			decision.PolicyStatus = PolicySuggested
			decision.ExecutionStatus = ExecPortfolioRej
			out = append(out, decision)
			continue
		}
		decision.PolicyStatus = PolicyAccepted
		decision.ExecutionStatus = ExecPending
		reserved[base] = struct{}{}
		openCount++
		out = append(out, decision)
	}
	return out
}

func ShouldForceExit(cfg config.Jev, position Position, now time.Time) bool {
	if position.Status != PositionOpen || position.OpenedAt.IsZero() {
		return false
	}
	return now.Sub(position.OpenedAt) >= cfg.MaxHoldDuration
}

func EnterRejectReason(cfg config.Jev, answer ModelAnswer) string {
	if answer.ExpectedNetReturn24h == nil || !answer.ExpectedNetReturn24h.IsPositive() ||
		answer.ExpectedNetReturn24h.LessThan(cfg.MinExpectedNetReturn24h) {
		return "expected_net_return_below_minimum"
	}
	if answer.EnterProbability.LessThan(cfg.MinEnterProbability) {
		return "enter_probability_below_minimum"
	}
	if answer.Confidence.LessThan(cfg.MinEnterConfidence) {
		return "enter_confidence_below_minimum"
	}
	return ""
}

func ShouldHonorEnter(cfg config.Jev, answer ModelAnswer) bool {
	return answer.Decision == DecisionEnter && EnterRejectReason(cfg, answer) == ""
}

func ShouldHonorExit(cfg config.Jev, answer ModelAnswer) bool {
	return answer.Decision == DecisionExit && !answer.ExitProbability.LessThan(cfg.MinExitProbability)
}
