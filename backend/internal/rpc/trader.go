package rpc

import (
	"context"
	"errors"
	"strings"
	"time"

	shopspringdecimal "github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/trader"
)

type traderService interface {
	ListInstruments(context.Context, string, int64, string) ([]trader.Instrument, error)
	PlaceOrder(context.Context, trader.PlaceOrderInput) (trader.Order, error)
	GetOrder(context.Context, string, string) (trader.Order, error)
	ListOrders(context.Context, string, int64, string, int, string) ([]trader.Order, string, error)
	CancelOrder(context.Context, string, string) (trader.Order, error)
	CreateTwap(context.Context, trader.CreateTwapInput) (trader.TwapJob, error)
	GetTwap(context.Context, string, string) (trader.TwapJob, error)
	ListTwaps(context.Context, string, int64, string, string, int, string) ([]trader.TwapJob, string, error)
	ListTwapOrders(context.Context, string, string) ([]trader.Order, error)
	CancelTwap(context.Context, string, string) (trader.TwapJob, error)
	CreateArbitrageCombination(context.Context, trader.CreateArbitrageInput) (trader.ArbitrageCombination, error)
	GetArbitrageCombination(context.Context, string, string) (
		trader.ArbitrageCombination, []trader.Order,
		[]trader.ArbitrageExecution, []trader.ArbitrageEvent, error,
	)
	ListArbitrageCombinations(context.Context, string, string, int, string) ([]trader.ArbitrageCombination, string, int64, error)
	CloseArbitrageCombination(context.Context, string, string) (trader.ArbitrageCombination, error)
}

type TraderServer struct {
	traderv1.UnimplementedTraderServiceServer
	service traderService
}

func NewTraderServer(service traderService) *TraderServer {
	return &TraderServer{service: service}
}

