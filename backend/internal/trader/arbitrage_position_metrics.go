package trader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

const (
	arbitragePositionMetricsInterval = 10 * time.Minute
	annualSeconds                    = 365 * 24 * 60 * 60
	metricsQualityComplete           = "complete"
	metricsQualityEstimated          = "estimated"
	metricsQualityPartial            = "partial"
)

const arbitrageMetricsWriteGuardSQL = `
	status='running'
	AND (metrics_calculated_at IS NULL OR metrics_calculated_at < $14)
	AND NOT EXISTS (
		SELECT 1 FROM trader_arbitrage_executions e
		WHERE e.combination_id=trader_arbitrage_combinations.id
		  AND e.status NOT IN ('completed','failed','canceled','dry_run')
	)
	AND NOT EXISTS (
		SELECT 1 FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=trader_arbitrage_combinations.id
		  AND o.filled_quantity>0
		  AND o.status NOT IN ('filled','canceled','rejected','expired')
	)`

type arbitrageMetricFill struct {
	orderID  string
	leg      string
	side     string
	quantity decimal.Decimal
	price    decimal.Decimal
	at       time.Time
}

type arbitrageLegCost struct {
	position decimal.Decimal
	average  decimal.Decimal
	realized decimal.Decimal
}

type arbitrageReplay struct {
	fills            []arbitrageMetricFill
	grossTurnover    decimal.Decimal
	coverageComplete bool
	orderRowCount    int
	fillRowCount     int
	loadDuration     time.Duration
}

type arbitragePositionMetricsStore interface {
	ListRunningArbitrageCombinationsForMetrics(context.Context) ([]ArbitrageCombination, error)
	PersistRunningArbitragePositionMetrics(
		context.Context, string, string, string, time.Time,
	) (arbitragePositionMetricsPersistResult, error)
	MarkOneShotExiting(context.Context, string, int64, string, map[string]any) (ArbitrageCombination, bool, error)
}

type MetricsApplyReason string

const (
	MetricsApplied         MetricsApplyReason = "applied"
	MetricsActiveWork      MetricsApplyReason = "active_work"
	MetricsVersionConflict MetricsApplyReason = "version_conflict"
	MetricsNotReady        MetricsApplyReason = "not_ready"
	MetricsInactive        MetricsApplyReason = "inactive"
	MetricsAlreadyCurrent  MetricsApplyReason = "already_current"
)

type arbitragePositionMetricsPersistResult struct {
	Combination ArbitrageCombination
	Applied     bool
	Reason      MetricsApplyReason
	ExitBasis   *oneShotExitBasis
}

type arbitrageRowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type accountFeeSnapshot struct {
	exchange      string
	spotMaker     *string
	spotTaker     *string
	contractMaker *string
	contractTaker *string
}

type terminalFilledOrder struct {
	accountID    int64
	exchange     string
	contractType string
	role         string
	quantity     decimal.Decimal
	price        decimal.Decimal
}

