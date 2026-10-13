package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) AcquireCycleLock(ctx context.Context) (CycleLease, bool, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire cycle lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, LockKey).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try cycle lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return newPostgresCycleLease(conn), true, nil
}

func newPostgresCycleLease(conn *pgxpool.Conn) *heldCycleLock {
	return &heldCycleLock{
		unlock: func(ctx context.Context) (bool, error) {
			var unlocked bool
			err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, LockKey).Scan(&unlocked)
			return unlocked, err
		},
		releaseToPool: func() {
			conn.Release()
		},
		destroy: func() {
			raw := conn.Hijack()
			if raw == nil {
				return
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), cycleLockCleanupTimeout)
			defer cancel()
			_ = raw.Close(closeCtx)
		},
	}
}

func (r *Repository) EnsureActiveRun(ctx context.Context, now time.Time, initialEquity decimal.Decimal, strategyVersion string) (Run, error) {
	if strategyVersion == "" {
		strategyVersion = StrategyVersion
	}
	if run, err := r.ActiveRun(ctx, strategyVersion); err != nil {
		return Run{}, err
	} else if run != nil {
		return *run, nil
	}
	_, err := r.pool.Exec(ctx, `
INSERT INTO jev_arb_runs (
	status, strategy_version, initial_equity, equity, peak_equity, created_at, updated_at
) VALUES ('active', $1, $2, $2, $2, $3, $3)`, strategyVersion, initialEquity.String(), now.UTC())
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			return Run{}, err
		}
	}
	run, err := r.ActiveRun(ctx, strategyVersion)
	if err != nil {
		return Run{}, err
	}
	if run == nil {
		return Run{}, fmt.Errorf("active jev run missing after insert")
	}
	return *run, nil
}

func (r *Repository) ActiveRun(ctx context.Context, strategyVersion string) (*Run, error) {
	if strategyVersion == "" {
		strategyVersion = StrategyVersion
	}
	row := r.pool.QueryRow(ctx, runSelect+` WHERE status = 'active' AND strategy_version=$1 LIMIT 1`, strategyVersion)
	run, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *Repository) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := r.pool.Query(ctx, runSelect+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *Repository) loadRun(ctx context.Context, id int64) (Run, error) {
	return scanRun(r.pool.QueryRow(ctx, runSelect+` WHERE id=$1`, id))
}

func (r *Repository) SaveRun(ctx context.Context, run Run) error {
	_, err := r.pool.Exec(ctx, `
UPDATE jev_arb_runs SET
	status=$2, initial_equity=$3, equity=$4, peak_equity=$5, net_pnl=$6, funding_pnl=$7,
	realized_spread_pnl=$8, unrealized_spread_pnl=$9, cumulative_cost=$10, max_drawdown=$11,
	snapshot_version=$12, last_successful_at=$13, last_decision_at=$14, last_market_data_at=$15,
	updated_at=now()
WHERE id=$1`,
		run.ID, run.Status, dec(run.InitialEquity), dec(run.Equity), dec(run.PeakEquity),
		dec(run.NetPnL), dec(run.FundingPnL), dec(run.RealizedSpreadPnL), dec(run.UnrealizedSpreadPnL),
		dec(run.CumulativeCost), dec(run.MaxDrawdown), run.SnapshotVersion,
		run.LastSuccessfulAt, run.LastDecisionAt, run.LastMarketDataAt,
	)
	return err
}

