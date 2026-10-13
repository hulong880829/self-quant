package exchange

import (
	"encoding/json"
	"strings"
)

const (
	AssetClassStock              = "stock"
	AssetClassETF                = "etf"
	AssetClassCrypto             = "crypto"
	AssetClassFX                 = "fx"
	AssetClassCommodity          = "commodity"
	AssetClassTradfiUnclassified = "tradfi_unclassified"
	AssetClassIndex              = "index"
	AssetClassPreIPO             = "pre_ipo"
	AssetClassSyntheticIndex     = "synthetic_index"
	AssetClassUnknown            = "unknown"

	PricePolicySame             = "same"
	PricePolicyStablecoinParity = "stablecoin_parity"
	PricePolicyForbid           = "forbid"

	SecTypeSTK    = "STK"
	SecTypeCrypto = "CRYPTO"
	SecTypeCash   = "CASH"
	SecTypeCmdty  = "CMDTY"
)

// UnderlyingReference is the venue's reference identity for one perpetual.
// PerpQuoteCurrency is the contract quote. UnderlyingCurrency is the IB
// instrument currency. Price policy is derived from those two currencies.
type UnderlyingReference struct {
	AssetClass             string
	Symbol                 string
	Region                 string
	PerpQuoteCurrency      string
	UnderlyingCurrency     string
	NativeListingCurrency  string
	PerpPriceCurrency      string
	PerpSettlementCurrency string
	FXBase                 string
	FXQuote                string
	PriceConversionPolicy  string
	Multiplier             float64
	FullName               string
	Source                 string
	PreMarket              bool
	Rewritten              bool
	DollarConverted        bool
}

func (r UnderlyingReference) Normalized() UnderlyingReference {
	r.AssetClass = strings.ToLower(strings.TrimSpace(r.AssetClass))
	r.Symbol = strings.ToUpper(strings.TrimSpace(r.Symbol))
	r.Region = strings.ToUpper(strings.TrimSpace(r.Region))
	r.PerpQuoteCurrency = strings.ToUpper(strings.TrimSpace(r.PerpQuoteCurrency))
	r.UnderlyingCurrency = strings.ToUpper(strings.TrimSpace(r.UnderlyingCurrency))
	r.NativeListingCurrency = strings.ToUpper(strings.TrimSpace(r.NativeListingCurrency))
	r.PerpPriceCurrency = strings.ToUpper(strings.TrimSpace(r.PerpPriceCurrency))
	r.PerpSettlementCurrency = strings.ToUpper(strings.TrimSpace(r.PerpSettlementCurrency))
	r.FXBase = strings.ToUpper(strings.TrimSpace(r.FXBase))
	r.FXQuote = strings.ToUpper(strings.TrimSpace(r.FXQuote))
	r.FullName = strings.TrimSpace(r.FullName)
	r.Source = strings.TrimSpace(r.Source)
	if r.Multiplier <= 0 {
		r.Multiplier = 1
	}
	priceCurrency := r.PerpPriceCurrency
	if priceCurrency == "" && r.DollarConverted {
		priceCurrency = "USD"
	}
	r.PriceConversionPolicy = PriceConversionPolicy(priceCurrency, r.UnderlyingCurrency)
	return r
}

func PriceConversionPolicy(perpQuote, underlying string) string {
	quote := strings.ToUpper(strings.TrimSpace(perpQuote))
	currency := strings.ToUpper(strings.TrimSpace(underlying))
	if quote == "" || currency == "" {
		return PricePolicyForbid
	}
	if quote == currency {
		return PricePolicySame
	}
	if stablecoin(quote) && stablecoin(currency) {
		return PricePolicyStablecoinParity
	}
	return PricePolicyForbid
}

func stablecoin(currency string) bool {
	switch currency {
	case "USD", "USDT", "USDC":
		return true
	default:
		return false
	}
}

func (r UnderlyingReference) CacheKey() string {
	item := r.Normalized()
	return strings.Join([]string{item.AssetClass, item.Symbol, item.UnderlyingCurrency, item.Region}, "|")
}

func (r UnderlyingReference) SecType() string {
	switch r.Normalized().AssetClass {
	case AssetClassStock, AssetClassETF:
		return SecTypeSTK
	case AssetClassCrypto:
		return SecTypeCrypto
	case AssetClassFX:
		return SecTypeCash
	case AssetClassCommodity:
		return SecTypeCmdty
	default:
		return ""
	}
}

