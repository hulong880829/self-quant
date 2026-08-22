package exchange

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOKXCancelReadsFinalOrderState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			_, _ = writer.Write([]byte(`{"code":"0","data":[{"ordId":"venue-1","sCode":"0"}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"code":"0","data":[{"ordId":"venue-1","state":"canceled","accFillSz":"0.4","avgPx":"100"}]}`))
	}))
	defer server.Close()

	adapter := newOKX(server.Client(), server.URL)
	result, err := adapter.CancelOrder(context.Background(), Credentials{
		APIKey: "key", APISecret: "secret", Passphrase: "passphrase",
	}, CancelRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP"},
		ClientOrderID: "client-1", VenueOrderID: "venue-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "canceled" || result.FilledQuantity != "0.4" || result.AveragePrice != "100" {
		t.Fatalf("result=%+v", result)
	}
}
