package exchange

import (
	"regexp"
	"strings"
)

// IBQuery is one ContractDetails request. Exchange is SMART for STK and
// precious-metal CMDTY, and IDEALPRO for CASH.
type IBQuery struct {
	SecType  string
	Symbol   string
	Currency string
	Exchange string
}

func (r UnderlyingReference) PriceCurrency() string {
	item := r.Normalized()
	if item.PerpPriceCurrency != "" {
		return item.PerpPriceCurrency
	}
	if item.DollarConverted {
		return "USD"
	}
	if item.AssetClass == AssetClassFX && item.FXQuote != "" {
		return item.FXQuote
	}
	return ""
}

// TradfiCandidate marks a stock or ETF that may later receive a listing.
// Category alone does not choose a country or currency.
func TradfiCandidate(class, symbol, perpQuote, source string) UnderlyingReference {
	return UnderlyingReference{
		AssetClass: class, Symbol: strings.ToUpper(strings.TrimSpace(symbol)),
		PerpQuoteCurrency: perpQuote, PerpSettlementCurrency: perpQuote, Source: source,
	}.Normalized()
}

func (r UnderlyingReference) IBQueries() []IBQuery {
	item := r.Normalized()
	if item.PreMarket || item.Rewritten || item.Symbol == "" {
		return nil
	}
	switch item.AssetClass {
	case AssetClassStock, AssetClassETF:
		if item.UnderlyingCurrency == "" {
			return nil
		}
		return []IBQuery{{
			SecType: SecTypeSTK, Symbol: item.Symbol,
			Currency: item.UnderlyingCurrency, Exchange: "SMART",
		}}
	case AssetClassFX:
		if item.FXBase == "" || item.FXQuote == "" {
			return nil
		}
		return []IBQuery{{
			SecType: SecTypeCash, Symbol: item.FXBase,
			Currency: item.FXQuote, Exchange: "IDEALPRO",
		}}
	case AssetClassCommodity:
		symbol := CommoditySymbol(item.Symbol)
		if symbol == "" || symbol == "GOLD" || symbol == "SILVER" {
			return nil
		}
		currency := item.UnderlyingCurrency
		if currency == "" {
			currency = "USD"
		}
		return []IBQuery{{
			SecType: SecTypeCmdty, Symbol: symbol, Currency: currency, Exchange: "SMART",
		}}
	default:
		return nil
	}
}

func USDStockReference(symbol, perpQuote, fullName, source string) UnderlyingReference {
	return UnderlyingReference{
		AssetClass:             AssetClassStock,
		Symbol:                 symbol,
		PerpQuoteCurrency:      perpQuote,
		PerpSettlementCurrency: perpQuote,
		UnderlyingCurrency:     "USD",
		NativeListingCurrency:  "USD",
		PerpPriceCurrency:      "USD",
		FullName:               fullName,
		Source:                 source,
	}.Normalized()
}

func USDETFReference(symbol, perpQuote, fullName, source string) UnderlyingReference {
	ref := USDStockReference(symbol, perpQuote, fullName, source)
	ref.AssetClass = AssetClassETF
	return ref.Normalized()
}

func RegionalStock(symbol, region, listing, perpQuote, fullName, source string) UnderlyingReference {
	return UnderlyingReference{
		AssetClass:             AssetClassStock,
		Symbol:                 symbol,
		Region:                 region,
		PerpQuoteCurrency:      perpQuote,
		PerpSettlementCurrency: perpQuote,
		UnderlyingCurrency:     listing,
		NativeListingCurrency:  listing,
		FullName:               fullName,
		Source:                 source,
	}.Normalized()
}

func FXReference(base, quote, settlement, source string) UnderlyingReference {
	return UnderlyingReference{
		AssetClass:             AssetClassFX,
		Symbol:                 base + quote,
		FXBase:                 base,
		FXQuote:                quote,
		PerpQuoteCurrency:      settlement,
		PerpSettlementCurrency: settlement,
		PerpPriceCurrency:      quote,
		UnderlyingCurrency:     quote,
		Source:                 source,
	}.Normalized()
}

func CommodityReference(symbol, currency, source string) UnderlyingReference {
	symbol = CommoditySymbol(symbol)
	if currency == "" {
		currency = "USD"
	}
	return UnderlyingReference{
		AssetClass:             AssetClassCommodity,
		Symbol:                 symbol,
		PerpQuoteCurrency:      currency,
		PerpSettlementCurrency: currency,
		UnderlyingCurrency:     currency,
		PerpPriceCurrency:      currency,
		Source:                 source,
	}.Normalized()
}

func PreMarketReference(symbol, source string) UnderlyingReference {
	return UnderlyingReference{
		AssetClass: AssetClassPreIPO, Symbol: symbol, PreMarket: true, Source: source,
	}.Normalized()
}

