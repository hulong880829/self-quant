package exchange

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStoreMetadataRevokesStaleClassification(t *testing.T) {
	stored := StoreMetadata([]byte(`{"contractModel":"linear","usEquityPerpetual":true,"tradefiClassificationSource":"binance.exchangeInfo.contractType=TRADIFI_PERPETUAL"}`), true, TradefiClassificationSourceBinance)
	if !IsUSEquityPerpetual(stored) {
		t.Fatalf("stored=%s", stored)
	}
	revoked := StoreMetadata(stored, false, "")
	if IsUSEquityPerpetual(revoked) || strings.Contains(string(revoked), "usEquityPerpetual") || strings.Contains(string(revoked), "tradefiClassificationSource") {
		t.Fatalf("revoked=%s", revoked)
	}
	if !strings.Contains(string(revoked), "linear") {
		t.Fatalf("unrelated metadata dropped: %s", revoked)
	}
	legacy := []byte(`{"contractType":"TRADIFI_PERPETUAL","ContractType":"TRADIFI_PERPETUAL","classification":"US_EQUITY","assetClass":"US_EQUITY","usEquityPerpetual":true}`)
	if IsUSEquityPerpetual(legacy) {
		t.Fatal("legacy metadata without a recognized source must not be a us equity perpetual")
	}
}

func TestBinanceTradifiWritesRecognizedSource(t *testing.T) {
	var payload binanceExchangeInfo
	mustJSON(t, `{"symbols":[{
		"symbol":"ZHIPUUSDT","baseAsset":"ZHIPU","quoteAsset":"USDT","marginAsset":"USDT",
		"status":"TRADING","contractType":"TRADIFI_PERPETUAL","filters":[]
	}]}`, &payload)
	item := parseBinanceInstruments(payload)[0]
	if item.USEquityPerpetual || item.Underlying.AssetClass != AssetClassTradfiUnclassified || item.Underlying.Routable() {
		t.Fatalf("class=%+v", item.Underlying)
	}
	if item.Underlying.Source != TradefiClassificationSourceBinance {
		t.Fatalf("source=%q", item.Underlying.Source)
	}
}

func TestUnsupportedVenuesStayUnclassified(t *testing.T) {
	okx := parseOKXInstruments([]okxInstrument{{
		InstID: "AAPL-USDT-SWAP", InstType: "SWAP", InstFamily: "AAPL-USDT", State: "live",
		BaseCcy: "AAPL", QuoteCcy: "USDT", SettleCcy: "USDT", CtValCcy: "AAPL",
		CtVal: "1", TickSz: "0.01", LotSz: "1", MinSz: "1",
	}}, ContractTypePerpetual)
	bybit := parseBybitInstruments([]bybitInstrument{{
		Symbol: "AAPLUSDT", ContractType: "LinearPerpetual", Status: "Trading",
		BaseCoin: "AAPL", QuoteCoin: "USDT", SettleCoin: "USDT", FundingInterval: "480",
	}}, ContractTypePerpetual)
	bitget := parseBitgetUTAInstruments([]bitgetUTAInstrument{{
		Symbol: "AAPLUSDT", BaseCoin: "AAPL", QuoteCoin: "USDT", Status: "online", Type: "perpetual",
		FundInterval: "8", PricePrecision: "2", QuantityPrecision: "3", MinOrderQty: "1", MinOrderAmount: "1",
	}}, "USDT-FUTURES")
	gate := parseGateInstruments([]gateContract{{
		Name: "AAPL_USDT", FundingInterval: 28800, QuantoMultiplier: "0.01", OrderSizeRound: "1",
	}})
	var aster asterExchangeInfo
	mustJSON(t, `{"symbols":[{"symbol":"AAPLUSDT","baseAsset":"AAPL","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","contractSize":1,"filters":[]}]}`, &aster)
	var lighter lighterOrderBookDetails
	mustJSON(t, `{"order_book_details":[{"symbol":"AAPL","market_id":1,"market_type":"perp","status":"active","multiplier":"1"}]}`, &lighter)
	var hyper hyperliquidMeta
	mustJSON(t, `{"universe":[{"name":"AAPL","isDelisted":false,"szDecimals":2}]}`, &hyper)
	groups := [][]Instrument{
		okx, bybit, bitget, gate, parseAsterInstruments(aster), parseLighterInstruments(lighter),
		parseHyperliquidInstruments(hyper, "xyz"),
		stampEntropyInstruments(parseHyperliquidInstruments(hyper, "io")),
	}
	for _, items := range groups {
		if len(items) != 1 {
			t.Fatalf("items=%+v", items)
		}
		item := items[0]
		stored := StoreMetadata(item.Metadata, item.USEquityPerpetual, item.TradefiClassificationSource)
		if item.USEquityPerpetual || IsUSEquityPerpetual(stored) {
			t.Fatalf("venue=%s marked equity metadata=%s", item.Exchange, stored)
		}
		var fields map[string]any
		if err := json.Unmarshal(stored, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["usEquityPerpetual"]; ok {
			t.Fatalf("venue=%s kept usEquityPerpetual", item.Exchange)
		}
		if _, ok := fields["tradefiClassificationSource"]; ok {
			t.Fatalf("venue=%s kept tradefiClassificationSource", item.Exchange)
		}
	}
}
