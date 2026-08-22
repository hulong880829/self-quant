package rpc

import (
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"selfquant/backend/internal/polymarket"
)

func TestMapPolymarketPrivateUpstreamErrors(t *testing.T) {
	tests := []struct {
		statusCode int
		want       codes.Code
	}{
		{http.StatusUnauthorized, codes.Unauthenticated},
		{http.StatusTooManyRequests, codes.ResourceExhausted},
		{http.StatusInternalServerError, codes.Unavailable},
	}
	for _, test := range tests {
		got := mapPolymarketError(&polymarket.CLOBError{StatusCode: test.statusCode})
		if status.Code(got) != test.want {
			t.Fatalf("http=%d grpc=%s want=%s", test.statusCode, status.Code(got), test.want)
		}
	}
}

func TestStreamSnapshotEventIncludesPriceDelta(t *testing.T) {
	observedAt := time.Date(2026, time.August, 11, 2, 30, 1, 0, time.UTC)
	event := polymarket.SnapshotEvent{
		Kind: polymarket.SnapshotEventPriceDelta,
		Snapshot: polymarket.Snapshot{
			Market:         polymarket.Market{ID: "market-1"},
			OpenPrice:      "63990.00",
			ChainlinkPrice: "63991.25",
			SourceUpdated:  observedAt,
		},
		DeltaPoints: []polymarket.PricePoint{{
			Timestamp:      observedAt,
			OpenPrice:      "63990.00",
			ChainlinkPrice: "63991.25",
		}},
	}

	snapshot := streamSnapshotEvent(event)
	if len(snapshot.GetPriceSeries()) != 1 {
		t.Fatalf("price series length = %d, want 1", len(snapshot.GetPriceSeries()))
	}
	point := snapshot.GetPriceSeries()[0]
	if point.GetChainlinkPrice() != "63991.25" {
		t.Fatalf("chainlink price = %q, want %q", point.GetChainlinkPrice(), "63991.25")
	}
	if point.GetTimestamp().AsTime() != observedAt {
		t.Fatalf("timestamp = %s, want %s", point.GetTimestamp().AsTime(), observedAt)
	}
}

func TestStreamSnapshotEventOmitsSeriesForQuoteUpdate(t *testing.T) {
	snapshot := streamSnapshotEvent(polymarket.SnapshotEvent{
		Kind: polymarket.SnapshotEventQuotes,
		Snapshot: polymarket.Snapshot{
			Market: polymarket.Market{ID: "market-1"},
			Series: []polymarket.PricePoint{{ChainlinkPrice: "63991.25"}},
		},
	})

	if len(snapshot.GetPriceSeries()) != 0 {
		t.Fatalf("price series length = %d, want 0", len(snapshot.GetPriceSeries()))
	}
}
