package trader

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (r *Repository) ListArbitrageControlSummaries(
	ctx context.Context,
	ids []string,
) (arbitrageControlSummaryBatch, error) {
	result := arbitrageControlSummaryBatch{
		Found: make(map[string]arbitrageControlSummary, len(ids)),
	}
	requested := uniqueNonEmptyIDs(ids)
	if len(requested) == 0 {
		return result, nil
	}
	parsed, missing := parseUUIDIDs(requested)
	result.Missing = missing
	if len(parsed) == 0 {
		return result, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text,version,status,runtime_state
		FROM trader_arbitrage_combinations
		WHERE id=ANY($1::uuid[])`, parsed)
	if err != nil {
		return arbitrageControlSummaryBatch{}, fmt.Errorf("list arbitrage control summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var summary arbitrageControlSummary
		if scanErr := rows.Scan(
			&id, &summary.Version, &summary.Status, &summary.RuntimeState,
		); scanErr != nil {
			return arbitrageControlSummaryBatch{}, fmt.Errorf("scan arbitrage control summary: %w", scanErr)
		}
		result.Found[id] = summary
	}
	if err = rows.Err(); err != nil {
		return arbitrageControlSummaryBatch{}, fmt.Errorf("list arbitrage control summaries: %w", err)
	}
	for _, id := range requested {
		if _, ok := result.Found[id]; ok {
			continue
		}
		if uuidMissing(result.Missing, id) {
			continue
		}
		if _, parseErr := uuid.Parse(id); parseErr != nil {
			continue
		}
		result.Missing = append(result.Missing, id)
	}
	return result, nil
}

func (r *Repository) RenewArbitrageLeases(
	ctx context.Context,
	ids []string,
	lease time.Duration,
) (map[string]bool, error) {
	renewed := make(map[string]bool, len(ids))
	parsed, _ := parseUUIDIDs(uniqueNonEmptyIDs(ids))
	if len(parsed) == 0 {
		return renewed, nil
	}
	if lease <= 0 {
		lease = time.Second
	}
	rows, err := r.pool.Query(ctx, `
		UPDATE trader_arbitrage_combinations
		SET scheduler_lease_until=now()+$2::interval
		WHERE id=ANY($1::uuid[])
		  AND status IN ('running','closing')
		  AND scheduler_lease_until>now()
		RETURNING id::text`, parsed, lease.String())
	if err != nil {
		return nil, fmt.Errorf("renew arbitrage leases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("scan renewed arbitrage lease: %w", scanErr)
		}
		renewed[id] = true
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("renew arbitrage leases: %w", err)
	}
	return renewed, nil
}

func (r *Repository) BatchUpdateArbitrageMarketSnapshots(
	ctx context.Context,
	snapshots []arbitrageMarketSnapshotWrite,
) ([]arbitrageMarketSnapshotBatchItem, error) {
	if len(snapshots) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(snapshots))
	asks := make([]string, 0, len(snapshots))
	bids := make([]string, 0, len(snapshots))
	versions := make([]int64, 0, len(snapshots))
	updatedAts := make([]time.Time, 0, len(snapshots))
	ords := make([]int32, 0, len(snapshots))
	sequences := make([]uint64, 0, len(snapshots))
	for index, snapshot := range snapshots {
		parsed, err := uuid.Parse(strings.TrimSpace(snapshot.CombinationID))
		if err != nil {
			return nil, fmt.Errorf("batch market snapshot id: %w", err)
		}
		ids = append(ids, parsed)
		asks = append(asks, snapshot.AskSpread)
		bids = append(bids, snapshot.BidSpread)
		versions = append(versions, snapshot.ExpectedVersion)
		updatedAts = append(updatedAts, snapshot.ExpectedUpdatedAt)
		ords = append(ords, int32(index))
		sequences = append(sequences, snapshot.Sequence)
	}
	rows, err := r.pool.Query(ctx, `
		WITH incoming AS (
			SELECT u.id,u.ask,u.bid,u.version,u.updated_at,u.ord
			FROM unnest(
				$1::uuid[],$2::text[],$3::text[],$4::bigint[],$5::timestamptz[],$6::int[]
			) AS u(id,ask,bid,version,updated_at,ord)
		),
		applied AS (
			UPDATE trader_arbitrage_combinations c SET
				current_ask_spread_bps=NULLIF(incoming.ask,'')::numeric,
				current_bid_spread_bps=NULLIF(incoming.bid,'')::numeric,
				updated_at=now()
			FROM incoming
			WHERE c.id=incoming.id
			  AND c.status='running'
			  AND c.runtime_state='monitoring'
			  AND c.market_data_stale=FALSE
			  AND c.version=incoming.version
			  AND c.updated_at=incoming.updated_at
			  AND NOT EXISTS (
				SELECT 1 FROM trader_arbitrage_executions e
				WHERE e.combination_id=c.id
				  AND e.status NOT IN ('completed','failed','canceled','dry_run')
			  )
			RETURNING c.id,c.version,c.status,c.runtime_state,c.market_data_stale,c.updated_at,
			          COALESCE(c.current_ask_spread_bps::text,'') AS ask,
			          COALESCE(c.current_bid_spread_bps::text,'') AS bid
		)
		SELECT incoming.id::text,
		       applied.id IS NOT NULL,
		       COALESCE(applied.version,c.version,0),
		       COALESCE(applied.status,c.status,''),
		       COALESCE(applied.runtime_state,c.runtime_state,''),
		       COALESCE(applied.market_data_stale,c.market_data_stale,FALSE),
		       COALESCE(applied.updated_at,c.updated_at,'epoch'::timestamptz),
		       COALESCE(applied.ask,COALESCE(c.current_ask_spread_bps::text,''),''),
		       COALESCE(applied.bid,COALESCE(c.current_bid_spread_bps::text,''),'')
		FROM incoming
		LEFT JOIN applied ON applied.id=incoming.id
		LEFT JOIN trader_arbitrage_combinations c
		  ON c.id=incoming.id AND applied.id IS NULL
		ORDER BY incoming.ord`,
		ids, asks, bids, versions, updatedAts, ords,
	)
	if err != nil {
		return nil, fmt.Errorf("batch update arbitrage market snapshots: %w", err)
	}
	defer rows.Close()
	items := make([]arbitrageMarketSnapshotBatchItem, 0, len(snapshots))
	for rows.Next() {
		var item arbitrageMarketSnapshotBatchItem
		if scanErr := rows.Scan(
			&item.CombinationID, &item.Applied,
			&item.Version, &item.Status, &item.RuntimeState,
			&item.MarketDataStale, &item.UpdatedAt,
			&item.AskSpread, &item.BidSpread,
		); scanErr != nil {
			return nil, fmt.Errorf("scan arbitrage market snapshot: %w", scanErr)
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("batch update arbitrage market snapshots: %w", err)
	}
	for index := range items {
		if index < len(sequences) {
			items[index].Sequence = sequences[index]
		}
	}
	return items, nil
}

func uniqueNonEmptyIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func parseUUIDIDs(ids []string) ([]uuid.UUID, []string) {
	parsed := make([]uuid.UUID, 0, len(ids))
	var missing []string
	for _, id := range ids {
		parsedID, err := uuid.Parse(id)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		parsed = append(parsed, parsedID)
	}
	return parsed, missing
}

func uuidMissing(ids []string, id string) bool {
	for _, item := range ids {
		if item == id {
			return true
		}
	}
	return false
}
