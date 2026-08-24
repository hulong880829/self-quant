package marketdata

import (
	"testing"
	"time"
)

func TestVenueParserFixtures(t *testing.T) {
	t.Parallel()
	received := time.Date(2026, 8, 22, 12, 0, 0, 123, time.UTC)
	tests := []struct {
		name    string
		key     Key
		fixture string
		bid     string
		ask     string
		millis  int64
	}{
		{
			name: "binance",
			key:  Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixture: `{"stream":"btcusdt@depth10@100ms","data":{"E":1724328000001,` +
				`"lastUpdateId":42,"bids":[["64000.10","1.2"]],"asks":[["64000.20","0.8"]]}}`,
			bid: "64000.10", ask: "64000.20", millis: 1724328000001,
		},
		{
			name: "binance perpetual",
			key:  Key{Venue: VenueBinance, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"stream":"btcusdt@depth10@100ms","data":{"e":"depthUpdate",` +
				`"E":1724328000001,"s":"BTCUSDT","b":[["64000.10","1.2"]],` +
				`"a":[["64000.20","0.8"]]}}`,
			bid: "64000.10", ask: "64000.20", millis: 1724328000001,
		},
		{
			name: "okx",
			key:  Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
			fixture: `{"arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},` +
				`"data":[{"bids":[["64001.1","1","0","1"]],` +
				`"asks":[["64001.2","1","0","1"]],"ts":"1724328000002"}]}`,
			bid: "64001.1", ask: "64001.2", millis: 1724328000002,
		},
		{
			name: "bybit",
			key:  Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328000003,` +
				`"data":{"s":"BTCUSDT","b":[["64002.1","1.2"]],"a":[["64002.2","0.8"]]}}`,
			bid: "64002.1", ask: "64002.2", millis: 1724328000003,
		},
		{
			name: "bitget",
			key:  Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"action":"snapshot","arg":{"instType":"USDT-FUTURES",` +
				`"channel":"books15","instId":"BTCUSDT"},"data":[{"bids":[["64003.1","1.2"]],` +
				`"asks":[["64003.2","0.8"]],"ts":"1724328000004"}]}`,
			bid: "64003.1", ask: "64003.2", millis: 1724328000004,
		},
		{
			name: "gate spot",
			key:  Key{Venue: VenueGate, Product: ProductSpot, Symbol: "BTC_USDT"},
			fixture: `{"time":1724328000,"time_ms":1724328000005,` +
				`"channel":"spot.order_book","event":"update",` +
				`"result":{"s":"BTC_USDT","bids":[["64004.1","2"]],` +
				`"asks":[["64004.2","3"]]}}`,
			bid: "64004.1", ask: "64004.2", millis: 1724328000005,
		},
		{
			name: "gate perpetual",
			key:  Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"},
			fixture: `{"time":1724328000,"time_ms":1724328000005,` +
				`"channel":"futures.order_book","event":"all",` +
				`"result":{"contract":"BTC_USDT","bids":[{"p":"64004.1","s":2}],` +
				`"asks":[{"p":"64004.2","s":3}]}}`,
			bid: "64004.1", ask: "64004.2", millis: 1724328000005,
		},
	}

	parsers := DefaultParsers()
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, matched, err := parsers[test.key.Venue](
				test.key, []byte(test.fixture), received,
			)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if !matched {
				t.Fatal("fixture did not match")
			}
			if got.Key != test.key || got.BidPrice != test.bid || got.AskPrice != test.ask {
				t.Fatalf("unexpected BBO: %#v", got)
			}
			if got.ReceiveTimestamp != received {
				t.Fatalf("receive timestamp = %v, want %v", got.ReceiveTimestamp, received)
			}
			if got.VenueTimestamp.UnixMilli() != test.millis {
				t.Fatalf(
					"venue timestamp = %d, want %d",
					got.VenueTimestamp.UnixMilli(),
					test.millis,
				)
			}
		})
	}
}

func TestParserIgnoresAcknowledgementAndOtherSymbol(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		key      Key
		fixtures []string
	}{
		{
			name: "binance",
			key:  Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"result":null,"id":1}`,
				`{"stream":"btcusdt@depth10@100ms","data":{"bids":[],"asks":[]}}`,
			},
		},
		{
			name: "okx",
			key:  Key{Venue: VenueOKX, Product: ProductSpot, Symbol: "BTC-USDT"},
			fixtures: []string{
				`{"event":"subscribe","arg":{"channel":"books5","instId":"BTC-USDT"}}`,
				`{"arg":{"channel":"books5","instId":"ETH-USDT"},"data":[{"bids":[["1"]],"asks":[["2"]]}]}`,
			},
		},
		{
			name: "bybit",
			key:  Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"success":true,"op":"subscribe"}`,
				`{"topic":"orderbook.1.BTCUSDT","type":"snapshot","data":{"s":"BTCUSDT","b":[],"a":[]}}`,
			},
		},
		{
			name: "bitget",
			key:  Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"event":"subscribe","arg":{"channel":"books15","instId":"BTCUSDT"}}`,
				`{"action":"snapshot","arg":{"channel":"books15","instId":"ETHUSDT"},"data":[]}`,
			},
		},
		{
			name: "gate",
			key:  Key{Venue: VenueGate, Product: ProductSpot, Symbol: "BTC_USDT"},
			fixtures: []string{
				`{"channel":"spot.order_book","event":"subscribe","result":{"status":"success"}}`,
				`{"channel":"spot.order_book","event":"update","result":{"s":"ETH_USDT","bids":[["1"]],"asks":[["2"]]}}`,
			},
		},
	}
	parsers := DefaultParsers()
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, fixture := range test.fixtures {
				_, matched, err := parsers[test.key.Venue](
					test.key, []byte(fixture), time.Now(),
				)
				if err != nil {
					t.Fatalf("parse non-data fixture: %v", err)
				}
				if matched {
					t.Fatalf("unexpected match for %s", fixture)
				}
			}
		})
	}
}

func TestGateParserRejectsDeltaEvent(t *testing.T) {
	t.Parallel()
	key := Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"}
	fixture := `{"channel":"futures.order_book","event":"update",` +
		`"result":[{"p":"64000","s":1,"c":"BTC_USDT"}]}`
	if _, matched, err := parseGate(key, []byte(fixture), time.Now()); err != nil {
		t.Fatal(err)
	} else if matched {
		t.Fatal("incremental Gate event matched snapshot parser")
	}
}
