package marketdata

import (
	"encoding/json"
	"testing"
)

func TestSubscriptionControlProtocols(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		venue       string
		fixture     string
		correlation string
		symbol      string
		wantError   bool
		deliver     bool
	}{
		{
			name: "binance success", venue: VenueBinance,
			fixture: `{"result":null,"id":11}`, correlation: "request:11",
		},
		{
			name: "binance error", venue: VenueBinance,
			fixture:     `{"id":12,"error":{"code":-1121,"msg":"Invalid symbol"}}`,
			correlation: "request:12", wantError: true,
		},
		{
			name: "okx success", venue: VenueOKX,
			fixture:     `{"event":"subscribe","arg":{"channel":"books5","instId":"BTC-USDT"}}`,
			correlation: "subscribe:BTC-USDT", symbol: "BTC-USDT",
		},
		{
			name: "okx error", venue: VenueOKX,
			fixture: `{"event":"error","code":"60012","msg":"Invalid request",` +
				`"arg":{"channel":"books5","instId":"BAD-USDT"}}`,
			symbol: "BAD-USDT", wantError: true,
		},
		{
			name: "bybit success", venue: VenueBybit,
			fixture:     `{"success":true,"ret_msg":"subscribe","req_id":"13","op":"subscribe"}`,
			correlation: "request:13",
		},
		{
			name: "bybit error", venue: VenueBybit,
			fixture:     `{"success":false,"ret_msg":"handler not found","req_id":"14","op":"subscribe"}`,
			correlation: "request:14", wantError: true,
		},
		{
			name: "bitget success", venue: VenueBitget,
			fixture: `{"event":"subscribe","arg":{"instType":"SPOT","channel":"books15",` +
				`"instId":"BTCUSDT"}}`,
			correlation: "subscribe:BTCUSDT", symbol: "BTCUSDT",
		},
		{
			name: "bitget error", venue: VenueBitget,
			fixture: `{"event":"error","arg":{"instType":"SPOT","channel":"books15",` +
				`"instId":"BADUSDT"},"code":30001,"msg":"does not exist"}`,
			symbol: "BADUSDT", wantError: true,
		},
		{
			name: "gate success", venue: VenueGate,
			fixture: `{"id":15,"channel":"futures.book_ticker","event":"subscribe",` +
				`"error":null,"result":{"status":"success"}}`,
			correlation: "request:15",
		},
		{
			name: "gate error", venue: VenueGate,
			fixture: `{"id":16,"channel":"futures.book_ticker","event":"subscribe",` +
				`"error":{"code":2,"message":"Unknown contract"}}`,
			correlation: "request:16", wantError: true,
		},
		{
			name: "aster success", venue: VenueAster,
			fixture: `{"id":17,"result":null}`, correlation: "request:17",
		},
		{
			name: "aster error", venue: VenueAster,
			fixture:     `{"id":18,"code":-1121,"msg":"Invalid symbol"}`,
			correlation: "request:18", wantError: true,
		},
		{
			name: "hyperliquid success", venue: VenueHyperliquid,
			fixture: `{"channel":"subscriptionResponse","data":{"method":"subscribe",` +
				`"subscription":{"type":"bbo","coin":"BTC"}}}`,
			correlation: "subscribe:BTC", symbol: "BTC",
		},
		{
			name: "hyperliquid unsubscribe", venue: VenueHyperliquid,
			fixture: `{"channel":"subscriptionResponse","data":{"method":"unsubscribe",` +
				`"subscription":{"type":"bbo","coin":"BTC"}}}`,
			correlation: "unsubscribe:BTC", symbol: "BTC",
		},
		{
			name: "hyperliquid error", venue: VenueHyperliquid,
			fixture:   `{"channel":"error","data":"Invalid subscription"}`,
			wantError: true,
		},
		{
			name: "lighter snapshot acknowledgement", venue: VenueLighter,
			fixture: `{"type":"subscribed/order_book","channel":"order_book:1",` +
				`"order_book":{"nonce":100,"begin_nonce":0,"bids":[],"asks":[]}}`,
			correlation: "subscribe:lighter:1", deliver: true,
		},
		{
			name: "lighter unsubscribe acknowledgement", venue: VenueLighter,
			fixture:     `{"type":"unsubscribed","channel":"order_book:1"}`,
			correlation: "unsubscribe:lighter:1",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			control, handled, err := parseSubscriptionControl(
				test.venue, []byte(test.fixture),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !handled {
				t.Fatal("subscription control was not recognized")
			}
			if control.correlation != test.correlation || control.symbol != test.symbol {
				t.Fatalf("control = %#v", control)
			}
			if (control.err != nil) != test.wantError {
				t.Fatalf("control error = %v, wantError=%t", control.err, test.wantError)
			}
			if control.deliver != test.deliver {
				t.Fatalf("control deliver = %t, want %t", control.deliver, test.deliver)
			}
		})
	}
}

func TestSubscriptionRequestsCarryCorrelatableIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key         Key
		correlation string
		assert      func(*testing.T, map[string]any)
	}{
		{
			key:         Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			correlation: "request:42",
			assert: func(t *testing.T, request map[string]any) {
				if request["id"] != float64(42) {
					t.Fatalf("Binance id = %#v", request["id"])
				}
			},
		},
		{
			key:         Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
			correlation: "subscribe:BTC-USDT-SWAP",
		},
		{
			key:         Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			correlation: "request:42",
			assert: func(t *testing.T, request map[string]any) {
				if request["req_id"] != "42" {
					t.Fatalf("Bybit req_id = %#v", request["req_id"])
				}
			},
		},
		{
			key:         Key{Venue: VenueBitget, Product: ProductSpot, Symbol: "BTCUSDT"},
			correlation: "subscribe:BTCUSDT",
		},
		{
			key:         Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"},
			correlation: "request:42",
			assert: func(t *testing.T, request map[string]any) {
				if request["id"] != float64(42) {
					t.Fatalf("Gate id = %#v", request["id"])
				}
			},
		},
		{
			key:         Key{Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "BTC"},
			correlation: "subscribe:BTC",
			assert: func(t *testing.T, request map[string]any) {
				if request["method"] != "subscribe" {
					t.Fatalf("Hyperliquid method = %#v", request["method"])
				}
			},
		},
		{
			key:         Key{Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			correlation: "request:42",
			assert: func(t *testing.T, request map[string]any) {
				if request["method"] != "SUBSCRIBE" {
					t.Fatalf("Aster method = %#v", request["method"])
				}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.key.Venue, func(t *testing.T) {
			t.Parallel()
			value, correlation := subscriptionRequestWithID(test.key, true, 42)
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(encoded, &request); err != nil {
				t.Fatal(err)
			}
			if correlation != test.correlation {
				t.Fatalf("correlation = %q, want %q", correlation, test.correlation)
			}
			if test.assert != nil {
				test.assert(t, request)
			}
		})
	}
}
