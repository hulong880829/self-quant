package spread

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"selfquant/backend/internal/config"
)

func TestHistoryQueryUsesBucketAggregation(t *testing.T) {
	query := historyQuery("market_data", "crypto_bbo", 60, "")
	for _, fragment := range []string{
		"PREWHERE venue = @venue",
		"canonical_symbol = @venue_symbol",
		"argMaxIf(ask_price, ts, product = 'spot')",
		"argMaxIf(ask_price, ts, product = 'perpetual')",
		"INTERVAL 60 SECOND",
		"HAVING bucket >= @from AND bucket < @to",
		"market_data.crypto_bbo",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q:\n%s", fragment, query)
		}
	}
	if strings.Contains(query, "JOIN") {
		t.Fatal("query should not self-join")
	}
}

func TestHistoryQueryUsesCrossVenuePerpetualAsks(t *testing.T) {
	query := historyQuery("market_data", "crypto_bbo", 60, "okx")
	for _, fragment := range []string{
		"PREWHERE venue IN (@venue, @compare_venue)",
		"product = 'perpetual'",
		"venue = @venue AND canonical_symbol = @venue_symbol",
		"venue = @compare_venue AND canonical_symbol = @compare_symbol",
		"INTERVAL 60 SECOND",
		"HAVING bucket >= @from AND bucket < @to",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q:\n%s", fragment, query)
		}
	}
	if strings.Contains(query, "JOIN") {
		t.Fatal("query should not self-join")
	}
	if strings.Contains(query, "product = 'spot'") {
		t.Fatal("cross-venue query should not read spot")
	}
}

func TestNormalizeRequestPreservesPerVenueCanonicalSymbols(t *testing.T) {
	request, err := NormalizeRequest(HistoryRequest{
		Venue: "hyperliquid", CompareVenue: "binance",
		BaseAsset: "ZEC", QuoteAsset: "USDT",
		VenueCanonicalSymbol: "zecusdc", CompareVenueCanonicalSymbol: "zecusdt",
		Range: Range24h,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.VenueCanonicalSymbol != "ZECUSDC" ||
		request.CompareVenueCanonicalSymbol != "ZECUSDT" {
		t.Fatalf("request=%+v", request)
	}
	query := historyQuery("market_data", "crypto_bbo", 60, request.CompareVenue)
	if !strings.Contains(query, "canonical_symbol = @venue_symbol") ||
		!strings.Contains(query, "canonical_symbol = @compare_symbol") {
		t.Fatalf("query does not address legs independently:\n%s", query)
	}
}

func TestRepositoryQueryHistoryIntegration(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CLICKHOUSE_ADDR")) == "" {
		t.Skip("CLICKHOUSE_ADDR is not set")
	}
	cfg, err := config.SpreadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	repo, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	for _, item := range []struct {
		rangeValue Range
		resolution int
	}{
		{Range1h, 5},
		{Range24h, 60},
		{Range7d, 300},
	} {
		history, err := repo.QueryHistory(ctx, HistoryRequest{
			Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: item.rangeValue,
		})
		if err != nil {
			t.Fatalf("%s: %v", item.rangeValue, err)
		}
		if history.CanonicalSymbol != "BTCUSDT" || history.ResolutionSeconds != item.resolution {
			t.Fatalf("%s history=%+v", item.rangeValue, history)
		}
		if len(history.Points) > item.rangeValue.ExpectedBuckets() {
			t.Fatalf("%s points=%d expected<=%d", item.rangeValue, len(history.Points), item.rangeValue.ExpectedBuckets())
		}
		t.Logf(
			"%s availability=%s points=%d coverage=%s",
			item.rangeValue, history.Availability, len(history.Points), history.Summary.Coverage,
		)
	}
	service := NewService(repo, time.Minute, 4)
	start := time.Now()
	if _, err := service.GetHistory(ctx, HistoryRequest{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h,
	}); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	start = time.Now()
	if _, err := service.GetHistory(ctx, HistoryRequest{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h,
	}); err != nil {
		t.Fatal(err)
	}
	hot := time.Since(start)
	if hot >= cold && hot > 20*time.Millisecond {
		t.Fatalf("cached query was not faster: cold=%s hot=%s", cold, hot)
	}
	t.Logf("cold=%s hot=%s", cold, hot)
}