// Routable reports whether this identity has an IB query. Crypto, indexes,
// pre-IPO, bonds, rewritten tickers, and missing official classes do not.
func (r UnderlyingReference) Routable() bool {
	return len(r.IBQueries()) > 0
}

func ListingCurrency(region string) string {
	switch strings.ToUpper(strings.TrimSpace(region)) {
	case "US":
		return "USD"
	case "HK":
		return "HKD"
	case "JP":
		return "JPY"
	default:
		return ""
	}
}

func PrimaryExchanges(region string) []string {
	switch strings.ToUpper(strings.TrimSpace(region)) {
	case "US":
		return []string{"NASDAQ", "NYSE", "ARCA", "AMEX"}
	case "HK":
		return []string{"SEHK"}
	case "JP":
		return []string{"TSEJ"}
	default:
		return nil
	}
}

func ExchangeInRegion(region, primaryExchange string) bool {
	wanted := strings.ToUpper(strings.TrimSpace(primaryExchange))
	for _, item := range PrimaryExchanges(region) {
		if item == wanted {
			return true
		}
	}
	return false
}

// Confirmed commodities stay empty until a product is explicitly listed as
// SYMBOL|EXCHANGE. An empty region never confirms a commodity.
var confirmedCommodities = map[string]struct{}{}

func ConfirmedCommodity(symbol, exchange string) bool {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	exchange = strings.ToUpper(strings.TrimSpace(exchange))
	if symbol == "" || exchange == "" {
		return false
	}
	_, ok := confirmedCommodities[symbol+"|"+exchange]
	return ok
}

func CryptoReference(symbol, perpQuote, source string) UnderlyingReference {
	return UnderlyingReference{
		AssetClass:         AssetClassCrypto,
		Symbol:             symbol,
		PerpQuoteCurrency:  perpQuote,
		UnderlyingCurrency: "USD",
		Multiplier:         1,
		Source:             source,
	}.Normalized()
}

func StockReference(symbol, region, perpQuote, fullName, source string) UnderlyingReference {
	listing := ListingCurrency(region)
	ref := UnderlyingReference{
		AssetClass:             AssetClassStock,
		Symbol:                 symbol,
		Region:                 region,
		PerpQuoteCurrency:      perpQuote,
		PerpSettlementCurrency: perpQuote,
		UnderlyingCurrency:     listing,
		NativeListingCurrency:  listing,
		Multiplier:             1,
		FullName:               fullName,
		Source:                 source,
	}
	if strings.EqualFold(strings.TrimSpace(region), "US") {
		ref.PerpPriceCurrency = "USD"
	}
	return ref.Normalized()
}

func ETFReference(symbol, region, perpQuote, fullName, source string) UnderlyingReference {
	ref := StockReference(symbol, region, perpQuote, fullName, source)
	ref.AssetClass = AssetClassETF
	return ref.Normalized()
}

func (item *Instrument) SetUnderlying(ref UnderlyingReference) {
	ref = ref.Normalized()
	item.Underlying = ref
	item.USEquityPerpetual = false
	item.TradefiClassificationSource = ""
	item.Metadata = storeUnderlying(item.Metadata, ref)
}

func storeUnderlying(metadata json.RawMessage, ref UnderlyingReference) json.RawMessage {
	fields := map[string]any{}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &fields)
	}
	delete(fields, "usEquityPerpetual")
	delete(fields, "tradefiClassificationSource")
	if ref.AssetClass == "" {
		delete(fields, "underlyingReference")
	} else {
		fields["underlyingReference"] = ref
	}
	result, err := json.Marshal(fields)
	if err != nil {
		return metadata
	}
	return result
}

func TagCryptoPerpetuals(items []Instrument, source string) []Instrument {
	for i := range items {
		if items[i].ContractType == ContractTypePerpetual && items[i].Underlying.AssetClass == "" {
			items[i].SetUnderlying(CryptoReference(items[i].BaseAsset, items[i].QuoteAsset, source))
		}
	}
	return items
}

func UnderlyingFromMetadata(metadata json.RawMessage) UnderlyingReference {
	fields := map[string]any{}
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return UnderlyingReference{}
	}
	raw, ok := fields["underlyingReference"]
	if !ok {
		return UnderlyingReference{}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return UnderlyingReference{}
	}
	var ref UnderlyingReference
	if err := json.Unmarshal(encoded, &ref); err != nil {
		return UnderlyingReference{}
	}
	return ref.Normalized()
}
