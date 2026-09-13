package portfolio

import (
	"strings"

	"github.com/shopspring/decimal"
)

func walletDEXZeroOrEmpty(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return true
	}
	parsed, err := decimal.NewFromString(trimmed)
	if err != nil {
		return false
	}
	return parsed.IsZero()
}

func walletDEXStableUSD(asset string) bool {
	switch normalizeSymbol(asset) {
	case "USDC", "USDT", "USD":
		return true
	default:
		return false
	}
}

func walletDEXAmount(value string) (decimal.Decimal, bool) {
	if walletDEXZeroOrEmpty(value) {
		return decimal.Zero, false
	}
	parsed, err := parseDecimal(strings.TrimSpace(value))
	if err != nil {
		return decimal.Zero, false
	}
	return parsed, true
}

func walletDEXParseOptional(value string) (decimal.Decimal, error) {
	if walletDEXZeroOrEmpty(value) {
		return decimal.Zero, nil
	}
	return parseDecimal(strings.TrimSpace(value))
}

func walletDEXFirstMeaningful(values ...string) string {
	for _, value := range values {
		if !walletDEXZeroOrEmpty(value) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func walletDEXAdd(values ...string) string {
	sum := decimal.Zero
	for _, value := range values {
		parsed, ok := walletDEXAmount(value)
		if !ok {
			continue
		}
		sum = sum.Add(parsed)
	}
	return sum.String()
}

func walletDEXRiskPercent(maintenance, equity string) string {
	if walletDEXZeroOrEmpty(maintenance) || walletDEXZeroOrEmpty(equity) {
		return ""
	}
	return riskPercent(maintenance, equity)
}
