package exchange

import (
	"encoding/json"
	"testing"
)

func TestInstrumentOrderConstraints(t *testing.T) {
	t.Run("binance", func(t *testing.T) {
		var payload binanceExchangeInfo
		if err := json.Unmarshal([]byte(`{"symbols":[{
			"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT",
			"marginAsset":"USDT","status":"TRADING","contractType":"PERPETUAL",
			"filters":[
				{"filterType":"PRICE_FILTER","tickSize":"0.1"},
				{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.002","maxQty":"100"},
				{"filterType":"MARKET_LOT_SIZE","stepSize":"0.01","minQty":"0.01","maxQty":"20"},
				{"filterType":"MIN_NOTIONAL","notional":"5"}
			]}]}`), &payload); err != nil {
			t.Fatal(err)
		}
		item := parseBinanceInstruments(payload)[0]
		assertKnownConstraints(t, item, 0.002, 5)
		assertMarketConstraints(t, item, "100", "0.01", "0.01", "20", "5")
	})

	t.Run("aster", func(t *testing.T) {
		var payload asterExchangeInfo
		if err := json.Unmarshal([]byte(`{"symbols":[{
			"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT",
			"marginAsset":"USDT","status":"TRADING","contractType":"PERPETUAL",
			"filters":[
				{"filterType":"PRICE_FILTER","tickSize":"0.1"},
				{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"1000"},
				{"filterType":"MARKET_LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"120"},
				{"filterType":"MIN_NOTIONAL","notional":"5"}
			]}]}`), &payload); err != nil {
			t.Fatal(err)
		}
		item := parseAsterInstruments(payload)[0]
		assertKnownConstraints(t, item, 0.001, 5)
		assertMarketConstraints(t, item, "1000", "0.001", "0.001", "120", "5")
	})

	t.Run("hyperliquid", func(t *testing.T) {
		var meta hyperliquidMeta
		if err := json.Unmarshal([]byte(`{"universe":[{"name":"BTC","szDecimals":5}]}`), &meta); err != nil {
			t.Fatal(err)
		}
		item := parseHyperliquidInstruments(meta, "")[0]
		if item.QuantityStep != 0.00001 || item.MinQuantity != 0.00001 ||
			item.MinQuantityStatus != ConstraintKnown ||
			item.PriceTick != 0.1 ||
			item.MinNotional != 10 || item.MinNotionalStatus != ConstraintKnown ||
			item.MaxQuantityStatus != ConstraintNotApplicable ||
			item.MarketQuantityStep != "0.00001" || item.MarketQuantityStepStatus != ConstraintKnown ||
			item.MarketMinQuantity != "0.00001" || item.MarketMinQuantityStatus != ConstraintKnown ||
			item.MarketMaxQuantityStatus != ConstraintNotApplicable ||
			item.MarketMinNotional != "10" || item.MarketMinNotionalStatus != ConstraintKnown {
			t.Fatalf("hyperliquid rules=%+v", item)
		}
	})

	t.Run("lighter", func(t *testing.T) {
		var payload lighterOrderBookDetails
		if err := json.Unmarshal([]byte(`{"order_book_details":[{
			"symbol":"BTC","market_id":1,"market_type":"perp","status":"active",
			"multiplier":"1","supported_size_decimals":5,"supported_price_decimals":2,
			"min_base_amount":"0.0001","min_quote_amount":"10"
		}]}`), &payload); err != nil {
			t.Fatal(err)
		}
		item := parseLighterInstruments(payload)[0]
		assertKnownConstraints(t, item, 0.0001, 10)
		if item.PriceTick != 0.01 || item.QuantityStep != 0.00001 {
			t.Fatalf("lighter rules=%+v", item)
		}
		if item.MaxQuantityStatus != ConstraintNotApplicable ||
			item.MarketQuantityStep != "0.00001" ||
			item.MarketQuantityStepStatus != ConstraintKnown ||
			item.MarketMinQuantity != "0.0001" ||
			item.MarketMinQuantityStatus != ConstraintKnown ||
			item.MarketMaxQuantityStatus != ConstraintNotApplicable ||
			item.MarketMinNotional != "10" ||
			item.MarketMinNotionalStatus != ConstraintKnown {
			t.Fatalf("lighter market rules=%+v", item)
		}
	})

	t.Run("bybit", func(t *testing.T) {
		var item bybitInstrument
		if err := json.Unmarshal([]byte(`{
			"symbol":"BTCUSDT","contractType":"LinearPerpetual","status":"Trading",
			"baseCoin":"BTC","quoteCoin":"USDT","settleCoin":"USDT",
			"fundingInterval":"480","priceFilter":{"tickSize":"0.1"},
			"lotSizeFilter":{"qtyStep":"0.001","minOrderQty":"0.002","maxOrderQty":"100",
				"maxMktOrderQty":"20","minNotionalValue":"5"}
		}`), &item); err != nil {
			t.Fatal(err)
		}
		parsed := parseBybitInstruments([]bybitInstrument{item}, ContractTypePerpetual)[0]
		assertKnownConstraints(t, parsed, 0.002, 5)
		assertMarketConstraints(t, parsed, "100", "0.001", "0.002", "20", "5")
	})

	t.Run("okx", func(t *testing.T) {
		item := parseOKXInstruments([]okxInstrument{{
			InstID: "BTC-USDT-SWAP", InstType: "SWAP", InstFamily: "BTC-USDT",
			State: "live", CtType: "linear", SettleCcy: "USDT", CtValCcy: "BTC",
			CtVal: "0.01", TickSz: "0.1", LotSz: "0.1", MinSz: "0.2",
			MaxLmtSz: "100", MaxMktSz: "20",
		}}, ContractTypePerpetual)[0]
		if item.MinQuantity != 0.2 || item.MinQuantityStatus != ConstraintKnown ||
			item.MinNotionalStatus != ConstraintNotApplicable {
			t.Fatalf("constraints=%+v", item)
		}
	})

	t.Run("gate", func(t *testing.T) {
		item := parseGateInstruments([]gateContract{{
			Name: "BTC_USDT", QuantoMultiplier: "0.001",
			OrderPriceRound: "0.1", OrderSizeMin: "2", OrderSizeMax: "100",
			MarketOrderSizeMax: "20",
		}})[0]
		if item.MinQuantity != 2 || item.MinQuantityStatus != ConstraintKnown ||
			item.MinNotionalStatus != ConstraintNotApplicable {
			t.Fatalf("constraints=%+v", item)
		}
		assertMarketConstraints(t, item, "100", "1", "2", "20", "")
	})

	t.Run("bitget", func(t *testing.T) {
		item := parseBitgetInstruments([]bitgetInstrument{{
			Symbol: "BTCUSDT", BaseCoin: "BTC", QuoteCoin: "USDT",
			SymbolStatus: "normal", SymbolType: "perpetual", SettleCoin: "USDT",
			SizeMultiplier: "0.001", PriceEndStep: "1", PricePlace: "1",
			VolumePlace: "3", MinTradeNum: "0.002", MinTradeUSDT: "5",
		}})[0]
		assertKnownConstraints(t, item, 0.002, 5)
		if item.QuantityStep != 0.001 {
			t.Fatalf("quantity step=%v", item.QuantityStep)
		}
	})

	t.Run("bitget UTA", func(t *testing.T) {
		item := parseBitgetUTAInstruments([]bitgetUTAInstrument{{
			Category: "USDT-FUTURES", Symbol: "BTCUSDT",
			BaseCoin: "BTC", QuoteCoin: "USDT", Status: "online", Type: "perpetual",
			PricePrecision: "2", PriceMultiplier: "0.02",
			QuantityPrecision: "3", QuantityMultiplier: "0.002",
			MinOrderQty: "0.004", MaxOrderQty: "100", MaxMarketOrderQty: "20",
			MinOrderAmount: "5", FundInterval: "8",
		}}, "USDT-FUTURES")[0]
		assertKnownConstraints(t, item, 0.004, 5)
		assertMarketConstraints(t, item, "100", "0.002", "0.004", "20", "5")
		if item.PriceTick != 0.02 || item.QuantityStep != 0.002 {
			t.Fatalf("UTA rules=%+v", item)
		}
	})
}

