package trader

import (
	"context"
	"errors"
	"strings"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

var errCircuitOpenOrderMismatch = errors.New("persisted order does not match venue result")

func (e *ArbitrageExecutor) ReconcileCircuitOpen(
	ctx context.Context,
	combination ArbitrageCombination,
) error {
	return e.recoverCircuitOpen(ctx, combination)
}

func (e *ArbitrageExecutor) confirmCircuitOpenOrder(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
) (Order, error) {
	if explicitVenueRejectedZeroFill(order) || confirmedAbsentReliableZeroFill(order) {
		return order, nil
	}
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
