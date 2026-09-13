package funding

import (
	"math"
	"testing"
	"time"
)

func TestBuildSpreadsDirectionAndMetrics(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	nextRate := 0.0004
	spreads := BuildSpreads([]Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			Rate: 0.0008, NextRate: &nextRate, IntervalHours: 8,
			Cumulative24h: 0.001, Cumulative7d: 0.009,
			PositionNotionalUSD: 8_000_000, Turnover24hUSD: 90_000_000,
			SourceUpdatedAt: now.Add(-2 * time.Minute),
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			Rate: 0.0001, IntervalHours: 1,
			Cumulative24h: 0.003, Cumulative7d: 0.002,
			PositionNotionalUSD: 5_000_000, Turnover24hUSD: 100_000_000,
			SourceUpdatedAt: now.Add(-time.Minute),
		},
	})

	if len(spreads) != 1 {
		t.Fatalf("spreads=%d want=1", len(spreads))
	}
	got := spreads[0]
	if got.Long.Exchange != "binance" || got.Short.Exchange != "okx" {
		t.Fatalf("direction long=%s short=%s", got.Long.Exchange, got.Short.Exchange)
	}
	if got.Long.EffectiveRate != nextRate {
		t.Fatalf("effective rate=%v want=%v", got.Long.EffectiveRate, nextRate)
	}
	assertFloat(t, got.SingleAnnualized, (0.0001/1-0.0004/8)*24*365)
	assertFloat(t, got.Annualized24h, (0.003-0.001)*365)
	assertFloat(t, got.Annualized7d, (0.002-0.009)*365/7)
	if got.MinPositionNotionalUSD != 5_000_000 || got.MinTurnover24hUSD != 90_000_000 {
		t.Fatalf("minimum liquidity metrics=%+v", got)
	}
	if !got.UpdatedAt.Equal(now.Add(-2 * time.Minute)) {
		t.Fatalf("updated_at=%v", got.UpdatedAt)
	}
	if got.History24hComplete != nil || got.History7dComplete != nil {
		t.Fatal("coverage fields must stay unset")
	}
}

func TestBuildSpreadsCreatesUniqueDistinctExchangePairs(t *testing.T) {
	rates := []Rate{
		{Exchange: "a", ExchangeSymbol: "BTC-A", GlobalSymbol: "BTC", IntervalHours: 1},
		{Exchange: "a", ExchangeSymbol: "BTC-Z", GlobalSymbol: "BTC", IntervalHours: 1},
		{Exchange: "b", ExchangeSymbol: "BTC-B", GlobalSymbol: "BTC", IntervalHours: 1},
		{Exchange: "c", ExchangeSymbol: "BTC-C", GlobalSymbol: "BTC", IntervalHours: 1},
		{Exchange: "a", ExchangeSymbol: "ETH-A", GlobalSymbol: "ETH", IntervalHours: 1},
	}
	spreads := BuildSpreads(rates)
	if len(spreads) != 3 {
		t.Fatalf("spreads=%d want=3", len(spreads))
	}
	pairs := map[string]bool{}
	for _, spread := range spreads {
		key := spread.Long.Exchange + "-" + spread.Short.Exchange
		if pairs[key] {
			t.Fatalf("duplicate pair %q", key)
		}
		pairs[key] = true
		if spread.Long.Exchange == spread.Short.Exchange {
			t.Fatalf("same-exchange pair: %+v", spread)
		}
	}
}

func TestBuildSpreadsSkipsInvalidIntervalsAndDoesNotMergeSymbols(t *testing.T) {
	spreads := BuildSpreads([]Rate{
		{Exchange: "a", GlobalSymbol: "BTCUSDT", IntervalHours: 8},
		{Exchange: "b", GlobalSymbol: "BTCUSDT", IntervalHours: 0},
		{Exchange: "c", GlobalSymbol: "btcusdt", IntervalHours: 8},
		{Exchange: "d", GlobalSymbol: "", IntervalHours: 8},
	})
	if len(spreads) != 0 {
		t.Fatalf("spreads=%+v want none", spreads)
	}
}

func TestBuildSpreadsPairsOnlyHyperliquidUSDCWithOtherVenueUSDT(t *testing.T) {
	rates := []Rate{
		{
			Exchange: "hyperliquid", ExchangeSymbol: "BTC", GlobalSymbol: "BTCUSDC",
			BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 1, Rate: -0.0001,
		},
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, Rate: 0.0002,
		},
		{
			Exchange: "bybit", ExchangeSymbol: "BTCUSDC", GlobalSymbol: "BTCUSDC",
			BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 8, Rate: 0.0003,
		},
	}

	spreads := BuildSpreads(rates)
	if len(spreads) != 1 {
		t.Fatalf("spreads=%+v want one Hyperliquid/USDT pair", spreads)
	}
	got := spreads[0]
	if got.GlobalSymbol != "BTCUSDT" || got.QuoteAsset != "USDT" {
		t.Fatalf("display identity=%s/%s", got.GlobalSymbol, got.QuoteAsset)
	}
	legs := map[string]SpreadLeg{got.Long.Exchange: got.Long, got.Short.Exchange: got.Short}
	if legs["hyperliquid"].GlobalSymbol != "BTCUSDC" ||
		legs["hyperliquid"].QuoteAsset != "USDC" ||
		legs["binance"].GlobalSymbol != "BTCUSDT" ||
		legs["binance"].QuoteAsset != "USDT" {
		t.Fatalf("native leg identities not preserved: %+v", legs)
	}
	if _, pairedWithUSDC := legs["bybit"]; pairedWithUSDC {
		t.Fatalf("Hyperliquid must not pair with another USDC contract: %+v", got)
	}
}

func assertFloat(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