func CommoditySymbol(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "XAU", "XAUUSD":
		return "XAUUSD"
	case "XAG", "XAGUSD":
		return "XAGUSD"
	case "XPT", "XPTUSD":
		return "XPTUSD"
	case "XPD", "XPDUSD":
		return "XPDUSD"
	default:
		return strings.ToUpper(strings.TrimSpace(raw))
	}
}

var fxQuotes = []string{"USDT", "USDC", "USD", "JPY", "EUR", "GBP", "AUD", "CAD", "CHF", "NZD", "CNH", "HKD", "SGD", "KRW", "CNY"}

func SplitFXPair(raw string) (string, string, bool) {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	symbol = strings.TrimSuffix(symbol, "USDT")
	symbol = strings.TrimSuffix(symbol, "USDC")
	symbol = strings.ReplaceAll(symbol, "/", "")
	symbol = strings.ReplaceAll(symbol, "-", "")
	symbol = strings.ReplaceAll(symbol, "_", "")
	for _, quote := range fxQuotes {
		if quote == "USDT" || quote == "USDC" {
			continue
		}
		if strings.HasSuffix(symbol, quote) && len(symbol) > len(quote) {
			base := strings.TrimSuffix(symbol, quote)
			if base != "" && base != quote {
				return base, quote, true
			}
		}
	}
	return "", "", false
}

var (
	hkSuffix = regexp.MustCompile(`^0*([0-9]+)HK$`)
	hkPrefix = regexp.MustCompile(`^HK0*([0-9]+)$`)
	digits   = regexp.MustCompile(`^0*([0-9]+)$`)
)

func NormalizeEquityTicker(region, raw string) (string, bool) {
	region = strings.ToUpper(strings.TrimSpace(region))
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	switch region {
	case "HK":
		return hkEquityCode(raw)
	case "CN", "KR":
		return numericCode(raw)
	case "US":
		return CanonicalUSTicker(raw), true
	default:
		return strings.ToUpper(raw), true
	}
}

func hkEquityCode(raw string) (string, bool) {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	if match := hkSuffix.FindStringSubmatch(upper); match != nil {
		return stripLeadingZeros(match[1]), true
	}
	if match := hkPrefix.FindStringSubmatch(upper); match != nil {
		return stripLeadingZeros(match[1]), true
	}
	return numericCode(upper)
}

func numericCode(raw string) (string, bool) {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	match := digits.FindStringSubmatch(upper)
	if match == nil {
		return "", false
	}
	return stripLeadingZeros(match[1]), true
}

func stripLeadingZeros(raw string) string {
	trimmed := strings.TrimLeft(raw, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

func CanonicalUSTicker(raw string) string {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	symbol = strings.ReplaceAll(symbol, ".", " ")
	return strings.Join(strings.Fields(symbol), " ")
}

func TickersEqual(left, right string) bool {
	if canonical := CanonicalUSTicker(left); canonical != "" && canonical == CanonicalUSTicker(right) {
		return true
	}
	leftCode, leftOK := numericCode(left)
	rightCode, rightOK := numericCode(right)
	return leftOK && rightOK && leftCode == rightCode
}

func ExecutionAdapter(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "binance", "okx", "bybit", "bitget", "gate", "hyperliquid", "lighter", "aster":
		return true
	default:
		return false
	}
}

func AnnotationIdentity(text string) (UnderlyingReference, bool) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return UnderlyingReference{}, false
	}
	upper := strings.ToUpper(raw)
	if strings.Contains(upper, "BOND") {
		return UnderlyingReference{AssetClass: AssetClassUnknown, Source: "annotation.bond"}, true
	}
	if strings.HasPrefix(upper, "KRX:") {
		code, ok := numericCode(strings.TrimPrefix(upper, "KRX:"))
		if !ok {
			return UnderlyingReference{}, false
		}
		ref := RegionalStock(code, "KR", "KRW", "USDC", "", "annotation")
		return ref, true
	}
	if strings.HasPrefix(upper, "HK") {
		code, ok := hkEquityCode(upper)
		if ok {
			return RegionalStock(code, "HK", "HKD", "USDC", "", "annotation"), true
		}
	}
	if strings.Contains(raw, "/") {
		parts := strings.Split(upper, "/")
		if len(parts) == 2 {
			base, quote := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			switch base {
			case "XAU", "XAG", "XPT", "XPD":
				if quote == "USD" {
					return CommodityReference(base+"USD", "USD", "annotation"), true
				}
			}
			if base != "" && quote != "" {
				return FXReference(base, quote, quote, "annotation"), true
			}
		}
	}
	return UnderlyingReference{}, false
}
