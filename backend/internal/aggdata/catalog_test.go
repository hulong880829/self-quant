package aggdata

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCatalogUnionsActiveAndShards(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Refresh(); !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("expected missing manifest, got %v", err)
	}
	manifest := `{
		"version":1,
		"state":"recording",
		"sample_interval_ms":200,
		"retention_hours":24,
		"depth":50,
		"active":[
			{"segment":"/selfquant.mds.agg_perp_binance.btcusdt.aggbbo.2","kind":"aggbbo"}
		],
		"shards":[
			"2026-08-12/14/agg_perp_binance/ethusdt/aggbbo.sqrec.zst",
			"2026-08-12/14/agg_perp_binance/btcusdt/aggorderbook.sqrec.zst"
		]
	}`
	if err := os.WriteFile(filepath.Join(root, "manifest.v1.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := catalog.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snapshot := catalog.Snapshot()
	if !changed || len(snapshot.Markets) != 2 {
		t.Fatalf("unexpected catalog: %+v", snapshot)
	}
	btc := snapshot.Markets[0]
	if btc.Symbol != "BTCUSDT" || !btc.Recording ||
		btc.Segments[KindBBO] == "" || len(btc.Shards) != 1 {
		t.Fatalf("unexpected BTC market: %+v", btc)
	}
}

func TestCatalogRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{
		"version":1,"state":"stopped","sample_interval_ms":200,
		"retention_hours":24,"depth":20,"active":[],
		"shards":["../outside/aggbbo.sqrec.zst"]
	}`
	if err := os.WriteFile(filepath.Join(root, "manifest.v1.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Refresh(); err == nil {
		t.Fatal("traversal manifest unexpectedly accepted")
	}
}

func TestServerCORSPreflight(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(
		catalog, NewStore(), NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 2, time.Minute, 50*time.Millisecond,
	)
	request := httptest.NewRequest(http.MethodOptions, "/v1/markets", nil)
	request.Header.Set("Origin", "http://localhost:3000")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent ||
		response.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("unexpected preflight response: %d %v", response.Code, response.Header())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/markets", nil)
	request.Header.Set("Origin", "https://untrusted.example")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("untrusted origin returned %d", response.Code)
	}
}

func TestServerStreamsCompactOrderBook(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	segment := "/sq.agg_binance.btcusdt.aggorderbook.2"
	store := NewStore()
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: segment},
	}}})
	store.BeginLearning(segment)
	payload := make([]byte, 69+2*85)
	copy(payload[14:30], "BTC")
	copy(payload[30:46], "USDT")
	payload[46], payload[54], payload[55], payload[56] = 1, 2, 3, 1
	binary.LittleEndian.PutUint32(payload[57:61], 1)
	binary.LittleEndian.PutUint32(payload[61:65], 1)
	binary.LittleEndian.PutUint16(payload[65:67], 1)
	binary.LittleEndian.PutUint16(payload[67:69], 1)
	putCompactLevel(payload[69:154], 100, 5)
	putCompactLevel(payload[154:239], 101, 6)
	frame, err := DecodeGatewayFrame(gatewayFrameForTest(KindBook, 2, 3, payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(frame); err != nil {
		t.Fatal(err)
	}

	handler := NewServer(
		catalog, store, NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 2, time.Minute, 5*time.Millisecond,
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
		"op": "subscribe", "symbol": "BTCUSDT", "channel": "orderbook", "depth": 20,
	}); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	sawBinary := false
	for range 3 {
		messageType, body, err := connection.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if messageType == websocket.BinaryMessage {
			sawBinary = len(body) >= 4 && binary.LittleEndian.Uint32(body[:4]) == browserMagic
			break
		}
	}
	if !sawBinary {
		t.Fatal("compact SQAB orderbook frame was not streamed")
	}
}
