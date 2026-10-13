package jev

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestPromptOmitsUnsetSampleCount(t *testing.T) {
	cfg := testJevConfig(t)
	prompt := BuildDecisionPrompt(cfg, Candidate{Key: "k", Kind: KindSingle, BaseAsset: "BTC"}, nil, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), false)
	if !strings.Contains(prompt, "DecisionPromptV4") {
		t.Fatalf("prompt=%s", prompt)
	}
	if strings.Contains(prompt, `"basis_sample_count":0`) {
		t.Fatal("unset sample count must not serialize as 0")
	}
	if !strings.Contains(prompt, `"basis_sample_count":null`) {
		t.Fatalf("expected null sample count: %s", prompt)
	}
	if strings.Contains(prompt, "opportunity") {
		t.Fatal("prompt must not include opportunity block")
	}
}

func TestPromptIncludesMidBasisFields(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	count := 18
	version := BasisModelVersion
	prompt := BuildDecisionPrompt(cfg, Candidate{
		Key: "k", Kind: KindSingle, BaseAsset: "BTC",
		CurrentMidBasisBPS: decimalPtr(decimal.RequireFromString("12.5")),
		BasisSampleCount:   &count,
		BasisModelVersion:  &version,
	}, &Position{ID: 3, EntryMidBasisBPS: decimalPtr(decimal.RequireFromString("10"))}, now, false)
	if !containsAll(prompt, "current_mid_basis_bps", "entry_mid_basis_bps", "basis_change_since_entry_bps", `"basis_sample_count":18`) {
		t.Fatalf("prompt=%s", prompt)
	}
}
