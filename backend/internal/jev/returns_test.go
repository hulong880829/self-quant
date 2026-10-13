package jev

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestApplyLocalReturnsDeductsCostOnce(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.PaperRoundTripCostBPS = decimal.NewFromInt(12)
	cfg.MaxHoldDuration = 72 * time.Hour
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	candidate := Candidate{
		FundingReturn24h:        decimal.RequireFromString("0.004"),
		ExpectedBasisReturn24h:  decimalPtr(decimal.RequireFromString("0.002")),
		BasisForecastConfidence: decimalPtr(decimal.RequireFromString("0.5")),
		LongQuote:               freshQuote("binance", "BTCUSDT", ProductSpot, now, "100"),
		ShortQuote:              freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "101"),
	}
	got := ApplyLocalReturns(cfg, candidate)
	wantGross := decimal.RequireFromString("0.005")
	wantNet := decimal.RequireFromString("0.0038")
	if !got.ExpectedNetReturn24h.Equal(wantNet) {
		t.Fatalf("net=%s want=%s", got.ExpectedNetReturn24h, wantNet)
	}
	wantHorizon := decimal.RequireFromString("0.004").Mul(decimal.NewFromInt(72)).Div(decimal.NewFromInt(24)).
		Add(decimal.RequireFromString("0.001")).Sub(decimal.RequireFromString("0.0012"))
	if !got.HoldingHorizonNetReturn.Equal(wantHorizon) {
		t.Fatalf("horizon=%s want=%s", got.HoldingHorizonNetReturn, wantHorizon)
	}
	if got.ExpectedPaybackMinutes == nil {
		t.Fatal("payback missing")
	}
	wantPayback := decimal.NewFromInt(24 * 60).Mul(decimal.RequireFromString("0.0012")).Div(wantGross)
	if !got.ExpectedPaybackMinutes.Equal(wantPayback) {
		t.Fatalf("payback=%s want=%s", got.ExpectedPaybackMinutes, wantPayback)
	}
}

func TestBasisContributionDoesNotScaleNegative(t *testing.T) {
	if !BasisContribution(decimal.RequireFromString("-0.01"), decimal.RequireFromString("0.2")).Equal(decimal.RequireFromString("-0.01")) {
		t.Fatal("negative basis must not be weakened")
	}
	if !BasisContribution(decimal.RequireFromString("0.01"), decimal.RequireFromString("0.2")).Equal(decimal.RequireFromString("0.002")) {
		t.Fatal("positive basis must shrink by confidence")
	}
}

func TestEntryFilterRejectsMissingOrExpiredForecast(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	candidate := ApplyLocalReturns(cfg, Candidate{
		FundingReturn24h:          decimal.RequireFromString("0.004"),
		ExpectedBasisReturn24h:    decimalPtr(decimal.RequireFromString("0.002")),
		BasisForecastConfidence:   decimalPtr(decimal.NewFromInt(1)),
		BasisSampleCount:          intPtr(20),
		BasisForecastCalculatedAt: timePtr(now.Add(-30 * time.Minute)),
		LongQuote:                 freshQuote("binance", "BTCUSDT", ProductSpot, now, "100"),
		ShortQuote:                freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "101"),
	})
	if EntryFilterOK(cfg, candidate, 30*time.Minute) {
		t.Fatal("stale forecast must not enter")
	}
	candidate.BasisForecastCalculatedAt = timePtr(now)
	if !EntryFilterOK(cfg, candidate, time.Minute) {
		t.Fatal("fresh positive forecast must enter")
	}
}

func TestTwentyFourHourBasisIsNotTripled(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MaxHoldDuration = 72 * time.Hour
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	got := ApplyLocalReturns(cfg, Candidate{
		FundingReturn24h:        decimal.Zero,
		ExpectedBasisReturn24h:  decimalPtr(decimal.RequireFromString("0.003")),
		BasisForecastConfidence: decimalPtr(decimal.NewFromInt(1)),
		LongQuote:               freshQuote("binance", "BTCUSDT", ProductSpot, now, "100"),
		ShortQuote:              freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "101"),
	})
	want := decimal.RequireFromString("0.003").Sub(decimal.RequireFromString("0.0012"))
	if !got.ExpectedNetReturn24h.Equal(want) || !got.HoldingHorizonNetReturn.Equal(want) {
		t.Fatalf("net=%s horizon=%s", got.ExpectedNetReturn24h, got.HoldingHorizonNetReturn)
	}
}
