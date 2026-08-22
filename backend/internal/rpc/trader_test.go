package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/trader"
)

type fakeTrader struct {
	order       trader.Order
	combination trader.ArbitrageCombination
	err         error
}

func (f fakeTrader) ListInstruments(context.Context, string, int64, string) ([]trader.Instrument, error) {
	return nil, f.err
}

func (f fakeTrader) PlaceOrder(context.Context, trader.PlaceOrderInput) (trader.Order, error) {
	return f.order, f.err
}

func (f fakeTrader) GetOrder(context.Context, string, string) (trader.Order, error) {
	return f.order, f.err
}

func (f fakeTrader) ListOrders(context.Context, string, int64, string, int, string) ([]trader.Order, string, error) {
	return nil, "", f.err
}

func (f fakeTrader) CancelOrder(context.Context, string, string) (trader.Order, error) {
	return f.order, f.err
}

func (f fakeTrader) CreateTwap(context.Context, trader.CreateTwapInput) (trader.TwapJob, error) {
	return trader.TwapJob{}, f.err
}

func (f fakeTrader) GetTwap(context.Context, string, string) (trader.TwapJob, error) {
	return trader.TwapJob{}, f.err
}

func (f fakeTrader) ListTwaps(context.Context, string, int64, string, string, int, string) ([]trader.TwapJob, string, error) {
	return nil, "", f.err
}

func (f fakeTrader) ListTwapOrders(context.Context, string, string) ([]trader.Order, error) {
	return nil, f.err
}

func (f fakeTrader) CancelTwap(context.Context, string, string) (trader.TwapJob, error) {
	return trader.TwapJob{}, f.err
}

func (f fakeTrader) CreateArbitrageCombination(
	context.Context, trader.CreateArbitrageInput,
) (trader.ArbitrageCombination, error) {
	return f.combination, f.err
}

func (f fakeTrader) GetArbitrageCombination(
	context.Context, string, string,
) (trader.ArbitrageCombination, []trader.Order, []trader.ArbitrageExecution, []trader.ArbitrageEvent, error) {
	return f.combination, nil, nil, nil, f.err
}

func (f fakeTrader) ListArbitrageCombinations(
	context.Context, string, string, int, string,
) ([]trader.ArbitrageCombination, string, int64, error) {
	return []trader.ArbitrageCombination{f.combination}, "", 1, f.err
}

func (f fakeTrader) CloseArbitrageCombination(
	context.Context, string, string,
) (trader.ArbitrageCombination, error) {
	return f.combination, f.err
}

func TestArbitrageCombinationMapping(t *testing.T) {
	service := fakeTrader{combination: trader.ArbitrageCombination{
		ID: "combo-1", AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000", OrderNotional: "500", MaxDeltaNotional: "10",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a", Status: "running",
		LegA: trader.ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 11, Exchange: "binance",
			ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: trader.ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 22, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
	}}
	server := NewTraderServer(service)
	response, err := server.CreateArbitrageCombination(
		context.Background(), &traderv1.CreateArbitrageCombinationRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	item := response.GetCombination()
	if item.GetId() != "combo-1" || item.GetAskThresholdBps() != "12" ||
		item.GetBidThresholdBps() != "-8" ||
		item.GetLegA().GetExchange() != "binance" ||
		item.GetLegB().GetExchange() != "okx" {
		t.Fatalf("combination=%v", item)
	}
}

func TestMapTraderErrors(t *testing.T) {
	server := NewTraderServer(fakeTrader{err: trader.ErrIdempotencyConflict})
	_, err := server.PlaceOrder(context.Background(), &traderv1.PlaceOrderRequest{})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%v", status.Code(err))
	}
	server = NewTraderServer(fakeTrader{err: trader.ErrInvalidArgument})
	_, err = server.GetOrder(context.Background(), &traderv1.GetOrderRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code=%v", status.Code(err))
	}
	if !errors.Is(trader.ErrNotFound, trader.ErrNotFound) {
		t.Fatal("sentinel")
	}
	server = NewTraderServer(fakeTrader{err: trader.ErrNotFound})
	_, err = server.GetOrder(context.Background(), &traderv1.GetOrderRequest{Token: "tok", OrderId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code=%v", status.Code(err))
	}
	mapped := mapTraderError(errors.New("upstream status 401: apikey=secret"))
	if status.Code(mapped) != codes.Internal || strings.Contains(status.Convert(mapped).Message(), "apikey") {
		t.Fatalf("mapped=%v", mapped)
	}
	mapped = mapTraderError(fmt.Errorf("%w: postgres detail", trader.ErrPersistence))
	if status.Code(mapped) != codes.Internal ||
		status.Convert(mapped).Message() != "order persistence failed" {
		t.Fatalf("persistence mapped=%v", mapped)
	}
	mapped = mapTraderError(fmt.Errorf("%w: retry later", trader.ErrVenueRateLimited))
	if status.Code(mapped) != codes.ResourceExhausted {
		t.Fatalf("rate limit mapped=%v", mapped)
	}
	mapped = mapTraderError(fmt.Errorf("%w: upstream down", trader.ErrVenueUnavailable))
	if status.Code(mapped) != codes.Unavailable ||
		status.Convert(mapped).Message() != "venue unavailable" {
		t.Fatalf("unavailable mapped=%v", mapped)
	}
}

func TestPlaceOrderKeepsPersistedVenueRejectionInResponse(t *testing.T) {
	server := NewTraderServer(fakeTrader{
		order: trader.Order{ID: "order-1", Status: "rejected", ErrorCode: "venue_rejected"},
		err:   fmt.Errorf("%w: rejected", trader.ErrVenueRejected),
	})
	response, err := server.PlaceOrder(context.Background(), &traderv1.PlaceOrderRequest{})
	if err != nil || response.GetOrder().GetStatus() != "rejected" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestPlaceOrderReturnsMappedPersistedUpstreamFailure(t *testing.T) {
	server := NewTraderServer(fakeTrader{
		order: trader.Order{ID: "order-1", Status: "rejected"},
		err:   fmt.Errorf("%w: retry later", trader.ErrVenueRateLimited),
	})
	response, err := server.PlaceOrder(context.Background(), &traderv1.PlaceOrderRequest{})
	if response != nil || status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("response=%+v err=%v", response, err)
	}

	server = NewTraderServer(fakeTrader{
		order: trader.Order{ID: "order-2", Status: "unknown"},
		err:   fmt.Errorf("%w: timed out", trader.ErrVenueUncertain),
	})
	response, err = server.PlaceOrder(context.Background(), &traderv1.PlaceOrderRequest{})
	if response != nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
