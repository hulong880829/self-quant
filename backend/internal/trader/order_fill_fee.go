package trader

import (
	"strings"

	"github.com/shopspring/decimal"
)

func normalizeStoredFillFee(fill OrderFill) (any, any, string) {
	role := strings.ToLower(strings.TrimSpace(fill.LiquidityRole))
	switch role {
	case "maker", "taker", "unknown":
	default:
		role = "unknown"
	}
	if strings.TrimSpace(fill.TradeID) == "" {
		return nil, nil, role
	}
	rawAmount := strings.TrimSpace(fill.FeeAmount)
	currency := strings.ToUpper(strings.TrimSpace(fill.FeeCurrency))
	if rawAmount == "" || currency == "" {
		return nil, nil, role
	}
	amount, err := decimal.NewFromString(rawAmount)
	if err != nil {
		return nil, nil, role
	}
	return amount.String(), currency, role
}

func orderLiquidityRole(makerCount, takerCount, otherCount int) string {
	if otherCount > 0 || (makerCount == 0 && takerCount == 0) {
		return "unknown"
	}
	if makerCount > 0 && takerCount > 0 {
		return "mixed"
	}
	if makerCount > 0 {
		return "maker"
	}
	return "taker"
}
