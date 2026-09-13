package aggdata

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestServerStreamsFairPriceAndNewEpoch(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultFairPriceConfig()
	config.DepthK = 1
	store, err := NewStoreWithFairPrice(config)
	if err != nil {
		t.Fatal(err)
	}
	segment := "/sq.agg_binance.btcusdt.aggorderbook.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: segment},
	}}})
	store.BeginLearning(segment)
	applyFairTestBook(t, store, 9, 50, 1_000_000_000)

	handler := NewServer(
		catalog, store, NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 4,
		time.Minute, 50*time.Millisecond, 2*time.Millisecond, 10*time.Millisecond,
	)
	httpServer := httptest.NewServer(handler.Handler())
	defer httpServer.Close()
	headers := http.Header{"Origin": []string{"http://localhost:3000"}}
	connection, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/stream", headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{
		"op": "subscribe", "profile": "agg_binance",
		"symbol": "BTCUSDT", "channel": "fairprice",
	}); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	var acknowledgement struct {
		OK bool `json:"ok"`
	}
	if err := connection.ReadJSON(&acknowledgement); err != nil {
		t.Fatal(err)
	}
	if !acknowledgement.OK {
		t.Fatal("fairprice subscribe was rejected")
	}
	first := readFairMessage(t, connection)
	if first.RingEpoch != "9" || first.Sequence != "50" {
		t.Fatalf("unexpected first fair message: %+v", first)
	}

	applyFairTestBook(t, store, 10, 1, 2_000_000_000)
	second := readFairMessage(t, connection)
	if second.RingEpoch != "10" || second.Sequence != "1" {
		t.Fatalf("new epoch was not streamed: %+v", second)
	}
	heartbeat := readFairMessage(t, connection)
	if heartbeat.RingEpoch != "10" || heartbeat.Sequence != "1" {
		t.Fatalf("unexpected heartbeat: %+v", heartbeat)
	}
}

func TestServerFairPriceInvalidGraceAndRecovery(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultFairPriceConfig()
	config.DepthK = 1
	store, err := NewStoreWithFairPrice(config)
	if err != nil {
		t.Fatal(err)
	}
	segment := "/sq.agg_binance.btcusdt.aggorderbook.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: segment},
	}}})
	store.BeginLearning(segment)
	applyFairTestBook(t, store, 9, 50, 1_000_000_000)

	handler := NewServer(
		catalog, store, NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 4,
		time.Minute, 50*time.Millisecond, 2*time.Millisecond, time.Minute,
	)
	handler.fairInvalidGrace = 40 * time.Millisecond
	httpServer := httptest.NewServer(handler.Handler())
	defer httpServer.Close()
	headers := http.Header{"Origin": []string{"http://localhost:3000"}}
	connection, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/stream", headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{
		"op": "subscribe", "profile": "agg_binance",
		"symbol": "BTCUSDT", "channel": "fairprice",
	}); err != nil {
		t.Fatal(err)
	}
	var acknowledgement struct {
		OK bool `json:"ok"`
	}
	if err := connection.ReadJSON(&acknowledgement); err != nil {
		t.Fatal(err)
	}
	if !acknowledgement.OK {
		t.Fatal("fairprice subscribe was rejected")
	}
	if first := readFairMessage(t, connection); first.Sequence != "50" {
		t.Fatalf("unexpected first fair message: %+v", first)
	}

	applyInvalidFairTestBook(t, store, 9, 51, 2_000_000_000)
	type streamEvent struct {
		Op       string `json:"op"`
		Channel  string `json:"channel"`
		Reason   string `json:"reason"`
		Sequence string `json:"seq"`
	}
	result := make(chan struct {
		event streamEvent
		err   error
	}, 1)
	go func() {
		var event streamEvent
		err := connection.ReadJSON(&event)
		result <- struct {
			event streamEvent
			err   error
		}{event: event, err: err}
	}()
	select {
	case value := <-result:
		t.Fatalf("message arrived before invalid grace: event=%+v err=%v", value.event, value.err)
	case <-time.After(15 * time.Millisecond):
	}
	select {
	case value := <-result:
		if value.err != nil {
			t.Fatal(value.err)
		}
		if value.event.Op != "reset" || value.event.Channel != "fairprice" ||
			value.event.Reason != "source_stale" {
			t.Fatalf("unexpected reset after invalid grace: %+v", value.event)
		}
	case <-time.After(time.Second):
		t.Fatal("fairprice reset was not sent after invalid grace")
	}

	applyFairTestBook(t, store, 9, 52, 3_000_000_000)
	recovered := readFairMessage(t, connection)
	if recovered.Sequence != "52" {
		t.Fatalf("fairprice did not recover with the new sequence: %+v", recovered)
	}
}

