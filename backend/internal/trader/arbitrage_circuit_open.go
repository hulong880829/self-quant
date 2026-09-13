package trader

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

var errCircuitOpenOrderMismatch = errors.New("persisted order does not match venue result")

func (e *ArbitrageExecutor) ReconcileCircuitOpen(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) error {
	if e == nil || e.store == nil {
		return nil
	}
	if !combination.CircuitOpen || combination.Status != "running" {
		return nil
	}
	if e.catalog == nil || e.credentials == nil || e.venues == nil || e.portfolios == nil {
		e.logger.Info("circuit open reconcile skipped",
			"combination_id", combination.ID, "reason", "dependencies_unavailable")
		return nil
	}
	timeout := e.timeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	orders, err := e.store.ListArbitrageOrders(
		queryCtx, combination.OwnerUsername, combination.ID,
	)
	if err != nil {
		return err
	}
	var current []Order
	for _, order := range orders {
		if order.ArbitrageExecutionID == execution.ID {
			current = append(current, order)
			continue
		}
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			e.logger.Info("circuit open reconcile skipped",
				"combination_id", combination.ID, "reason", "other_orders_unconfirmed")
			return nil
		}
	}

	instrumentA, credentialsA, adapterA, err := e.executionDependencies(
		queryCtx, combination.OwnerUsername, combination.LegA,
	)
	if err != nil {
		return err
	}
	if credentialsA.Exchange == "" {
		credentialsA.Exchange = combination.LegA.Exchange
	}
	instrumentB, credentialsB, adapterB, err := e.executionDependencies(
		queryCtx, combination.OwnerUsername, combination.LegB,
	)
	if err != nil {
		return err
	}
	if credentialsB.Exchange == "" {
		credentialsB.Exchange = combination.LegB.Exchange
	}
	for _, order := range current {
		if terminalStatus(order.Status) {
			continue
		}
		instrument, credentials, adapter := instrumentA, credentialsA, adapterA
		if order.ArbitrageLeg == "b" {
			instrument, credentials, adapter = instrumentB, credentialsB, adapterB
		}
		if _, confirmErr := e.confirmCircuitOpenOrder(
			queryCtx, credentials, instrument, adapter, order,
		); confirmErr != nil {
			e.logger.Info("circuit open reconcile skipped",
				"combination_id", combination.ID, "order_id", order.ID,
				"reason", "order_unconfirmed", "error", confirmErr)
			return nil
		}
	}

	snapshotA, err := e.circuitOpenAccountSnapshot(queryCtx, credentialsA)
	if err != nil {
		return err
	}
	snapshotB, err := e.circuitOpenAccountSnapshot(queryCtx, credentialsB)
	if err != nil {
		return err
	}
	venueA, err := venueBasePosition(snapshotA, combination.LegA, instrumentA)
	if err != nil {
		return err
	}
	venueB, err := venueBasePosition(snapshotB, combination.LegB, instrumentB)
	if err != nil {
		return err
	}
	markA := e.circuitOpenMark(combination.LegA, snapshotA)
	markB := e.circuitOpenMark(combination.LegB, snapshotB)
	if !markA.IsPositive() || !markB.IsPositive() {
		e.logger.Info("circuit open reconcile skipped",
			"combination_id", combination.ID, "reason", "valuation_incomplete")
		return nil
	}

	applied, ok, err := e.store.ApplyCircuitOpenExternalReconcile(
		ctx, circuitOpenExternalReconcileRequest{
			CombinationID:   combination.ID,
			ExecutionID:     execution.ID,
			ExpectedVersion: combination.Version,
			VenueA:          venueA,
			VenueB:          venueB,
			MarkA:           markA,
			MarkB:           markB,
			InstrumentA:     instrumentA,
			InstrumentB:     instrumentB,
		},
	)
	if err != nil {
		return err
	}
	if !ok {
		e.logger.Info("circuit open reconcile skipped",
			"combination_id", combination.ID, "reason", "recover_gates")
		return nil
	}
	e.logger.Info("circuit open externally reconciled",
		"combination_id", applied.ID,
		"execution_id", execution.ID,
		"position_notional", applied.PositionNotional,
		"target_notional", applied.TargetNotional,
		"over_target", arbitragePositionOverTarget(
			parseDecimal(applied.PositionNotional),
			parsePositiveDecimal(applied.TargetNotional),
		),
		"carry", applied.CarryBaseQuantity)
	return nil
}

