package exchange

import "testing"

func TestEquityTickerNormalizationIsReversibleFormatOnly(t *testing.T) {
	code, ok := NormalizeEquityTicker("HK", "1211HK")
	if !ok || code != "1211" {
		t.Fatalf("1211HK=%s ok=%v", code, ok)
	}
	code, ok = NormalizeEquityTicker("HK", "HK0700")
	if !ok || code != "700" {
		t.Fatalf("HK0700=%s ok=%v", code, ok)
	}
	code, ok = NormalizeEquityTicker("HK", "07747")
	if !ok || code != "7747" {
		t.Fatalf("07747=%s ok=%v", code, ok)
	}
	if _, ok := NormalizeEquityTicker("HK", "BYD"); ok {
		t.Fatal("company name must not become a listing code")
	}
	if _, ok := NormalizeEquityTicker("CN", "TENCENT"); ok {
		t.Fatal("TENCENT must not become a numeric code")
	}
	if CanonicalUSTicker("BRK.B") != CanonicalUSTicker("BRK B") {
		t.Fatal("share class punctuation must compare equal")
	}
	if TickersEqual("STXSTOCK", "STX") || TickersEqual("AAPU", "AAPL") {
		t.Fatal("ticker text must not be rewritten")
	}
	if !TickersEqual("00700", "700") {
		t.Fatal("numeric listing codes compare without leading zeros")
	}
}

func TestFXPriceCurrencyIsTheQuoteLeg(t *testing.T) {
	eur := FXReference("EUR", "USD", "USDT", "test")
	jpy := FXReference("USD", "JPY", "USDT", "test")
	if eur.PriceCurrency() != "USD" || jpy.PriceCurrency() != "JPY" {
		t.Fatalf("eur=%s jpy=%s", eur.PriceCurrency(), jpy.PriceCurrency())
	}
	queries := jpy.IBQueries()
	if len(queries) != 1 || queries[0].SecType != SecTypeCash || queries[0].Exchange != "IDEALPRO" || queries[0].Symbol != "USD" || queries[0].Currency != "JPY" {
		t.Fatalf("queries=%+v", queries)
	}
}

func TestCommodityDoesNotQueryStock(t *testing.T) {
	gold := CommodityReference("XAU", "USD", "test")
	queries := gold.IBQueries()
	if len(queries) != 1 || queries[0].SecType != SecTypeCmdty || queries[0].Symbol != "XAUUSD" || queries[0].Exchange != "SMART" {
		t.Fatalf("gold=%+v", queries)
	}
	paxg := CommodityReference("PAXG", "USD", "test").IBQueries()
	if len(paxg) != 1 || paxg[0].Symbol != "PAXG" || paxg[0].SecType != SecTypeCmdty {
		t.Fatalf("paxg=%+v", paxg)
	}
	if CommodityReference("GOLD", "USD", "test").Routable() {
		t.Fatal("GOLD needs an explicit XAU annotation")
	}
	cl := CommodityReference("CL", "USD", "test").IBQueries()
	if len(cl) != 1 || cl[0].SecType != SecTypeCmdty || cl[0].Symbol != "CL" {
		t.Fatalf("cl=%+v", cl)
	}
}

func TestVenueClassesFollowOfficialFields(t *testing.T) {
	openai := parseOKXInstruments([]okxInstrument{{
		InstID: "OPENAI-USDT-SWAP", InstType: "SWAP", InstFamily: "OPENAI-USDT", State: "live", CtValCcy: "OPENAI",
		BaseCcy: "OPENAI", QuoteCcy: "USDT", SettleCcy: "USDT", CtVal: "1", TickSz: "0.01", LotSz: "1", MinSz: "1",
		InstCategory: "3", RuleType: "pre_market",
	}}, ContractTypePerpetual)
	if openai[0].Underlying.Routable() {
		t.Fatalf("openai=%+v", openai[0].Underlying)
	}
	aapl := parseOKXInstruments([]okxInstrument{{
		InstID: "AAPL-USDT-SWAP", InstType: "SWAP", InstFamily: "AAPL-USDT", State: "live", CtValCcy: "AAPL",
		BaseCcy: "AAPL", QuoteCcy: "USDT", SettleCcy: "USDT", CtVal: "1", TickSz: "0.01", LotSz: "1", MinSz: "1",
		InstCategory: "3", RuleType: "normal",
	}}, ContractTypePerpetual)
	if aapl[0].Underlying.AssetClass != AssetClassStock || aapl[0].Underlying.Routable() || aapl[0].Underlying.UnderlyingCurrency != "" {
		t.Fatalf("aapl=%+v", aapl[0].Underlying)
	}
	bybit := parseBybitInstruments([]bybitInstrument{{
		Symbol: "BYDUSDT", ContractType: "LinearPerpetual", Status: "Trading",
		BaseCoin: "BYD", QuoteCoin: "USDT", SettleCoin: "USDT", FundingInterval: "480",
		SymbolType: "stock", UnderlyingTicker: "1211HK", MarketRegion: "HK",
	}}, ContractTypePerpetual)
	if bybit[0].Underlying.Symbol != "1211" || bybit[0].Underlying.NativeListingCurrency != "HKD" || bybit[0].Underlying.PriceCurrency() != "USD" {
		t.Fatalf("byd=%+v", bybit[0].Underlying)
	}
	fx := parseBybitInstruments([]bybitInstrument{{
		Symbol: "USDJPYUSDT", ContractType: "LinearPerpetual", Status: "Trading",
		BaseCoin: "USDJPY", QuoteCoin: "USDT", SettleCoin: "USDT", FundingInterval: "480",
		SymbolType: "forex", UnderlyingTicker: "USDJPY",
	}}, ContractTypePerpetual)
	if fx[0].Underlying.PriceCurrency() != "JPY" || fx[0].Underlying.IBQueries()[0].Exchange != "IDEALPRO" {
		t.Fatalf("usdjpy=%+v query=%+v", fx[0].Underlying, fx[0].Underlying.IBQueries())
	}
	gate := parseGateInstruments([]gateContract{{
		Name: "ANTHROPIC_USDT", ContractType: "stocks", IsPreMarket: true, QuantoMultiplier: "1", FundingInterval: 28800,
	}})
	if gate[0].Underlying.Routable() {
		t.Fatalf("gate=%+v", gate[0].Underlying)
	}
}

func TestExactUSDStockQuery(t *testing.T) {
	ref := USDStockReference("AAPL", "USDT", "Apple", "test")
	queries := ref.IBQueries()
	if len(queries) != 1 || queries[0].SecType != SecTypeSTK || queries[0].Exchange != "SMART" || queries[0].Currency != "USD" || ref.PriceCurrency() != "USD" {
		t.Fatalf("query=%+v price=%s", queries, ref.PriceCurrency())
	}
	if PreMarketReference("OPENAI", "test").Routable() {
		t.Fatal("pre-market must not be queried")
	}
}
