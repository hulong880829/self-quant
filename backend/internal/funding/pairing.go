package funding

import (
	"math"
	"strings"
)

const normalizedStableQuote = "USDT"

func RankingCanonicalSymbol(rate Rate) string {
	base := strings.ToUpper(strings.TrimSpace(rate.BaseAsset))
	if base == "" {
		return strings.ToUpper(strings.TrimSpace(rate.GlobalSymbol))
	}
	quote := strings.ToUpper(strings.TrimSpace(rate.QuoteAsset))
	if quote == "USDC" || quote == normalizedStableQuote {
		return base + normalizedStableQuote
	}
	return strings.ToUpper(strings.TrimSpace(rate.GlobalSymbol))
}

func HistorySourceSymbol(rate Rate) string {
	base := strings.ToUpper(strings.TrimSpace(rate.BaseAsset))
	quote := strings.ToUpper(strings.TrimSpace(rate.QuoteAsset))
	if base != "" && (quote == "USDC" || quote == normalizedStableQuote) {
		return base + quote
	}
	return strings.ToUpper(strings.TrimSpace(rate.GlobalSymbol))
}

func PairingSymbol(rate Rate) string {
	if isHyperCoreUSDC(rate) {
		return strings.ToUpper(strings.TrimSpace(rate.BaseAsset)) + normalizedStableQuote
	}
	return rate.GlobalSymbol
}

func PairingSymbols(rate Rate) []string {
	if isLighterUSDC(rate) {
		base := strings.ToUpper(strings.TrimSpace(rate.BaseAsset))
		return []string{base + "USDC", base + normalizedStableQuote}
	}
	return []string{PairingSymbol(rate)}
}

func PairingQuoteAsset(rate Rate) string {
	if isHyperCoreUSDC(rate) {
		return normalizedStableQuote
	}
	return rate.QuoteAsset
}

func PairableRates(left, right Rate) bool {
	if left.Exchange == right.Exchange || !strings.EqualFold(left.BaseAsset, right.BaseAsset) {
		return false
	}
	if !entropySpreadPairingAllowed(left, right) {
		return false
	}
	if !multipliersCompatible(left, right) {
		return false
	}
	if isHyperCoreUSDC(left) || isHyperCoreUSDC(right) {
		if isHyperCoreUSDC(left) && isHyperCoreUSDC(right) {
			return true
		}
		if isHyperCoreUSDC(left) && isLighterUSDC(right) {
			return true
		}
		if isHyperCoreUSDC(right) && isLighterUSDC(left) {
			return true
		}
		return (isHyperCoreUSDC(left) && isUSDTContract(right)) ||
			(isHyperCoreUSDC(right) && isUSDTContract(left))
	}
	if (isLighterUSDC(left) && isUSDTContract(right)) ||
		(isLighterUSDC(right) && isUSDTContract(left)) {
		return true
	}
	return left.GlobalSymbol == right.GlobalSymbol
}

var entropySpreadPairingSymbols = map[string]struct{}{
	"io:ANTH": {},
	"io:SNDK": {},
}

func entropySpreadPairingAllowed(left, right Rate) bool {
	if !strings.EqualFold(left.Exchange, "entropy") &&
		!strings.EqualFold(right.Exchange, "entropy") {
		return true
	}
	symbol := left.ExchangeSymbol
	if strings.EqualFold(right.Exchange, "entropy") {
		symbol = right.ExchangeSymbol
	}
	_, ok := entropySpreadPairingSymbols[symbol]
	return ok
}

func isHyperCoreUSDC(rate Rate) bool {
	return isHyperliquidUSDC(rate) || isEntropyUSDC(rate)
}

func isHyperliquidUSDC(rate Rate) bool {
	return strings.EqualFold(rate.Exchange, "hyperliquid") &&
		strings.EqualFold(rate.QuoteAsset, "USDC")
}

func isEntropyUSDC(rate Rate) bool {
	return strings.EqualFold(rate.Exchange, "entropy") &&
		strings.EqualFold(rate.QuoteAsset, "USDC")
}

func isLighterUSDC(rate Rate) bool {
	return strings.EqualFold(rate.Exchange, "lighter") &&
		strings.EqualFold(rate.QuoteAsset, "USDC")
}

func isUSDTContract(rate Rate) bool {
	if strings.EqualFold(rate.Exchange, "hyperliquid") ||
		strings.EqualFold(rate.Exchange, "entropy") {
		return false
	}
	return strings.EqualFold(rate.QuoteAsset, normalizedStableQuote)
}

func usesMultiplierPairing(rate Rate) bool {
	switch strings.ToLower(rate.Exchange) {
	case "aster", "lighter":
		return true
	default:
		return false
	}
}

func NormalizeContractMultiplier(value float64) float64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 1
	}
	return value
}

func multipliersCompatible(left, right Rate) bool {
	if !usesMultiplierPairing(left) && !usesMultiplierPairing(right) {
		return true
	}
	if !validContractMultiplier(left.ContractMultiplier) ||
		!validContractMultiplier(right.ContractMultiplier) {
		return false
	}
	leftMultiplier := left.ContractMultiplier
	rightMultiplier := right.ContractMultiplier
	scale := math.Max(math.Abs(leftMultiplier), math.Abs(rightMultiplier))
	return math.Abs(leftMultiplier-rightMultiplier) <= scale*1e-9
}

func validContractMultiplier(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func canonicalSpreadKey(left, right Rate) string {
	first := left.Exchange + "\x00" + left.ExchangeSymbol
	second := right.Exchange + "\x00" + right.ExchangeSymbol
	if first > second {
		first, second = second, first
	}
	return first + "\x00" + second
}

func FilterRatesByExchange(rates []Rate, allowed map[string]bool) []Rate {
	if len(allowed) == 0 {
		return rates
	}
	result := make([]Rate, 0, len(rates))
	for _, rate := range rates {
		if allowed[strings.ToLower(rate.Exchange)] {
			result = append(result, rate)
		}
	}
	return result
}
