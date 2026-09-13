package exchange

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	result, err := adapter.CancelAndGetOrder(context.Background(), Credentials{
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

func TestOKXCancelDoesNotInventTerminalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			_, _ = writer.Write([]byte(`{"code":"0","data":[{"ordId":"venue-1","sCode":"0"}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{
			"code":"0","data":[{"ordId":"venue-1","state":"partially_filled",
			"accFillSz":"0.4","avgPx":"100"}]
		}`))
	}))
	defer server.Close()

	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "passphrase"},
		CancelRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			},
			VenueOrderID: "venue-1",
		},
	)
	if !errors.Is(err, ErrUncertain) || result.Status != "partially_filled" ||
		result.FilledQuantity != "0.4" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestOKXCancelQueryFailureIsUncertain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			_, _ = writer.Write([]byte(`{"code":"0","data":[{"ordId":"venue-1","sCode":"0"}]}`))
			return
		}
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"code":"500","msg":"busy"}`))
	}))
	defer server.Close()
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "passphrase"},
		CancelRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			},
			VenueOrderID: "venue-1",
		},
	)
	if !errors.Is(err, ErrUncertain) || result.Status != "pending" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

const okxCancel51400Code1 = `{"code":"1","data":[{"sCode":"51400","sMsg":"Cancellation failed as the order has been filled, canceled or does not exist.","ordId":"venue-1"}]}`

const okxCancel51400Code0 = `{"code":"0","data":[{"sCode":"51400","sMsg":"Cancellation failed as the order has been filled, canceled or does not exist.","ordId":"venue-1"}]}`

func TestOKXCancel51400GetFilled(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK,
		`{"code":"0","data":[{"ordId":"venue-1","state":"filled","accFillSz":"70","avgPx":"100.5"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "filled" || result.FilledQuantity != "70" || result.AveragePrice != "100.5" {
		t.Fatalf("result=%+v", result)
	}
	if cancels.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("cancels=%d gets=%d, want 1 and 1", cancels.Load(), gets.Load())
	}
}

func TestOKXCancel51400GetCanceledZeroFill(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t, okxCancel51400Code0, http.StatusOK,
		`{"code":"0","data":[{"ordId":"venue-1","state":"canceled","accFillSz":"0","avgPx":"0"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "canceled" || result.FilledQuantity != "0" {
		t.Fatalf("result=%+v", result)
	}
	if cancels.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
	}
}

func TestOKXCancel51400GetCanceledPartialFill(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK,
		`{"code":"0","data":[{"ordId":"venue-1","state":"canceled","accFillSz":"12.5","avgPx":"101"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "canceled" || result.FilledQuantity != "12.5" || result.AveragePrice != "101" {
		t.Fatalf("result=%+v", result)
	}
}

func TestOKXCancel51400GetNonTerminalIsAmbiguous(t *testing.T) {
	for _, test := range []struct {
		state, want string
	}{
		{state: "live", want: "open"},
		{state: "partially_filled", want: "partially_filled"},
		{state: "pending", want: "pending"},
		{state: "", want: "unknown"},
	} {
		t.Run(test.want, func(t *testing.T) {
			cancels, gets := newOKXCancel51400Counts()
			body := `{"code":"0","data":[{"ordId":"venue-1","state":"` + test.state + `","accFillSz":"3","avgPx":"100"}]}`
			server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK, body, 0, cancels, gets)
			result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
				context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
			)
			if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
				t.Fatalf("err=%v", err)
			}
			if result.Status != test.want {
				t.Fatalf("status=%q want %q result=%+v", result.Status, test.want, result)
			}
			if cancels.Load() != 1 || gets.Load() != 1 {
				t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
			}
		})
	}
}

func TestOKXCancel51400GetTimeoutOr5xxIsAmbiguous(t *testing.T) {
	t.Run("5xx", func(t *testing.T) {
		cancels, gets := newOKXCancel51400Counts()
		server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusInternalServerError,
			`{"code":"500","msg":"busy"}`, 0, cancels, gets)
		result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
			context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
		)
		if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
			t.Fatalf("err=%v", err)
		}
		if result.Status != "unknown" {
			t.Fatalf("result=%+v", result)
		}
		if cancels.Load() != 1 || gets.Load() != 1 {
			t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		cancels, gets := newOKXCancel51400Counts()
		server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK, `{}`, 400*time.Millisecond, cancels, gets)
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
			ctx, okxCancelTestCredentials(), okxCancelTestRequest(),
		)
		if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
			t.Fatalf("err=%v", err)
		}
		if result.Status != "unknown" {
			t.Fatalf("result=%+v", result)
		}
		if cancels.Load() != 1 || gets.Load() != 1 {
			t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
		}
	})
}