type settledFundingEvent struct {
	leg  string
	at   time.Time
	rate decimal.Decimal
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func applyArbitrageFill(state *arbitrageLegCost, fill arbitrageMetricFill) {
	signed := fill.quantity
	if fill.side == "sell" {
		signed = signed.Neg()
	}
	if state.position.IsZero() {
		state.position = signed
		state.average = fill.price
		return
	}
	if state.position.Sign() == signed.Sign() {
		total := state.position.Abs().Add(signed.Abs())
		state.average = state.average.Mul(state.position.Abs()).
			Add(fill.price.Mul(signed.Abs())).Div(total)
		state.position = state.position.Add(signed)
		return
	}
	closed := decimal.Min(state.position.Abs(), signed.Abs())
	state.realized = state.realized.Add(
		fill.price.Sub(state.average).
			Mul(closed).
			Mul(decimal.NewFromInt(int64(state.position.Sign()))),
	)
	next := state.position.Add(signed)
	if next.IsZero() {
		state.position = decimal.Zero
		state.average = decimal.Zero
		return
	}
	if next.Sign() != state.position.Sign() {
		state.average = fill.price
	}
	state.position = next
}

func applyAttributedArbitrageFill(
	state *arbitrageLegCost,
	venuePosition *decimal.Decimal,
	fill arbitrageMetricFill,
	askSign decimal.Decimal,
) {
	signed := fill.quantity
	if fill.side == "sell" {
		signed = signed.Neg()
	}
	askQuantity := signed.Mul(askSign)
	venueAskPosition := venuePosition.Mul(askSign)
	attributed := decimal.Zero
	if askQuantity.IsPositive() {
		neutralized := decimal.Min(
			askQuantity,
			decimal.Max(venueAskPosition.Neg(), decimal.Zero),
		)
		attributed = askQuantity.Sub(neutralized)
	} else if askQuantity.IsNegative() {
		owned := state.position.Mul(askSign)
		if owned.IsPositive() {
			attributed = decimal.Min(askQuantity.Abs(), owned)
		}
	}
	if attributed.IsPositive() {
		attributedFill := fill
		attributedFill.quantity = attributed
		applyArbitrageFill(state, attributedFill)
	}
	*venuePosition = venuePosition.Add(signed)
}

func replayArbitrageCosts(
	fills []arbitrageMetricFill,
	until time.Time,
	baselines ...decimal.Decimal,
) (arbitrageLegCost, arbitrageLegCost) {
	var legA, legB arbitrageLegCost
	venueA, venueB := decimal.Zero, decimal.Zero
	if len(baselines) >= 2 {
		venueA, venueB = baselines[0], baselines[1]
	}
	for _, fill := range fills {
		if !until.IsZero() && fill.at.After(until) {
			break
		}
		if fill.leg == "a" {
			applyAttributedArbitrageFill(
				&legA, &venueA, fill, decimal.NewFromInt(1),
			)
		} else if fill.leg == "b" {
			applyAttributedArbitrageFill(
				&legB, &venueB, fill, decimal.NewFromInt(-1),
			)
		}
	}
	return legA, legB
}

func replayArbitrageExposure(
	fills []arbitrageMetricFill,
	start, end time.Time,
	baselines ...decimal.Decimal,
) (decimal.Decimal, decimal.Decimal) {
	if end.Before(start) {
		return decimal.Zero, decimal.Zero
	}
	var legA, legB arbitrageLegCost
	venueA, venueB := decimal.Zero, decimal.Zero
	if len(baselines) >= 2 {
		venueA, venueB = baselines[0], baselines[1]
	}
	cursor := start
	notionalSeconds := decimal.Zero
	holdingSeconds := decimal.Zero
	settle := func(at time.Time) {
		if at.Before(cursor) {
			return
		}
		elapsed := decimal.NewFromFloat(at.Sub(cursor).Seconds())
		paired := decimal.Min(legA.position.Abs(), legB.position.Abs())
		if paired.IsPositive() {
			mark := legA.average.Add(legB.average).Div(decimal.NewFromInt(2))
			if mark.IsPositive() {
				notionalSeconds = notionalSeconds.Add(paired.Mul(mark).Mul(elapsed))
				holdingSeconds = holdingSeconds.Add(elapsed)
			}
		}
		cursor = at
	}
	for _, fill := range fills {
		if fill.at.Before(start) {
			if fill.leg == "a" {
				applyAttributedArbitrageFill(
					&legA, &venueA, fill, decimal.NewFromInt(1),
				)
			} else {
				applyAttributedArbitrageFill(
					&legB, &venueB, fill, decimal.NewFromInt(-1),
				)
			}
			continue
		}
		if fill.at.After(end) {
			break
		}
		settle(fill.at)
		if fill.leg == "a" {
			applyAttributedArbitrageFill(
				&legA, &venueA, fill, decimal.NewFromInt(1),
			)
		} else {
			applyAttributedArbitrageFill(
				&legB, &venueB, fill, decimal.NewFromInt(-1),
			)
		}
	}
	settle(end)
	return notionalSeconds, holdingSeconds
}

func loadArbitrageReplay(
	ctx context.Context,
	queryer arbitrageRowsQuerier,
	combinationID string,
	strictAverageCoverage bool,
) (arbitrageReplay, error) {
	started := time.Now()
	query := `/* trader:arbitrage_replay_orders */
		SELECT o.id::text,o.arbitrage_leg,o.side,
		       o.filled_quantity::text,o.average_price::text,
		       COALESCE(o.last_venue_event_at,o.updated_at)
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=$1::uuid
		  AND o.status IN ('filled','canceled','rejected','expired')
		  AND o.filled_quantity>0
		  AND o.arbitrage_leg IN ('a','b')`
	if !strictAverageCoverage {
		query += `
		  AND o.average_price>0`
	}
	orderRows, err := queryer.Query(ctx, query, combinationID)
	if err != nil {
		return arbitrageReplay{}, err
	}
	replay := arbitrageReplay{coverageComplete: true}
	for orderRows.Next() {
		var id, leg, side, quantityRaw, priceRaw string
		var at time.Time
		if scanErr := orderRows.Scan(&id, &leg, &side, &quantityRaw, &priceRaw, &at); scanErr != nil {
			orderRows.Close()
			return arbitrageReplay{}, scanErr
		}
		quantity, quantityErr := decimal.NewFromString(quantityRaw)
		price, priceErr := decimal.NewFromString(priceRaw)
		if quantityErr != nil || priceErr != nil || !quantity.IsPositive() {
			orderRows.Close()
			return arbitrageReplay{}, fmt.Errorf("invalid arbitrage order aggregate %s", id)
		}
		if !price.IsPositive() {
			if strictAverageCoverage {
				replay.coverageComplete = false
				continue
			}
			orderRows.Close()
			return arbitrageReplay{}, fmt.Errorf("invalid arbitrage order aggregate %s", id)
		}
		replay.fills = append(replay.fills, arbitrageMetricFill{
			orderID: id, leg: leg, side: side, quantity: quantity, price: price, at: at,
		})
		replay.orderRowCount++
		replay.fillRowCount++
		replay.grossTurnover = replay.grossTurnover.Add(quantity.Mul(price))
	}
	if err := orderRows.Err(); err != nil {
		orderRows.Close()
		return arbitrageReplay{}, err
	}
	orderRows.Close()
	sort.SliceStable(replay.fills, func(i, j int) bool {
		if replay.fills[i].at.Equal(replay.fills[j].at) {
			return replay.fills[i].orderID < replay.fills[j].orderID
		}
		return replay.fills[i].at.Before(replay.fills[j].at)
	})
	replay.loadDuration = time.Since(started)
	return replay, nil
}

func nullableDecimal(value decimal.Decimal, available bool) any {
	if !available {
		return nil
	}
	return value.String()
}

type arbitragePositionMetricsWritePlan struct {
	fillOK                 bool
	writeAnnualized        bool
	quality                string
	fundingHistoryComplete bool
}

func planArbitragePositionMetricsWrite(
	replay arbitrageReplay,
	feesComplete, fundingComplete bool,
) arbitragePositionMetricsWritePlan {
	fillOK := replay.coverageComplete
	writeAnnualized := feesComplete && fundingComplete && fillOK
	quality := metricsQualityPartial
	if writeAnnualized {
		if replay.coverageComplete {
			quality = metricsQualityComplete
		} else {
			quality = metricsQualityEstimated
		}
	}
	return arbitragePositionMetricsWritePlan{
		fillOK:                 fillOK,
		writeAnnualized:        writeAnnualized,
		quality:                quality,
		fundingHistoryComplete: fundingComplete,
	}
}

func calculateCombinedAnnualized(
	profit, exposure, holding decimal.Decimal,
	thresholdHours float64,
) (decimal.Decimal, bool) {
	if !holding.IsPositive() || !exposure.IsPositive() {
		return decimal.Zero, false
	}
	effectiveSeconds := decimal.Max(
		holding,
		decimal.NewFromFloat(thresholdHours*3600),
	)
	denominator := exposure.Div(holding).Mul(effectiveSeconds)
	if !denominator.IsPositive() {
		return decimal.Zero, false
	}
	return profit.Mul(decimal.NewFromInt(annualSeconds)).Div(denominator), true
}

func tradingFeeCostRate(exchange string, storedRate decimal.Decimal) decimal.Decimal {
	if strings.EqualFold(strings.TrimSpace(exchange), "okx") {
		return storedRate.Neg()
	}
	return storedRate
}

func accountFeeRate(
	snapshot accountFeeSnapshot,
	role, contractType string,
) (decimal.Decimal, bool) {
	maker := strings.EqualFold(strings.TrimSpace(role), "maker")
	spot := strings.EqualFold(strings.TrimSpace(contractType), "spot")
	var raw *string
	switch {
	case spot && maker:
		raw = snapshot.spotMaker
	case spot && !maker:
		raw = snapshot.spotTaker
	case !spot && maker:
		raw = snapshot.contractMaker
	default:
		raw = snapshot.contractTaker
	}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return decimal.Zero, false
	}
	parsed, err := decimal.NewFromString(strings.TrimSpace(*raw))
	if err != nil {
		return decimal.Zero, false
	}
	return parsed, true
}

