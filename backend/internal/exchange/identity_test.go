package exchange

import (
	"encoding/json"
	"testing"
)

func TestIdentityFixturesDecodeOfficialShapes(t *testing.T) {
	var binance binanceExchangeInfo
	mustJSON(t, binanceFixture, &binance)
	parsed := parseBinanceInstruments(binance)
	bySymbol := map[string]Instrument{}
	for _, item := range parsed {
		bySymbol[item.ExchangeSymbol] = item
	}
	if bySymbol["AAPLUSDT"].Underlying.Symbol != "AAPL" || !bySymbol["AAPLUSDT"].Underlying.Routable() {
		t.Fatalf("aapl=%+v", bySymbol["AAPLUSDT"].Underlying)
	}
	if bySymbol["BYDUSDT"].Underlying.Routable() || bySymbol["BYDUSDT"].Underlying.UnderlyingCurrency == "USD" {
		t.Fatalf("byd=%+v", bySymbol["BYDUSDT"].Underlying)
	}
	if bySymbol["BTCUSDT"].Underlying.AssetClass != AssetClassCrypto {
		t.Fatalf("btc=%+v", bySymbol["BTCUSDT"].Underlying)
	}

	var bitget bitgetEnvelope[[]bitgetUTAInstrument]
	mustJSON(t, bitgetFixture, &bitget)
	bitgetItems := parseBitgetUTAInstruments(bitget.Data, "USDT-FUTURES")
	if bitgetItems[0].Underlying.Routable() || !bool(bitget.Data[0].IsRwa) {
		t.Fatalf("bitget=%+v rwa=%v", bitgetItems[0].Underlying, bitget.Data[0].IsRwa)
	}

	raw := json.RawMessage(hyperliquidAnnotationFixture)
	annotations := decodeHyperliquidAnnotations(raw)
	if annotations["xyz:BYD"].DisplayName == "" || len(annotations["xyz:BYD"].Keywords) == 0 {
		t.Fatalf("annotations=%+v", annotations)
	}
	if _, ok := keywordListing("hk700"); !ok {
		t.Fatal("hk700 keyword should be recognizable only as a veto")
	}

	var books lighterOrderBookDetails
	mustJSON(t, lighterOrderBookFixture, &books)
	tokens, err := decodeLighterTokens(json.RawMessage(lighterTokenFixture))
	if err != nil {
		t.Fatal(err)
	}
	lighterItems := applyLighterDirectory(parseLighterOrderBooks(books), tokens)
	lighterBySymbol := map[string]Instrument{}
	for _, item := range lighterItems {
		lighterBySymbol[item.ExchangeSymbol] = item
	}
	if _, ok := lighterBySymbol["BYD"]; ok {
		t.Fatal("lighter BYD must be excluded")
	}
	if lighterBySymbol["AAPL"].Underlying.Routable() || lighterBySymbol["AAPL"].Underlying.AssetClass != AssetClassStock {
		t.Fatalf("lighter aapl=%+v", lighterBySymbol["AAPL"].Underlying)
	}
	if lighterBySymbol["BTC"].Underlying.AssetClass != AssetClassCrypto {
		t.Fatalf("lighter btc=%+v", lighterBySymbol["BTC"].Underlying)
	}

	var okx okxEnvelope[okxInstrument]
	mustJSON(t, okxFixture, &okx)
	okxItems := parseOKXInstruments(okx.Data, ContractTypePerpetual)
	if okxItems[0].BaseAsset != "XIAOMI" || okxItems[0].Underlying.Routable() {
		t.Fatalf("okx=%+v", okxItems[0].Underlying)
	}
	var aster asterExchangeInfo
	mustJSON(t, asterFixture, &aster)
	asterItems := parseAsterInstruments(aster)
	if asterItems[0].Underlying.Routable() || asterItems[1].Underlying.Routable() || asterItems[2].Underlying.PreMarket == false && asterItems[2].Underlying.Routable() {
		t.Fatalf("aster=%+v %+v %+v", asterItems[0].Underlying, asterItems[1].Underlying, asterItems[2].Underlying)
	}
}

func TestTwoSourcesConnectAndOneSourceDoesNot(t *testing.T) {
	binance := equity("binance", "AAPLUSDT", "AAPL", "USDT")
	bybit := equity("bybit", "AAPLUSDT", "AAPL", "USDT")
	okx := candidate("okx", "AAPL-USDT-SWAP", "AAPL", "USDT")
	alone := ApplyTradfiIdentities([]Instrument{okx}, []Instrument{binance, okx}, nil)
	if alone[0].Underlying.Routable() {
		t.Fatal("one official source must not connect AAPL")
	}
	linked := ApplyTradfiIdentities([]Instrument{okx}, []Instrument{binance, bybit, okx}, nil)
	if linked[0].Underlying.Symbol != "AAPL" || linked[0].Underlying.Region != "US" || !linked[0].Underlying.Routable() {
		t.Fatalf("linked=%+v", linked[0].Underlying)
	}
	if linked[0].Underlying.PerpPriceCurrency != "USD" {
		t.Fatalf("price=%s", linked[0].Underlying.PerpPriceCurrency)
	}
}

