package marketdata

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestEightVenueDeterministicSoak(t *testing.T) {
	tests := []struct {
		name     string
		key      Key
		fixtures []string
		prices   []string
	}{
		{
			name: "binance",
			key:  Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"stream":"btcusdt@depth10@100ms","data":{"E":1724328000001,"bids":[["100","1"]],"asks":[["101","1"]]}}`,
				`{"stream":"btcusdt@depth10@100ms","data":{"E":1724328000002,"bids":[["102","1"]],"asks":[["103","1"]]}}`,
				`{"stream":"btcusdt@depth10@100ms","data":{"E":1724328000003,"bids":[["104","1"]],"asks":[["105","1"]]}}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "okx",
			key:  Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
			fixtures: []string{
				`{"arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},"data":[{"bids":[["100","1"]],"asks":[["101","1"]],"ts":"1724328000001"}]}`,
				`{"arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},"data":[{"bids":[["102","1"]],"asks":[["103","1"]],"ts":"1724328000002"}]}`,
				`{"arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},"data":[{"bids":[["104","1"]],"asks":[["105","1"]],"ts":"1724328000003"}]}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "bybit",
			key:  Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328000001,"data":{"s":"BTCUSDT","b":[["100","1"]],"a":[["101","1"]]}}`,
				`{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328000002,"data":{"s":"BTCUSDT","b":[["102","1"]],"a":[["103","1"]]}}`,
				`{"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":1724328000003,"data":{"s":"BTCUSDT","b":[["104","1"]],"a":[["105","1"]]}}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "bitget",
			key:  Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"action":"snapshot","arg":{"channel":"books15","instId":"BTCUSDT"},"data":[{"bids":[["100","1"]],"asks":[["101","1"]],"ts":"1724328000001"}]}`,
				`{"action":"snapshot","arg":{"channel":"books15","instId":"BTCUSDT"},"data":[{"bids":[["102","1"]],"asks":[["103","1"]],"ts":"1724328000002"}]}`,
				`{"action":"snapshot","arg":{"channel":"books15","instId":"BTCUSDT"},"data":[{"bids":[["104","1"]],"asks":[["105","1"]],"ts":"1724328000003"}]}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "gate",
			key:  Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"},
			fixtures: []string{
				`{"channel":"futures.book_ticker","event":"update","result":{"t":1724328000001,"s":"BTC_USDT","b":"100","a":"101"}}`,
				`{"channel":"futures.book_ticker","event":"update","result":{"t":1724328000002,"s":"BTC_USDT","b":"102","a":"103"}}`,
				`{"channel":"futures.book_ticker","event":"update","result":{"t":1724328000003,"s":"BTC_USDT","b":"104","a":"105"}}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "hyperliquid",
			key:  Key{Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "BTC"},
			fixtures: []string{
				`{"channel":"bbo","data":{"coin":"BTC","time":1724328000001,"bbo":[{"px":"100","sz":"1"},{"px":"101","sz":"1"}]}}`,
				`{"channel":"bbo","data":{"coin":"BTC","time":1724328000002,"bbo":[{"px":"102","sz":"1"},{"px":"103","sz":"1"}]}}`,
				`{"channel":"bbo","data":{"coin":"BTC","time":1724328000003,"bbo":[{"px":"104","sz":"1"},{"px":"105","sz":"1"}]}}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "aster",
			key:  Key{Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixtures: []string{
				`{"e":"bookTicker","s":"BTCUSDT","b":"100","B":"1","a":"101","A":"1","E":1724328000001}`,
				`{"e":"bookTicker","s":"BTCUSDT","b":"102","B":"1","a":"103","A":"1","E":1724328000002}`,
				`{"e":"bookTicker","s":"BTCUSDT","b":"104","B":"1","a":"105","A":"1","E":1724328000003}`,
			},
			prices: []string{"100", "102", "104"},
		},
		{
			name: "lighter",
			key:  Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"},
			fixtures: []string{
				`{"type":"subscribed/order_book","channel":"order_book:1","timestamp":1724328000001,"order_book":{"nonce":100,"begin_nonce":0,"bids":[{"price":"100","size":"1"}],"asks":[{"price":"101","size":"1"}]}}`,
				`{"type":"update/order_book","channel":"order_book:1","timestamp":1724328000002,"order_book":{"nonce":101,"begin_nonce":100,"bids":[{"price":"100","size":"0"},{"price":"102","size":"1"}],"asks":[{"price":"101","size":"0"},{"price":"103","size":"1"}]}}`,
				`{"type":"subscribed/order_book","channel":"order_book:1","timestamp":1724328000003,"order_book":{"nonce":200,"begin_nonce":0,"bids":[{"price":"104","size":"1"}],"asks":[{"price":"105","size":"1"}]}}`,
			},
			prices: []string{"100", "102", "104"},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			first := newFakeConnection()
			second := newFakeConnection()
			connector := &fakeConnector{
				connections: []*fakeConnection{first, second},
				connected:   make(chan int, 4),
			}
			manager, err := New(Options{
				Connector: connector,
				Parsers: map[string]Parser{
					test.key.Venue: DefaultParsers()[test.key.Venue],
				},
				ParserFactories:  defaultParserFactories(),
				StaleAfter:       time.Second,
				ReconnectInitial: time.Millisecond,
				ReconnectMax:     2 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			subscription, err := manager.Subscribe(context.Background(), test.key)
			if err != nil {
				t.Fatal(err)
			}
			defer subscription.Close()
			waitForConnectCount(t, connector.connected, 1)

			first.reads <- fakeRead{payload: []byte(test.fixtures[0])}
			waitForPrice(t, manager, test.key, test.prices[0])
			first.reads <- fakeRead{payload: []byte(test.fixtures[1])}
			waitForPrice(t, manager, test.key, test.prices[1])

			first.reads <- fakeRead{err: io.ErrUnexpectedEOF}
			waitForNoBBO(t, manager, test.key)
			waitForConnectCount(t, connector.connected, 2)
			second.reads <- fakeRead{payload: []byte(test.fixtures[2])}
			waitForPrice(t, manager, test.key, test.prices[2])

			stats := manager.Stats()
			if stats.Updates != 3 || stats.Disconnects != 1 || stats.Reconnects != 1 {
				t.Fatalf("soak stats = %+v", stats)
			}
		})
	}
}

func waitForNoBBO(t *testing.T, manager *Manager, key Key) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := manager.Latest(key); errors.Is(err, ErrNoValue) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("BBO %v remained available", key)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPublicMarketDataSoak(t *testing.T) {
	if os.Getenv("RUN_PUBLIC_MARKETDATA_SOAK") != "1" {
		t.Skip("set RUN_PUBLIC_MARKETDATA_SOAK=1 for public read-only WSS soak")
	}
	duration := 30 * time.Second
	if value := os.Getenv("PUBLIC_MARKETDATA_SOAK_DURATION"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid PUBLIC_MARKETDATA_SOAK_DURATION %q", value)
		}
		duration = parsed
	}
	keys := []Key{
		{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
		{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
		{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
		{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "BTCUSDT"},
		{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"},
		{Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "BTC"},
		{Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT"},
		{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"},
	}
	manager, err := New(Options{StaleAfter: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	type sample struct {
		key Key
		bbo BBO
	}
	samples := make(chan sample, 256)
	subscriptions := make([]*Subscription, 0, len(keys))
	for _, key := range keys {
		subscription, err := manager.Subscribe(ctx, key)
		if err != nil {
			t.Fatalf("subscribe %v: %v", key, err)
		}
		subscriptions = append(subscriptions, subscription)
		go func(key Key, updates <-chan BBO) {
			for {
				select {
				case <-ctx.Done():
					return
				case bbo, ok := <-updates:
					if !ok {
						return
					}
					select {
					case samples <- sample{key: key, bbo: bbo}:
					case <-ctx.Done():
						return
					}
				}
			}
		}(key, subscription.Updates())
	}
	defer func() {
		for _, subscription := range subscriptions {
			_ = subscription.Close()
		}
	}()

	counts := make(map[Key]int)
	last := make(map[Key]time.Time)
	maxGap := make(map[Key]time.Duration)
	for {
		select {
		case item := <-samples:
			bid, bidErr := strconv.ParseFloat(item.bbo.BidPrice, 64)
			ask, askErr := strconv.ParseFloat(item.bbo.AskPrice, 64)
			if bidErr != nil || askErr != nil || bid <= 0 || ask <= 0 || bid > ask {
				t.Fatalf("invalid public BBO for %v: %#v", item.key, item.bbo)
			}
			if previous := last[item.key]; !previous.IsZero() {
				if gap := item.bbo.ReceiveTimestamp.Sub(previous); gap > maxGap[item.key] {
					maxGap[item.key] = gap
				}
			}
			last[item.key] = item.bbo.ReceiveTimestamp
			counts[item.key]++
		case <-ctx.Done():
			for _, key := range keys {
				if counts[key] < 2 {
					t.Errorf("%v received %d BBO updates, want at least 2", key, counts[key])
				}
				if maxGap[key] > 15*time.Second {
					t.Errorf("%v maximum BBO gap = %v", key, maxGap[key])
				}
			}
			stats := manager.Stats()
			if stats.Reconnects > uint64(len(keys)*3) {
				t.Errorf("excessive public WSS reconnects: %+v", stats)
			}
			t.Logf("public read-only WSS soak stats: %+v; counts=%v; maxGap=%v", stats, counts, maxGap)
			return
		}
	}
}

func TestSlowConsumerSoakDoesNotLeakPublishers(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection},
		connected:   make(chan int, 2),
	}
	manager, err := New(Options{
		Connector: connector,
		Parsers:   map[string]Parser{VenueBinance: testParser},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	key := Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"}
	subscription, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	waitForConnectCount(t, connector.connected, 1)

	var producers sync.WaitGroup
	producers.Add(1)
	go func() {
		defer producers.Done()
		for index := 0; index < 10_000; index++ {
			connection.reads <- fakeRead{payload: []byte(strconv.Itoa(index))}
		}
	}()
	completed := make(chan struct{})
	go func() {
		producers.Wait()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked behind slow subscriber")
	}
	waitForPrice(t, manager, key, "9999")
}