func estimateTradingFees(
	orders []terminalFilledOrder,
	accounts map[int64]accountFeeSnapshot,
) (decimal.Decimal, bool) {
	total := decimal.Zero
	for _, order := range orders {
		snapshot, ok := accounts[order.accountID]
		if !ok {
			return decimal.Zero, false
		}
		stored, ok := accountFeeRate(snapshot, order.role, order.contractType)
		if !ok {
			return decimal.Zero, false
		}
		notional := order.quantity.Mul(order.price).Abs()
		total = total.Add(notional.Mul(tradingFeeCostRate(order.exchange, stored)))
	}
	return total, true
}

func (r *Repository) ListRunningArbitrageCombinationsForMetrics(
	ctx context.Context,
) ([]ArbitrageCombination, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations c
		WHERE c.status='running'
		  AND c.venue_baseline_captured_at IS NOT NULL
		  AND EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=c.id AND o.filled_quantity>0
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=c.id
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=c.id
			  AND o.filled_quantity>0
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		  )`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ArbitrageCombination
	for rows.Next() {
		var item ArbitrageCombination
		if scanErr := rows.Scan(arbitrageCombinationScanTargets(&item)...); scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) PersistRunningArbitragePositionMetrics(
	ctx context.Context,
	combinationID, legAMidRaw, legBMidRaw string,
	now time.Time,
) (arbitragePositionMetricsPersistResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	legAMid, errA := decimal.NewFromString(legAMidRaw)
	legBMid, errB := decimal.NewFromString(legBMidRaw)
	pricesAvailable := errA == nil && errB == nil && legAMid.IsPositive() && legBMid.IsPositive()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var createdAt time.Time
	var venueBaselineCapturedAt *time.Time
	var baselineARaw, baselineBRaw *string
	var legAExchange, legBExchange, status string
	if err = tx.QueryRow(ctx, `
		SELECT created_at,venue_baseline_captured_at,
		       leg_a_venue_baseline_base_position::text,
		       leg_b_venue_baseline_base_position::text,
		       leg_a_exchange,leg_b_exchange,status
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`, combinationID,
	).Scan(
		&createdAt, &venueBaselineCapturedAt, &baselineARaw, &baselineBRaw,
		&legAExchange, &legBExchange, &status,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return arbitragePositionMetricsPersistResult{Reason: MetricsInactive}, nil
		}
		return arbitragePositionMetricsPersistResult{}, err
	}
	if !strings.EqualFold(status, "running") {
		item, loadErr := r.getArbitrageCombinationByID(ctx, tx, combinationID)
		if loadErr != nil {
			return arbitragePositionMetricsPersistResult{}, loadErr
		}
		return arbitragePositionMetricsPersistResult{
			Combination: item, Reason: MetricsInactive,
		}, nil
	}
	if venueBaselineCapturedAt == nil {
		item, loadErr := r.getArbitrageCombinationByID(ctx, tx, combinationID)
		return arbitragePositionMetricsPersistResult{
			Combination: item, Reason: MetricsNotReady,
		}, loadErr
	}

	strictAverageCoverage := strings.EqualFold(legAExchange, "hyperliquid") ||
		strings.EqualFold(legBExchange, "hyperliquid")
	replay, err := loadArbitrageReplay(ctx, tx, combinationID, strictAverageCoverage)
	if err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	r.recordArbitrageReplay(replay)
	baselineA := parseDecimal(stringValue(baselineARaw))
	baselineB := parseDecimal(stringValue(baselineBRaw))
	costFills := replay.fills
	if !replay.coverageComplete {
		costFills = nil
	}
	legA, legB := replayArbitrageCosts(
		costFills, time.Time{}, baselineA, baselineB,
	)
	exposure, holding := replayArbitrageExposure(
		costFills, createdAt, now, baselineA, baselineB,
	)

	funding, fundingComplete, err := loadArbitrageFundingPnL(
		ctx, tx, combinationID, replay, now, baselineA, baselineB,
	)
	if err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	feeOrders, err := loadTerminalFilledOrders(ctx, tx, combinationID)
	if err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	accounts, err := loadAccountFeeSnapshots(ctx, tx, combinationID)
	if err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	estimatedFee, feesComplete := estimateTradingFees(feeOrders, accounts)

	pairedInventory := legA.position.IsPositive() != legB.position.IsPositive() &&
		!legA.position.IsZero() && !legB.position.IsZero() &&
		legA.average.IsPositive() && legB.average.IsPositive()
	entrySpread := decimal.Zero
	if pairedInventory {
		entrySpread = legB.average.Div(legA.average).
			Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(10000))
	}
	legAUnrealized := decimal.Zero
	legBUnrealized := decimal.Zero
	if pricesAvailable && !legA.position.IsZero() && legA.average.IsPositive() {
		legAUnrealized = legA.position.Mul(legAMid.Sub(legA.average))
	}
	if pricesAvailable && !legB.position.IsZero() && legB.average.IsPositive() {
		legBUnrealized = legB.position.Mul(legBMid.Sub(legB.average))
	}
	realized := legA.realized.Add(legB.realized)
	writePlan := planArbitragePositionMetricsWrite(replay, feesComplete, fundingComplete)
	fillOK := writePlan.fillOK

	annualized := decimal.Zero
	thresholdHours := 0.0
	writeAnnualized := writePlan.writeAnnualized
	if writeAnnualized {
		var thresholdErr error
		thresholdHours, thresholdErr = r.arbitrageFundingThresholdHours(ctx, tx, combinationID)
		if thresholdErr != nil {
			return arbitragePositionMetricsPersistResult{}, thresholdErr
		}
		profit := realized.Add(funding).Sub(estimatedFee)
		annualized, writeAnnualized = calculateCombinedAnnualized(
			profit, exposure, holding, thresholdHours,
		)
	}
	quality := metricsQualityPartial
	if writeAnnualized {
		if replay.coverageComplete {
			quality = metricsQualityComplete
		} else {
			quality = metricsQualityEstimated
		}
	}

	var updated ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			gross_turnover_notional=CASE WHEN $20::bool THEN $2::numeric ELSE gross_turnover_notional END,
			leg_a_average_entry_price=CASE WHEN $20::bool THEN $3::numeric ELSE NULL END,
			leg_b_average_entry_price=CASE WHEN $20::bool THEN $4::numeric ELSE NULL END,
			average_entry_spread_bps=CASE WHEN $20::bool THEN $5::numeric ELSE NULL END,
			leg_a_unrealized_pnl=CASE WHEN NOT $20::bool THEN NULL WHEN $6::bool THEN $7::numeric ELSE leg_a_unrealized_pnl END,
			leg_b_unrealized_pnl=CASE WHEN NOT $20::bool THEN NULL WHEN $6::bool THEN $8::numeric ELSE leg_b_unrealized_pnl END,
			realized_spread_pnl=CASE WHEN $20::bool THEN $9::numeric ELSE realized_spread_pnl END,
			estimated_funding_pnl=CASE WHEN $10::bool THEN $11::numeric ELSE estimated_funding_pnl END,
			notional_exposure_seconds=CASE WHEN $20::bool THEN $12::numeric ELSE notional_exposure_seconds END,
			holding_seconds=CASE WHEN $20::bool THEN $13::numeric ELSE holding_seconds END,
			exposure_updated_at=CASE WHEN $20::bool THEN $14 ELSE exposure_updated_at END,
			combined_position_annualized=CASE WHEN NOT $20::bool THEN NULL WHEN $15::bool THEN $16::numeric ELSE combined_position_annualized END,
			estimated_trading_fee=CASE WHEN NOT $20::bool THEN NULL WHEN $15::bool THEN $17::numeric ELSE estimated_trading_fee END,
			metrics_calculated_at=CASE WHEN NOT $20::bool THEN NULL WHEN $15::bool THEN $14 ELSE metrics_calculated_at END,
			funding_history_complete=$18,
			metrics_quality=$19
		WHERE id=$1::uuid AND `+arbitrageMetricsWriteGuardSQL+`
		RETURNING `+arbitrageCombinationColumns,
		combinationID,
		replay.grossTurnover.String(),
		nullableDecimal(legA.average, !legA.position.IsZero() && legA.average.IsPositive()),
		nullableDecimal(legB.average, !legB.position.IsZero() && legB.average.IsPositive()),
		nullableDecimal(entrySpread, pairedInventory),
		pricesAvailable,
		nullableDecimal(legAUnrealized, pricesAvailable),
		nullableDecimal(legBUnrealized, pricesAvailable),
		realized.String(),
		fundingComplete,
		funding.String(),
		exposure.String(),
		holding.String(),
		now,
		writeAnnualized,
		nullableDecimal(annualized, writeAnnualized),
		nullableDecimal(estimatedFee, writeAnnualized),
		writePlan.fundingHistoryComplete,
		quality,
		fillOK,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		reason, classErr := classifyMetricsApplyMiss(ctx, tx, combinationID, now)
		if classErr != nil {
			return arbitragePositionMetricsPersistResult{}, classErr
		}
		current, loadErr := r.getArbitrageCombinationByID(ctx, tx, combinationID)
		if loadErr != nil {
			return arbitragePositionMetricsPersistResult{}, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return arbitragePositionMetricsPersistResult{}, err
		}
		return arbitragePositionMetricsPersistResult{
			Combination: current, Reason: reason,
		}, nil
	}
	if err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return arbitragePositionMetricsPersistResult{}, err
	}
	result := arbitragePositionMetricsPersistResult{
		Combination: updated, Applied: true, Reason: MetricsApplied,
	}
	if writeAnnualized {
		if basis, ok := buildOneShotExitBasis(
			updated, legA, legB, realized, funding, estimatedFee,
			exposure, holding, thresholdHours, accounts, now,
		); ok {
			result.ExitBasis = basis
		}
	}
	return result, nil
}

