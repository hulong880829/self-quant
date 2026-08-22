package aggdata

import (
	"context"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"selfquant/backend/internal/database"
)

func TestFairPriceRepositoryPrecisionAndOrdering(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository := NewFairPriceRepository(pool)
	profile := "fair_repo_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM fair_price_snapshots WHERE profile=$1`, profile)
	})
	observedAt := time.Now().UTC().Truncate(time.Second)
	crossBPS := 12.5
	snapshot := &FairPriceSnapshot{
		Profile: profile, Symbol: "BTCUSDT", ModelID: "test-v1",
		RingEpoch: math.MaxUint64, RingSequence: math.MaxUint64 - 1,
		Generation: math.MaxUint64 - 2, WallNS: 100, ExchangeTSNS: 99,
		PriceScale: 18, QuantityScale: 8, Ready: true,
		Price:           FixedValue{Mantissa: 123_456_789_012_345_678, Scale: 18},
		PriceRaw:        FixedValue{Mantissa: 123_456_789_012_345_679, Scale: 18},
		Mid:             FixedValue{Mantissa: 123_456_789_012_345_680, Scale: 18},
		Microprice:      FixedValue{Mantissa: 123_456_789_012_345_681, Scale: 18},
		BestBid:         FixedValue{Mantissa: 123_456_789_012_345_600, Scale: 18},
		BestAsk:         FixedValue{Mantissa: 123_456_789_012_345_700, Scale: 18},
		EffectiveBid:    FixedValue{Mantissa: 123_456_789_012_345_610, Scale: 18},
		EffectiveAsk:    FixedValue{Mantissa: 123_456_789_012_345_690, Scale: 18},
		CrossedQuantity: &FixedValue{Mantissa: 12_345_678, Scale: 8},
		Imbalance:       0.5, SpreadBPS: 1.2, CrossBPS: &crossBPS,
		DepthBidNotional: 100, DepthAskNotional: 101,
		ActiveVenueMask: 1, Crossed: true, Degraded: true,
		DegradedReasons: []string{"book_crossed"},
	}
	if err := repository.Upsert(ctx, observedAt, []*FairPriceSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	var epoch, sequence, price, crossedQuantity string
	if err := pool.QueryRow(ctx, `
		SELECT ring_epoch::text,ring_sequence::text,price::text,crossed_quantity::text
		FROM fair_price_snapshots
		WHERE profile=$1 AND symbol=$2 AND observed_at=$3`,
		profile, snapshot.Symbol, observedAt,
	).Scan(&epoch, &sequence, &price, &crossedQuantity); err != nil {
		t.Fatal(err)
	}
	if epoch != "18446744073709551615" ||
		sequence != "18446744073709551614" ||
		price != "0.123456789012345678" ||
		crossedQuantity != "0.123456780000000000" {
		t.Fatalf("precision was not preserved: %q %q %q %q",
			epoch, sequence, price, crossedQuantity)
	}
	history, err := repository.QueryRange(
		ctx,
		profile,
		snapshot.Symbol,
		snapshot.ModelID,
		observedAt.Add(-time.Second),
		observedAt.Add(time.Second),
		time.Second,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 ||
		history[0].RingEpoch != "18446744073709551615" ||
		history[0].Price.Mantissa != snapshot.Price.Mantissa ||
		history[0].Price.Scale != snapshot.Price.Scale {
		t.Fatalf("unexpected history query: %+v", history)
	}

	snapshot.WallNS = 90
	snapshot.RingEpoch = 1
	snapshot.RingSequence = 1
	snapshot.Price.Mantissa = 999
	if err := repository.Upsert(ctx, observedAt, []*FairPriceSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT ring_epoch::text FROM fair_price_snapshots
		WHERE profile=$1 AND symbol=$2 AND observed_at=$3`,
		profile, snapshot.Symbol, observedAt,
	).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != "18446744073709551615" {
		t.Fatalf("older source overwrote row: epoch=%s", epoch)
	}

	snapshot.WallNS = 101
	if err := repository.Upsert(ctx, observedAt, []*FairPriceSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT ring_epoch::text,ring_sequence::text FROM fair_price_snapshots
		WHERE profile=$1 AND symbol=$2 AND observed_at=$3`,
		profile, snapshot.Symbol, observedAt,
	).Scan(&epoch, &sequence); err != nil {
		t.Fatal(err)
	}
	if epoch != "1" || sequence != "1" {
		t.Fatalf("new epoch with lower sequence did not update: %s/%s", epoch, sequence)
	}

	oldTime := observedAt.Add(-25 * time.Hour)
	snapshot.WallNS = 102
	if err := repository.Upsert(ctx, oldTime, []*FairPriceSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.DeleteExpired(ctx, observedAt.Add(-24*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d rows, want 1", deleted)
	}
}
