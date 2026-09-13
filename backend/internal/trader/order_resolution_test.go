package trader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfquant/backend/internal/trader/exchange"
)

type resolutionTestAdapter struct {
	getResult    exchange.Result
	getErr       error
	resolution   exchange.OrderResolution
	resolveErr   error
	resolveCalls int
}

func (a *resolutionTestAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}

func (a *resolutionTestAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (a *resolutionTestAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return a.getResult, a.getErr
}

func (a *resolutionTestAdapter) CancelOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (a *resolutionTestAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

func (a *resolutionTestAdapter) ResolveOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.OrderResolution, error) {
	a.resolveCalls++
	return a.resolution, a.resolveErr
}

func TestResolveVenueOrderFallsBackOnlyForUnresolvedLookup(t *testing.T) {
	adapter := &resolutionTestAdapter{
		getResult: exchange.Result{Status: "unknown"},
		resolution: exchange.OrderResolution{
			Result: exchange.Result{
				VenueOrderID: "venue-1", Status: "canceled",
				FilledQuantity: "0",
			},
			Found: true,
		},
	}
	resolution, err := resolveVenueOrder(
		context.Background(), adapter, exchange.Credentials{},
		exchange.QueryRequest{},
	)
	if err != nil || !resolution.Found ||
		resolution.Result.Status != "canceled" ||
		adapter.resolveCalls != 1 {
		t.Fatalf(
			"resolution=%+v calls=%d err=%v",
			resolution, adapter.resolveCalls, err,
		)
	}

	adapter.getResult = exchange.Result{Status: "filled"}
	adapter.resolveCalls = 0
	resolution, err = resolveVenueOrder(
		context.Background(), adapter, exchange.Credentials{},
		exchange.QueryRequest{},
	)
	if err != nil || !resolution.Found || adapter.resolveCalls != 0 {
		t.Fatalf(
			"direct resolution=%+v calls=%d err=%v",
			resolution, adapter.resolveCalls, err,
		)
	}
}

func TestResolveVenueOrderResolverFailureIsNotConfirmedAbsent(t *testing.T) {
	adapter := &resolutionTestAdapter{
		getErr:     exchange.ErrOrderNotFound,
		resolveErr: errors.New("history unavailable"),
	}
	resolution, err := resolveVenueOrder(
		context.Background(), adapter, exchange.Credentials{},
		exchange.QueryRequest{},
	)
	if err == nil || resolution.ConfirmedAbsent {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestResolveVenueOrderRejectsContradictoryAbsenceEvidence(t *testing.T) {
	tests := []exchange.OrderResolution{
		{
			Result: exchange.Result{VenueOrderID: "venue-1", Status: "open"},
			Found:  true, Active: true, ConfirmedAbsent: true,
		},
		{
			Result:          exchange.Result{VenueOrderID: "venue-1", Status: "canceled"},
			ConfirmedAbsent: true,
		},
		{
			Active: true, ConfirmedAbsent: true,
		},
	}
	for _, invalid := range tests {
		adapter := &resolutionTestAdapter{
			getErr:     exchange.ErrOrderNotFound,
			resolution: invalid,
		}
		resolution, err := resolveVenueOrder(
			context.Background(), adapter, exchange.Credentials{},
			exchange.QueryRequest{},
		)
		if !errors.Is(err, exchange.ErrUncertain) {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	}
}

func TestConfirmedAbsentZeroFillReject(t *testing.T) {
	pending := Order{Status: "pending", FilledQuantity: "0"}
	if _, ok := confirmedAbsentZeroFillReject(pending); ok {
		t.Fatal("first ConfirmedAbsent must not reject")
	}
	pending.AbsenceConfirmations = confirmedAbsentRejectAfter - 1
	result, ok := confirmedAbsentZeroFillReject(pending)
	if !ok || result.Status != "rejected" || result.FilledQuantity != "0" ||
		result.ErrorCode != errorConfirmedAbsentAfterUncertainSubmit {
		t.Fatalf("reject=%+v ok=%v", result, ok)
	}

	filled := Order{
		Status: "filled", FilledQuantity: "0",
		AbsenceConfirmations: confirmedAbsentRejectAfter,
	}
	if _, ok := confirmedAbsentZeroFillReject(filled); ok {
		t.Fatal("terminal filled must not reject")
	}
	withVenue := pending
	withVenue.VenueOrderID = "oid-1"
	if _, ok := confirmedAbsentZeroFillReject(withVenue); ok {
		t.Fatal("venue order id must not auto-reject")
	}
	withFill := pending
	withFill.FilledQuantity = "0.01"
	if _, ok := confirmedAbsentZeroFillReject(withFill); ok {
		t.Fatal("positive fill must not auto-reject")
	}
	invalidFill := pending
	invalidFill.FilledQuantity = "abc"
	if _, ok := confirmedAbsentZeroFillReject(invalidFill); ok {
		t.Fatal("illegal filled quantity must not auto-reject")
	}
}

func TestConfirmedAbsentReliableZeroFill(t *testing.T) {
	valid := Order{
		Status: "rejected", FilledQuantity: "0",
		ErrorCode: errorConfirmedAbsentAfterUncertainSubmit,
	}
	if !confirmedAbsentReliableZeroFill(valid) {
		t.Fatal("expected reliable zero fill signature")
	}
	for _, invalid := range []Order{
		{Status: "rejected", FilledQuantity: "abc", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit},
		{Status: "rejected", FilledQuantity: "-1", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit},
		{Status: "filled", FilledQuantity: "0", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit},
		{Status: "rejected", FilledQuantity: "0", VenueOrderID: "oid-1", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit},
		{Status: "rejected", FilledQuantity: "0"},
		{Status: "canceled", FilledQuantity: "0", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit},
	} {
		if confirmedAbsentReliableZeroFill(invalid) {
			t.Fatalf("unexpected match: %+v", invalid)
		}
	}
}

func TestResolveVenueOrderBybitRealtimeThenExactHistory(t *testing.T) {
	var realtime, history int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		combined := request.URL.String() + " " + string(body)
		if strings.Contains(combined, "cursor") || strings.Contains(combined, "startTime") {
			t.Fatalf("unbounded history query: %s", combined)
		}
		switch request.URL.Path {
		case "/v5/order/realtime":
			realtime++
			if request.URL.Query().Get("orderId") != "venue-1" {
				t.Fatalf("realtime query=%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 0, "result": map[string]any{"list": []any{}},
			})
		case "/v5/order/history":
			history++
			if request.URL.Query().Get("orderId") != "venue-1" {
				t.Fatalf("history query=%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 0, "result": map[string]any{"list": []any{}},
			})
		default:
			t.Fatalf("path=%s", request.URL.Path)
		}
	}))
	defer server.Close()
	adapter, ok := exchange.NewRegistry(server.Client(), map[string]string{"bybit": server.URL}).Adapter("bybit")
	if !ok {
		t.Fatal("missing bybit adapter")
	}
	resolution, err := resolveVenueOrder(
		context.Background(),
		adapter,
		exchange.Credentials{APIKey: "k", APISecret: "s"},
		exchange.QueryRequest{
			Instrument: exchange.Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			},
			VenueOrderID: "venue-1",
		},
	)
	if err != nil || !resolution.ConfirmedAbsent || realtime != 1 || history != 1 {
		t.Fatalf("resolution=%+v realtime=%d history=%d err=%v", resolution, realtime, history, err)
	}
}
