package jev

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestEnterFillIsDeltaNeutralAndCostsHalfRoundTrip(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	long := Quote{
		Key: NormalizeMarketKey("binance", "BTCUSDT", ProductSpot),
		Bid: decimal.RequireFromString("49.5"), Ask: decimal.NewFromInt(50),
		BidQty: decimal.NewFromInt(10), AskQty: decimal.NewFromInt(10), TS: now, Available: true,
	}
	short := Quote{
		Key: NormalizeMarketKey("okx", "BTCUSDT", ProductPerpetual),
		Bid: decimal.NewFromInt(40), Ask: decimal.RequireFromString("40.5"),
		BidQty: decimal.NewFromInt(10), AskQty: decimal.NewFromInt(10), TS: now, Available: true,
	}
	fill, err := EnterFill(cfg, now, long, short)
	if err != nil {
		t.Fatal(err)
	}
	if !fill.Qty.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("qty=%s", fill.Qty)
	}
	position := ApplyEnter(1, Candidate{Kind: KindCross, BaseAsset: "BTC"}, fill, now, long, short)
	if position.EntryMidBasisBPS == nil {
		t.Fatal("entry mid basis must be stored")
	}
	wantMid := decimal.RequireFromString("40.25").Div(decimal.RequireFromString("49.75")).Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(10000))
	if !position.EntryMidBasisBPS.Equal(wantMid) {
		t.Fatalf("entry mid=%s want=%s", position.EntryMidBasisBPS, wantMid)
	}
	if !position.LongQty.Add(position.ShortQty).IsZero() || !position.ShortQty.IsNegative() {
		t.Fatalf("qty long=%s short=%s", position.LongQty, position.ShortQty)
	}
	wantCost := HalfRoundTripCost(decimal.NewFromInt(100), decimal.NewFromInt(12))
	if !fill.Cost.Equal(wantCost) {
		t.Fatalf("cost=%s want=%s", fill.Cost, wantCost)
	}
}

func TestAtomicFillRequiresBothLegs(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Now().UTC()
	long := freshQuote("binance", "BTCUSDT", ProductSpot, now, "50")
	short := freshQuote("okx", "BTCUSDT", ProductPerpetual, now.Add(-2*time.Minute), "40")
	if _, err := EnterFill(cfg, now, long, short); err == nil {
		t.Fatal("stale short leg must block both fills")
	}
}

func TestFundingSettlementRequiresBBOWindow(t *testing.T) {
	settled := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	okTS := settled.Add(-30 * time.Second)
	if !SettlementMidValid(settled, okTS, time.Minute) {
		t.Fatal("expected valid window")
	}
	if SettlementMidValid(settled, settled.Add(time.Second), time.Minute) {
		t.Fatal("future bbo must be rejected")
	}
	if SettlementMidValid(settled, settled.Add(-2*time.Minute), time.Minute) {
		t.Fatal("stale bbo must be rejected")
	}
}

func TestFundingPnLShortReceivesPositiveRate(t *testing.T) {
	qty := decimal.NewFromInt(2).Neg()
	got := FundingPnLDelta(qty, decimal.NewFromInt(100), decimal.RequireFromString("0.001"))
	if !got.Equal(decimal.RequireFromString("0.2")) {
		t.Fatalf("pnl=%s", got)
	}
}

func TestApplyEnterSetsPerpWatermarkToOpenedAt(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	long := freshQuote("binance", "BTCUSDT", ProductSpot, now, "50")
	short := freshQuote("binance", "BTCUSDT", ProductPerpetual, now, "50")
	fill, err := EnterFill(cfg, now, long, short)
	if err != nil {
		t.Fatal(err)
	}
	single := ApplyEnter(1, Candidate{
		Kind: KindSingle, BaseAsset: "BTC", LongProduct: ProductSpot, ShortProduct: ProductPerpetual,
	}, fill, now, long, short)
	if single.FundingCheckedThroughLong != nil {
		t.Fatalf("spot long watermark=%v", single.FundingCheckedThroughLong)
	}
	if single.FundingCheckedThroughShort == nil || !single.FundingCheckedThroughShort.Equal(now) {
		t.Fatalf("short watermark=%v", single.FundingCheckedThroughShort)
	}
	cross := ApplyEnter(1, Candidate{
		Kind: KindCross, BaseAsset: "ETH", LongProduct: ProductPerpetual, ShortProduct: ProductPerpetual,
	}, fill, now, long, short)
	if cross.FundingCheckedThroughLong == nil || !cross.FundingCheckedThroughLong.Equal(now) {
		t.Fatalf("cross long watermark=%v", cross.FundingCheckedThroughLong)
	}
	if cross.FundingCheckedThroughShort == nil || !cross.FundingCheckedThroughShort.Equal(now) {
		t.Fatalf("cross short watermark=%v", cross.FundingCheckedThroughShort)
	}
}

func TestHistoryTruncatedUsesBoundaryAndLengthFloor(t *testing.T) {
	opened := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	old := make([]HistoryPoint, HistoryLimit)
	for i := range old {
		old[i] = HistoryPoint{SettledAt: opened.Add(-time.Duration(HistoryLimit-i) * time.Hour)}
	}
	if HistoryTruncated(nil, opened, old, HistoryLimit) {
		t.Fatal("100 points at or before opened must not truncate")
	}
	eq := make([]HistoryPoint, HistoryLimit)
	for i := range eq {
		eq[i] = HistoryPoint{SettledAt: opened.Add(time.Duration(i) * time.Minute)}
	}
	if HistoryTruncated(nil, opened, eq, HistoryLimit) {
		t.Fatal("oldest equal to boundary must be complete")
	}
	watermark := opened.Add(time.Hour)
	if HistoryTruncated(&watermark, opened, []HistoryPoint{{SettledAt: watermark.Add(-time.Second)}}, HistoryLimit) {
		t.Fatal("short page must not truncate")
	}
	gap := make([]HistoryPoint, HistoryLimit)
	for i := range gap {
		gap[i] = HistoryPoint{SettledAt: opened.Add(time.Hour + time.Duration(i)*time.Minute)}
	}
	if !HistoryTruncated(nil, opened, gap, HistoryLimit) {
		t.Fatal("oldest after boundary at limit must truncate")
	}
	over := append(gap, HistoryPoint{SettledAt: opened.Add(2 * time.Hour)})
	if !HistoryTruncated(nil, opened, over, HistoryLimit) {
		t.Fatal("len > limit with gap must truncate")
	}
}

func TestRecomputeRunTotalsIncludesArchivedBaseline(t *testing.T) {
	run := Run{
		InitialEquity: decimal.NewFromInt(10000), PeakEquity: decimal.NewFromInt(10000),
		ArchivedRealizedSpreadPnL: decimal.NewFromInt(5), ArchivedFundingPnL: decimal.NewFromInt(2),
		ArchivedCostPaid: decimal.NewFromInt(1),
	}
	got := RecomputeRunTotals(run, []Position{{
		RealizedSpreadPnL: decimal.NewFromInt(3), FundingPnL: decimal.NewFromInt(1), CostPaid: decimal.NewFromInt(1),
	}})
	if !got.RealizedSpreadPnL.Equal(decimal.NewFromInt(8)) || !got.FundingPnL.Equal(decimal.NewFromInt(3)) ||
		!got.CumulativeCost.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("%+v", got)
	}
	if !got.NetPnL.Equal(decimal.NewFromInt(9)) || !got.Equity.Equal(decimal.NewFromInt(10009)) {
		t.Fatalf("net=%s equity=%s", got.NetPnL, got.Equity)
	}
}
