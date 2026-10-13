package trader

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

type orderAverageSnapshot struct {
	status, filled, average, failures, errCode, errMsg, nextReconcile string
	lastStream                                                        *time.Time
	updatedAt                                                         time.Time
}

func TestArbitrageFillAverageCompensateRepositoryCAS(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "fill-avg-cas", true)
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_orders SET average_price=0, status='rejected',
			client_order_id='cas-b', venue_order_id='venue-b'
		WHERE id=$1::uuid`, env.orderB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_orders SET filled_quantity=0, average_price=0, status='rejected'
		WHERE id=$1::uuid`, env.orderA.ID); err != nil {
		t.Fatal(err)
	}

	listed, err := env.repo.ListMissingArbitrageOrderAverages(ctx, []string{env.combo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != env.orderB.ID {
		t.Fatalf("listed=%+v", listed)
	}

	before := loadOrderAverageSnapshot(t, ctx, env, env.orderB.ID)
	var executionStatus, comboStatus, comboPhase string
	if err := env.pool.QueryRow(ctx, `
		SELECT e.status,c.status,COALESCE(c.one_shot_phase,'')
		FROM trader_arbitrage_executions e
		JOIN trader_arbitrage_combinations c ON c.id=e.combination_id
		WHERE e.id=$1::uuid`, env.execution.ID,
	).Scan(&executionStatus, &comboStatus, &comboPhase); err != nil {
		t.Fatal(err)
	}

	updated, err := env.repo.TrySetArbitrageOrderAverage(ctx, env.orderB.ID, "101", "0.009")
	if err != nil || !updated {
		t.Fatalf("first cas updated=%v err=%v", updated, err)
	}
	after := loadOrderAverageSnapshot(t, ctx, env, env.orderB.ID)
	if after.status != before.status || after.filled != before.filled ||
		after.nextReconcile != before.nextReconcile ||
		after.failures != before.failures || after.errCode != before.errCode ||
		after.errMsg != before.errMsg {
		t.Fatalf("cas changed protected fields: before=%+v after=%+v", before, after)
	}
	if streamChanged(before.lastStream, after.lastStream) {
		t.Fatalf("last_stream_event_at changed: %v -> %v", before.lastStream, after.lastStream)
	}
	if after.average != "101" {
		t.Fatalf("average=%s", after.average)
	}
	listed, err = env.repo.ListMissingArbitrageOrderAverages(ctx, []string{env.combo.ID})
	if err != nil || len(listed) != 0 {
		t.Fatalf("priced order still listed=%+v err=%v", listed, err)
	}
	again, err := env.repo.TrySetArbitrageOrderAverage(ctx, env.orderB.ID, "99", "0.009")
	if err != nil || again {
		t.Fatalf("second cas updated=%v err=%v", again, err)
	}
	kept := loadOrderAverageSnapshot(t, ctx, env, env.orderB.ID)
	if kept.average != "101" {
		t.Fatalf("second cas overwrote average=%s", kept.average)
	}
	var laterExecution, laterStatus, laterPhase string
	if err := env.pool.QueryRow(ctx, `
		SELECT e.status,c.status,COALESCE(c.one_shot_phase,'')
		FROM trader_arbitrage_executions e
		JOIN trader_arbitrage_combinations c ON c.id=e.combination_id
		WHERE e.id=$1::uuid`, env.execution.ID,
	).Scan(&laterExecution, &laterStatus, &laterPhase); err != nil {
		t.Fatal(err)
	}
	if laterExecution != executionStatus || laterStatus != comboStatus || laterPhase != comboPhase {
		t.Fatalf("execution/combination changed: %s %s %s", laterExecution, laterStatus, laterPhase)
	}
}

func TestArbitragePositionMetricsFillAverageRestoresSpread(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "fill-avg-steem", true)
	setAccountContractFees(t, ctx, env.pool, env.accountA, "0.0002", "0.0004")
	setAccountContractFees(t, ctx, env.pool, env.accountB, "-0.0002", "-0.0005")
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_orders SET average_price=0 WHERE id=$1::uuid`, env.orderB.ID); err != nil {
		t.Fatal(err)
	}

	now := env.combo.CreatedAt.Add(2 * time.Hour)
	before := persistMetrics(t, ctx, env.repo, env.combo.ID, "100", "101", now).Combination
	beforeQuality, _, _ := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if before.AverageEntrySpreadBps != "" || before.LegBAverageEntryPrice != "" {
		t.Fatalf("missing leg B average still produced spread: %+v", before)
	}

	var clientID, venueID, filled string
	if err := env.pool.QueryRow(ctx, `
		SELECT COALESCE(client_order_id,''),COALESCE(venue_order_id,''),filled_quantity::text
		FROM trader_orders WHERE id=$1::uuid`, env.orderB.ID,
	).Scan(&clientID, &venueID, &filled); err != nil {
		t.Fatal(err)
	}
	adapter := &countingGetOrderAdapter{}
	compensator := NewArbitrageFillAverageCompensator(
		env.repo,
		stubRecentAverages{items: []orderstream.RecentFillAverage{{
			Key: orderstream.Key{
				Account: strconv.FormatInt(env.accountB, 10),
				Venue:   "okx",
				Product: "perpetual",
			},
			ClientOrderID:    clientID,
			VenueOrderID:     venueID,
			CumulativeFilled: filled,
			AveragePrice:     "101",
		}}},
		NewInstrumentCatalog(env.pool, time.Minute),
		stubAverageCompensateCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": adapter, "binance": adapter}),
		nil, "token", time.Second, slog.Default(),
	)
	compensator.Compensate(ctx, []string{env.combo.ID})
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	afterSnap := loadOrderAverageSnapshot(t, ctx, env, env.orderB.ID)
	if afterSnap.average != "101" {
		t.Fatalf("leg B average=%s", afterSnap.average)
	}

	updated := persistMetrics(t, ctx, env.repo, env.combo.ID, "100", "101", now.Add(time.Minute)).Combination
	quality, _, _ := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if updated.LegAAverageEntryPrice != "100" || updated.LegBAverageEntryPrice != "101" ||
		updated.AverageEntrySpreadBps != "100" {
		t.Fatalf("restored metrics=%+v", updated)
	}
	if updated.NotionalExposureSeconds == "" || updated.NotionalExposureSeconds == "0" ||
		updated.HoldingSeconds == "" || updated.HoldingSeconds == "0" {
		t.Fatalf("exposure/holding missing: %+v", updated)
	}
	if updated.CombinedPositionAnnualized == "" || quality != metricsQualityComplete {
		t.Fatalf("annualized=%s quality=%s beforeQuality=%s",
			updated.CombinedPositionAnnualized, quality, beforeQuality)
	}
}

func loadOrderAverageSnapshot(
	t *testing.T,
	ctx context.Context,
	env metricsTestEnv,
	orderID string,
) orderAverageSnapshot {
	t.Helper()
	var snap orderAverageSnapshot
	var lastStream *time.Time
	if err := env.pool.QueryRow(ctx, `
		SELECT status,filled_quantity::text,trim_scale(average_price)::text,
		       next_reconcile_at::text,last_stream_event_at,updated_at,
		       reconcile_failures::text,COALESCE(error_code,''),COALESCE(error_message,'')
		FROM trader_orders WHERE id=$1::uuid`, orderID,
	).Scan(
		&snap.status, &snap.filled, &snap.average,
		&snap.nextReconcile, &lastStream, &snap.updatedAt,
		&snap.failures, &snap.errCode, &snap.errMsg,
	); err != nil {
		t.Fatal(err)
	}
	snap.lastStream = lastStream
	return snap
}

func streamChanged(before, after *time.Time) bool {
	if before == nil && after == nil {
		return false
	}
	if before == nil || after == nil {
		return true
	}
	return !before.Equal(*after)
}