func TestExplicitBitgetEdgeAndCurrency(t *testing.T) {
	gate := candidate("gate", "BYD_USDT", "BYD", "USDT")
	gate.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusOK, Rows: []ConstituentRow{
		{Exchange: "iTick", Symbol: "1211_HKD"},
		{Exchange: "BitgetFutures", Symbol: "BYD_USDT"},
	}})
	bitget := candidate("bitget", "BYDUSDT", "BYD", "USDT")
	bitget.Metadata = metadataJSON(t, map[string]any{"symbolType": "stock", "isRwa": true})
	items := ApplyTradfiIdentities([]Instrument{gate, bitget}, []Instrument{gate, bitget}, nil)
	if items[0].Underlying.Symbol != "1211" || items[0].Underlying.Region != "HK" || items[0].Underlying.UnderlyingCurrency != "HKD" {
		t.Fatalf("gate=%+v", items[0].Underlying)
	}
	if items[1].Underlying.Symbol != "1211" || items[1].Underlying.UnderlyingCurrency != "HKD" {
		t.Fatalf("bitget=%+v", items[1].Underlying)
	}
	if items[1].Underlying.PriceConversionPolicy != PricePolicyForbid {
		t.Fatal("USD-priced HKD listing must stay off the page")
	}
	missing := bitget
	missing.Metadata = metadataJSON(t, map[string]any{"symbolType": "stock", "isRwa": false})
	without := ApplyTradfiIdentities([]Instrument{missing}, []Instrument{gate, missing}, nil)
	if without[0].Underlying.Routable() {
		t.Fatal("bitget edge requires isRwa")
	}
	crypto := CryptoReference("BTC", "USDT", "bitget")
	btc := Instrument{Exchange: "bitget", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", ContractType: ContractTypePerpetual}
	btc.SetUnderlying(crypto)
	btc.Metadata = metadataJSON(t, map[string]any{"symbolType": "crypto", "isRwa": false})
	kept := ApplyTradfiIdentities([]Instrument{btc}, []Instrument{gate, btc}, nil)
	if kept[0].Underlying.AssetClass != AssetClassCrypto {
		t.Fatalf("crypto=%+v", kept[0].Underlying)
	}
}

func TestSamsungNeedsTwoSourcesAndFXIsNotStripped(t *testing.T) {
	first := candidate("gate", "SAMSUNG_USDT", "SAMSUNG", "USDT")
	first.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusOK, Rows: []ConstituentRow{{Exchange: "iTick", Symbol: "005930_KRW"}}})
	second := candidate("binance", "SAMSUNGUSDT", "SAMSUNG", "USDT")
	second.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusOK, Rows: []ConstituentRow{{Exchange: "iTick", Symbol: "005930_KRW"}}})
	lighter := candidate("lighter", "SAMSUNGUSD", "SAMSUNGUSD", "USDC")
	one := ApplyTradfiIdentities([]Instrument{lighter}, []Instrument{first, lighter}, nil)
	if one[0].Underlying.Routable() {
		t.Fatal("one source must not accept SAMSUNG")
	}
	two := ApplyTradfiIdentities([]Instrument{lighter}, []Instrument{first, second, lighter}, nil)
	if two[0].Underlying.Symbol != "005930" || two[0].Underlying.Region != "KR" || two[0].Underlying.UnderlyingCurrency != "KRW" {
		t.Fatalf("samsung=%+v", two[0].Underlying)
	}
	fx := candidate("lighter", "EURUSD", "EURUSD", "USDC")
	fx.Underlying.AssetClass = AssetClassFX
	keys := candidateKeys("EURUSD")
	if len(keys) != 1 || keys[0] != "EURUSD" {
		t.Fatalf("keys=%v", keys)
	}
	samsungKeys := candidateKeys("SAMSUNGUSD")
	if len(samsungKeys) != 2 || samsungKeys[1] != "SAMSUNG" {
		t.Fatalf("samsung keys=%v", samsungKeys)
	}
}

func TestKeywordCannotCreateHongKongIdentity(t *testing.T) {
	item := candidate("hyperliquid", "xyz:BYD", "BYD", "USDC")
	item.Metadata = metadataJSON(t, map[string]any{"annotation": map[string]any{"keywords": []string{"hk700"}, "displayName": "BYD"}})
	items := ApplyTradfiIdentities([]Instrument{item}, []Instrument{item}, nil)
	if items[0].Underlying.Routable() || items[0].Underlying.Symbol == "700" {
		t.Fatalf("keyword=%+v", items[0].Underlying)
	}
}

