package funding

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"selfquant/backend/internal/exchange"
	"selfquant/backend/internal/funding/ibkr"
)

const (
	MatchVerifiedListing    = "verified_listing"
	MatchVerifiedIdentifier = "verified_identifier"
	MatchCandidate          = "candidate"
	MatchAmbiguous          = "ambiguous"
	MatchNotFound           = "not_found"
)

type PairMatch struct {
	MatchClass        string
	VerifiedAt        time.Time
	PerpPriceCurrency string
	Multiplier        float64
}

func resolutionFromContracts(ref exchange.UnderlyingReference, contracts []ibkr.Contract, now time.Time) IBResolution {
	ref = ref.Normalized()
	resolution := IBResolution{
		Symbol: ref.CacheKey(), UnderlyingSymbol: ref.Symbol, AssetClass: ref.AssetClass,
		UnderlyingCurrency: ref.UnderlyingCurrency, Region: ref.Region, SecType: ref.SecType(),
		LastVerified: now, NextVerify: now.Add(tradefiNegativeTTL),
		PairMatches: map[string]PairMatch{},
	}
	unique := map[int64]ibkr.Contract{}
	for _, contract := range contracts {
		if !contractMatchesReference(ref, contract) {
			continue
		}
		unique[contract.ConID] = contract
	}
	switch len(unique) {
	case 0:
		resolution.Status = MatchNotFound
	case 1:
		var contract ibkr.Contract
		for _, item := range unique {
			contract = item
		}
		conID := contract.ConID
		resolution.Active = true
		resolution.Status = "active"
		resolution.ConID = &conID
		resolution.PrimaryExchange = contract.PrimaryExchange
		resolution.LocalSymbol = contract.LocalSymbol
		resolution.TradingClass = contract.TradingClass
		resolution.LongName = contract.LongName
		resolution.NextVerify = now.Add(tradefiActiveTTL)
	default:
		resolution.Status = MatchAmbiguous
	}
	return resolution
}

func contractMatchesReference(ref exchange.UnderlyingReference, contract ibkr.Contract) bool {
	ref = ref.Normalized()
	if contract.ConID <= 0 || !secTypeAllowed(ref, contract.SecType) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(contract.Currency), ref.UnderlyingCurrency) {
		return false
	}
	wanted := ref.Symbol
	if ref.AssetClass == exchange.AssetClassFX {
		wanted = ref.FXBase
	}
	if !exchange.TickersEqual(contract.Symbol, wanted) && !exchange.TickersEqual(contract.LocalSymbol, wanted) {
		return false
	}
	switch ref.AssetClass {
	case exchange.AssetClassStock, exchange.AssetClassETF:
		return stockPrimaryAllowed(ref, contract.PrimaryExchange)
	case exchange.AssetClassCommodity:
		return !strings.EqualFold(contract.SecType, exchange.SecTypeSTK)
	default:
		return true
	}
}

func secTypeAllowed(ref exchange.UnderlyingReference, secType string) bool {
	secType = strings.ToUpper(strings.TrimSpace(secType))
	if ref.AssetClass == exchange.AssetClassCommodity {
		return secType == exchange.SecTypeCmdty || secType == exchange.SecTypeCash
	}
	return strings.EqualFold(secType, ref.SecType())
}

func stockPrimaryAllowed(ref exchange.UnderlyingReference, primary string) bool {
	region := ref.Region
	if region == "" && ref.UnderlyingCurrency == "USD" {
		region = "US"
	}
	switch region {
	case "KR", "CN":
		return strings.TrimSpace(primary) != ""
	default:
		return exchange.ExchangeInRegion(region, primary)
	}
}

func verifiedMatch(class string) bool {
	switch class {
	case MatchVerifiedListing, MatchVerifiedIdentifier:
		return true
	default:
		return false
	}
}

func verifiedClass(assetClass string) string {
	switch assetClass {
	case exchange.AssetClassStock, exchange.AssetClassETF:
		return MatchVerifiedListing
	case exchange.AssetClassFX, exchange.AssetClassCommodity:
		return MatchVerifiedIdentifier
	default:
		return ""
	}
}

func pricesMatch(perpPrice, multiplier, ibMid, threshold float64, policy string) bool {
	if policy != exchange.PricePolicySame && policy != exchange.PricePolicyStablecoinParity {
		return false
	}
	if multiplier <= 0 {
		multiplier = 1
	}
	if perpPrice <= 0 || ibMid <= 0 || threshold < 0 {
		return false
	}
	compared := perpPrice * multiplier
	return math.Abs(compared-ibMid)/ibMid <= threshold
}

func priceCompatible(perpPrice, ibCurrency string) bool {
	left := strings.ToUpper(strings.TrimSpace(perpPrice))
	right := strings.ToUpper(strings.TrimSpace(ibCurrency))
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	return stableCoin(left) && stableCoin(right)
}

func stableCoin(currency string) bool {
	switch currency {
	case "USD", "USDT", "USDC":
		return true
	default:
		return false
	}
}

func pairKey(exchangeName, exchangeSymbol string) string {
	return strings.ToLower(exchangeName) + "|" + exchangeSymbol
}

func (item IBResolution) metadataJSON() json.RawMessage {
	payload := map[string]any{
		"assetClass":         item.AssetClass,
		"underlyingSymbol":   item.UnderlyingSymbol,
		"underlyingCurrency": item.UnderlyingCurrency,
		"region":             item.Region,
		"secType":            item.SecType,
		"longName":           item.LongName,
		"candidateAliases":   item.Aliases,
		"pairMatches":        item.PairMatches,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

func resolutionFromMetadata(item *IBResolution, raw json.RawMessage) {
	fields := map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}
	item.AssetClass, _ = fields["assetClass"].(string)
	item.UnderlyingSymbol, _ = fields["underlyingSymbol"].(string)
	item.UnderlyingCurrency, _ = fields["underlyingCurrency"].(string)
	item.Region, _ = fields["region"].(string)
	item.SecType, _ = fields["secType"].(string)
	item.LongName, _ = fields["longName"].(string)
	if rawAliases, ok := fields["candidateAliases"].([]any); ok {
		for _, alias := range rawAliases {
			if text, ok := alias.(string); ok && text != "" {
				item.Aliases = append(item.Aliases, text)
			}
		}
	}
	encoded, err := json.Marshal(fields["pairMatches"])
	if err != nil {
		return
	}
	matches := map[string]PairMatch{}
	if err := json.Unmarshal(encoded, &matches); err == nil {
		item.PairMatches = matches
	}
}
