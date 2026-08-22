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
