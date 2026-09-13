package funding

import (
	"testing"
	"time"
)

func TestSnapshotLookupPrefersExactOverHigherAnnualizedFallback(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Unix(100, 0).UTC()
	store.Replace([]Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDC", GlobalSymbol: "BTCUSDC",
			BaseAsset: "BTC", QuoteAsset: "USDT", AnnualizedRate: 0.9, Cumulative24h: 0.01,
		},
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", AnnualizedRate: 0.1, Cumulative24h: 0.002,
		},
	}, 2, now)
	rate, ok := store.View().LookupRate(RateLookupKey{
		Exchange: "Binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	})
	if !ok || rate.ExchangeSymbol != "BTCUSDT" || rate.Cumulative24h != 0.002 {
		t.Fatalf("exact must win: ok=%v rate=%+v", ok, rate)
	}
}

func TestSnapshotLookupFallsBackToFirstSnapshotRow(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Unix(100, 0).UTC()
	store.Replace([]Rate{
		{
			Exchange: "hyperliquid", ExchangeSymbol: "xyz:SKR", GlobalSymbol: "SKRUSDT",
			BaseAsset: "SKR", QuoteAsset: "USDT", Cumulative24h: 0.004,
		},
		{
			Exchange: "hyperliquid", ExchangeSymbol: "SKR", GlobalSymbol: "SKRUSDT",
			BaseAsset: "SKR", QuoteAsset: "USDT", Cumulative24h: 0.001,
		},
	}, 2, now)
	rate, ok := store.View().LookupRate(RateLookupKey{
		Exchange: "hyperliquid", ExchangeSymbol: "SKRUSDT", BaseAsset: "SKR", QuoteAsset: "USDT",
	})
	if !ok || rate.ExchangeSymbol != "xyz:SKR" {
		t.Fatalf("fallback must use snapshot order: ok=%v rate=%+v", ok, rate)
	}
}

func TestSnapshotLookupMiss(t *testing.T) {
	store := NewSnapshotStore()
	store.Replace([]Rate{{
		Exchange: "binance", ExchangeSymbol: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT",
	}}, 1, time.Unix(1, 0).UTC())
	if _, ok := store.View().LookupRate(RateLookupKey{
		Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}); ok {
		t.Fatal("missing key must miss")
	}
}
