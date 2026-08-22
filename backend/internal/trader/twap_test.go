package trader

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

func TestTwapSliceFireAtIsStableAndInsideSlice(t *testing.T) {
	start := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	first := twapSliceFireAt("07b6f8d7-b25b-4e14-a866-5542cc09403f", 2, start, end, 30)
	second := twapSliceFireAt("07b6f8d7-b25b-4e14-a866-5542cc09403f", 2, start, end, 30)
	sliceStart := start.Add(time.Minute)
	if !first.Equal(second) || first.Before(sliceStart) || !first.Before(sliceStart.Add(30*time.Second)) {
		t.Fatalf("fire time %s is not stable inside slice", first)
	}
}

func TestCalculateSliceQuantityCatchUpCapAndStep(t *testing.T) {
	qty := calculateSliceQuantity(
		decimal.RequireFromString("10"), time.Minute, 5*time.Minute,
		decimal.RequireFromString("1.5"), decimal.RequireFromString("0.1"),
	)
	if !qty.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("capped quantity=%s", qty)
	}
	catchUp := calculateSliceQuantity(
		decimal.RequireFromString("10"), time.Minute, 90*time.Second,
		decimal.Zero, decimal.RequireFromString("0.1"),
	)
	if !catchUp.Equal(decimal.RequireFromString("6.6")) {
		t.Fatalf("catch-up quantity=%s", catchUp)
	}
}

func TestMakerPriceUsesPassiveBBOAndHardLimit(t *testing.T) {
	instrument := Instrument{PriceTick: "0.1"}
	buy, err := makerPrice(TwapJob{Side: "buy", LimitPrice: "99.8"}, instrument, exchange.BBO{
		BidPrice: "100.07", AskPrice: "100.12",
	})
	if err != nil || buy != "99.8" {
		t.Fatalf("buy=%s err=%v", buy, err)
	}
	sell, err := makerPrice(TwapJob{Side: "sell", LimitPrice: "100.2"}, instrument, exchange.BBO{
		BidPrice: "100.01", AskPrice: "100.11",
	})
	if err != nil || sell != "100.2" {
		t.Fatalf("sell=%s err=%v", sell, err)
	}
}

func TestMakerRetryUsesLatestBBOAndOnlyUnfilledQuantity(t *testing.T) {
	job := TwapJob{Side: "buy", CurrentSlice: 2, CurrentAttempt: 1}
	instrument := Instrument{PriceTick: "0.1", QuantityStep: "0.01"}
	quantity, err := makerRetryQuantityFromOrders(job, instrument, []Order{{
		Quantity: "1.00", FilledQuantity: "0.37", TwapSliceIndex: 2, TwapAttemptIndex: 0,
	}})
	if err != nil || !quantity.Equal(decimal.RequireFromString("0.63")) {
		t.Fatalf("retry quantity=%s err=%v", quantity, err)
	}
	first, err := makerPrice(job, instrument, exchange.BBO{BidPrice: "100.04", AskPrice: "100.10"})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := makerPrice(job, instrument, exchange.BBO{BidPrice: "99.84", AskPrice: "99.90"})
	if err != nil || first != "100" || latest != "99.8" {
		t.Fatalf("maker prices first=%s latest=%s err=%v", first, latest, err)
	}
	if deterministicTwapClientID("07b6f8d7-b25b-4e14-a866-5542cc09403f", 2, 0) ==
		deterministicTwapClientID("07b6f8d7-b25b-4e14-a866-5542cc09403f", 2, 1) {
		t.Fatal("attempt client order IDs must be unique")
	}
}

func TestTwapSliceEndCapsAtExecutionWindow(t *testing.T) {
	start := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	job := TwapJob{
		StartAt: start, EndAt: start.Add(95 * time.Second),
		IntervalSeconds: 60, CurrentSlice: 1,
	}
	if got := twapSliceEnd(job); !got.Equal(job.EndAt) {
		t.Fatalf("slice end=%s want=%s", got, job.EndAt)
	}
}

func TestNormalizeTwapRejectsImpossibleMaxAndTimeout(t *testing.T) {
	now := time.Now().UTC()
	_, err := normalizeTwapInput(CreateTwapInput{
		Token: "token", TradingAccountID: 1, InstrumentID: 2, Side: "buy",
		TotalQuantity: "10", StartAt: now.Add(time.Minute), EndAt: now.Add(6 * time.Minute),
		IntervalSeconds: 60, MaxQuantity: "1", ExecutionType: "maker",
		OrderTimeoutSeconds: 60, IdempotencyKey: "key",
	}, now)
	if err != ErrInvalidArgument {
		t.Fatalf("err=%v", err)
	}
}