func TestOKXCancel51400Get51603IsAmbiguousNotCanceled(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK,
		`{"code":"51603","msg":"Order does not exist","data":[]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status == "canceled" || result.Status == "rejected" {
		t.Fatalf("result=%+v", result)
	}
	if result.Status != "unknown" {
		t.Fatalf("result=%+v", result)
	}
	if gets.Load() != 1 {
		t.Fatalf("gets=%d", gets.Load())
	}
}

func TestOKXCancel51400QueryRejectedIsAmbiguous(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK,
		`{"code":"0","data":[{"ordId":"venue-1","state":"rejected","accFillSz":"0"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "unknown" {
		t.Fatalf("result=%+v", result)
	}
}

func TestOKXCancelOtherRejectDoesNotGetOrder(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t,
		`{"code":"1","data":[{"sCode":"51401","sMsg":"Cancellation failed as the order does not exist.","ordId":"venue-1"}]}`,
		http.StatusOK, `{"code":"0","data":[{"ordId":"venue-1","state":"canceled"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "rejected" || result.ErrorCode != "51401" {
		t.Fatalf("result=%+v", result)
	}
	if cancels.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
	}
}

func TestOKXPlaceOrderErrorDoesNotGetOrder(t *testing.T) {
	var places, gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			gets.Add(1)
			_, _ = writer.Write([]byte(`{"code":"0","data":[]}`))
			return
		}
		if strings.Contains(request.URL.Path, "cancel-order") {
			http.Error(writer, "cancel must not run", http.StatusInternalServerError)
			return
		}
		places.Add(1)
		_, _ = writer.Write([]byte(`{"code":"1","data":[{"sCode":"51008","sMsg":"Order failed. Insufficient USDT margin in account"}]}`))
	}))
	t.Cleanup(server.Close)
	result, err := newOKX(server.Client(), server.URL).PlaceOrder(
		context.Background(), okxCancelTestCredentials(),
		OrderRequest{
			Instrument: Instrument{
				ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT",
			},
			ClientOrderID: "client-place", Side: "buy", OrderType: "limit",
			Quantity: "1", Price: "100",
		},
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "rejected" || result.ErrorCode != "51008" {
		t.Fatalf("result=%+v", result)
	}
	if places.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("places=%d gets=%d", places.Load(), gets.Load())
	}
}

func okxCancelTestCredentials() Credentials {
	return Credentials{APIKey: "key", APISecret: "secret", Passphrase: "passphrase"}
}

func okxCancelTestRequest() CancelRequest {
	return CancelRequest{
		Instrument: Instrument{
			ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		ClientOrderID: "client-1", VenueOrderID: "venue-1",
	}
}

func newOKXCancel51400Counts() (*atomic.Int32, *atomic.Int32) {
	return &atomic.Int32{}, &atomic.Int32{}
}

func newOKXCancel51400Server(
	t *testing.T,
	cancelBody string,
	getStatus int,
	getBody string,
	getDelay time.Duration,
	cancels, gets *atomic.Int32,
) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			if gets != nil {
				gets.Add(1)
			}
			if getDelay > 0 {
				select {
				case <-request.Context().Done():
					return
				case <-time.After(getDelay):
				}
			}
			if getStatus != 0 && getStatus != http.StatusOK {
				writer.WriteHeader(getStatus)
			}
			_, _ = writer.Write([]byte(getBody))
			return
		}
		if cancels != nil {
			cancels.Add(1)
		}
		_, _ = writer.Write([]byte(cancelBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestOKXCancelOrderCommandOnlyDoesNotGet(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t,
		`{"code":"0","data":[{"ordId":"venue-1","sCode":"0"}]}`,
		http.StatusOK,
		`{"code":"0","data":[{"ordId":"venue-1","state":"canceled","accFillSz":"0.4","avgPx":"100"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "pending" || !result.LocalCommandAck {
		t.Fatalf("result=%+v", result)
	}
	if cancels.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
	}
}

func TestOKXCancelOrder51400DoesNotGet(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t, okxCancel51400Code1, http.StatusOK,
		`{"code":"0","data":[{"ordId":"venue-1","state":"filled","accFillSz":"1","avgPx":"100"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "unknown" || result.ErrorCode != "51400" || result.LocalCommandAck {
		t.Fatalf("result=%+v", result)
	}
	if cancels.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
	}
}

func TestOKXCancelOrderOtherRejectIsUnknown(t *testing.T) {
	cancels, gets := newOKXCancel51400Counts()
	server := newOKXCancel51400Server(t,
		`{"code":"1","data":[{"sCode":"51401","sMsg":"Cancellation failed as the order does not exist.","ordId":"venue-1"}]}`,
		http.StatusOK, `{"code":"0","data":[{"ordId":"venue-1","state":"canceled"}]}`,
		0, cancels, gets)
	result, err := newOKX(server.Client(), server.URL).CancelOrder(
		context.Background(), okxCancelTestCredentials(), okxCancelTestRequest(),
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "unknown" || result.ErrorCode != "51401" {
		t.Fatalf("result=%+v", result)
	}
	if cancels.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("cancels=%d gets=%d", cancels.Load(), gets.Load())
	}
}