func TestConflictingListingsFailClosed(t *testing.T) {
	hk := candidate("gate", "XIAOMI_USDT", "XIAOMI", "USDT")
	hk.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusOK, Rows: []ConstituentRow{{Exchange: "iTick", Symbol: "1810_HKD"}}})
	kr := candidate("binance", "XIAOMIUSDT", "XIAOMI", "USDT")
	kr.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusOK, Rows: []ConstituentRow{{Exchange: "iTick", Symbol: "005930_KRW"}}})
	okx := candidate("okx", "XIAOMI-USDT-SWAP", "XIAOMI", "USDT")
	items := ApplyTradfiIdentities([]Instrument{okx}, []Instrument{hk, kr, okx}, nil)
	if items[0].Underlying.Routable() {
		t.Fatalf("conflict=%+v", items[0].Underlying)
	}
}

func TestConstituentLastKnownGoodStates(t *testing.T) {
	previous := equity("binance", "BYDUSDT", "1211", "USDT")
	previous.Underlying.Region = "HK"
	previous.Underlying.UnderlyingCurrency = "HKD"
	previous.Underlying.NativeListingCurrency = "HKD"
	previous.Underlying.PerpPriceCurrency = "USD"
	previous.Underlying.DollarConverted = true
	previous = Instrument{Exchange: "binance", ExchangeSymbol: "BYDUSDT", BaseAsset: "BYD", QuoteAsset: "USDT", ContractType: ContractTypePerpetual}
	ref := RegionalStock("1211", "HK", "HKD", "USDT", "", "previous")
	ref.PerpPriceCurrency = "USD"
	ref.DollarConverted = true
	previous.SetUnderlying(ref)

	errored := candidate("binance", "BYDUSDT", "BYD", "USDT")
	errored.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusError})
	kept := ApplyTradfiIdentities([]Instrument{errored}, []Instrument{errored}, []Instrument{previous})
	if kept[0].Underlying.Symbol != "1211" {
		t.Fatalf("error must reuse previous identity: %+v", kept[0].Underlying)
	}

	empty := candidate("binance", "BYDUSDT", "BYD", "USDT")
	empty.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusEmpty})
	dropped := ApplyTradfiIdentities([]Instrument{empty}, []Instrument{empty}, []Instrument{previous})
	if dropped[0].Underlying.Routable() {
		t.Fatal("empty official evidence must not reuse the previous identity")
	}

	changed := candidate("binance", "BYDUSDT", "BYD", "USDT")
	changed.Metadata = writeConstituents(nil, ConstituentEvidence{Status: constituentStatusOK, Rows: []ConstituentRow{{Exchange: "iTick", Symbol: "1810_HKD"}}})
	conflict := ApplyTradfiIdentities([]Instrument{changed}, []Instrument{changed}, []Instrument{previous})
	if conflict[0].Underlying.Routable() || conflict[0].Underlying.AssetClass != AssetClassUnknown {
		t.Fatalf("conflict=%+v", conflict[0].Underlying)
	}

	pre := candidate("okx", "BYD-USDT-SWAP", "BYD", "USDT")
	pre.SetUnderlying(PreMarketReference("BYD", "okx"))
	skipped := ApplyTradfiIdentities([]Instrument{pre}, []Instrument{pre}, []Instrument{previous})
	if skipped[0].Underlying.Routable() || !skipped[0].Underlying.PreMarket {
		t.Fatalf("premarket=%+v", skipped[0].Underlying)
	}
}

func equity(exchangeName, symbol, base, quote string) Instrument {
	item := candidate(exchangeName, symbol, base, quote)
	item.SetUnderlying(USDStockReference(base, quote, "", exchangeName+".direct"))
	return item
}

func candidate(exchangeName, symbol, base, quote string) Instrument {
	item := Instrument{
		Exchange: exchangeName, ExchangeSymbol: symbol, BaseAsset: base, QuoteAsset: quote,
		ContractType: ContractTypePerpetual, Status: "active",
	}
	item.SetUnderlying(TradfiCandidate(AssetClassStock, base, quote, exchangeName+".category"))
	return item
}

func metadataJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

