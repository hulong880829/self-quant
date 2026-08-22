package polymarket

import (
	"log/slog"
	"testing"
	"time"
)

func TestParseCLOBQuoteMessagesSingleEvent(t *testing.T) {
	deltas := parseCLOBQuoteMessages([]byte(`{
		"asset_id":"tok-up","best_bid":"0.41","best_ask":"0.43"
	}`))
	if len(deltas) != 1 {
		t.Fatalf("deltas=%d", len(deltas))
	}
	if deltas[0].tokenID != "tok-up" || deltas[0].bid != "0.41" || deltas[0].ask != "0.43" {
		t.Fatalf("%+v", deltas[0])
	}
}

func TestParseCLOBQuoteMessagesArrayAndCamelCase(t *testing.T) {
	deltas := parseCLOBQuoteMessages([]byte(`[
		{"assetId":"a","bestBid":"0.1","bestAsk":"0.2"},
		{"asset_id":"b","best_bid":0.3,"best_ask":0.4}
	]`))
	if len(deltas) != 2 {
		t.Fatalf("deltas=%d", len(deltas))
	}
	if deltas[0].bid != "0.1" || deltas[1].bid != "0.3" {
		t.Fatalf("%+v", deltas)
	}
}

func TestParseCLOBQuoteMessagesPriceChanges(t *testing.T) {
	deltas := parseCLOBQuoteMessages([]byte(`{
		"price_changes":[
			{"asset_id":"up","best_bid":"0.55"},
			{"asset_id":"down","best_ask":"0.44"}
		]
	}`))
	if len(deltas) != 2 {
		t.Fatalf("deltas=%d %+v", len(deltas), deltas)
	}
}

func TestParseCLOBQuoteMessagesIgnoresBookWithoutQuotes(t *testing.T) {
	deltas := parseCLOBQuoteMessages([]byte(`{
		"asset_id":"tok","bids":[{"price":"0.4","size":"10"}]
	}`))
	if len(deltas) != 0 {
		t.Fatalf("expected empty, got %+v", deltas)
	}
}

func TestParseCLOBQuoteMessagesSkipsLargeBookWithoutQuoteKeys(t *testing.T) {
	body := []byte(`{"event_type":"book","asset_id":"tok","bids":[`)
	for index := 0; index < 200; index++ {
		if index > 0 {
			body = append(body, ',')
		}
		body = append(body, []byte(`{"price":"0.4","size":"1"}`)...)
	}
	body = append(body, []byte(`],"asks":[]}`)...)
	if deltas := parseCLOBQuoteMessages(body); len(deltas) != 0 {
		t.Fatalf("expected skip, got %+v", deltas)
	}
}

func TestApplyQuoteBatchKeepsLatestAndSkipsUnchanged(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "m1", Asset: "BTC", Period: "5m", Active: true,
		UpTokenID: "up", DownTokenID: "down",
		WindowStart: now.Add(-time.Minute),
		WindowEnd:   now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{
		Market: market, Series: []PricePoint{{Timestamp: now, ChainlinkPrice: "1"}},
		UpBid: "0.40", UpAsk: "0.41",
	})
	stream := NewMarketStream("ws://example", store, slog.Default())
	links := map[string]tokenMarketLink{
		"up":   {marketID: "m1", outcome: "up"},
		"down": {marketID: "m1", outcome: "down"},
	}
	stream.applyQuoteBatch(map[string]quoteDelta{
		"up": {tokenID: "up", bid: "0.40", ask: "0.41"},
	}, links)
	snapshot, _ := store.Get("m1")
	if snapshot.UpBid != "0.40" {
		t.Fatalf("unexpected mutate on unchanged: %q", snapshot.UpBid)
	}

	stream.applyQuoteBatch(map[string]quoteDelta{
		"up":   {tokenID: "up", bid: "0.61", ask: "0.62"},
		"down": {tokenID: "down", bid: "0.38", ask: "0.39"},
	}, links)
	snapshot, _ = store.Get("m1")
	if snapshot.UpBid != "0.61" || snapshot.DownAsk != "0.39" {
		t.Fatalf("%+v", snapshot)
	}
	if len(snapshot.Series) != 1 {
		t.Fatalf("series mutated: %d", len(snapshot.Series))
	}
}

func TestBuildTokenSubscriptionFingerprintStable(t *testing.T) {
	markets := []Market{
		{ID: "b", UpTokenID: "z", DownTokenID: "a"},
		{ID: "a", UpTokenID: "m", DownTokenID: "n"},
	}
	_, assets, key := buildTokenSubscription(markets)
	if len(assets) != 4 {
		t.Fatalf("assets=%d", len(assets))
	}
	_, _, key2 := buildTokenSubscription(markets)
	if key != key2 {
		t.Fatalf("fingerprint unstable %q vs %q", key, key2)
	}
	_, _, key3 := buildTokenSubscription([]Market{
		{ID: "a", UpTokenID: "m", DownTokenID: "n"},
		{ID: "b", UpTokenID: "z", DownTokenID: "CHANGED"},
	})
	if key == key3 {
		t.Fatal("expected fingerprint change")
	}
}

func TestNotifyMarketsChangedNonBlocking(t *testing.T) {
	stream := NewMarketStream("ws://example", NewSnapshotStore(), slog.Default())
	stream.NotifyMarketsChanged()
	stream.NotifyMarketsChanged()
	stream.NotifyMarketsChanged()
}

func TestShardAssets(t *testing.T) {
	shards := shardAssets([]string{"a", "b", "c", "d", "e"}, 2)
	if len(shards) != 3 {
		t.Fatalf("shards=%d", len(shards))
	}
	if len(shards[2]) != 1 || shards[2][0] != "e" {
		t.Fatalf("%+v", shards[2])
	}
}

func TestParseCLOBQuoteMessagesSkipsBookEventType(t *testing.T) {
	body := []byte(`{"event_type":"book","asset_id":"tok","best_bid":"0.1","bids":[{"price":"0.1"}]}`)
	if deltas := parseCLOBQuoteMessages(body); len(deltas) != 0 {
		t.Fatalf("book events must be skipped, got %+v", deltas)
	}
}

func TestApplyQuoteBatchPublishesLatestUpAndDownTogether(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "m1", Asset: "BTC", Period: "5m", Active: true,
		UpTokenID: "up", DownTokenID: "down",
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{Market: market, UpBid: "0.4", DownAsk: "0.5"})
	events, cancel := store.Subscribe("m1")
	defer cancel()
	stream := NewMarketStream("ws://example", store, slog.Default())
	stream.applyQuoteBatch(map[string]quoteDelta{
		"up":   {tokenID: "up", bid: "0.71", ask: "0.72"},
		"down": {tokenID: "down", bid: "0.28", ask: "0.29"},
	}, map[string]tokenMarketLink{
		"up": {marketID: "m1", outcome: "up"}, "down": {marketID: "m1", outcome: "down"},
	})
	snapshot, _ := store.Get("m1")
	if snapshot.UpBid != "0.71" || snapshot.DownAsk != "0.29" {
		t.Fatalf("%+v", snapshot)
	}
	event := <-events
	if event.Kind != SnapshotEventQuotes || event.Snapshot.UpAsk != "0.72" {
		t.Fatalf("%+v", event)
	}
}
