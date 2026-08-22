package aggdata

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeFairPriceHistory struct {
	points     []FairPriceHistoryPoint
	err        error
	profile    string
	symbol     string
	modelID    string
	resolution time.Duration
}

func (f *fakeFairPriceHistory) QueryHistory(
	_ context.Context,
	profile, symbol, modelID string,
	_, _ time.Time,
	resolution time.Duration,
	_ int,
) ([]FairPriceHistoryPoint, error) {
	f.profile = profile
	f.symbol = symbol
	f.modelID = modelID
	f.resolution = resolution
	return f.points, f.err
}

func TestFairPriceHistoryEndpointUsesProfileAndCurrentModel(t *testing.T) {
	root := t.TempDir()
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultFairPriceConfig()
	store, err := NewStoreWithFairPrice(config)
	if err != nil {
		t.Fatal(err)
	}
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_primary", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: "/sq.agg_primary.btcusdt.aggorderbook.2"},
	}}})
	reader := &fakeFairPriceHistory{points: []FairPriceHistoryPoint{{
		ObservedAt:      time.Date(2026, 8, 13, 13, 40, 1, 0, time.UTC),
		RingEpoch:       "18446744073709551615",
		RingSequence:    "7",
		SourceWallNS:    1_786_512_001_000_000_000,
		Price:           FixedValue{Mantissa: 6_374_667, Scale: 2},
		DegradedReasons: []string{},
	}}}
	server := NewServer(
		catalog, store, NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 4,
		time.Minute, 50*time.Millisecond,
	)
	server.SetFairPriceHistory(reader)
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/markets/BTCUSDT/fair-price-history?profile=agg_primary"+
			"&start=2026-08-13T13:40:00Z&end=2026-08-13T13:45:00Z&resolution=auto",
		nil,
	)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("history returned %d: %s", response.Code, response.Body.String())
	}
	var body fairPriceHistoryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if reader.profile != "agg_primary" || reader.symbol != "BTCUSDT" ||
		reader.modelID != store.FairPriceModelID() ||
		reader.resolution != time.Second {
		t.Fatalf("unexpected query: %+v", reader)
	}
	if body.ResolutionMS != 1000 || len(body.Points) != 1 ||
		body.Points[0].RingEpoch != "18446744073709551615" ||
		body.Points[0].Price.Mantissa != "6374667" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestFairPriceHistoryEndpointRejectsInvalidRange(t *testing.T) {
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
		Identity: Identity{Profile: "agg_primary", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: "/sq.agg_primary.btcusdt.aggorderbook.2"},
	}}})
	server := NewServer(
		catalog, store, NewHistory(root, 1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"", []string{"http://localhost:3000"}, 2, 4,
		time.Minute, 50*time.Millisecond,
	)
	server.SetFairPriceHistory(&fakeFairPriceHistory{})
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/markets/BTCUSDT/fair-price-history?profile=agg_primary"+
			"&start=2026-08-12T13:40:00Z&end=2026-08-13T13:40:01Z",
		nil,
	)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid range returned %d", response.Code)
	}
}

func TestFixedFromDecimalStringUsesRequestedScale(t *testing.T) {
	value, err := fixedFromDecimalString("63746.670000000000000000", 5)
	if err != nil {
		t.Fatal(err)
	}
	if value.Mantissa != 6_374_667_000 || value.Scale != 5 {
		t.Fatalf("unexpected fixed value: %+v", value)
	}
	if _, err := fixedFromDecimalString("1.000001", 5); err == nil {
		t.Fatal("precision loss was accepted")
	}
}
