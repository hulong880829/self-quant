package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBybitOKXAndUnsignedVenuesUseOnlyRealFields(t *testing.T) {
	bybit := parseBybitInstruments([]bybitInstrument{{
		Symbol: "AAPLUSDT", ContractType: "LinearPerpetual", Status: "Trading",
		BaseCoin: "AAPL", QuoteCoin: "USDT", SettleCoin: "USDT", FundingInterval: "480",
		UnderlyingTicker: "AAPL", MarketRegion: "US", FullName: "Apple Inc",
	}}, ContractTypePerpetual)
	if bybit[0].Underlying.AssetClass != AssetClassStock || bybit[0].Underlying.UnderlyingCurrency != "USD" || !bybit[0].Underlying.Routable() {
		t.Fatalf("bybit=%+v", bybit[0].Underlying)
	}
	okx := parseOKXInstruments([]okxInstrument{{
		InstID: "AAPL-USDT-SWAP", InstType: "SWAP", InstFamily: "AAPL-USDT", State: "live",
		BaseCcy: "AAPL", QuoteCcy: "USDT", SettleCcy: "USDT", CtValCcy: "AAPL",
		CtVal: "1", TickSz: "0.01", LotSz: "1", MinSz: "1", InstCategory: "stock",
	}}, ContractTypePerpetual)
	if okx[0].Underlying.AssetClass != AssetClassStock || okx[0].Underlying.Routable() || okx[0].Underlying.UnderlyingCurrency != "" {
		t.Fatalf("okx stock category is not a US listing: %+v", okx[0].Underlying)
	}
	bitget := parseBitgetUTAInstruments([]bitgetUTAInstrument{{
		Symbol: "AAPLUSDT", BaseCoin: "AAPL", QuoteCoin: "USDT", Status: "online", Type: "perpetual",
		FundInterval: "8", PricePrecision: "2", QuantityPrecision: "3", MinOrderQty: "1", MinOrderAmount: "1",
	}}, "USDT-FUTURES")
	if bitget[0].Underlying.AssetClass != AssetClassCrypto {
		t.Fatalf("bitget=%+v", bitget[0].Underlying)
	}
}

func TestAsterStockIgnoresCoinUnderlyingType(t *testing.T) {
	var payload asterExchangeInfo
	mustJSON(t, `{"symbols":[
		{"symbol":"AAPLUSDT","baseAsset":"AAPL","quoteAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"COIN","underlyingSubType":["STOCK"]},
		{"symbol":"QQQUSDT","baseAsset":"QQQ","quoteAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"COIN","underlyingSubType":["ETF"]},
		{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","underlyingType":"COIN","underlyingSubType":["Semiconductor"]}
	]}`, &payload)
	items := parseAsterInstruments(payload)
	if items[0].Underlying.AssetClass != AssetClassStock || items[0].Underlying.Routable() {
		t.Fatalf("stock=%+v", items[0].Underlying)
	}
	if items[1].Underlying.AssetClass != AssetClassETF {
		t.Fatalf("etf=%+v", items[1].Underlying)
	}
	if items[2].Underlying.AssetClass != AssetClassCrypto || items[2].Underlying.SecType() != SecTypeCrypto {
		t.Fatalf("crypto=%+v", items[2].Underlying)
	}
}

func TestHyperliquidCategoryUsesNormalizedSymbol(t *testing.T) {
	var meta hyperliquidMeta
	mustJSON(t, `{"universe":[
		{"name":"AAPL","isDelisted":false,"szDecimals":2},
		{"name":"BTC","isDelisted":false,"szDecimals":2},
		{"name":"GOLD","isDelisted":false,"szDecimals":2},
		{"name":"SP500","isDelisted":false,"szDecimals":2},
		{"name":"OPENAI","isDelisted":false,"szDecimals":2}
	]}`, &meta)
	rawName := parseHyperliquidPerps(meta, "xyz", map[string]string{"AAPL": "stocks"}, nil)
	if rawName[0].Underlying.AssetClass != AssetClassUnknown || rawName[0].Underlying.Routable() {
		t.Fatalf("raw name must not classify xyz:AAPL: %+v", rawName[0].Underlying)
	}
	categories := map[string]string{
		"xyz:AAPL": "stocks", "xyz:BTC": "crypto", "xyz:GOLD": "commodities",
		"xyz:SP500": "indices", "xyz:OPENAI": "preipo",
	}
	items := parseHyperliquidPerps(meta, "xyz", categories, nil)
	if items[0].ExchangeSymbol != "xyz:AAPL" || items[0].Underlying.AssetClass != AssetClassStock || items[0].Underlying.Routable() {
		t.Fatalf("aapl=%+v", items[0].Underlying)
	}
	if items[1].Underlying.SecType() != SecTypeCrypto || items[2].Underlying.Routable() || items[3].Underlying.Routable() || items[4].Underlying.Routable() {
		t.Fatalf("classes=%s %s %s %s", items[1].Underlying.AssetClass, items[2].Underlying.AssetClass, items[3].Underlying.AssetClass, items[4].Underlying.AssetClass)
	}
}

func TestHyperliquidMetaOrCategoryFailureReturnsNoCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
			Dex  string `json:"dex"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(writer, "bad", http.StatusBadRequest)
			return
		}
		switch payload.Type {
		case "perpDexs":
			_, _ = writer.Write([]byte(`[null,{"name":"xyz"},{"name":"io"}]`))
		case "perpCategories":
			http.Error(writer, "categories down", http.StatusInternalServerError)
		default:
			http.Error(writer, "meta down", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	adapter := &Hyperliquid{client: newClient(server.URL, 3*time.Second)}
	instruments, err := adapter.SyncInstruments(context.Background(), ContractTypePerpetual)
	if err == nil || instruments != nil {
		t.Fatalf("instruments=%v err=%v", instruments, err)
	}

	metaFails := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
			Dex  string `json:"dex"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(writer, "bad", http.StatusBadRequest)
			return
		}
		switch payload.Type {
		case "perpDexs":
			_, _ = writer.Write([]byte(`[null,{"name":"xyz"}]`))
		case "perpCategories":
			_, _ = writer.Write([]byte(`[["xyz:AAPL","stocks"]]`))
		case "metaAndAssetCtxs":
			if payload.Dex == "xyz" {
				http.Error(writer, "xyz down", http.StatusInternalServerError)
				return
			}
			_, _ = writer.Write([]byte(`[{"universe":[{"name":"BTC","isDelisted":false}]},[{"funding":"0.0001"}]]`))
		}
	}))
	defer metaFails.Close()
	failed := &Hyperliquid{client: newClient(metaFails.URL, 3*time.Second)}
	instruments, err = failed.SyncInstruments(context.Background(), ContractTypePerpetual)
	if err == nil || instruments != nil {
		t.Fatalf("partial catalog instruments=%v err=%v", instruments, err)
	}
}
