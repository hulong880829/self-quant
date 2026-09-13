package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

const (
	confirmedAbsentRejectAfter               = 3
	errorConfirmedAbsentAfterUncertainSubmit = "CONFIRMED_ABSENT_AFTER_UNCERTAIN_SUBMIT"
)

// confirmedAbsentZeroFillReject returns a zero-fill rejected snapshot when a
// locally non-terminal order with no venue ID and no fills has been
// ConfirmedAbsent enough times. Callers must not write status "closed":
// exchange.normalizeStatus maps closed to filled.
func confirmedAbsentZeroFillReject(order Order) (exchange.Result, bool) {
	if terminalStatus(order.Status) {
		return exchange.Result{}, false
	}
	if strings.TrimSpace(order.VenueOrderID) != "" {
		return exchange.Result{}, false
	}
	filled := strings.TrimSpace(order.FilledQuantity)
	if filled != "" && !strictZeroFilledQuantity(filled) {
		return exchange.Result{}, false
	}
	if order.AbsenceConfirmations+1 < confirmedAbsentRejectAfter {
		return exchange.Result{}, false
	}
	return exchange.Result{
		Status:         "rejected",
		FilledQuantity: "0",
		ErrorCode:      errorConfirmedAbsentAfterUncertainSubmit,
	}, true
}

// resolveVenueOrder keeps single-order lookup and bounded history resolution
// consistent across synchronous queries, recovery, arbitrage and reconciliation.
// ConfirmedAbsent is evidence that both history and active-order queries
// completed successfully; callers must still decide whether their stored state
// is already authoritative or remains uncertain.
func resolveVenueOrder(
	ctx context.Context,
	adapter exchange.Adapter,
	credentials exchange.Credentials,
	request exchange.QueryRequest,
) (exchange.OrderResolution, error) {
	result, err := adapter.GetOrder(ctx, credentials, request)
	if err == nil && resolvedVenueOrder(result) {
		return exchange.OrderResolution{
			Result: result,
			Found:  true,
			Active: !terminalStatus(result.Status),
		}, nil
	}
	if err != nil && !errors.Is(err, exchange.ErrOrderNotFound) {
		return exchange.OrderResolution{Result: result}, err
	}
	resolver, ok := adapter.(exchange.OrderResolver)
	if !ok {
		if err != nil {
			return exchange.OrderResolution{Result: result}, err
		}
		return exchange.OrderResolution{Result: result}, fmt.Errorf(
			"%w: venue returned no resolved order state",
			exchange.ErrUncertain,
		)
	}
	resolution, resolveErr := resolver.ResolveOrder(ctx, credentials, request)
	if resolveErr != nil {
		return resolution, resolveErr
	}
	if resolution.Found {
		if resolution.ConfirmedAbsent {
			return resolution, fmt.Errorf(
				"%w: resolver returned contradictory found and absent evidence",
				exchange.ErrUncertain,
			)
		}
		if !resolvedVenueOrder(resolution.Result) {
			return resolution, fmt.Errorf(
				"%w: resolver returned an invalid order state",
				exchange.ErrUncertain,
			)
		}
		resolution.Active = !terminalStatus(resolution.Result.Status)
		return resolution, nil
	}
	if resolution.ConfirmedAbsent {
		if resolution.Active || resolvedVenueOrder(resolution.Result) {
			return resolution, fmt.Errorf(
				"%w: resolver returned order state with confirmed absence",
				exchange.ErrUncertain,
			)
		}
		return resolution, nil
	}
	return resolution, fmt.Errorf(
		"%w: resolver returned neither an order nor confirmed absence",
		exchange.ErrUncertain,
	)
}

func resolvedVenueOrder(result exchange.Result) bool {
	status := strings.ToLower(strings.TrimSpace(result.Status))
	return status != "" && status != "unknown"
}

func strictZeroFilledQuantity(value string) bool {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	return err == nil && parsed.IsZero()
}

func confirmedAbsentReliableZeroFill(order Order) bool {
	return strings.EqualFold(strings.TrimSpace(order.Status), "rejected") &&
		strings.TrimSpace(order.VenueOrderID) == "" &&
		order.ErrorCode == errorConfirmedAbsentAfterUncertainSubmit &&
		strictZeroFilledQuantity(order.FilledQuantity)
}

func filledWithZeroQuantity(order Order) bool {
	return strings.EqualFold(strings.TrimSpace(order.Status), "filled") &&
		strictZeroFilledQuantity(order.FilledQuantity)
}
