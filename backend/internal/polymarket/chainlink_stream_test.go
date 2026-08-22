package polymarket

import (
	"encoding/json"
	"testing"
	"time"
)

func TestChainlinkSubscriptionsUsesSingleUnfilteredTopic(t *testing.T) {
	subscriptions := chainlinkSubscriptions()
	if len(subscriptions) != 1 {
		t.Fatalf("subscriptions=%d", len(subscriptions))
	}
	if subscriptions[0]["topic"] != chainlinkTopic ||
		subscriptions[0]["type"] != "update" {
		t.Fatalf("subscription=%v", subscriptions[0])
	}
	if _, ok := subscriptions[0]["filters"]; ok {
		t.Fatalf("unexpected filter=%q", subscriptions[0]["filters"])
	}
}

func TestParseChainlinkMessageAcceptsChainlinkTopic(t *testing.T) {
	raw := map[string]any{
		"topic": "crypto_prices_chainlink",
		"payload": map[string]any{
			"symbol":    "btc/usd",
			"value":     64965.33,
			"timestamp": float64(time.Now().UnixMilli()),
		},
	}
	asset, price, _, ok := parseChainlinkMessage(raw)
	if !ok || asset != "BTC" || price != "64965.33" {
		t.Fatalf("asset=%q price=%q ok=%v", asset, price, ok)
	}
}

func TestParseChainlinkMessageAcceptsSupportedAltcoin(t *testing.T) {
	raw := map[string]any{
		"topic": "crypto_prices_chainlink",
		"payload": map[string]any{
			"symbol": "eth/usd",
			"value":  1921.45,
		},
	}
	asset, price, _, ok := parseChainlinkMessage(raw)
	if !ok || asset != "ETH" || price != "1921.45" {
		t.Fatalf("asset=%q price=%q ok=%v", asset, price, ok)
	}
}

func TestParseChainlinkMessageRejectsUnsupportedAsset(t *testing.T) {
	raw := map[string]any{
		"topic": "crypto_prices_chainlink",
		"payload": map[string]any{
			"symbol": "zec/usd",
			"value":  506.38,
		},
	}
	if asset, price, _, ok := parseChainlinkMessage(raw); ok || asset != "" || price != "" {
		t.Fatalf("unsupported asset accepted asset=%q price=%q ok=%v", asset, price, ok)
	}
}

func TestParseChainlinkMessageRejectsBinanceTopic(t *testing.T) {
	raw := map[string]any{
		"topic": "crypto_prices",
		"payload": map[string]any{
			"symbol": "btc/usdt",
			"value":  65001.46,
		},
	}
	if asset, price, _, ok := parseChainlinkMessage(raw); ok || asset != "" || price != "" {
		t.Fatalf("binance topic accepted asset=%q price=%q ok=%v", asset, price, ok)
	}
}

func TestParsePeriodFromSlug(t *testing.T) {
	if got := parsePeriodFromSlug("btc-updown-15m-1723093500"); got != "15m" {
		t.Fatalf("period=%q", got)
	}
	if got := parsePeriodFromSlug("btc-updown-5m-1723093500"); got != "5m" {
		t.Fatalf("period=%q", got)
	}
}

func TestParsePeriodDoesNotConfuse15mWith5m(t *testing.T) {
	if got := parsePeriod("BTC UP OR DOWN 15M"); got != "15m" {
		t.Fatalf("period=%q", got)
	}
}

func TestIntervalWindowFromSlugUsesPeriodDuration(t *testing.T) {
	start, end, ok := intervalWindowFromSlug("btc-updown-15m-1723093500", "15m")
	if !ok {
		t.Fatal("expected slug window")
	}
	if end.Sub(start) != 15*time.Minute {
		t.Fatalf("duration=%s", end.Sub(start))
	}
}

func TestNormalizeGammaMarket15mWindow(t *testing.T) {
	source := gammaMarket{
		ID: "456", ConditionID: "0xdef", Slug: "btc-updown-15m-1723093500",
		Question:  "Bitcoin Up or Down - 15 Min",
		StartDate: "2026-08-08T08:00:00Z", EndDate: "2026-08-08T08:05:00Z",
		Outcomes:        json.RawMessage(`"[\"Up\",\"Down\"]"`),
		ClobTokenIDs:    json.RawMessage(`["31","32"]`),
		MinimumTickSize: 0.01, Active: true,
	}
	market, ok := normalizeGammaMarket(source)
	if !ok {
		t.Fatal("expected normalized market")
	}
	if market.Period != "15m" {
		t.Fatalf("period=%q", market.Period)
	}
	if market.WindowEnd.Sub(market.WindowStart) != 15*time.Minute {
		t.Fatalf("window=%s", market.WindowEnd.Sub(market.WindowStart))
	}
}

func TestDownsampleSeries(t *testing.T) {
	points := make([]PricePoint, 600)
	for index := range points {
		points[index] = PricePoint{
			Timestamp:      time.Unix(int64(index), 0).UTC(),
			ChainlinkPrice: "1",
		}
	}
	result := DownsampleSeries(points, 300)
	if len(result) != 300 {
		t.Fatalf("len=%d", len(result))
	}
}

func TestChainlinkObservationMapKeepsLatestPerAsset(t *testing.T) {
	pending := make(map[string]chainlinkObservation)
	t1 := time.Unix(1, 0).UTC()
	t2 := time.Unix(2, 0).UTC()
	pending["BTC"] = chainlinkObservation{asset: "BTC", price: "100", observedAt: t1}
	pending["ETH"] = chainlinkObservation{asset: "ETH", price: "200", observedAt: t1}
	pending["BTC"] = chainlinkObservation{asset: "BTC", price: "101", observedAt: t2}
	if len(pending) != 2 {
		t.Fatalf("len=%d", len(pending))
	}
	if pending["BTC"].price != "101" || !pending["BTC"].observedAt.Equal(t2) {
		t.Fatalf("%+v", pending["BTC"])
	}
}

func TestRecordChainlinkPriceSamplesOncePerSecond(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC().Truncate(time.Second)
	market := Market{
		ID: "m1", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{
		Market: market, ChainlinkPrice: "65000", SourceUpdated: now,
		Series: []PricePoint{{Timestamp: now, ChainlinkPrice: "65000"}},
	})
	service := newOpenTestService(store)
	service.RecordChainlinkPrice(nil, "BTC", "65001", now.Add(200*time.Millisecond))
	snap, _ := store.Get("m1")
	if snap.ChainlinkPrice != "65001" || len(snap.Series) != 1 {
		t.Fatalf("scalar refresh failed: %+v", snap)
	}
	service.RecordChainlinkPrice(nil, "BTC", "65002", now.Add(time.Second))
	snap, _ = store.Get("m1")
	if snap.ChainlinkPrice != "65002" || len(snap.Series) != 2 {
		t.Fatalf("sample failed: %+v", snap)
	}
}