func (s *TraderServer) ListInstruments(
	ctx context.Context,
	request *traderv1.ListInstrumentsRequest,
) (*traderv1.ListInstrumentsResponse, error) {
	items, err := s.service.ListInstruments(
		ctx, request.GetToken(), request.GetTradingAccountId(), request.GetContractType(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListInstrumentsResponse{
		Items: make([]*traderv1.Instrument, 0, len(items)), ServerTime: timestamppb.Now(),
	}
	for _, item := range items {
		response.Items = append(response.Items, instrumentToProto(item))
	}
	return response, nil
}

func (s *TraderServer) PlaceOrder(
	ctx context.Context,
	request *traderv1.PlaceOrderRequest,
) (*traderv1.PlaceOrderResponse, error) {
	order, err := s.service.PlaceOrder(ctx, trader.PlaceOrderInput{
		Token: request.GetToken(), TradingAccountID: request.GetTradingAccountId(),
		InstrumentID: request.GetInstrumentId(), Side: request.GetSide(),
		OrderType: request.GetOrderType(), Quantity: request.GetQuantity(),
		Price: request.GetPrice(), IdempotencyKey: request.GetIdempotencyKey(),
	})
	if err != nil {
		if order.ID != "" && errors.Is(err, trader.ErrVenueRejected) {
			return &traderv1.PlaceOrderResponse{Order: traderOrderToProto(order)}, nil
		}
		return nil, mapTraderError(err)
	}
	return &traderv1.PlaceOrderResponse{Order: traderOrderToProto(order)}, nil
}

func (s *TraderServer) GetOrder(
	ctx context.Context,
	request *traderv1.GetOrderRequest,
) (*traderv1.GetOrderResponse, error) {
	order, err := s.service.GetOrder(ctx, request.GetToken(), request.GetOrderId())
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.GetOrderResponse{Order: traderOrderToProto(order)}, nil
}

func (s *TraderServer) ListOrders(
	ctx context.Context,
	request *traderv1.ListOrdersRequest,
) (*traderv1.ListOrdersResponse, error) {
	items, nextCursor, err := s.service.ListOrders(
		ctx, request.GetToken(), request.GetTradingAccountId(), request.GetView(),
		int(request.GetLimit()), request.GetCursor(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListOrdersResponse{
		Items: make([]*traderv1.Order, 0, len(items)), ServerTime: timestamppb.Now(),
		NextCursor: nextCursor,
	}
	for _, item := range items {
		response.Items = append(response.Items, traderOrderToProto(item))
	}
	return response, nil
}

func (s *TraderServer) CancelOrder(
	ctx context.Context,
	request *traderv1.CancelOrderRequest,
) (*traderv1.CancelOrderResponse, error) {
	order, err := s.service.CancelOrder(ctx, request.GetToken(), request.GetOrderId())
	if err != nil && order.ID == "" {
		return nil, mapTraderError(err)
	}
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.CancelOrderResponse{Order: traderOrderToProto(order)}, nil
}

func (s *TraderServer) CreateTwap(
	ctx context.Context,
	request *traderv1.CreateTwapRequest,
) (*traderv1.CreateTwapResponse, error) {
	job, err := s.service.CreateTwap(ctx, trader.CreateTwapInput{
		Token: request.GetToken(), TradingAccountID: request.GetTradingAccountId(),
		InstrumentID: request.GetInstrumentId(), Side: request.GetSide(),
		TotalQuantity: request.GetTotalQuantity(),
		StartAt:       protoTraderTime(request.GetStartAt()), EndAt: protoTraderTime(request.GetEndAt()),
		IntervalSeconds: int(request.GetIntervalSeconds()), LimitPrice: request.GetLimitPrice(),
		MaxQuantity: request.GetMaxQuantity(), ExecutionType: request.GetExecutionType(),
		OrderTimeoutSeconds: int(request.GetOrderTimeoutSeconds()),
		IdempotencyKey:      request.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.CreateTwapResponse{Job: twapToProto(job)}, nil
}

func (s *TraderServer) GetTwap(
	ctx context.Context,
	request *traderv1.GetTwapRequest,
) (*traderv1.GetTwapResponse, error) {
	job, err := s.service.GetTwap(ctx, request.GetToken(), request.GetTwapId())
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.GetTwapResponse{Job: twapToProto(job)}, nil
}

func (s *TraderServer) ListTwaps(
	ctx context.Context,
	request *traderv1.ListTwapsRequest,
) (*traderv1.ListTwapsResponse, error) {
	items, next, err := s.service.ListTwaps(
		ctx, request.GetToken(), request.GetTradingAccountId(), request.GetView(),
		request.GetStatus(), int(request.GetLimit()), request.GetCursor(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListTwapsResponse{
		Items:      make([]*traderv1.TwapJob, 0, len(items)),
		ServerTime: timestamppb.Now(), NextCursor: next,
	}
	for _, item := range items {
		response.Items = append(response.Items, twapToProto(item))
	}
	return response, nil
}

func (s *TraderServer) ListTwapOrders(
	ctx context.Context,
	request *traderv1.ListTwapOrdersRequest,
) (*traderv1.ListTwapOrdersResponse, error) {
	items, err := s.service.ListTwapOrders(ctx, request.GetToken(), request.GetTwapId())
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListTwapOrdersResponse{
		Items: make([]*traderv1.Order, 0, len(items)), ServerTime: timestamppb.Now(),
	}
	for _, item := range items {
		response.Items = append(response.Items, traderOrderToProto(item))
	}
	return response, nil
}

func (s *TraderServer) CancelTwap(
	ctx context.Context,
	request *traderv1.CancelTwapRequest,
) (*traderv1.CancelTwapResponse, error) {
	job, err := s.service.CancelTwap(ctx, request.GetToken(), request.GetTwapId())
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.CancelTwapResponse{Job: twapToProto(job)}, nil
}

func (s *TraderServer) CreateArbitrageCombination(
	ctx context.Context,
	request *traderv1.CreateArbitrageCombinationRequest,
) (*traderv1.CreateArbitrageCombinationResponse, error) {
	item, err := s.service.CreateArbitrageCombination(ctx, trader.CreateArbitrageInput{
		Token: request.GetToken(), IdempotencyKey: request.GetIdempotencyKey(),
		LegATradingAccountID: request.GetLegATradingAccountId(),
		LegAInstrumentID:     request.GetLegAInstrumentId(),
		LegBTradingAccountID: request.GetLegBTradingAccountId(),
		LegBInstrumentID:     request.GetLegBInstrumentId(),
		AskThresholdBps:      request.GetAskThresholdBps(), BidThresholdBps: request.GetBidThresholdBps(),
		TargetNotional: request.GetTargetNotional(), OrderNotional: request.GetOrderNotional(),
		MaxDeltaNotional: request.GetMaxDeltaNotional(), ExecutionMode: request.GetExecutionMode(),
		MakerLeg: request.GetMakerLeg(),
	})
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.CreateArbitrageCombinationResponse{
		Combination: arbitrageCombinationToProto(item),
	}, nil
}

func (s *TraderServer) GetArbitrageCombination(
	ctx context.Context,
	request *traderv1.GetArbitrageCombinationRequest,
) (*traderv1.GetArbitrageCombinationResponse, error) {
	item, orders, executions, events, err := s.service.GetArbitrageCombination(
		ctx, request.GetToken(), request.GetCombinationId(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.GetArbitrageCombinationResponse{
		Combination:      arbitrageCombinationToProto(item),
		Orders:           make([]*traderv1.Order, 0, len(orders)),
		RecentExecutions: make([]*traderv1.ArbitrageExecution, 0, len(executions)),
		RecentEvents:     make([]*traderv1.ArbitrageEvent, 0, len(events)),
	}
	for _, order := range orders {
		response.Orders = append(response.Orders, traderOrderToProto(order))
	}
	for _, execution := range executions {
		trigger := execution.TriggerAskSpread
		if execution.Direction == "bid" {
			trigger = execution.TriggerBidSpread
		}
		filled := shopspringdecimal.Min(
			decimalOrZero(execution.LegAFilledQuantity),
			decimalOrZero(execution.LegBFilledQuantity),
		).String()
		response.RecentExecutions = append(response.RecentExecutions, &traderv1.ArbitrageExecution{
			Id: execution.ID, Direction: execution.Direction, Status: execution.Status,
			TriggerSpreadBps: trigger, TargetBaseQuantity: execution.TargetBaseQuantity,
			FilledBaseQuantity: filled, DeltaNotional: execution.DeltaNotional,
			ErrorMessage: execution.ErrorMessage,
			CreatedAt:    optionalTraderTimestamp(execution.CreatedAt),
			UpdatedAt:    optionalTraderTimestamp(execution.UpdatedAt),
		})
	}
	for _, event := range events {
		response.RecentEvents = append(response.RecentEvents, &traderv1.ArbitrageEvent{
			Id: event.ID, Type: event.Type, Message: event.Message,
			CreatedAt: optionalTraderTimestamp(event.CreatedAt),
		})
	}
	return response, nil
}

func (s *TraderServer) ListArbitrageCombinations(
	ctx context.Context,
	request *traderv1.ListArbitrageCombinationsRequest,
) (*traderv1.ListArbitrageCombinationsResponse, error) {
	items, next, total, err := s.service.ListArbitrageCombinations(
		ctx, request.GetToken(), request.GetView(), int(request.GetLimit()), request.GetCursor(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListArbitrageCombinationsResponse{
		Items:      make([]*traderv1.ArbitrageCombination, 0, len(items)),
		ServerTime: timestamppb.Now(), NextCursor: next, Total: total,
	}
	for _, item := range items {
		response.Items = append(response.Items, arbitrageCombinationToProto(item))
	}
	return response, nil
}

func (s *TraderServer) CloseArbitrageCombination(
	ctx context.Context,
	request *traderv1.CloseArbitrageCombinationRequest,
) (*traderv1.CloseArbitrageCombinationResponse, error) {
	item, err := s.service.CloseArbitrageCombination(
		ctx, request.GetToken(), request.GetCombinationId(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.CloseArbitrageCombinationResponse{
		Combination: arbitrageCombinationToProto(item),
	}, nil
}

func instrumentToProto(item trader.Instrument) *traderv1.Instrument {
	return &traderv1.Instrument{
		Id: item.ID, Exchange: item.Exchange, ContractType: item.ContractType,
		ExchangeSymbol: item.ExchangeSymbol, BaseAsset: item.BaseAsset,
		QuoteAsset: item.QuoteAsset, SettleAsset: item.SettleAsset,
		ContractSize: item.ContractSize, PriceTick: item.PriceTick, QuantityStep: item.QuantityStep,
	}
}

func traderOrderToProto(item trader.Order) *traderv1.Order {
	return &traderv1.Order{
		Id: item.ID, IdempotencyKey: item.IdempotencyKey,
		TradingAccountId: item.TradingAccountID, ProductName: item.ProductName,
		Exchange: item.Exchange, InstrumentId: item.InstrumentID,
		ContractType: item.ContractType, ExchangeSymbol: item.ExchangeSymbol,
		BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset,
		ClientOrderId: item.ClientOrderID, VenueOrderId: item.VenueOrderID,
		Side: item.Side, OrderType: item.OrderType, Quantity: item.Quantity,
		Price: item.Price, FilledQuantity: item.FilledQuantity, AveragePrice: item.AveragePrice,
		Status: item.Status, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage,
		CreatedAt: timestamppb.New(item.CreatedAt), UpdatedAt: timestamppb.New(item.UpdatedAt),
		LastReconciledAt: optionalTraderTimestamp(item.LastReconciledAt), SyncState: item.SyncState,
		TwapJobId: item.TwapJobID, TwapSliceIndex: int32(item.TwapSliceIndex),
		TwapAttemptIndex:     int32(item.TwapAttemptIndex),
		ArbitrageExecutionId: item.ArbitrageExecutionID,
		ArbitrageLeg:         item.ArbitrageLeg, ArbitrageRole: item.ArbitrageRole,
	}
}

func twapToProto(item trader.TwapJob) *traderv1.TwapJob {
	progress := "0"
	total, totalErr := shopspringdecimal.NewFromString(item.TotalQuantity)
	filled, filledErr := shopspringdecimal.NewFromString(item.FilledQuantity)
	if totalErr == nil && filledErr == nil && total.IsPositive() {
		progress = filled.Div(total).Mul(shopspringdecimal.NewFromInt(100)).StringFixed(2)
	}
	return &traderv1.TwapJob{
		Id: item.ID, IdempotencyKey: item.IdempotencyKey,
		TradingAccountId: item.TradingAccountID, ProductName: item.ProductName,
		Exchange: item.Exchange, InstrumentId: item.InstrumentID,
		ContractType: item.ContractType, ExchangeSymbol: item.ExchangeSymbol,
		BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset, Side: item.Side,
		TotalQuantity: item.TotalQuantity, FilledQuantity: item.FilledQuantity,
		AveragePrice: item.AveragePrice, StartAt: optionalTraderTimestamp(item.StartAt),
		EndAt: optionalTraderTimestamp(item.EndAt), IntervalSeconds: int32(item.IntervalSeconds),
		LimitPrice: item.LimitPrice, MaxQuantity: item.MaxQuantity,
		ExecutionType: item.ExecutionType, OrderTimeoutSeconds: int32(item.OrderTimeoutSeconds),
		Status: item.Status, CurrentSlice: int32(item.CurrentSlice),
		CurrentAttempt: int32(item.CurrentAttempt),
		NextActionAt:   optionalTraderTimestamp(item.NextActionAt), ActiveOrderId: item.ActiveOrderID,
		ErrorMessage: item.ErrorMessage, CreatedAt: optionalTraderTimestamp(item.CreatedAt),
		UpdatedAt: optionalTraderTimestamp(item.UpdatedAt), StartedAt: optionalTraderTimestamp(item.StartedAt),
		ClosedAt: optionalTraderTimestamp(item.ClosedAt), Progress: progress,
	}
}

func arbitrageCombinationToProto(item trader.ArbitrageCombination) *traderv1.ArbitrageCombination {
	leg := func(value trader.ArbitrageLeg) *traderv1.ArbitrageLeg {
		return &traderv1.ArbitrageLeg{
			TradingAccountId: value.TradingAccountID, InstrumentId: value.InstrumentID,
			ProductName: value.ProductName, AccountName: value.AccountName,
			Exchange: value.Exchange, ContractType: value.ContractType,
			ExchangeSymbol: value.ExchangeSymbol, BaseAsset: value.BaseAsset,
			QuoteAsset: value.QuoteAsset,
		}
	}
	return &traderv1.ArbitrageCombination{
		Id: item.ID, IdempotencyKey: item.IdempotencyKey,
		LegA: leg(item.LegA), LegB: leg(item.LegB),
		AskThresholdBps: item.AskThresholdBps, BidThresholdBps: item.BidThresholdBps,
		TargetNotional: item.TargetNotional, OrderNotional: item.OrderNotional,
		MaxDeltaNotional: item.MaxDeltaNotional, ExecutionMode: item.ExecutionMode,
		MakerLeg: item.MakerLeg, Status: item.Status,
		CompletedNotional:   item.CompletedNotional,
		CurrentAskSpreadBps: item.CurrentAskSpreadBps,
		CurrentBidSpreadBps: item.CurrentBidSpreadBps,
		MarketDataStale:     item.MarketDataStale, ErrorMessage: item.ErrorMessage,
		CreatedAt: optionalTraderTimestamp(item.CreatedAt),
		UpdatedAt: optionalTraderTimestamp(item.UpdatedAt),
		ClosedAt:  optionalTraderTimestamp(item.ClosedAt),
	}
}

func protoTraderTime(value *timestamppb.Timestamp) time.Time {
	if value == nil || !value.IsValid() {
		return time.Time{}
	}
	return value.AsTime()
}

func optionalTraderTimestamp(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() || value.Equal(time.Unix(0, 0).UTC()) {
		return nil
	}
	return timestamppb.New(value)
}

func decimalOrZero(value string) shopspringdecimal.Decimal {
	parsed, err := shopspringdecimal.NewFromString(value)
	if err != nil {
		return shopspringdecimal.Zero
	}
	return parsed
}

func mapTraderError(err error) error {
	message := strings.TrimSpace(err.Error())
	switch {
	case errors.Is(err, trader.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, "invalid order request")
	case errors.Is(err, trader.ErrNotFound), errors.Is(err, trader.ErrInstrumentUnavailable):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, trader.ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists, "idempotency key conflict")
	case errors.Is(err, trader.ErrActiveTwapLimit):
		return status.Error(codes.ResourceExhausted, "active twap limit reached")
	case errors.Is(err, trader.ErrUnsupportedExchange):
		return status.Error(codes.InvalidArgument, "unsupported exchange")
	case errors.Is(err, trader.ErrOrderNotCancelable):
		return status.Error(codes.FailedPrecondition, "order is not cancelable")
	case errors.Is(err, trader.ErrTwapNotCancelable):
		return status.Error(codes.FailedPrecondition, "twap is not cancelable")
	case errors.Is(err, trader.ErrArbitrageNotClosable):
		return status.Error(codes.FailedPrecondition, "arbitrage combination is not closable")
	case errors.Is(err, trader.ErrArbitrageConflict):
		return status.Error(codes.Aborted, "arbitrage combination conflict")
	case errors.Is(err, trader.ErrMarketDataStale):
		return status.Error(codes.Unavailable, "market data is stale")
	case errors.Is(err, trader.ErrRiskLimit):
		return status.Error(codes.FailedPrecondition, "arbitrage risk limit exceeded")
	case errors.Is(err, trader.ErrPersistence):
		return status.Error(codes.Internal, "order persistence failed")
	case errors.Is(err, trader.ErrVenueRateLimited):
		return status.Error(codes.ResourceExhausted, "venue rate limited")
	case errors.Is(err, trader.ErrVenueRejected):
		return status.Error(codes.FailedPrecondition, "venue rejected order")
	case errors.Is(err, trader.ErrVenueUncertain):
		return status.Error(codes.Unavailable, "order result is uncertain")
	case errors.Is(err, trader.ErrVenueUnavailable):
		return status.Error(codes.Unavailable, "venue unavailable")
	case strings.Contains(message, "unauthenticated") || strings.Contains(message, "invalid session"):
		return status.Error(codes.Unauthenticated, "invalid session")
	default:
		return status.Error(codes.Internal, "trader request failed")
	}
}
