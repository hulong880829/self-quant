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
			fixture: `{"e":"bookTicker","E":1724328000001,"s":"BTCUSDT",` +
				`"b":"64000.10","B":"1.2","a":"64000.20","A":"0.8"}`,
			bid: "64000.10", ask: "64000.20", millis: 1724328000001,
		},
		{
			name: "okx",
			key:  Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
			fixture: `{"arg":{"channel":"tickers","instId":"BTC-USDT-SWAP"},` +
				`"data":[{"instId":"BTC-USDT-SWAP","bidPx":"64001.1",` +
				`"askPx":"64001.2","ts":"1724328000002"}]}`,
			bid: "64001.1", ask: "64001.2", millis: 1724328000002,
		},
		{
			name: "bybit",
			key:  Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"topic":"tickers.BTCUSDT","ts":1724328000003,` +
				`"data":{"symbol":"BTCUSDT","bid1Price":"64002.1","ask1Price":"64002.2"}}`,
			bid: "64002.1", ask: "64002.2", millis: 1724328000003,
		},
		{
			name: "bitget",
			key:  Key{Venue: VenueBitget, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixture: `{"arg":{"instType":"SPOT","channel":"ticker","instId":"BTCUSDT"},` +
				`"data":[{"bidPr":"64003.1","askPr":"64003.2","ts":"1724328000004"}]}`,
			bid: "64003.1", ask: "64003.2", millis: 1724328000004,
		},
		{
			name: "gate",
			key:  Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"},
			fixture: `{"time":1724328000,"time_ms":1724328000005,` +
				`"channel":"futures.book_ticker","event":"update",` +
				`"result":{"s":"BTC_USDT","b":"64004.1","a":"64004.2"}}`,
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
	key := Key{Venue: VenueOKX, Product: ProductSpot, Symbol: "BTC-USDT"}
	parser := DefaultParsers()[VenueOKX]
	for _, fixture := range []string{
		`{"event":"subscribe","arg":{"channel":"tickers","instId":"BTC-USDT"}}`,
		`{"data":[{"instId":"ETH-USDT","bidPx":"1","askPx":"2","ts":"1724328000000"}]}`,
	} {
		_, matched, err := parser(key, []byte(fixture), time.Now())
		if err != nil {
			t.Fatalf("parse non-data fixture: %v", err)
		}
		if matched {
			t.Fatalf("unexpected match for %s", fixture)
		}
	}
}
