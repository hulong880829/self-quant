package aggdata

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type FairPriceRepository struct {
	pool *pgxpool.Pool
}

type FairPriceHistoryPoint struct {
	ObservedAt      time.Time
	RingEpoch       string
	RingSequence    string
	SourceWallNS    uint64
	Price           FixedValue
	Degraded        bool
	DegradedReasons []string
}

func NewFairPriceRepository(pool *pgxpool.Pool) *FairPriceRepository {
	return &FairPriceRepository{pool: pool}
}

func (r *FairPriceRepository) Upsert(
	ctx context.Context,
	observedAt time.Time,
	snapshots []*FairPriceSnapshot,
) error {
	if len(snapshots) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	queued := 0
	for _, snapshot := range snapshots {
		if snapshot == nil || !snapshot.Ready {
			continue
		}
		if snapshot.WallNS > math.MaxInt64 || snapshot.ExchangeTSNS > math.MaxInt64 {
			return fmt.Errorf("fair price timestamp exceeds PostgreSQL BIGINT")
		}
		batch.Queue(`
			INSERT INTO fair_price_snapshots (
				profile,symbol,model_id,observed_at,
				ring_epoch,ring_sequence,generation,source_wall_ns,exchange_ts_ns,
				price_scale,quantity_scale,
				price,price_raw,mid,microprice,
				best_bid,best_ask,effective_bid,effective_ask,
				impact_bid,impact_ask,crossed_quantity,
				imbalance,spread_bps,cross_bps,
				weighted_bid_depth,weighted_ask_depth,
				depth_bid_notional,depth_ask_notional,active_venue_mask,
				crossed,degraded,degraded_reasons
			) VALUES (
				$1,$2,$3,$4,
				$5,$6,$7,$8,$9,
				$10,$11,
				$12,$13,$14,$15,
				$16,$17,$18,$19,
				$20,$21,$22,
				$23,$24,$25,
				$26,$27,
				$28,$29,$30,
				$31,$32,$33
			)
			ON CONFLICT (profile,symbol,model_id,observed_at) DO UPDATE SET
				ring_epoch=EXCLUDED.ring_epoch,
				ring_sequence=EXCLUDED.ring_sequence,
				generation=EXCLUDED.generation,
				source_wall_ns=EXCLUDED.source_wall_ns,
				exchange_ts_ns=EXCLUDED.exchange_ts_ns,
				price_scale=EXCLUDED.price_scale,
				quantity_scale=EXCLUDED.quantity_scale,
				price=EXCLUDED.price,
				price_raw=EXCLUDED.price_raw,
				mid=EXCLUDED.mid,
				microprice=EXCLUDED.microprice,
				best_bid=EXCLUDED.best_bid,
				best_ask=EXCLUDED.best_ask,
				effective_bid=EXCLUDED.effective_bid,
				effective_ask=EXCLUDED.effective_ask,
				impact_bid=EXCLUDED.impact_bid,
				impact_ask=EXCLUDED.impact_ask,
				crossed_quantity=EXCLUDED.crossed_quantity,
				imbalance=EXCLUDED.imbalance,
				spread_bps=EXCLUDED.spread_bps,
				cross_bps=EXCLUDED.cross_bps,
				weighted_bid_depth=EXCLUDED.weighted_bid_depth,
				weighted_ask_depth=EXCLUDED.weighted_ask_depth,
				depth_bid_notional=EXCLUDED.depth_bid_notional,
				depth_ask_notional=EXCLUDED.depth_ask_notional,
				active_venue_mask=EXCLUDED.active_venue_mask,
				crossed=EXCLUDED.crossed,
				degraded=EXCLUDED.degraded,
				degraded_reasons=EXCLUDED.degraded_reasons
			WHERE EXCLUDED.source_wall_ns > fair_price_snapshots.source_wall_ns`,
			snapshot.Profile, snapshot.Symbol, snapshot.ModelID, observedAt.UTC(),
			strconv.FormatUint(snapshot.RingEpoch, 10),
			strconv.FormatUint(snapshot.RingSequence, 10),
			strconv.FormatUint(snapshot.Generation, 10),
			int64(snapshot.WallNS), int64(snapshot.ExchangeTSNS),
			int16(snapshot.PriceScale), int16(snapshot.QuantityScale),
			fixedDecimalString(snapshot.Price),
			fixedDecimalString(snapshot.PriceRaw),
			fixedDecimalString(snapshot.Mid),
			fixedDecimalString(snapshot.Microprice),
			fixedDecimalString(snapshot.BestBid),
			fixedDecimalString(snapshot.BestAsk),
			fixedDecimalString(snapshot.EffectiveBid),
			fixedDecimalString(snapshot.EffectiveAsk),
			nullableFixedDecimal(snapshot.ImpactBid),
			nullableFixedDecimal(snapshot.ImpactAsk),
			nullableFixedDecimal(snapshot.CrossedQuantity),
			snapshot.Imbalance, snapshot.SpreadBPS, snapshot.CrossBPS,
			snapshot.WeightedBidDepth, snapshot.WeightedAskDepth,
			snapshot.DepthBidNotional, snapshot.DepthAskNotional,
			int32(snapshot.ActiveVenueMask),
			snapshot.Crossed, snapshot.Degraded, snapshot.DegradedReasons,
		)
		queued++
	}
	if queued == 0 {
		return nil
	}
	results := r.pool.SendBatch(ctx, batch)
	for range batch.Len() {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("upsert fair price snapshot: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("close fair price batch: %w", err)
	}
	return nil
}

func (r *FairPriceRepository) DeleteExpired(
	ctx context.Context,
	cutoff time.Time,
	limit int,
) (int64, error) {
	command, err := r.pool.Exec(ctx, `
		DELETE FROM fair_price_snapshots
		WHERE ctid IN (
			SELECT ctid
			FROM fair_price_snapshots
			WHERE observed_at < $1
			ORDER BY observed_at
			LIMIT $2
		)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired fair prices: %w", err)
	}
	return command.RowsAffected(), nil
}

func (r *FairPriceRepository) QueryRange(
	ctx context.Context,
	profile, symbol, modelID string,
	start, end time.Time,
	resolution time.Duration,
	limit int,
) ([]FairPriceHistoryPoint, error) {
	rows, err := r.pool.Query(ctx, `
		WITH sampled AS (
			SELECT
				observed_at,
				ring_epoch::text AS ring_epoch,
				ring_sequence::text AS ring_sequence,
				source_wall_ns,
				price::text AS price,
				price_scale,
				degraded,
				degraded_reasons,
				ROW_NUMBER() OVER (
					PARTITION BY date_bin($6::interval, observed_at, TIMESTAMPTZ '1970-01-01 00:00:00+00')
					ORDER BY observed_at DESC
				) AS bucket_rank
			FROM fair_price_snapshots
			WHERE profile=$1
				AND symbol=$2
				AND model_id=$3
				AND observed_at >= $4
				AND observed_at <= $5
		)
		SELECT observed_at,ring_epoch,ring_sequence,source_wall_ns,
			price,price_scale,degraded,degraded_reasons
		FROM sampled
		WHERE bucket_rank=1
		ORDER BY observed_at ASC
		LIMIT $7`,
		profile, symbol, modelID, start.UTC(), end.UTC(),
		strconv.FormatInt(resolution.Milliseconds(), 10)+" milliseconds", limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query fair price history: %w", err)
	}
	defer rows.Close()
	result := make([]FairPriceHistoryPoint, 0, min(limit, 1024))
	for rows.Next() {
		var point FairPriceHistoryPoint
		var wallNS int64
		var priceText string
		var sourceScale int16
		if err := rows.Scan(
			&point.ObservedAt,
			&point.RingEpoch,
			&point.RingSequence,
			&wallNS,
			&priceText,
			&sourceScale,
			&point.Degraded,
			&point.DegradedReasons,
		); err != nil {
			return nil, fmt.Errorf("scan fair price history: %w", err)
		}
		if wallNS < 0 || sourceScale < 0 || sourceScale > 18 {
			return nil, fmt.Errorf("invalid persisted fair price metadata")
		}
		outputScale := uint8(sourceScale)
		if outputScale <= 15 {
			outputScale += 3
		} else {
			outputScale = 18
		}
		price, err := fixedFromDecimalString(priceText, outputScale)
		if err != nil {
			return nil, fmt.Errorf("decode persisted fair price: %w", err)
		}
		point.SourceWallNS = uint64(wallNS)
		point.Price = price
		if point.DegradedReasons == nil {
			point.DegradedReasons = []string{}
		}
		result = append(result, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fair price history: %w", err)
	}
	return result, nil
}

func fixedFromDecimalString(value string, scale uint8) (FixedValue, error) {
	if scale > 18 {
		return FixedValue{}, fmt.Errorf("scale %d is out of range", scale)
	}
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(value, "-")
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return FixedValue{}, fmt.Errorf("invalid decimal %q", value)
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > int(scale) {
		extra := fraction[scale:]
		if strings.Trim(extra, "0") != "" {
			return FixedValue{}, fmt.Errorf("decimal has precision beyond scale %d", scale)
		}
		fraction = fraction[:scale]
	}
	fraction += strings.Repeat("0", int(scale)-len(fraction))
	digits := strings.TrimLeft(parts[0]+fraction, "0")
	if digits == "" {
		digits = "0"
	}
	integer, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return FixedValue{}, fmt.Errorf("invalid decimal %q", value)
	}
	if negative {
		integer.Neg(integer)
	}
	if !integer.IsInt64() {
		return FixedValue{}, fmt.Errorf("decimal overflows int64 at scale %d", scale)
	}
	return FixedValue{Mantissa: integer.Int64(), Scale: scale}, nil
}

func nullableFixedDecimal(value *FixedValue) any {
	if value == nil {
		return nil
	}
	return fixedDecimalString(*value)
}