func TestServerRejectsFairPriceDepth(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStoreWithFairPrice(DefaultFairPriceConfig())
	if err != nil {
		t.Fatal(err)
	}
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: "/sq.agg_binance.btcusdt.aggorderbook.2"},
	}}})
	handler := NewServer(
		catalog, store, NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 4,
		time.Minute, 50*time.Millisecond,
	)
	httpServer := httptest.NewServer(handler.Handler())
	defer httpServer.Close()
	headers := http.Header{"Origin": []string{"http://localhost:3000"}}
	connection, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/stream", headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{
		"op": "subscribe", "symbol": "BTCUSDT", "channel": "fairprice", "depth": 20,
	}); err != nil {
		t.Fatal(err)
	}
	var acknowledgement struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := connection.ReadJSON(&acknowledgement); err != nil {
		t.Fatal(err)
	}
	if acknowledgement.OK || !strings.Contains(acknowledgement.Error, "depth") {
		t.Fatalf("unexpected acknowledgement: %+v", acknowledgement)
	}
}

func applyFairTestBook(
	t *testing.T,
	store *Store,
	epoch, sequence, wall uint64,
) {
	t.Helper()
	snapshot := fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 10}},
		[]Level{{Price: 101, Quantity: 10}},
		wall,
	)
	book := snapshot.Book
	book.VenueSlotIDs[0], book.MemberCount, book.MemberMask, book.ActiveMask = 1, 1, 1, 1
	if err := store.Apply(GatewayFrame{
		Kind: KindBook, Format: 2, TopicID: 7,
		RingEpoch: epoch, RingSequence: sequence, Generation: wall,
		WallNS: wall, Book: book,
	}); err != nil {
		t.Fatal(err)
	}
}

func applyInvalidFairTestBook(
	t *testing.T,
	store *Store,
	epoch, sequence, wall uint64,
) {
	t.Helper()
	snapshot := fairTestSnapshot(
		[]Level{{Price: 101, Quantity: 1}},
		[]Level{{Price: 100, Quantity: 1}},
		wall,
	)
	book := snapshot.Book
	book.VenueSlotIDs[0], book.MemberCount, book.MemberMask, book.ActiveMask = 1, 1, 1, 1
	if err := store.Apply(GatewayFrame{
		Kind: KindBook, Format: 2, TopicID: 7,
		RingEpoch: epoch, RingSequence: sequence, Generation: wall,
		WallNS: wall, Book: book,
	}); err != nil {
		t.Fatal(err)
	}
}

func readFairMessage(t *testing.T, connection *websocket.Conn) struct {
	Channel   string `json:"channel"`
	RingEpoch string `json:"ring_epoch"`
	Sequence  string `json:"seq"`
} {
	t.Helper()
	for range 4 {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var result struct {
			Channel   string `json:"channel"`
			RingEpoch string `json:"ring_epoch"`
			Sequence  string `json:"seq"`
		}
		if err := json.Unmarshal(payload, &result); err != nil {
			t.Fatal(err)
		}
		if result.Channel == "fairprice" {
			return result
		}
	}
	t.Fatal("fairprice message was not received")
	return struct {
		Channel   string `json:"channel"`
		RingEpoch string `json:"ring_epoch"`
		Sequence  string `json:"seq"`
	}{}
}
