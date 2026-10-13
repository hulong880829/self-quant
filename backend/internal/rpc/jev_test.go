package rpc

import (
	"testing"

	d "github.com/shopspring/decimal"
	"selfquant/backend/internal/jev"
)

func TestDecisionProtoLeavesHistoricalMetricsUnset(t *testing.T) {
	item := jev.Decision{
		ID: 7, CandidateKey: "single:binance:BTCUSDT:BTCUSDT", Kind: jev.KindSingle,
		Decision: jev.DecisionHold, PolicyStatus: jev.PolicyAccepted, ExecutionStatus: jev.ExecNotApplicable,
	}
	got := decisionProto(item)
	if got.GetExpectedNetReturn_24H() != "" || got.GetBasisModelVersion() != "" {
		t.Fatalf("historical amounts must be empty: %+v", got)
	}
	if got.BasisSampleCount != nil || got.GetBasisSampleCount() != 0 {
		t.Fatalf("unset sample count must stay unset, ptr=%v value=%d", got.BasisSampleCount, got.GetBasisSampleCount())
	}
}

func TestDecisionProtoSetsOptionalSampleCount(t *testing.T) {
	count := 18
	version := jev.BasisModelVersion
	net := d.RequireFromString("0.002")
	item := jev.Decision{
		ID: 8, ExpectedNetReturn24h: &net, BasisSampleCount: &count, BasisModelVersion: &version,
	}
	got := decisionProto(item)
	if got.BasisSampleCount == nil || got.GetBasisSampleCount() != 18 {
		t.Fatalf("sample count %+v", got.BasisSampleCount)
	}
	if got.GetBasisModelVersion() != jev.BasisModelVersion || got.GetExpectedNetReturn_24H() != "0.002" {
		t.Fatalf("%+v", got)
	}
}

func TestTradeProtoJoinsTwoLegs(t *testing.T) {
	item := jev.Trade{
		ID: 3, PositionID: 9, Side: jev.DecisionEnter, StrategyVersion: jev.StrategyVersion,
		BaseAsset: "BTC", Kind: jev.KindSingle, LongExchange: "binance", LongSymbol: "BTCUSDT", LongProduct: jev.ProductSpot,
		ShortExchange: "okx", ShortSymbol: "BTCUSDT", ShortProduct: jev.ProductPerpetual,
		LongFillPrice: d.RequireFromString("100"), ShortFillPrice: d.RequireFromString("101"),
	}
	got := tradeProto(item)
	if got.GetStrategyVersion() != jev.StrategyVersion || got.GetBaseAsset() != "BTC" ||
		got.GetLongExchange() != "binance" || got.GetShortProduct() != jev.ProductPerpetual {
		t.Fatalf("%+v", got)
	}
}

func TestFilterStatsIncludeBasis(t *testing.T) {
	single := singleFilterProto(jev.SingleFilterStats{Input: 10, ContractPassed: 8, BasisForecastPassed: 3, Final: 2})
	cross := crossFilterProto(jev.CrossFilterStats{Input: 9, ContractPassed: 7, BasisForecastPassed: 1, Final: 1})
	if single.GetBasisForecastPassed() != 3 || cross.GetBasisForecastPassed() != 1 {
		t.Fatalf("single=%+v cross=%+v", single, cross)
	}
}
