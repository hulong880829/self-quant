package polymarket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCLOBListTradesUsesL2AuthAndNormalizesFixture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/data/trades" ||
			request.Header.Get("POLY_API_KEY") != "key" ||
			request.Header.Get("POLY_SIGNATURE") == "" ||
			request.Header.Get("POLY_ADDRESS") == "" {
			t.Errorf("invalid authenticated trade request: %s", request.URL.String())
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"next_cursor":"","data":[{"trade_id":"trade-1",`+
			`"order_id":"order-1","market":"market-1","asset_id":"asset-1",`+
			`"side":"BUY","price":"0.4","size":"5","fee":"0.01",`+
			`"match_time":"2026-08-13T01:00:00Z"}]}`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, time.Second)
	trades, err := client.ListTrades(context.Background(), Credentials{
		SignerAddress: "0x1111111111111111111111111111111111111111",
		APIKey:        "key", APISecret: "c2VjcmV0", Passphrase: "pass",
	}, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 || trades[0].ID != "trade-1" ||
		trades[0].Side != "buy" || trades[0].MatchedAt.IsZero() {
		t.Fatalf("trades=%+v", trades)
	}
}
