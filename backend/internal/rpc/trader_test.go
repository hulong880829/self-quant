package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/trader"
)

type fakeTrader struct {
	order       trader.Order
	combination trader.ArbitrageCombination
	profile     trader.AccountProfileResult
	err         error
}

func (f fakeTrader) ListInstruments(context.Context, string, int64, string) ([]trader.Instrument, error) {
	return nil, f.err
}

func (f fakeTrader) ApplyAccountProfile(
	context.Context, string, int64,
) (trader.AccountProfileResult, error) {
	return f.profile, f.err
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

func (f fakeTrader) UpdateArbitrageCombination(
	context.Context, trader.UpdateArbitrageInput,
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
		TargetNotional: "10000", OrderNotional: "500",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a", Status: "running",
		RuntimeState: "hedging", LegABasePosition: "1.2",
		LegBBasePosition: "-1.1", CarryBaseQuantity: "0.1",
		GrossTurnoverNotional: "2500",
		LegAAverageEntryPrice: "100", LegBAverageEntryPrice: "101",
		AverageEntrySpreadBps: "100",
		LegAUnrealizedPnl:     "2.5", LegBUnrealizedPnl: "-1.5",
		CombinedPositionAnnualized: "0.1842", FundingHistoryComplete: true,
		LegAVenueBaselineBasePosition: "10",
		LegBVenueBaselineBasePosition: "-10",
		VenueBaselineCapturedAt:       time.Now().UTC(),
		LegAVenueNotional:             "2500",
		LegBVenueNotional:             "-2525",
		LegAVenueValuationPrice:       "100",
		LegBVenueValuationPrice:       "101",
		LegAVenueValuationAt:          time.Now().UTC(),
		LegBVenueValuationAt:          time.Now().UTC(),
		LegA: trader.ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 11, Exchange: "binance",
			ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: trader.ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 22, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		EarlyExitFunding8hAnnualizedFloor: "0.05",
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
		item.GetLegB().GetExchange() != "okx" ||
		item.GetRuntimeState() != "hedging" ||
		item.GetCarryBaseQuantity() != "0.1" ||
		item.GetGrossTurnoverNotional() != "2500" ||
		item.GetAverageEntrySpreadBps() != "100" ||
		item.GetCombinedPositionAnnualized() != "0.1842" ||
		item.GetLegAVenueBaselineBasePosition() != "10" ||
		item.GetLegBVenueBaselineBasePosition() != "-10" ||
		item.GetLegAExpectedBasePosition() != "11.2" ||
		item.GetLegBExpectedBasePosition() != "-11.1" ||
		item.GetLegAVenueNotional() != "2500" ||
		item.GetLegBVenueNotional() != "-2525" ||
		item.GetLegAVenueValuationPrice() != "100" ||
		item.GetLegBVenueValuationPrice() != "101" ||
		item.GetLegAVenueValuationAt() == nil ||
		item.GetLegBVenueValuationAt() == nil ||
		item.GetVenueBaselineCapturedAt() == nil ||
		!item.GetFundingHistoryComplete() ||
		item.GetEarlyExitFunding_8HAnnualizedFloor() != "0.05" {
		t.Fatalf("combination=%v", item)
	}
	ask := "-7"
	updated, err := server.UpdateArbitrageCombination(
		context.Background(),
		&traderv1.UpdateArbitrageCombinationRequest{
			CombinationId: "combo-1", AskThresholdBps: &ask,
		},
	)
	if err != nil || updated.GetCombination().GetId() != "combo-1" {
		t.Fatalf("updated=%v err=%v", updated, err)
	}
}

type capturingTrader struct {
	fakeTrader
	got trader.CreateArbitrageInput
}

func (c *capturingTrader) CreateArbitrageCombination(
	_ context.Context, in trader.CreateArbitrageInput,
) (trader.ArbitrageCombination, error) {
	c.got = in
	c.combination.EarlyExitFunding8hAnnualizedFloor = in.EarlyExitFunding8hAnnualizedFloor
	return c.combination, c.err
}

func TestCreateArbitrageCombinationMapsFunding8hFloor(t *testing.T) {
	for _, floor := range []string{"", "0.05", "-0.10"} {
		capture := &capturingTrader{}
		server := NewTraderServer(capture)
		response, err := server.CreateArbitrageCombination(
			context.Background(),
			&traderv1.CreateArbitrageCombinationRequest{
				EarlyExitFunding_8HAnnualizedFloor: floor,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if capture.got.EarlyExitFunding8hAnnualizedFloor != floor {
			t.Fatalf("input floor=%q got=%q", floor, capture.got.EarlyExitFunding8hAnnualizedFloor)
		}
		if response.GetCombination().GetEarlyExitFunding_8HAnnualizedFloor() != floor {
			t.Fatalf("proto floor=%q", response.GetCombination().GetEarlyExitFunding_8HAnnualizedFloor())
		}
	}
}

func TestAccountProfileMapping(t *testing.T) {
	server := NewTraderServer(fakeTrader{profile: trader.AccountProfileResult{
		TradingAccountID: 7,
		ProductName:      "Funding Arb",
		AccountName:      "main",
		Exchange:         "binance",
		OverallStatus:    "manual_required",
		Steps: []trader.AccountProfileStepResult{{
			Step: "one_way_position", Status: "manual_required",
			Code: "OPEN_ORDERS", Message: "cancel orders manually",
		}},
	}})
	response, err := server.ApplyAccountProfile(
		context.Background(),
		&traderv1.ApplyAccountProfileRequest{Token: "token", TradingAccountId: 7},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := response.GetResult()
	if result.GetTradingAccountId() != 7 ||
		result.GetOverallStatus() != "manual_required" ||
		len(result.GetSteps()) != 1 ||
		result.GetSteps()[0].GetCode() != "OPEN_ORDERS" {
		t.Fatalf("result=%+v", result)
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
	mapped = mapTraderError(fmt.Errorf(
		"%w: 账户 main 的 okx BTC-USDT-SWAP 已被运行中套利组合 combo-1 占用",
		trader.ErrActiveArbitrageInstrumentConflict,
	))
	if status.Code(mapped) != codes.AlreadyExists ||
		!strings.HasPrefix(
			status.Convert(mapped).Message(),
			"active_arbitrage_instrument_conflict: 账户 main",
		) {
		t.Fatalf("active conflict mapped=%v", mapped)
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
