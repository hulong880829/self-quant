package trader

import (
	"strings"

	"github.com/shopspring/decimal"
)

var tenThousand = decimal.NewFromInt(10_000)

func calculateArbitrageSpreads(
	legABid, legAAsk, legBBid, legBAsk decimal.Decimal,
) (askSpread, bidSpread decimal.Decimal, ok bool) {
	if !legABid.IsPositive() || !legAAsk.IsPositive() ||
		!legBBid.IsPositive() || !legBAsk.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}
	askSpread = legBAsk.Div(legAAsk).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
	bidSpread = legBBid.Div(legABid).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
	return askSpread, bidSpread, true
}

func arbitrageTriggered(
	askSpread, bidSpread, askThreshold, bidThreshold decimal.Decimal,
) string {
	if askSpread.GreaterThanOrEqual(askThreshold) {
		return "ask"
	}
	if bidSpread.LessThanOrEqual(bidThreshold) {
		return "bid"
	}
	return ""
}

type arbitragePositionPlan struct {
	Direction         string
	Effect            string
	ReduceOnly        bool
	RequestedNotional decimal.Decimal
}

func planArbitragePosition(
	position, target, order decimal.Decimal,
	direction string,
) (arbitragePositionPlan, bool) {
	if !target.IsPositive() || !order.IsPositive() {
		return arbitragePositionPlan{}, false
	}
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "ask":
		if position.GreaterThanOrEqual(target) {
			return arbitragePositionPlan{}, false
		}
		if position.IsNegative() {
			size := decimal.Min(order, position.Abs())
			if !size.IsPositive() {
				return arbitragePositionPlan{}, false
			}
			return arbitragePositionPlan{
				Direction: "ask", Effect: "close", ReduceOnly: true, RequestedNotional: size,
			}, true
		}
		size := decimal.Min(order, target.Sub(position))
		if !size.IsPositive() {
			return arbitragePositionPlan{}, false
		}
		return arbitragePositionPlan{
			Direction: "ask", Effect: "open", ReduceOnly: false, RequestedNotional: size,
		}, true
	case "bid":
		if position.LessThanOrEqual(target.Neg()) {
			return arbitragePositionPlan{}, false
		}
		if position.IsPositive() {
			size := decimal.Min(order, position)
			if !size.IsPositive() {
				return arbitragePositionPlan{}, false
			}
			return arbitragePositionPlan{
				Direction: "bid", Effect: "close", ReduceOnly: true, RequestedNotional: size,
			}, true
		}
		size := decimal.Min(order, target.Add(position))
		if !size.IsPositive() {
			return arbitragePositionPlan{}, false
		}
		return arbitragePositionPlan{
			Direction: "bid", Effect: "open", ReduceOnly: false, RequestedNotional: size,
		}, true
	default:
		return arbitragePositionPlan{}, false
	}
}

func signedPositionDelta(direction string, fillA, fillB, mark decimal.Decimal) decimal.Decimal {
	completed := decimal.Min(fillA, fillB).Mul(mark)
	if !completed.IsPositive() {
		return decimal.Zero
	}
	if strings.EqualFold(strings.TrimSpace(direction), "bid") {
		return completed.Neg()
	}
	return completed
}

func executionNotional(combination ArbitrageCombination, execution ArbitrageExecution) decimal.Decimal {
	if requested := parsePositiveDecimal(execution.RequestedNotional); requested.IsPositive() {
		return requested
	}
	return parsePositiveDecimal(combination.OrderNotional)
}

func executionHasExposure(execution ArbitrageExecution) bool {
	return parseDecimal(execution.LegAFilledQuantity).IsPositive() ||
		parseDecimal(execution.LegBFilledQuantity).IsPositive()
}

func arbitrageBackoff(failures int) decimal.Decimal {
	if failures <= 0 {
		return decimal.NewFromInt(2)
	}
	seconds := 2 << (failures - 1)
	if seconds > 60 {
		seconds = 60
	}
	return decimal.NewFromInt(int64(seconds))
}

func arbitrageLegSides(direction string) (legA, legB string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "ask":
		return "buy", "sell", true
	case "bid":
		return "sell", "buy", true
	default:
		return "", "", false
	}
}

func baseQuantityForNotional(notional, price, step decimal.Decimal) decimal.Decimal {
	if !notional.IsPositive() || !price.IsPositive() {
		return decimal.Zero
	}
	return floorToStep(notional.Div(price), step)
}

func deltaNotional(
	legABaseFilled, legBBaseFilled, markPrice decimal.Decimal,
) decimal.Decimal {
	if !markPrice.IsPositive() {
		return decimal.Zero
	}
	return legABaseFilled.Sub(legBBaseFilled).Abs().Mul(markPrice)
}