func (r *Repository) ListAllPositions(ctx context.Context, runID int64) ([]Position, error) {
	rows, err := r.pool.Query(ctx, positionSelect+` WHERE run_id=$1 ORDER BY id DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

func (r *Repository) ListOpenPositions(ctx context.Context, runID int64) ([]Position, error) {
	rows, err := r.pool.Query(ctx, positionSelect+` WHERE run_id=$1 AND status='open' ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

func (r *Repository) ListOpenPositionsExceptStrategy(ctx context.Context, strategyVersion string) ([]Position, error) {
	query := `
SELECT p.id, p.run_id, p.kind, p.status, p.candidate_key, p.base_asset, p.quote_asset, p.global_symbol,
	p.long_exchange, p.long_symbol, p.long_product, p.long_canonical_symbol, p.long_quote_asset,
	p.short_exchange, p.short_symbol, p.short_product, p.short_canonical_symbol, p.short_quote_asset,
	p.entry_mid_basis_bps::text, p.shared_base_qty::text, p.long_qty::text, p.short_qty::text,
	p.long_entry_price::text, p.short_entry_price::text, p.long_mark_price::text, p.short_mark_price::text,
	p.realized_spread_pnl::text, p.unrealized_spread_pnl::text, p.funding_pnl::text, p.cost_paid::text,
	p.funding_checked_through_long, p.funding_checked_through_short, p.exit_pending_reason, p.degraded_reason,
	p.opened_at, p.closed_at, p.last_mark_at, p.strategy_version, p.exit_confirm_count,
	p.net_if_exit::text, p.net_if_exit_at, p.bbo_missing_since
FROM jev_arb_positions p
JOIN jev_arb_runs r ON r.id = p.run_id
WHERE p.status='open' AND r.strategy_version <> $1
ORDER BY p.id`
	rows, err := r.pool.Query(ctx, query, strategyVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

func (r *Repository) ListPositionsPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Position, int64, error) {
	limit = ClampLimit(limit)
	args := []any{runID, limit + 1}
	query := positionSelect + ` WHERE run_id=$1`
	if cursor > 0 {
		query += ` AND id < $3`
		args = append(args, cursor)
	}
	query += ` ORDER BY id DESC LIMIT $2`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, err := scanPositions(rows)
	if err != nil {
		return nil, 0, err
	}
	return pageIDs(items, limit, func(item Position) int64 { return item.ID })
}

func (r *Repository) InsertPosition(ctx context.Context, position Position) (Position, error) {
	row := r.pool.QueryRow(ctx, `
INSERT INTO jev_arb_positions (
	run_id, kind, status, candidate_key, base_asset, quote_asset, global_symbol,
	long_exchange, long_symbol, long_product, long_canonical_symbol, long_quote_asset,
	short_exchange, short_symbol, short_product, short_canonical_symbol, short_quote_asset,
	entry_mid_basis_bps, shared_base_qty, long_qty, short_qty, long_entry_price, short_entry_price,
	long_mark_price, short_mark_price, realized_spread_pnl, unrealized_spread_pnl,
	funding_pnl, cost_paid, funding_checked_through_long, funding_checked_through_short,
	exit_pending_reason, degraded_reason, opened_at, closed_at, last_mark_at,
	strategy_version, exit_confirm_count, net_if_exit, net_if_exit_at, bbo_missing_since
) VALUES (
	$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41
) RETURNING id`,
		position.RunID, position.Kind, position.Status, position.CandidateKey, position.BaseAsset,
		position.QuoteAsset, position.GlobalSymbol, position.LongExchange, position.LongSymbol,
		position.LongProduct, position.LongCanonicalSymbol, position.LongQuoteAsset,
		position.ShortExchange, position.ShortSymbol, position.ShortProduct, position.ShortCanonicalSymbol,
		position.ShortQuoteAsset, nullDecimal(position.EntryMidBasisBPS),
		dec(position.SharedBaseQty), dec(position.LongQty), dec(position.ShortQty),
		dec(position.LongEntryPrice), dec(position.ShortEntryPrice), dec(position.LongMarkPrice),
		dec(position.ShortMarkPrice), dec(position.RealizedSpreadPnL), dec(position.UnrealizedSpreadPnL),
		dec(position.FundingPnL), dec(position.CostPaid), position.FundingCheckedThroughLong,
		position.FundingCheckedThroughShort, nullString(position.ExitPendingReason),
		nullString(position.DegradedReason), position.OpenedAt, position.ClosedAt, position.LastMarkAt,
		nullStrategy(position.StrategyVersion), position.ExitConfirmCount, nullDecimal(position.NetIfExit),
		position.NetIfExitAt, position.BBOMissingSince,
	)
	if err := row.Scan(&position.ID); err != nil {
		return Position{}, err
	}
	return position, nil
}

func (r *Repository) SavePosition(ctx context.Context, position Position) error {
	_, err := r.pool.Exec(ctx, `
UPDATE jev_arb_positions SET
	status=$2, shared_base_qty=$3, long_qty=$4, short_qty=$5, long_entry_price=$6, short_entry_price=$7,
	long_mark_price=$8, short_mark_price=$9, realized_spread_pnl=$10, unrealized_spread_pnl=$11,
	funding_pnl=$12, cost_paid=$13, funding_checked_through_long=$14, funding_checked_through_short=$15,
	exit_pending_reason=$16, degraded_reason=$17, closed_at=$18, last_mark_at=$19,
	exit_confirm_count=$20, net_if_exit=$21, net_if_exit_at=$22, bbo_missing_since=$23, updated_at=now()
WHERE id=$1`,
		position.ID, position.Status, dec(position.SharedBaseQty), dec(position.LongQty), dec(position.ShortQty),
		dec(position.LongEntryPrice), dec(position.ShortEntryPrice), dec(position.LongMarkPrice),
		dec(position.ShortMarkPrice), dec(position.RealizedSpreadPnL), dec(position.UnrealizedSpreadPnL),
		dec(position.FundingPnL), dec(position.CostPaid), position.FundingCheckedThroughLong,
		position.FundingCheckedThroughShort, nullString(position.ExitPendingReason),
		nullString(position.DegradedReason), position.ClosedAt, position.LastMarkAt,
		position.ExitConfirmCount, nullDecimal(position.NetIfExit), position.NetIfExitAt, position.BBOMissingSince,
	)
	return err
}

func (r *Repository) InsertDecision(ctx context.Context, decision Decision) (Decision, error) {
	raw := decision.RawResponse
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	row := r.pool.QueryRow(ctx, `
INSERT INTO jev_arb_decisions (
	run_id, position_id, candidate_key, kind, decision, policy_status, execution_status,
	enter_probability, exit_probability, confidence, expected_net_return_24h,
	funding_return_24h, holding_horizon_net_return, expected_payback_minutes,
	current_mid_basis_bps, entry_mid_basis_bps, basis_change_since_entry_bps,
	expected_basis_return_24h, basis_return_p10, basis_profit_probability, basis_forecast_confidence,
	basis_sample_count, basis_model_version, basis_forecast_calculated_at, rationale,
	prompt_version, policy_version, model_requested, model_actual, raw_response, error_message,
	decision_at, execution_started_at, fill_deadline,
	base_asset, quote_asset, long_canonical_symbol, long_quote_asset, short_canonical_symbol, short_quote_asset,
	strategy_version, parse_error_type
) VALUES (
	$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42
) RETURNING id`,
		decision.RunID, decision.PositionID, decision.CandidateKey, decision.Kind, decision.Decision,
		decision.PolicyStatus, decision.ExecutionStatus, dec(decision.EnterProbability),
		dec(decision.ExitProbability), dec(decision.Confidence), nullDecimal(decision.ExpectedNetReturn24h),
		nullDecimal(decision.FundingReturn24h), nullDecimal(decision.HoldingHorizonNetReturn),
		nullDecimal(decision.ExpectedPaybackMinutes), nullDecimal(decision.CurrentMidBasisBPS),
		nullDecimal(decision.EntryMidBasisBPS), nullDecimal(decision.BasisChangeSinceEntryBPS),
		nullDecimal(decision.ExpectedBasisReturn24h), nullDecimal(decision.BasisReturnP10),
		nullDecimal(decision.BasisProfitProbability), nullDecimal(decision.BasisForecastConfidence),
		nullInt(decision.BasisSampleCount), nullStringPtr(decision.BasisModelVersion),
		decision.BasisForecastCalculatedAt, nullString(decision.Rationale), decision.PromptVersion,
		decision.PolicyVersion, nullString(decision.ModelRequested), nullString(decision.ModelActual), raw,
		nullString(decision.ErrorMessage), decision.DecisionAt, decision.ExecutionStartedAt, decision.FillDeadline,
		decision.BaseAsset, decision.QuoteAsset, decision.LongCanonicalSymbol, decision.LongQuoteAsset,
		decision.ShortCanonicalSymbol, decision.ShortQuoteAsset, nullStrategy(decision.StrategyVersion),
		nullString(decision.ParseErrorType),
	)
	if err := row.Scan(&decision.ID); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func (r *Repository) SaveDecision(ctx context.Context, decision Decision) error {
	_, err := r.pool.Exec(ctx, `
UPDATE jev_arb_decisions SET
	position_id=$2, policy_status=$3, execution_status=$4, error_message=$5,
	execution_started_at=$6, fill_deadline=$7
WHERE id=$1`,
		decision.ID, decision.PositionID, decision.PolicyStatus, decision.ExecutionStatus,
		nullString(decision.ErrorMessage), decision.ExecutionStartedAt, decision.FillDeadline,
	)
	return err
}

func (r *Repository) ListPendingDecisions(ctx context.Context, runID int64) ([]Decision, error) {
	rows, err := r.pool.Query(ctx, decisionSelect+` WHERE run_id=$1 AND execution_status='pending' ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecisions(rows)
}

func (r *Repository) ListDecisionsPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Decision, int64, error) {
	limit = ClampLimit(limit)
	args := []any{runID, limit + 1}
	query := decisionSelect + ` WHERE run_id=$1`
	if cursor > 0 {
		query += ` AND id < $3`
		args = append(args, cursor)
	}
	query += ` ORDER BY id DESC LIMIT $2`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, err := scanDecisions(rows)
	if err != nil {
		return nil, 0, err
	}
	return pageIDs(items, limit, func(item Decision) int64 { return item.ID })
}

func (r *Repository) InsertTrade(ctx context.Context, trade Trade) (Trade, error) {
	row := r.pool.QueryRow(ctx, `
INSERT INTO jev_arb_trades (
	run_id, position_id, decision_id, side, shared_base_qty, long_fill_price, short_fill_price,
	notional, cost, filled_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
RETURNING id`,
		trade.RunID, trade.PositionID, trade.DecisionID, trade.Side, dec(trade.SharedBaseQty),
		dec(trade.LongFillPrice), dec(trade.ShortFillPrice), dec(trade.Notional), dec(trade.Cost), trade.FilledAt,
	)
	if err := row.Scan(&trade.ID); err != nil {
		return Trade{}, err
	}
	return trade, nil
}

func (r *Repository) ListTradesPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Trade, int64, error) {
	limit = ClampLimit(limit)
	args := []any{runID, limit + 1}
	query := tradeSelect + ` WHERE t.run_id=$1`
	if cursor > 0 {
		query += ` AND t.id < $3`
		args = append(args, cursor)
	}
	query += ` ORDER BY t.id DESC LIMIT $2`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, err := scanTrades(rows)
	if err != nil {
		return nil, 0, err
	}
	return pageIDs(items, limit, func(item Trade) int64 { return item.ID })
}

func (r *Repository) InsertEquityPoint(ctx context.Context, runID int64, point EquityPoint) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO jev_arb_equity_points (run_id, ts, equity, net_pnl)
VALUES ($1,$2,$3,$4)`, runID, point.TS.UTC(), dec(point.Equity), dec(point.NetPnL))
	return err
}

func (r *Repository) ListEquityPoints(ctx context.Context, runID int64, from *time.Time) ([]EquityPoint, error) {
	query := `SELECT ts, equity::text, net_pnl::text FROM jev_arb_equity_points WHERE run_id=$1`
	args := []any{runID}
	if from != nil {
		query += ` AND ts >= $2`
		args = append(args, from.UTC())
	}
	query += ` ORDER BY ts`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EquityPoint, 0)
	for rows.Next() {
		var point EquityPoint
		var equity, pnl string
		if err := rows.Scan(&point.TS, &equity, &pnl); err != nil {
			return nil, err
		}
		point.Equity = parseDecimalString(equity)
		point.NetPnL = parseDecimalString(pnl)
		out = append(out, point)
	}
	return out, rows.Err()
}

func (r *Repository) PublishSnapshot(ctx context.Context, runID int64, snapshot Snapshot) (Snapshot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int64
	if err := tx.QueryRow(ctx, `
UPDATE jev_arb_runs SET snapshot_version = snapshot_version + 1, updated_at=now()
WHERE id=$1
RETURNING snapshot_version`, runID).Scan(&version); err != nil {
		return Snapshot{}, err
	}
	snapshot.Version = version
	snapshot.ETag = etagFor(version)
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO jev_arb_snapshots (run_id, snapshot_version, payload)
VALUES ($1,$2,$3)`, runID, version, payload); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (r *Repository) LatestSnapshot(ctx context.Context, runID int64) (*Snapshot, error) {
	var payload []byte
	err := r.pool.QueryRow(ctx, `
SELECT payload FROM jev_arb_snapshots
WHERE run_id=$1
ORDER BY snapshot_version DESC
LIMIT 1`, runID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

const runSelect = `
SELECT id, status, strategy_version, initial_equity::text, equity::text, peak_equity::text, net_pnl::text,
	funding_pnl::text, realized_spread_pnl::text, unrealized_spread_pnl::text,
	cumulative_cost::text, max_drawdown::text, snapshot_version,
	last_successful_at, last_decision_at, last_market_data_at,
	archived_realized_spread_pnl::text, archived_funding_pnl::text,
	archived_cost_paid::text, archived_position_count
FROM jev_arb_runs`

const positionSelect = `
SELECT id, run_id, kind, status, candidate_key, base_asset, quote_asset, global_symbol,
	long_exchange, long_symbol, long_product, long_canonical_symbol, long_quote_asset,
	short_exchange, short_symbol, short_product, short_canonical_symbol, short_quote_asset,
	entry_mid_basis_bps::text, shared_base_qty::text, long_qty::text, short_qty::text, long_entry_price::text, short_entry_price::text,
	long_mark_price::text, short_mark_price::text, realized_spread_pnl::text, unrealized_spread_pnl::text,
	funding_pnl::text, cost_paid::text, funding_checked_through_long, funding_checked_through_short,
	exit_pending_reason, degraded_reason, opened_at, closed_at, last_mark_at,
	strategy_version, exit_confirm_count, net_if_exit::text, net_if_exit_at, bbo_missing_since
FROM jev_arb_positions`

const decisionSelect = `
SELECT id, run_id, position_id, candidate_key, kind, decision, policy_status, execution_status,
	enter_probability::text, exit_probability::text, confidence::text, expected_net_return_24h::text,
	funding_return_24h::text, holding_horizon_net_return::text, expected_payback_minutes::text,
	current_mid_basis_bps::text, entry_mid_basis_bps::text, basis_change_since_entry_bps::text,
	expected_basis_return_24h::text, basis_return_p10::text, basis_profit_probability::text,
	basis_forecast_confidence::text, basis_sample_count, basis_model_version, basis_forecast_calculated_at,
	rationale, prompt_version, policy_version, model_requested, model_actual, raw_response, error_message,
	decision_at, execution_started_at, fill_deadline,
	base_asset, quote_asset, long_canonical_symbol, long_quote_asset, short_canonical_symbol, short_quote_asset,
	strategy_version, parse_error_type
FROM jev_arb_decisions`

const tradeSelect = `
SELECT t.id, t.run_id, t.position_id, t.decision_id, t.side, t.shared_base_qty::text, t.long_fill_price::text,
	t.short_fill_price::text, t.notional::text, t.cost::text, t.filled_at,
	p.strategy_version, p.base_asset, p.kind, p.long_exchange, p.long_symbol, p.long_product,
	p.short_exchange, p.short_symbol, p.short_product
FROM jev_arb_trades t
JOIN jev_arb_positions p ON p.id = t.position_id`

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(row scanner) (Run, error) {
	var run Run
	var initial, equity, peak, net, funding, realized, unrealized, cost, drawdown string
	var archivedRealized, archivedFunding, archivedCost string
	err := row.Scan(
		&run.ID, &run.Status, &run.StrategyVersion, &initial, &equity, &peak, &net, &funding, &realized, &unrealized,
		&cost, &drawdown, &run.SnapshotVersion, &run.LastSuccessfulAt, &run.LastDecisionAt, &run.LastMarketDataAt,
		&archivedRealized, &archivedFunding, &archivedCost, &run.ArchivedPositionCount,
	)
	if err != nil {
		return Run{}, err
	}
	run.InitialEquity = parseDecimalString(initial)
	run.Equity = parseDecimalString(equity)
	run.PeakEquity = parseDecimalString(peak)
	run.NetPnL = parseDecimalString(net)
	run.FundingPnL = parseDecimalString(funding)
	run.RealizedSpreadPnL = parseDecimalString(realized)
	run.UnrealizedSpreadPnL = parseDecimalString(unrealized)
	run.CumulativeCost = parseDecimalString(cost)
	run.MaxDrawdown = parseDecimalString(drawdown)
	run.ArchivedRealizedSpreadPnL = parseDecimalString(archivedRealized)
	run.ArchivedFundingPnL = parseDecimalString(archivedFunding)
	run.ArchivedCostPaid = parseDecimalString(archivedCost)
	return run, nil
}

func scanPositions(rows pgx.Rows) ([]Position, error) {
	out := make([]Position, 0)
	for rows.Next() {
		var item Position
		var shared, longQty, shortQty, longEntry, shortEntry, longMark, shortMark string
		var realized, unrealized, funding, cost string
		var entryMid *string
		var exitPending, degraded *string
		var netIfExit *string
		if err := rows.Scan(
			&item.ID, &item.RunID, &item.Kind, &item.Status, &item.CandidateKey, &item.BaseAsset,
			&item.QuoteAsset, &item.GlobalSymbol, &item.LongExchange, &item.LongSymbol, &item.LongProduct,
			&item.LongCanonicalSymbol, &item.LongQuoteAsset,
			&item.ShortExchange, &item.ShortSymbol, &item.ShortProduct,
			&item.ShortCanonicalSymbol, &item.ShortQuoteAsset, &entryMid,
			&shared, &longQty, &shortQty,
			&longEntry, &shortEntry, &longMark, &shortMark, &realized, &unrealized, &funding, &cost,
			&item.FundingCheckedThroughLong, &item.FundingCheckedThroughShort, &exitPending, &degraded,
			&item.OpenedAt, &item.ClosedAt, &item.LastMarkAt,
			&item.StrategyVersion, &item.ExitConfirmCount, &netIfExit, &item.NetIfExitAt, &item.BBOMissingSince,
		); err != nil {
			return nil, err
		}
		item.EntryMidBasisBPS = parseOptionalPtr(entryMid)
		item.SharedBaseQty = parseDecimalString(shared)
		item.LongQty = parseDecimalString(longQty)
		item.ShortQty = parseDecimalString(shortQty)
		item.LongEntryPrice = parseDecimalString(longEntry)
		item.ShortEntryPrice = parseDecimalString(shortEntry)
		item.LongMarkPrice = parseDecimalString(longMark)
		item.ShortMarkPrice = parseDecimalString(shortMark)
		item.RealizedSpreadPnL = parseDecimalString(realized)
		item.UnrealizedSpreadPnL = parseDecimalString(unrealized)
		item.FundingPnL = parseDecimalString(funding)
		item.CostPaid = parseDecimalString(cost)
		item.ExitPendingReason = deref(exitPending)
		item.DegradedReason = deref(degraded)
		item.NetIfExit = parseOptionalPtr(netIfExit)
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanDecisions(rows pgx.Rows) ([]Decision, error) {
	out := make([]Decision, 0)
	for rows.Next() {
		var item Decision
		var enter, exit, conf, expected, fundingRet, holdRet, payback *string
		var currentMid, entryMid, changeSince, expectedBasis, p10, profit, confBasis *string
		var sampleCount *int
		var modelVersion *string
		var rationale, modelReq, modelAct, errMsg, parseErr *string
		if err := rows.Scan(
			&item.ID, &item.RunID, &item.PositionID, &item.CandidateKey, &item.Kind, &item.Decision,
			&item.PolicyStatus, &item.ExecutionStatus, &enter, &exit, &conf, &expected,
			&fundingRet, &holdRet, &payback, &currentMid, &entryMid, &changeSince,
			&expectedBasis, &p10, &profit, &confBasis, &sampleCount, &modelVersion, &item.BasisForecastCalculatedAt,
			&rationale, &item.PromptVersion, &item.PolicyVersion, &modelReq, &modelAct, &item.RawResponse, &errMsg,
			&item.DecisionAt, &item.ExecutionStartedAt, &item.FillDeadline,
			&item.BaseAsset, &item.QuoteAsset, &item.LongCanonicalSymbol, &item.LongQuoteAsset,
			&item.ShortCanonicalSymbol, &item.ShortQuoteAsset, &item.StrategyVersion, &parseErr,
		); err != nil {
			return nil, err
		}
		item.EnterProbability = parseOptional(enter)
		item.ExitProbability = parseOptional(exit)
		item.Confidence = parseOptional(conf)
		item.ExpectedNetReturn24h = parseOptionalPtr(expected)
		item.FundingReturn24h = parseOptionalPtr(fundingRet)
		item.HoldingHorizonNetReturn = parseOptionalPtr(holdRet)
		item.ExpectedPaybackMinutes = parseOptionalPtr(payback)
		item.CurrentMidBasisBPS = parseOptionalPtr(currentMid)
		item.EntryMidBasisBPS = parseOptionalPtr(entryMid)
		item.BasisChangeSinceEntryBPS = parseOptionalPtr(changeSince)
		item.ExpectedBasisReturn24h = parseOptionalPtr(expectedBasis)
		item.BasisReturnP10 = parseOptionalPtr(p10)
		item.BasisProfitProbability = parseOptionalPtr(profit)
		item.BasisForecastConfidence = parseOptionalPtr(confBasis)
		item.BasisSampleCount = sampleCount
		item.BasisModelVersion = modelVersion
		item.Rationale = deref(rationale)
		item.ModelRequested = deref(modelReq)
		item.ModelActual = deref(modelAct)
		item.ErrorMessage = deref(errMsg)
		item.ParseErrorType = deref(parseErr)
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanTrades(rows pgx.Rows) ([]Trade, error) {
	out := make([]Trade, 0)
	for rows.Next() {
		var item Trade
		var qty, longPx, shortPx, notional, cost string
		if err := rows.Scan(
			&item.ID, &item.RunID, &item.PositionID, &item.DecisionID, &item.Side, &qty, &longPx, &shortPx,
			&notional, &cost, &item.FilledAt,
			&item.StrategyVersion, &item.BaseAsset, &item.Kind, &item.LongExchange, &item.LongSymbol, &item.LongProduct,
			&item.ShortExchange, &item.ShortSymbol, &item.ShortProduct,
		); err != nil {
			return nil, err
		}
		item.SharedBaseQty = parseDecimalString(qty)
		item.LongFillPrice = parseDecimalString(longPx)
		item.ShortFillPrice = parseDecimalString(shortPx)
		item.Notional = parseDecimalString(notional)
		item.Cost = parseDecimalString(cost)
		out = append(out, item)
	}
	return out, rows.Err()
}

func pageIDs[T any](items []T, limit int, id func(T) int64) ([]T, int64, error) {
	var next int64
	if len(items) > limit {
		next = id(items[limit-1])
		items = items[:limit]
	}
	return items, next, nil
}

func dec(value decimal.Decimal) string { return value.String() }

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullStrategy(value string) string {
	if strings.TrimSpace(value) == "" {
		return StrategyVersion
	}
	return value
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func parseOptional(value *string) decimal.Decimal {
	if ptr := parseOptionalPtr(value); ptr != nil {
		return *ptr
	}
	return decimal.Zero
}

func parseOptionalPtr(value *string) *decimal.Decimal {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	parsed := parseDecimalString(*value)
	return &parsed
}

func nullDecimal(value *decimal.Decimal) any {
	if value == nil {
		return nil
	}
	return value.String()
}

func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullStringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return nullString(*value)
}

func etagFor(version int64) string {
	return `"` + strconv.FormatInt(version, 10) + `"`
}

func ParseCursor(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if parsed < 0 {
		return 0
	}
	return parsed
}

func FormatCursor(value int64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}