const binanceFixture = `{
  "symbols": [
    {"symbol":"BYDUSDT","baseAsset":"BYD","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"TRADIFI_PERPETUAL","underlyingType":"HK_EQUITY","underlyingSubType":["TradFi"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"0.01","minQty":"0.01"}]},
    {"symbol":"AAPLUSDT","baseAsset":"AAPL","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"TRADIFI_PERPETUAL","underlyingType":"EQUITY","underlyingSubType":["TradFi"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"0.01","minQty":"0.01"}]},
    {"symbol":"XAUUSDT","baseAsset":"XAU","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"TRADIFI_PERPETUAL","underlyingType":"COMMODITY","underlyingSubType":["TradFi"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"0.01","minQty":"0.01"}]},
    {"symbol":"EURUSDT","baseAsset":"EURUSD","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"TRADIFI_PERPETUAL","underlyingType":"FX","underlyingSubType":["TradFi"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.0001"},{"filterType":"LOT_SIZE","stepSize":"1","minQty":"1"}]},
    {"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"COIN","underlyingSubType":["PoW"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.10"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001"}]}
  ]
}`

const bitgetFixture = `{
  "code": "00000",
  "msg": "success",
  "data": [
    {"symbol":"BYDUSDT","baseCoin":"BYD","quoteCoin":"USDT","symbolType":"stock","isRwa":"YES","status":"online","type":"perpetual","fundInterval":"8"},
    {"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","symbolType":"crypto","isRwa":"NO","status":"online","type":"perpetual","fundInterval":"8"}
  ]
}`

const hyperliquidAnnotationFixture = `[
  ["xyz:BYD", {"category":"stock","displayName":"BYD Company","keywords":["hk700","auto"]}]
]`

const lighterTokenFixture = `{
  "tokens": [
    {"symbol":"AAPL","backend_symbol":"AAPL","asset_type":"RWA","categories":["STOCK"],"is_allowed_mainnet":true},
    {"symbol":"SAMSUNGUSD","backend_symbol":null,"asset_type":"RWA","categories":["STOCK"],"is_allowed_mainnet":true},
    {"symbol":"BYD","backend_symbol":"BYD","asset_type":"RWA","categories":["NEW"],"is_allowed_mainnet":false},
    {"symbol":"BTC","backend_symbol":"BTC","asset_type":"CRYPTO","categories":["CRYPTO"],"is_allowed_mainnet":true}
  ]
}`

const lighterOrderBookFixture = `{
  "code": 200,
  "order_book_details": [
    {"symbol":"AAPL","market_id":1,"market_type":"perp","status":"active","multiplier":"1","supported_size_decimals":2,"supported_price_decimals":2,"min_base_amount":"0.01","min_quote_amount":"10","market_config":{"hidden":false,"force_reduce_only":false}},
    {"symbol":"SAMSUNGUSD","market_id":2,"market_type":"perp","status":"active","multiplier":"1","supported_size_decimals":2,"supported_price_decimals":2,"min_base_amount":"0.01","min_quote_amount":"10","market_config":{"hidden":false,"force_reduce_only":false}},
    {"symbol":"BYD","market_id":3,"market_type":"perp","status":"active","multiplier":"1","supported_size_decimals":2,"supported_price_decimals":2,"min_base_amount":"0.01","min_quote_amount":"10","market_config":{"hidden":false,"force_reduce_only":true}},
    {"symbol":"BTC","market_id":4,"market_type":"perp","status":"active","multiplier":"1","supported_size_decimals":4,"supported_price_decimals":1,"min_base_amount":"0.001","min_quote_amount":"10","market_config":{"hidden":false,"force_reduce_only":false}}
  ]
}`

const okxFixture = `{
  "code": "0",
  "data": [
    {"instId":"XIAOMI-USDT-SWAP","instType":"SWAP","instFamily":"XIAOMI-USDT","state":"live","ctVal":"1","ctValCcy":"XIAOMI","tickSz":"0.01","lotSz":"1","minSz":"1","settleCcy":"USDT","instCategory":"3","ruleType":"normal"},
    {"instId":"SAMSUNG-USDT-SWAP","instType":"SWAP","instFamily":"SAMSUNG-USDT","state":"live","ctVal":"1","ctValCcy":"SAMSUNG","tickSz":"0.01","lotSz":"1","minSz":"1","settleCcy":"USDT","instCategory":"3","ruleType":"normal"}
  ]
}`

const asterFixture = `{
  "symbols": [
    {"symbol":"XIAOMIUSDT","baseAsset":"XIAOMI","quoteAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"STOCK","underlyingSubType":["STOCK"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"1","minQty":"1"}]},
    {"symbol":"SAMSUNGUSDT","baseAsset":"SAMSUNG","quoteAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"STOCK","underlyingSubType":["STOCK"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"1","minQty":"1"}]},
    {"symbol":"OPENAIUSDT","baseAsset":"OPENAI","quoteAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"STOCK","underlyingSubType":["pre-launch","STOCK"],"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"1","minQty":"1"}]}
  ]
}`