func assertKnownConstraints(
	t *testing.T,
	item Instrument,
	minQuantity float64,
	minNotional float64,
) {
	t.Helper()
	if item.MinQuantity != minQuantity || item.MinNotional != minNotional ||
		item.MinQuantityStatus != ConstraintKnown ||
		item.MinNotionalStatus != ConstraintKnown {
		t.Fatalf("constraints=%+v", item)
	}
}

func assertMarketConstraints(
	t *testing.T,
	item Instrument,
	maxQuantity, marketStep, marketMin, marketMax, marketNotional string,
) {
	t.Helper()
	if item.MaxQuantity != maxQuantity ||
		item.MarketQuantityStep != marketStep ||
		item.MarketMinQuantity != marketMin ||
		item.MarketMaxQuantity != marketMax ||
		item.MarketMinNotional != marketNotional {
		t.Fatalf("market constraints=%+v", item)
	}
	for name, status := range map[string]string{
		"max":         item.MaxQuantityStatus,
		"market step": item.MarketQuantityStepStatus,
		"market min":  item.MarketMinQuantityStatus,
		"market max":  item.MarketMaxQuantityStatus,
	} {
		if status != ConstraintKnown {
			t.Fatalf("%s status=%s item=%+v", name, status, item)
		}
	}
	if marketNotional == "" {
		if item.MarketMinNotionalStatus != ConstraintNotApplicable {
			t.Fatalf("market notional status=%s", item.MarketMinNotionalStatus)
		}
	} else if item.MarketMinNotionalStatus != ConstraintKnown {
		t.Fatalf("market notional status=%s", item.MarketMinNotionalStatus)
	}
}
