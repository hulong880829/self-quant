package polymarket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDataClientParsesPositionEndDate(t *testing.T) {
	end := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/positions" || r.URL.Query().Get("user") != "0xwallet" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"asset":"token","conditionId":"condition","title":"Market","outcome":"Yes","size":2,"avgPrice":0.4,"curPrice":0.5,"initialValue":0.8,"currentValue":1,"cashPnl":0.2,"redeemable":false,"endDate":%q}]`, end.Format(time.RFC3339))
	}))
	defer server.Close()
	positions, err := NewDataClient(server.URL, time.Second).ListPositions(context.Background(), "0xwallet")
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || !positions[0].EndTime.Equal(end) {
		t.Fatalf("positions=%+v", positions)
	}
}