func (r *Repository) getArbitrageCombinationByID(
	ctx context.Context,
	queryer interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	combinationID string,
) (ArbitrageCombination, error) {
	var item ArbitrageCombination
	err := queryer.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`, combinationID,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return item, err
}

func classifyMetricsApplyMiss(
	ctx context.Context,
	tx interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	combinationID string,
	now time.Time,
) (MetricsApplyReason, error) {
	var status string
	var calculatedAt *time.Time
	var hasActiveExecution, hasActiveFilledOrder bool
	err := tx.QueryRow(ctx, `
		SELECT c.status,c.metrics_calculated_at,
		       EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=c.id
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		       ),
		       EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=c.id
			  AND o.filled_quantity>0
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		       )
		FROM trader_arbitrage_combinations c
		WHERE c.id=$1::uuid`, combinationID,
	).Scan(&status, &calculatedAt, &hasActiveExecution, &hasActiveFilledOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return MetricsInactive, nil
	}
	if err != nil {
		return "", fmt.Errorf("classify metrics apply miss: %w", err)
	}
	return metricsApplyMissReason(status, calculatedAt, now, hasActiveExecution, hasActiveFilledOrder), nil
}

func metricsApplyMissReason(
	status string,
	calculatedAt *time.Time,
	now time.Time,
	hasActiveExecution, hasActiveFilledOrder bool,
) MetricsApplyReason {
	if !strings.EqualFold(status, "running") {
		return MetricsInactive
	}
	if hasActiveExecution || hasActiveFilledOrder {
		return MetricsActiveWork
	}
	if calculatedAt != nil && !calculatedAt.Before(now) {
		return MetricsAlreadyCurrent
	}
	return MetricsNotReady
}

func loadAccountFeeSnapshots(
	ctx context.Context,
	queryer interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	combinationID string,
) (map[int64]accountFeeSnapshot, error) {
	rows, err := queryer.Query(ctx, `
		SELECT a.id,a.exchange,
		       a.spot_maker_fee_rate::text,a.spot_taker_fee_rate::text,
		       a.contract_maker_fee_rate::text,a.contract_taker_fee_rate::text
		FROM trading_accounts a
		JOIN trader_arbitrage_combinations c
		  ON a.id IN (c.leg_a_trading_account_id,c.leg_b_trading_account_id)
		WHERE c.id=$1::uuid`, combinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := make(map[int64]accountFeeSnapshot)
	for rows.Next() {
		var id int64
		var snapshot accountFeeSnapshot
		if scanErr := rows.Scan(
			&id, &snapshot.exchange,
			&snapshot.spotMaker, &snapshot.spotTaker,
			&snapshot.contractMaker, &snapshot.contractTaker,
		); scanErr != nil {
			return nil, scanErr
		}
		accounts[id] = snapshot
	}
	return accounts, rows.Err()
}

func loadTerminalFilledOrders(
	ctx context.Context,
	queryer interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	combinationID string,
) ([]terminalFilledOrder, error) {
	rows, err := queryer.Query(ctx, `
		SELECT o.trading_account_id,o.exchange,o.contract_type,
		       COALESCE(o.arbitrage_role,''),
		       o.filled_quantity::text,o.average_price::text
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=$1::uuid
		  AND o.filled_quantity>0
		  AND o.average_price>0
		  AND o.status IN ('filled','canceled','rejected','expired')`, combinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []terminalFilledOrder
	for rows.Next() {
		var item terminalFilledOrder
		var quantityRaw, priceRaw string
		if scanErr := rows.Scan(
			&item.accountID, &item.exchange, &item.contractType, &item.role,
			&quantityRaw, &priceRaw,
		); scanErr != nil {
			return nil, scanErr
		}
		quantity, quantityErr := decimal.NewFromString(quantityRaw)
		price, priceErr := decimal.NewFromString(priceRaw)
		if quantityErr != nil || priceErr != nil || !quantity.IsPositive() || !price.IsPositive() {
			return nil, fmt.Errorf("invalid terminal filled order for combination %s", combinationID)
		}
		item.quantity = quantity
		item.price = price
		orders = append(orders, item)
	}
	return orders, rows.Err()
}

func loadArbitrageFundingPnL(
	ctx context.Context,
	queryer interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	combinationID string,
	replay arbitrageReplay,
	now time.Time,
	baselineA, baselineB decimal.Decimal,
) (decimal.Decimal, bool, error) {
	rows, err := queryer.Query(ctx, `
		SELECT l.leg,f.funding_time,f.funding_rate::text
		FROM (
			SELECT id,'a'::text AS leg,leg_a_instrument_id AS instrument_id,
			       leg_a_contract_type AS contract_type,venue_baseline_captured_at
			FROM trader_arbitrage_combinations WHERE id=$1::uuid
			UNION ALL
			SELECT id,'b',leg_b_instrument_id,leg_b_contract_type,venue_baseline_captured_at
			FROM trader_arbitrage_combinations WHERE id=$1::uuid
		) l
		JOIN funding_rates f ON f.instrument_id=l.instrument_id AND f.record_kind='settled'
		WHERE l.contract_type='perpetual'
		  AND l.venue_baseline_captured_at IS NOT NULL
		  AND f.funding_time>=l.venue_baseline_captured_at
		  AND f.funding_time<=$2
		ORDER BY f.funding_time,l.leg`, combinationID, now)
	if err != nil {
		return decimal.Zero, false, err
	}
	defer rows.Close()
	total := decimal.Zero
	for rows.Next() {
		var event settledFundingEvent
		var rateRaw string
		if scanErr := rows.Scan(&event.leg, &event.at, &rateRaw); scanErr != nil {
			return decimal.Zero, false, scanErr
		}
		rate, rateErr := decimal.NewFromString(rateRaw)
		if rateErr != nil {
			return decimal.Zero, false, fmt.Errorf("invalid funding rate for combination %s", combinationID)
		}
		event.rate = rate
		legA, legB := replayArbitrageCosts(replay.fills, event.at, baselineA, baselineB)
		state := legA
		if event.leg == "b" {
			state = legB
		}
		if !state.position.IsZero() && state.average.IsPositive() {
			total = total.Add(state.position.Mul(state.average).Mul(event.rate).Neg())
		}
	}
	if err := rows.Err(); err != nil {
		return decimal.Zero, false, err
	}
	return total, true, nil
}

func (r *Repository) arbitrageFundingThresholdHours(
	ctx context.Context,
	tx pgx.Tx,
	combinationID string,
) (float64, error) {
	var hours float64
	err := tx.QueryRow(ctx, `
		WITH legs AS (
			SELECT leg_a_instrument_id AS instrument_id,leg_a_contract_type AS contract_type
			FROM trader_arbitrage_combinations WHERE id=$1::uuid
			UNION ALL
			SELECT leg_b_instrument_id,leg_b_contract_type
			FROM trader_arbitrage_combinations WHERE id=$1::uuid
		), periods AS (
			SELECT COALESCE(
				(SELECT interval_hours FROM funding_rates
				 WHERE instrument_id=l.instrument_id AND record_kind='current' LIMIT 1),
				(SELECT interval_hours FROM funding_rates
				 WHERE instrument_id=l.instrument_id AND record_kind='settled'
				 ORDER BY funding_time DESC LIMIT 1)
			) AS interval_hours
			FROM legs l WHERE l.contract_type='perpetual'
		)
		SELECT COALESCE(MAX(interval_hours),8)::float8 FROM periods`, combinationID,
	).Scan(&hours)
	return hours, err
}
