package polymarket

import (
	"encoding/json"
	"testing"
)

func TestNormalizeGammaMarket(t *testing.T) {
	source := gammaMarket{
		ID: "123", ConditionID: "0xabc", Slug: "bitcoin-up-or-down-5m",
		Question:  "Bitcoin BTC Up or Down - 5 Min",
		StartDate: "2026-08-08T08:00:00Z", EndDate: "2026-08-08T08:05:00Z",
		Outcomes:        json.RawMessage(`"[\"Up\",\"Down\"]"`),
		ClobTokenIDs:    json.RawMessage(`["11","22"]`),
		MinimumTickSize: 0.01, Active: true,
	}
	market, ok := normalizeGammaMarket(source)
	if !ok {
		t.Fatal("expected normalized market")
	}
	if market.Asset != "BTC" || market.Period != "5m" ||
		market.UpTokenID != "11" || market.DownTokenID != "22" {
		t.Fatalf("market=%+v", market)
	}
}

func TestNormalizeGammaMarketRejectsUnsupportedQuestion(t *testing.T) {
	_, ok := normalizeGammaMarket(gammaMarket{
		ID: "1", Question: "Will it rain?", Slug: "rain",
	})
	if ok {
		t.Fatal("unsupported market was accepted")
	}
}

func TestNormalizeEventMarketsPriceToBeat(t *testing.T) {
	events := []gammaEvent{{
		EventMetadata: struct {
			PriceToBeat float64 `json:"priceToBeat"`
		}{PriceToBeat: 64967.36},
		Markets: []gammaMarket{{
			ID: "3378777", ConditionID: "0xabc",
			Slug:      "bitcoin-up-or-down-august-8-2026-8am-et",
			Question:  "Bitcoin Up or Down - August 8, 8AM ET",
			StartDate: "2026-08-06T12:00:17Z", EndDate: "2026-08-08T13:00:00Z",
			EventStartTime: "2026-08-08T12:00:00Z",
			Outcomes:       json.RawMessage(`"[\"Up\",\"Down\"]"`),
			ClobTokenIDs:   json.RawMessage(`["11","22"]`),
			Active:         true,
		}},
	}}
	markets := normalizeEventMarkets(events)
	if len(markets) != 1 {
		t.Fatalf("markets=%d", len(markets))
	}
	if markets[0].GammaOpenPrice != "64967.36" {
		t.Fatalf("gamma open=%q", markets[0].GammaOpenPrice)
	}
}

func TestFormatGammaOpenPriceEmpty(t *testing.T) {
	if got := formatGammaOpenPrice(0); got != "" {
		t.Fatalf("formatGammaOpenPrice(0)=%q", got)
	}
}
