package jev

import (
	"fmt"

	"github.com/shopspring/decimal"
)

func RoundTripCost(notional, costBPS decimal.Decimal) decimal.Decimal {
	if !notional.IsPositive() || !costBPS.IsPositive() {
		return decimal.Zero
	}
	return notional.Mul(costBPS).Div(decimal.NewFromInt(10000))
}

func HalfRoundTripCost(notional, costBPS decimal.Decimal) decimal.Decimal {
	return RoundTripCost(notional, costBPS).Div(decimal.NewFromInt(2))
}

func SharedBaseQty(notional, longAsk, shortBid decimal.Decimal) (decimal.Decimal, error) {
	if !notional.IsPositive() {
		return decimal.Zero, fmt.Errorf("notional must be positive")
	}
	if !longAsk.IsPositive() || !shortBid.IsPositive() {
		return decimal.Zero, fmt.Errorf("leg prices must be positive")
	}
	longCapacity := notional.Div(longAsk)
	shortCapacity := notional.Div(shortBid)
	if longCapacity.LessThan(shortCapacity) {
		return longCapacity, nil
	}
	return shortCapacity, nil
}

func OneLegNotional(qty, price decimal.Decimal) decimal.Decimal {
	if !qty.IsPositive() || !price.IsPositive() {
		return decimal.Zero
	}
	return qty.Mul(price)
}
