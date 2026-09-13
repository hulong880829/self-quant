package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/stream"
	hltypes "github.com/Simon-Busch/hyperliquid-go/types"
)

func TestHyperliquidIOCNoMatchIsCanceledZeroFill(t *testing.T) {
	request := OrderRequest{TimeInForce: "IOC"}
	tests := []struct {
		name  string
		venue hltypes.Result
	}{
		{
			name: "documented error message",
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders.",
			},
		},
		{
			name: "production asset suffix",
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders. asset=231",
			},
		},
		{
			name: "production asset suffix and final period",
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders. asset=231.",
			},
		},
		{
			name:  "historical status",
			venue: hltypes.Result{Status: "iocCancelRejected", TotalSz: "0"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := hyperliquidPlacementOutcome(
				request, test.venue, hyperliquidCloid("client-1"), "client-1",
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "canceled" || result.FilledQuantity != "0" ||
				result.ErrorMessage != "" ||
				result.Reference.ReconcileStatus != "canceled" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestHyperliquidIOCNoMatchClassificationIsNarrow(t *testing.T) {
	tests := []struct {
		name    string
		request OrderRequest
		venue   hltypes.Result
	}{
		{
			name:    "same message for non IOC",
			request: OrderRequest{TimeInForce: "GTC"},
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders.",
			},
		},
		{
			name:    "partial fill is not zero fill cancellation",
			request: OrderRequest{TimeInForce: "IOC"},
			venue: hltypes.Result{
				TotalSz: "1",
				Error:   "Order could not immediately match against any resting orders. asset=231",
			},
		},
		{
			name:    "missing asset id",
			request: OrderRequest{TimeInForce: "IOC"},
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders. asset=",
			},
		},
		{
			name:    "non numeric asset id",
			request: OrderRequest{TimeInForce: "IOC"},
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders. asset=CASHCAT",
			},
		},
		{
			name:    "extra asset suffix text",
			request: OrderRequest{TimeInForce: "IOC"},
			venue: hltypes.Result{
				Error: "Order could not immediately match against any resting orders. asset=231 extra",
			},
		},
		{
			name:    "minimum notional",
			request: OrderRequest{TimeInForce: "IOC"},
			venue:   hltypes.Result{Error: "Order must have minimum value of $10. asset=231"},
		},
		{
			name:    "insufficient margin",
			request: OrderRequest{TimeInForce: "IOC"},
			venue:   hltypes.Result{Error: "Insufficient margin to place order."},
		},
		{
			name:    "tick precision",
			request: OrderRequest{TimeInForce: "IOC"},
			venue:   hltypes.Result{Error: "Price must be divisible by tick size."},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := hyperliquidPlacementOutcome(
				test.request, test.venue, hyperliquidCloid("client-1"), "client-1",
			)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("err=%v", err)
			}
			if result.Status != "rejected" ||
				result.Reference.ReconcileStatus != "rejected" ||
				result.ErrorMessage != test.venue.Error {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestHyperliquidIOCPositiveFillIsPreserved(t *testing.T) {
	venue := hltypes.Result{
		OID: 42, Status: "filled", TotalSz: "40", AvgPx: "0.25",
	}
	result, err := hyperliquidPlacementOutcome(
		OrderRequest{TimeInForce: "IOC"},
		venue,
		hyperliquidCloid("client-1"),
		"client-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "filled" || result.FilledQuantity != "40" ||
		result.AveragePrice != "0.25" || result.VenueOrderID != "42" {
		t.Fatalf("result=%+v", result)
	}
}

func TestQuotientDifferenceUsesDecimalSubtraction(t *testing.T) {
	t.Parallel()
	if got := quotientDifference("2.7", "2.13"); got != "0.57" {
		t.Fatalf("quotientDifference(2.7,2.13)=%q", got)
	}
}

func TestHyperliquidPlacementValidationFailureIsRejected(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "significant figures",
			err: &hltypes.ValidationError{
				Field: "Price", Code: "significant_figures", Got: 0.216187,
			},
		},
		{
			name: "other validation",
			err: &hltypes.ValidationError{
				Field: "Size", Code: "size_step_violation", Message: "size violates step",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := classifyHyperliquidError(test.err)
			result := hyperliquidPlacementFailureResult(
				hltypes.Result{}, hyperliquidCloid("client-1"), "client-1",
				test.err, classified,
			)
			if !errors.Is(classified, ErrRejected) {
				t.Fatalf("classified=%v", classified)
			}
			if result.Status != "rejected" || result.FilledQuantity != "0" ||
				result.ErrorMessage != test.err.Error() ||
				result.Reference.ReconcileStatus != "rejected" ||
				result.Reference.ClientOrderID != "client-1" ||
				result.Reference.Cloid != hyperliquidCloid("client-1") ||
				result.VenueOrderID != "" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestHyperliquidPlacementTransportFailureRemainsUncertain(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "canceled", err: context.Canceled},
		{name: "eof", err: io.EOF},
		{name: "connection", err: errors.New("connection reset by peer")},
		{name: "timeout", err: errors.New("request timeout")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := classifyHyperliquidError(test.err)
			result := hyperliquidPlacementFailureResult(
				hltypes.Result{}, hyperliquidCloid("client-1"), "client-1",
				test.err, classified,
			)
			if !errors.Is(classified, ErrUncertain) ||
				errors.Is(classified, ErrRejected) {
				t.Fatalf("classified=%v", classified)
			}
			if result.Status != "unknown" || result.FilledQuantity != "" ||
				result.ErrorMessage != "" ||
				result.Reference.ReconcileStatus != "unknown" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestHyperliquidAmbiguousCancelClassification(t *testing.T) {
	t.Parallel()
	err := hyperliquidCancelError("Order was never placed, already canceled, or filled.")
	if !errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	classified := classifyHyperliquidError(errors.New(
		"Order was never placed, already canceled, or filled. asset=1",
	))
	if !errors.Is(classified, ErrAmbiguousCancel) || errors.Is(classified, ErrRejected) {
		t.Fatalf("classified=%v", classified)
	}
}

func TestHyperliquidRateLimitClassificationIsUnchanged(t *testing.T) {
	err := errors.New("HTTP 429 rate limit")
	classified := classifyHyperliquidError(err)
	result := hyperliquidPlacementFailureResult(
		hltypes.Result{}, hyperliquidCloid("client-1"), "client-1",
		err, classified,
	)
	if !errors.Is(classified, ErrRateLimited) ||
		errors.Is(classified, ErrRejected) ||
		result.Status != "unknown" {
		t.Fatalf("result=%+v classified=%v", result, classified)
	}
}

func TestHyperliquidClientReusesMetadataAndIsolatesCredentials(t *testing.T) {
	var metaCalls, spotMetaCalls, outcomeMetaCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(writer, "invalid payload", http.StatusBadRequest)
			return
		}
		switch payload.Type {
		case "meta":
			metaCalls.Add(1)
			_, _ = writer.Write([]byte(
				`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`,
			))
		case "spotMeta":
			spotMetaCalls.Add(1)
			_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
		case "outcomeMeta":
			outcomeMetaCalls.Add(1)
			_, _ = writer.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected info type %q", payload.Type)
			http.Error(writer, "unexpected info type", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	adapter := newHyperliquid(server.Client(), server.URL).(*hyperliquidAdapter)
	base := Credentials{
		APIKey:         "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		APISecret:      dexRecoveryTestKey,
		SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	}
	acquire := func(credentials Credentials) {
		t.Helper()
		_, release, err := adapter.client(credentials, "BTC")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}

	first, release, err := adapter.client(base, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	second, release, err := adapter.client(base, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first != second {
		t.Fatal("same Hyperliquid credentials did not reuse SDK client")
	}
	if metaCalls.Load() != 1 || spotMetaCalls.Load() != 1 || outcomeMetaCalls.Load() != 1 {
		t.Fatalf(
			"same credentials metadata calls meta=%d spot=%d outcome=%d",
			metaCalls.Load(), spotMetaCalls.Load(), outcomeMetaCalls.Load(),
		)
	}

	differentSigner := base
	differentSigner.APISecret =
		"0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff81"
	differentOwner := base
	differentOwner.APIKey = "0x0000000000000000000000000000000000000001"
	differentVault := base
	differentVault.VaultAddress = "0x0000000000000000000000000000000000000002"
	for _, credentials := range []Credentials{differentSigner, differentOwner, differentVault} {
		acquire(credentials)
	}
	if metaCalls.Load() != 4 || spotMetaCalls.Load() != 4 || outcomeMetaCalls.Load() != 4 {
		t.Fatalf(
			"isolated credentials metadata calls meta=%d spot=%d outcome=%d",
			metaCalls.Load(), spotMetaCalls.Load(), outcomeMetaCalls.Load(),
		)
	}
}

func TestHyperliquidClientRefreshesCachedMetadataForNewSymbol(t *testing.T) {
	var metaCalls atomic.Int32
	var includeETH atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(writer, "invalid payload", http.StatusBadRequest)
			return
		}
		switch payload.Type {
		case "meta":
			metaCalls.Add(1)
			universe := `{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`
			if includeETH.Load() {
				universe = `{"universe":[` +
					`{"name":"BTC","szDecimals":5,"maxLeverage":50},` +
					`{"name":"ETH","szDecimals":4,"maxLeverage":50}]}`
			}
			_, _ = writer.Write([]byte(universe))
		case "spotMeta":
			_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
		case "outcomeMeta":
			_, _ = writer.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected info type %q", payload.Type)
			http.Error(writer, "unexpected info type", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	adapter := newHyperliquid(server.Client(), server.URL).(*hyperliquidAdapter)
	credentials := Credentials{
		APIKey:         "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		APISecret:      dexRecoveryTestKey,
		SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	}
	first, release, err := adapter.client(credentials, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	includeETH.Store(true)
	second, release, err := adapter.client(credentials, "ETH")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first == second {
		t.Fatal("metadata miss did not refresh cached SDK client")
	}
	if !hyperliquidClientSupportsSymbol(second, "ETH") || metaCalls.Load() != 2 {
		t.Fatalf("refreshed client missing ETH or meta calls=%d", metaCalls.Load())
	}
}

func TestHyperliquidClientConcurrentInitializationAndUseAreSerialized(t *testing.T) {
	var metaCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(writer, "invalid payload", http.StatusBadRequest)
			return
		}
		switch payload.Type {
		case "meta":
			metaCalls.Add(1)
			_, _ = writer.Write([]byte(
				`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`,
			))
		case "spotMeta":
			_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
		case "outcomeMeta":
			_, _ = writer.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected info type %q", payload.Type)
			http.Error(writer, "unexpected info type", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	adapter := newHyperliquid(server.Client(), server.URL).(*hyperliquidAdapter)
	credentials := Credentials{
		APIKey:         "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		APISecret:      dexRecoveryTestKey,
		SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	}
	const workers = 8
	clients := make(chan any, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			client, release, err := adapter.client(credentials, "BTC")
			if err != nil {
				errs <- err
				return
			}
			clients <- client
			release()
		}()
	}
	group.Wait()
	close(clients)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first any
	for client := range clients {
		if first == nil {
			first = client
			continue
		}
		if first != client {
			t.Fatal("concurrent initialization returned different SDK clients")
		}
	}
	if metaCalls.Load() != 1 {
		t.Fatalf("concurrent initialization fetched meta %d times", metaCalls.Load())
	}

	_, release, err := adapter.client(credentials, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	go func() {
		_, releaseSecond, acquireErr := adapter.client(credentials, "BTC")
		if acquireErr == nil {
			close(acquired)
			releaseSecond()
		}
	}()
	select {
	case <-acquired:
		t.Fatal("same-client signed operation lock was not serialized")
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("same-client signed operation lock was not released")
	}
}

func TestHyperliquidUnknownSymbolFailsBeforeExchangeRequest(t *testing.T) {
	var exchangeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/exchange":
			exchangeCalls.Add(1)
			http.Error(writer, "unexpected exchange request", http.StatusInternalServerError)
		case "/info":
			var payload struct {
				Type string `json:"type"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
				http.Error(writer, "invalid payload", http.StatusBadRequest)
				return
			}
			switch payload.Type {
			case "orderStatus":
				_, _ = writer.Write([]byte(`{"status":"unknown"}`))
			case "meta":
				_, _ = writer.Write([]byte(
					`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`,
				))
			case "spotMeta":
				_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
			case "outcomeMeta":
				_, _ = writer.Write([]byte(`{}`))
			default:
				t.Errorf("unexpected info type %q", payload.Type)
				http.Error(writer, "unexpected info type", http.StatusBadRequest)
			}
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			http.Error(writer, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	_, err := newHyperliquid(server.Client(), server.URL).PlaceOrder(
		context.Background(),
		Credentials{
			APIKey:         "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
			APISecret:      dexRecoveryTestKey,
			SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		},
		OrderRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "UNKNOWN",
			},
			ClientOrderID: "unknown-symbol", Side: "buy",
			OrderType: "limit", TimeInForce: "IOC", Quantity: "1", Price: "1",
		},
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown symbol err=%v", err)
	}
	if exchangeCalls.Load() != 0 {
		t.Fatalf("unknown symbol sent %d exchange requests", exchangeCalls.Load())
	}
}

func TestHyperliquidDexFromSymbol(t *testing.T) {
	if got := hyperliquidDexFromSymbol("xyz:ZHIPU"); got != "xyz" {
		t.Fatalf("dex=%q", got)
	}
	if got := hyperliquidDexFromSymbol("BTC"); got != "" {
		t.Fatalf("default dex=%q", got)
	}
	if got := hyperliquidDexFromSymbol(":ZHIPU"); got != "" {
		t.Fatalf("leading colon dex=%q", got)
	}
}

func TestHyperliquidClientIsolatesDefaultAndHIP3Dex(t *testing.T) {
	var defaultMeta, xyzMeta, perpDexs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !writeHyperliquidSDKInfo(t, writer, request, &defaultMeta, &xyzMeta, &perpDexs) {
			t.Errorf("unexpected info request")
			http.Error(writer, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	adapter := newHyperliquid(server.Client(), server.URL).(*hyperliquidAdapter)
	credentials := hyperliquidTestCredentials()
	defaultClient, release, err := adapter.client(credentials, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	hip3Client, release, err := adapter.client(credentials, "xyz:ZHIPU")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if defaultClient == hip3Client {
		t.Fatal("default and HIP-3 clients shared the same SDK instance")
	}
	if !hyperliquidClientSupportsSymbol(defaultClient, "BTC") ||
		hyperliquidClientSupportsSymbol(defaultClient, "xyz:ZHIPU") {
		t.Fatal("default client metadata should only include BTC")
	}
	if !hyperliquidClientSupportsSymbol(hip3Client, "xyz:ZHIPU") {
		t.Fatal("HIP-3 client missing xyz:ZHIPU")
	}
	if !hyperliquidClientSupportsSymbol(hip3Client, "ZHIPU") {
		t.Fatal("HIP-3 client missing bare ZHIPU alias")
	}
	reused, release, err := adapter.client(credentials, "xyz:ZHIPU")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if reused != hip3Client {
		t.Fatal("HIP-3 client was not reused")
	}
	if defaultMeta.Load() != 1 || xyzMeta.Load() != 1 || perpDexs.Load() != 1 {
		t.Fatalf(
			"metadata calls default=%d xyz=%d perpDexs=%d",
			defaultMeta.Load(), xyzMeta.Load(), perpDexs.Load(),
		)
	}
	if hip3Client.Info.CoinToAssetMap()["xyz:ZHIPU"] != 110000 {
		t.Fatalf("HIP-3 asset id=%v", hip3Client.Info.CoinToAssetMap())
	}
}

func TestHyperliquidCancelHIP3UsesBuilderDexClient(t *testing.T) {
	var exchangeCalls, xyzMeta, defaultMeta atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/exchange":
			exchangeCalls.Add(1)
			_, _ = writer.Write([]byte(
				`{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`,
			))
		case "/info":
			var payload struct {
				Type string `json:"type"`
			}
			body, _ := io.ReadAll(request.Body)
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Error(err)
				http.Error(writer, "invalid payload", http.StatusBadRequest)
				return
			}
			request.Body = io.NopCloser(strings.NewReader(string(body)))
			if writeHyperliquidSDKInfo(t, writer, request, &defaultMeta, &xyzMeta, nil) {
				return
			}
			if payload.Type == "orderStatus" {
				_, _ = writer.Write([]byte(`{
					"status":"order",
					"order":{
						"order":{"coin":"xyz:ZHIPU","oid":99,"cloid":"` + hyperliquidCloid("hip3-1") + `","sz":"0","origSz":"1"},
						"status":"canceled",
						"statusTimestamp":1700000000000
					}
				}`))
				return
			}
			t.Errorf("unexpected info type %q", payload.Type)
			http.Error(writer, "unexpected", http.StatusBadRequest)
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			http.Error(writer, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := newHyperliquid(server.Client(), server.URL).CancelOrder(
		context.Background(),
		hyperliquidTestCredentials(),
		CancelRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "xyz:ZHIPU",
			},
			ClientOrderID: "hip3-1", VenueOrderID: "99",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "pending" || result.VenueOrderID != "99" ||
		!result.LocalCommandAck || !result.Reference.EventAt.IsZero() {
		t.Fatalf("result=%+v", result)
	}
	if exchangeCalls.Load() != 1 || xyzMeta.Load() != 1 || defaultMeta.Load() != 0 {
		t.Fatalf(
			"exchange=%d xyzMeta=%d defaultMeta=%d",
			exchangeCalls.Load(), xyzMeta.Load(), defaultMeta.Load(),
		)
	}
}

func TestHyperliquidListPositionsMergesDefaultAndHIP3(t *testing.T) {
	var defaultCalls, xyzCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
			Dex  string `json:"dex"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(writer, "invalid payload", http.StatusBadRequest)
			return
		}
		if payload.Type != "clearinghouseState" {
			t.Errorf("unexpected info type %q", payload.Type)
			http.Error(writer, "unexpected", http.StatusBadRequest)
			return
		}
		if payload.Dex == "xyz" {
			xyzCalls.Add(1)
			_, _ = writer.Write([]byte(`{
				"marginSummary":{"accountValue":"40"},
				"withdrawable":"10",
				"assetPositions":[{"position":{"coin":"xyz:ZHIPU","szi":"2","entryPx":"5"}}]
			}`))
			return
		}
		defaultCalls.Add(1)
		_, _ = writer.Write([]byte(`{
			"marginSummary":{"accountValue":"100"},
			"withdrawable":"80",
			"assetPositions":[{"position":{"coin":"BTC","szi":"0.5","entryPx":"60000"}}]
		}`))
	}))
	defer server.Close()

	adapter := newHyperliquid(server.Client(), server.URL)
	positions, err := adapter.(PositionReader).ListPositions(context.Background(), hyperliquidTestCredentials())
	if err != nil {
		t.Fatal(err)
	}
	if defaultCalls.Load() != 1 || xyzCalls.Load() != 1 {
		t.Fatalf("clearinghouse default=%d xyz=%d", defaultCalls.Load(), xyzCalls.Load())
	}
	if len(positions) != 2 {
		t.Fatalf("positions=%+v", positions)
	}
	byInstrument := map[string]Position{}
	for _, position := range positions {
		byInstrument[position.Instrument] = position
	}
	if byInstrument["BTC"].Quantity != "0.5" || byInstrument["xyz:ZHIPU"].Quantity != "2" {
		t.Fatalf("positions=%+v", positions)
	}
	balances, err := adapter.(BalanceReader).ListBalances(context.Background(), hyperliquidTestCredentials())
	if err != nil {
		t.Fatal(err)
	}
	if len(balances) != 1 || balances[0].Total != "140" || balances[0].Available != "90" {
		t.Fatalf("balances=%+v", balances)
	}
}

func writeHyperliquidSDKInfo(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	defaultMeta, xyzMeta, perpDexs *atomic.Int32,
) bool {
	t.Helper()
	var payload struct {
		Type string `json:"type"`
		Dex  string `json:"dex"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Error(err)
		http.Error(writer, "invalid payload", http.StatusBadRequest)
		return true
	}
	switch payload.Type {
	case "meta":
		if payload.Dex == hyperliquidHIP3Dex {
			if xyzMeta != nil {
				xyzMeta.Add(1)
			}
			_, _ = writer.Write([]byte(
				`{"universe":[{"name":"xyz:ZHIPU","szDecimals":2,"maxLeverage":20}]}`,
			))
			return true
		}
		if defaultMeta != nil {
			defaultMeta.Add(1)
		}
		_, _ = writer.Write([]byte(
			`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`,
		))
		return true
	case "spotMeta":
		_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
		return true
	case "outcomeMeta":
		_, _ = writer.Write([]byte(`{}`))
		return true
	case "perpDexs":
		if perpDexs != nil {
			perpDexs.Add(1)
		}
		_, _ = writer.Write([]byte(`[null,{"name":"xyz"}]`))
		return true
	default:
		return false
	}
}

func hyperliquidTestCredentials() Credentials {
	return Credentials{
		APIKey:         "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		APISecret:      dexRecoveryTestKey,
		SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	}
}

func TestClassifyHyperliquidStreamSentinelsAreUncertain(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		stream.ErrNotConnected,
		stream.ErrConnectionLost,
		stream.ErrRequestTimeout,
		stream.ErrRequestCanceled,
		stream.ErrBadResponse,
		fmt.Errorf("%w: wrapped", stream.ErrNotConnected),
	}
	for _, err := range sentinels {
		classified := classifyHyperliquidError(err)
		if !errors.Is(classified, ErrUncertain) || errors.Is(classified, ErrRejected) {
			t.Fatalf("err=%v classified=%v", err, classified)
		}
	}
}

func TestClassifyHyperliquidNotConnectedStringIsUncertain(t *testing.T) {
	t.Parallel()
	classified := classifyHyperliquidError(errors.New("not connected"))
	if !errors.Is(classified, ErrUncertain) || errors.Is(classified, ErrRejected) {
		t.Fatalf("classified=%v, want ErrUncertain not ErrRejected", classified)
	}
}
