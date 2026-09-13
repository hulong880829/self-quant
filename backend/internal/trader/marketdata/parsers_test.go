package marketdata

import (
	"errors"
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
		bidQty  string
		askQty  string
		millis  int64
	}{
		{
			name: "binance",
			key:  Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixture: `{"stream":"btcusdt@depth10@100ms","data":{"E":1724328000001,` +
				`"lastUpdateId":42,"bids":[["64000.10","1.2"]],"asks":[["64000.20","0.8"]]}}`,
			bid: "64000.10", ask: "64000.20", bidQty: "1.2", askQty: "0.8", millis: 1724328000001,
		},
		{
			name: "binance perpetual",
			key:  Key{Venue: VenueBinance, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"stream":"btcusdt@depth10@100ms","data":{"e":"depthUpdate",` +
				`"E":1724328000001,"s":"BTCUSDT","b":[["64000.10","1.2"]],` +
				`"a":[["64000.20","0.8"]]}}`,
			bid: "64000.10", ask: "64000.20", bidQty: "1.2", askQty: "0.8", millis: 1724328000001,
		},
		{
			name: "okx",
			key:  Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
			fixture: `{"arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},` +
				`"data":[{"bids":[["64001.1","1","0","1"]],` +
				`"asks":[["64001.2","1","0","1"]],"ts":"1724328000002"}]}`,
			bid: "64001.1", ask: "64001.2", bidQty: "1", askQty: "1", millis: 1724328000002,
		},
		{
			name: "bybit",
			key:  Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328000003,` +
				`"data":{"s":"BTCUSDT","b":[["64002.1","1.2"]],"a":[["64002.2","0.8"]]}}`,
			bid: "64002.1", ask: "64002.2", bidQty: "1.2", askQty: "0.8", millis: 1724328000003,
		},
		{
			name: "bitget",
			key:  Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"action":"snapshot","arg":{"instType":"USDT-FUTURES",` +
				`"channel":"books15","instId":"BTCUSDT"},"data":[{"bids":[["64003.1","1.2"]],` +
				`"asks":[["64003.2","0.8"]],"ts":"1724328000004"}]}`,
			bid: "64003.1", ask: "64003.2", bidQty: "1.2", askQty: "0.8", millis: 1724328000004,
		},
		{
			name: "gate spot",
			key:  Key{Venue: VenueGate, Product: ProductSpot, Symbol: "BTC_USDT"},
			fixture: `{"time":1724328000,"time_ms":1724328000005,` +
				`"channel":"spot.order_book","event":"update",` +
				`"result":{"s":"BTC_USDT","bids":[["64004.1","2"]],` +
				`"asks":[["64004.2","3"]]}}`,
			bid: "64004.1", ask: "64004.2", bidQty: "2", askQty: "3", millis: 1724328000005,
		},
		{
			name: "gate perpetual",
			key:  Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BEAT_USDT"},
			fixture: `{"time":1724328000,"time_ms":1724328000005,` +
				`"channel":"futures.book_ticker","event":"update",` +
				`"result":{"t":1724328000006,"s":"BEAT_USDT",` +
				`"b":"0.1253","B":297,"a":"0.1254","A":26}}`,
			bid: "0.1253", ask: "0.1254", bidQty: "297", askQty: "26", millis: 1724328000006,
		},
		{
			name: "hyperliquid",
			key:  Key{Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "BTC"},
			fixture: `{"channel":"bbo","data":{"coin":"BTC","time":1724328000007,` +
				`"bbo":[{"px":"64005.1","sz":"1.2"},{"px":"64005.2","sz":"0.8"}]}}`,
			bid: "64005.1", ask: "64005.2", bidQty: "1.2", askQty: "0.8", millis: 1724328000007,
		},
		{
			name: "aster",
			key:  Key{Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"e":"bookTicker","u":520723843873,"s":"BTCUSDT",` +
				`"b":"64006.1","B":"3.237","a":"64006.2","A":"0.096",` +
				`"T":1724328000007,"E":1724328000008}`,
			bid: "64006.1", ask: "64006.2", bidQty: "3.237", askQty: "0.096", millis: 1724328000008,
		},
		{
			name: "lighter snapshot",
			key:  Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"},
			fixture: `{"channel":"order_book:1","timestamp":1724328000009,` +
				`"type":"subscribed/order_book","order_book":{"nonce":100,"begin_nonce":0,` +
				`"bids":[{"price":"64007.1","size":"1.2"}],` +
				`"asks":[{"price":"64007.2","size":"0.8"}]}}`,
			bid: "64007.1", ask: "64007.2", bidQty: "1.2", askQty: "0.8", millis: 1724328000009,
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
			if got.Key != test.key || got.BidPrice != test.bid || got.AskPrice != test.ask ||
				got.BidQuantity != test.bidQty || got.AskQuantity != test.askQty {
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

func TestLighterBookDeltaMaintainsBBOAndNonceContinuity(t *testing.T) {
	t.Parallel()
	key := Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"}
	parser := (&lighterBookParser{}).parse
	received := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	snapshot := `{"channel":"order_book:1","timestamp":1724328000001,` +
		`"type":"subscribed/order_book","order_book":{"nonce":100,"begin_nonce":0,` +
		`"bids":[{"price":"100.0","size":"1"},{"price":"99.0","size":"2"}],` +
		`"asks":[{"price":"101.0","size":"1"},{"price":"102.0","size":"2"}]}}`
	bbo, matched, err := parser(key, []byte(snapshot), received)
	if err != nil || !matched || bbo.BidPrice != "100.0" || bbo.AskPrice != "101.0" ||
		bbo.BidQuantity != "1" || bbo.AskQuantity != "1" {
		t.Fatalf("snapshot = (%#v, %t, %v)", bbo, matched, err)
	}

	delta := `{"channel":"order_book:1","timestamp":1724328000002,` +
		`"type":"update/order_book","order_book":{"nonce":101,"begin_nonce":100,` +
		`"bids":[{"price":"100.00","size":"0"},{"price":"100.5","size":"3"}],` +
		`"asks":[{"price":"101.00","size":"0"},{"price":"100.8","size":"4"}]}}`
	bbo, matched, err = parser(key, []byte(delta), received)
	if err != nil || !matched || bbo.BidPrice != "100.5" || bbo.AskPrice != "100.8" ||
		bbo.BidQuantity != "3" || bbo.AskQuantity != "4" {
		t.Fatalf("delta = (%#v, %t, %v)", bbo, matched, err)
	}

	gap := `{"channel":"order_book:1","timestamp":1724328000003,` +
		`"type":"update/order_book","order_book":{"nonce":103,"begin_nonce":99,` +
		`"bids":[],"asks":[]}}`
	if _, _, err := parser(key, []byte(gap), received); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("gap error = %v, want ErrSequenceGap", err)
	}
}

func TestLighterDeltaBeforeSnapshotFailsClosed(t *testing.T) {
	t.Parallel()
	key := Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"}
	delta := `{"channel":"order_book:1","timestamp":1724328000002,` +
		`"type":"update/order_book","order_book":{"nonce":101,"begin_nonce":100,` +
		`"bids":[{"price":"100","size":"1"}],"asks":[{"price":"101","size":"1"}]}}`
	if _, _, err := (&lighterBookParser{}).parse(
		key, []byte(delta), time.Now(),
	); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("delta-before-snapshot error = %v, want ErrSequenceGap", err)
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

func TestGateParserRejectsEmptyBookTickerSide(t *testing.T) {
	t.Parallel()
	key := Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BEAT_USDT"}
	fixture := `{"channel":"futures.book_ticker","event":"update",` +
		`"result":{"t":1724328000006,"s":"BEAT_USDT","b":"0.1253","a":""}}`
	if _, matched, err := parseGate(key, []byte(fixture), time.Now()); err != nil {
		t.Fatal(err)
	} else if matched {
		t.Fatal("empty Gate ask matched BBO parser")
	}
}

func TestBybitLevelOneAcceptsConsecutiveSnapshotsOnly(t *testing.T) {
	t.Parallel()
	key := Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"}
	received := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	for index, fixture := range []string{
		`{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328000003,` +
			`"data":{"s":"BTCUSDT","b":[["64002.1","1.2"]],"a":[["64002.2","0.8"]]}}`,
		`{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328003003,` +
			`"data":{"s":"BTCUSDT","b":[["64003.1","1.1"]],"a":[["64003.2","0.7"]]}}`,
	} {
		bbo, matched, err := parseBybit(key, []byte(fixture), received)
		if err != nil || !matched {
			t.Fatalf("snapshot %d = (%#v, %t, %v)", index, bbo, matched, err)
		}
		if bbo.BidPrice != []string{"64002.1", "64003.1"}[index] {
			t.Fatalf("snapshot %d bid = %s", index, bbo.BidPrice)
		}
	}

	delta := `{"topic":"orderbook.1.BTCUSDT","type":"delta","ts":1724328004003,` +
		`"data":{"s":"BTCUSDT","b":[["64004.1","1.0"]],"a":[["64004.2","0.6"]]}}`
	if _, matched, err := parseBybit(key, []byte(delta), received); err != nil {
		t.Fatal(err)
	} else if matched {
		t.Fatal("Bybit orderbook.1 delta unexpectedly matched")
	}
}

func TestSnapshotChannelsRejectEmptySingleSide(t *testing.T) {
	t.Parallel()
	received := time.Now()
	tests := []struct {
		name    string
		key     Key
		parser  Parser
		fixture string
	}{
		{
			name:   "binance",
			key:    Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			parser: parseBinance,
			fixture: `{"stream":"btcusdt@depth10@100ms",` +
				`"data":{"bids":[["1","1"]],"asks":[]}}`,
		},
		{
			name:   "okx",
			key:    Key{Venue: VenueOKX, Product: ProductSpot, Symbol: "BTC-USDT"},
			parser: parseOKX,
			fixture: `{"arg":{"channel":"books5","instId":"BTC-USDT"},` +
				`"data":[{"bids":[],"asks":[["2","1"]]}]}`,
		},
		{
			name:   "bybit",
			key:    Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			parser: parseBybit,
			fixture: `{"topic":"orderbook.1.BTCUSDT","type":"snapshot",` +
				`"data":{"s":"BTCUSDT","b":[["1","1"]],"a":[]}}`,
		},
		{
			name:   "bitget",
			key:    Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			parser: parseBitget,
			fixture: `{"action":"snapshot","arg":{"channel":"books15","instId":"BTCUSDT"},` +
				`"data":[{"bids":[],"asks":[["2","1"]]}]}`,
		},
		{
			name:   "gate spot",
			key:    Key{Venue: VenueGate, Product: ProductSpot, Symbol: "BTC_USDT"},
			parser: parseGate,
			fixture: `{"channel":"spot.order_book","event":"update",` +
				`"result":{"s":"BTC_USDT","bids":[["1","1"]],"asks":[]}}`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, matched, err := test.parser(test.key, []byte(test.fixture), received); err != nil {
				t.Fatal(err)
			} else if matched {
				t.Fatal("single-sided book unexpectedly matched")
			}
		})
	}
}

func TestHyperliquidNativeBBOParser(t *testing.T) {
	t.Parallel()
	key := Key{Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "CASHCAT"}
	received := time.Date(2026, 8, 22, 12, 0, 0, 123, time.UTC)
	ok := `{"channel":"bbo","data":{"coin":"CASHCAT","time":1724328000007,` +
		`"bbo":[{"px":"1.25","sz":"10"},{"px":"1.26","sz":"8"}]}}`
	got, matched, err := parseHyperliquid(key, []byte(ok), received)
	if err != nil || !matched {
		t.Fatalf("valid bbo = (%#v, %t, %v)", got, matched, err)
	}
	if got.BidPrice != "1.25" || got.AskPrice != "1.26" ||
		got.ReceiveTimestamp != received || got.VenueTimestamp.UnixMilli() != 1724328000007 {
		t.Fatalf("parsed BBO=%#v", got)
	}

	rejects := []string{
		`{"channel":"l2Book","data":{"coin":"CASHCAT","time":1724328000007,` +
			`"levels":[[{"px":"1.25","sz":"10"}],[{"px":"1.26","sz":"8"}]]}}`,
		`{"channel":"bbo","data":{"coin":"CASHCAT","time":1724328000007,` +
			`"bbo":[null,{"px":"1.26","sz":"8"}]}}`,
		`{"channel":"bbo","data":{"coin":"CASHCAT","time":1724328000007,` +
			`"bbo":[{"px":"1.25","sz":"10"},null]}}`,
		`{"channel":"bbo","data":{"coin":"CASHCAT","time":1724328000007,` +
			`"bbo":[{"px":"-1.25","sz":"10"},{"px":"1.26","sz":"8"}]}}`,
		`{"channel":"bbo","data":{"coin":"CASHCAT","time":1724328000007,` +
			`"bbo":[{"px":"1.26","sz":"10"},{"px":"1.25","sz":"8"}]}}`,
		`{"channel":"bbo","data":{"coin":"OTHER","time":1724328000007,` +
			`"bbo":[{"px":"1.25","sz":"10"},{"px":"1.26","sz":"8"}]}}`,
	}
	for _, fixture := range rejects {
		if _, matched, err := parseHyperliquid(key, []byte(fixture), received); err != nil {
			t.Fatalf("reject fixture err=%v fixture=%s", err, fixture)
		} else if matched {
			t.Fatalf("invalid hyperliquid frame matched: %s", fixture)
		}
	}
}