func (e *ArbitrageExecutor) confirmCircuitOpenOrder(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
) (Order, error) {
	if e.orderStreams != nil {
		e.restFallbacks.Add(1)
	}
	resolution, err := resolveVenueOrder(
		ctx, adapter, toVenueCredentials(credentials), exchange.QueryRequest{
			Instrument:    toVenueInstrument(instrument),
			ClientOrderID: order.ClientOrderID,
			VenueOrderID:  order.VenueOrderID,
			CreatedAt:     order.CreatedAt,
		},
	)
	if err != nil {
		return order, err
	}
	if resolution.ConfirmedAbsent {
		if confirmedAbsentReliableZeroFill(order) ||
			(terminalStatus(order.Status) && !strings.EqualFold(order.Status, "filled")) {
			if e.service == nil {
				return order, nil
			}
			return e.service.persistResult(ctx, order.ID, "arbitrage_observe", exchange.Result{
				Status: order.Status, FilledQuantity: order.FilledQuantity,
				AveragePrice: order.AveragePrice, VenueOrderID: order.VenueOrderID,
				ErrorCode: order.ErrorCode, ErrorMessage: order.ErrorMessage,
			})
		}
		if strings.EqualFold(order.Status, "filled") || !terminalStatus(order.Status) {
			return order, exchange.ErrOrderNotFound
		}
		if e.service == nil {
			return order, nil
		}
		return e.service.persistResult(ctx, order.ID, "arbitrage_observe", exchange.Result{
			Status: order.Status, FilledQuantity: order.FilledQuantity,
			AveragePrice: order.AveragePrice, VenueOrderID: order.VenueOrderID,
		})
	}
	normalized := normalizeVenueResult(order, resolution.Result)
	if !terminalStatus(normalized.Status) {
		return order, ErrVenueUncertain
	}
	if e.service == nil {
		if !circuitOpenOrderMatchesVenue(order, normalized) {
			return order, errCircuitOpenOrderMismatch
		}
		return order, nil
	}
	persisted, err := e.service.persistResult(
		ctx, order.ID, "arbitrage_observe", normalized,
	)
	if err != nil {
		return order, err
	}
	if !circuitOpenOrderMatchesVenue(persisted, normalized) {
		return persisted, errCircuitOpenOrderMismatch
	}
	return persisted, nil
}

func (e *ArbitrageExecutor) circuitOpenAccountSnapshot(
	ctx context.Context,
	credentials Credentials,
) (portfolio.Snapshot, error) {
	if e.portfolios == nil {
		return portfolio.Snapshot{}, ErrUnsupportedExchange
	}
	return e.portfolios.Snapshot(ctx, credentials.Exchange, portfolio.Credentials{
		APIKey: credentials.APIKey, APISecret: credentials.APISecret,
		Passphrase:   credentials.Passphrase,
		AccountIndex: credentials.AccountIndex,
	})
}

func (e *ArbitrageExecutor) circuitOpenMark(
	leg ArbitrageLeg,
	snapshot portfolio.Snapshot,
) decimal.Decimal {
	if mark := portfolioAuditMark(snapshot, leg); mark.IsPositive() {
		return mark
	}
	if e.market == nil {
		return decimal.Zero
	}
	bbo, err := e.latestForLeg(leg)
	if err != nil {
		return decimal.Zero
	}
	bid := parsePositiveDecimal(bbo.BidPrice)
	ask := parsePositiveDecimal(bbo.AskPrice)
	if bid.IsPositive() && ask.IsPositive() {
		return bid.Add(ask).Div(decimal.NewFromInt(2))
	}
	if bid.IsPositive() {
		return bid
	}
	return ask
}
