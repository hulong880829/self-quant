package rpc

import (
	"context"
	"errors"
	"strings"
	"time"

	shopspringdecimal "github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/trader"
)

type traderService interface {
	ListInstruments(context.Context, string, int64, string) ([]trader.Instrument, error)
	ApplyAccountProfile(context.Context, string, int64) (trader.AccountProfileResult, error)
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

type traderArbitrageReadService interface {
	PreflightArbitrageCombination(context.Context, trader.CreateArbitrageInput) (trader.ArbitragePreflight, error)
	ListArbitrageCombinationOrders(context.Context, string, string, trader.ArbitrageHistoryQuery) ([]trader.Order, string, error)
	ListArbitrageCombinationExecutions(context.Context, string, string, trader.ArbitrageHistoryQuery) ([]trader.ArbitrageExecution, string, error)
	ListArbitrageCombinationEvents(context.Context, string, string, trader.ArbitrageHistoryQuery) ([]trader.ArbitrageEvent, string, error)
}

type traderArbitrageConfigService interface {
	UpdateArbitrageCombination(context.Context, trader.UpdateArbitrageInput) (trader.ArbitrageCombination, error)
}

type traderCapabilityService interface {
	GetVenueCapabilities(context.Context, string, int64) (trader.VenueCapabilities, error)
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
	if capabilityService, ok := s.service.(traderCapabilityService); ok {
		capabilities, capabilityErr := capabilityService.GetVenueCapabilities(
			ctx, request.GetToken(), request.GetTradingAccountId(),
		)
		if capabilityErr == nil {
			response.Capabilities = &traderv1.VenueCapabilities{
				Products: capabilities.Products, QuoteAssets: capabilities.QuoteAssets,
				TimeInForce: capabilities.TimeInForce, PostOnly: capabilities.PostOnly,
				ReduceOnly: capabilities.ReduceOnly, MakerTwap: capabilities.MakerTwap,
				PrivateOrderStream: capabilities.PrivateOrderStream, OneWayOnly: capabilities.OneWayOnly,
			}
		}
	}
	for _, item := range items {
		response.Items = append(response.Items, instrumentToProto(item))
	}
	return response, nil
}

func (s *TraderServer) ApplyAccountProfile(
	ctx context.Context,
	request *traderv1.ApplyAccountProfileRequest,
) (*traderv1.ApplyAccountProfileResponse, error) {
	result, err := s.service.ApplyAccountProfile(
		ctx, request.GetToken(), request.GetTradingAccountId(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	steps := make([]*traderv1.AccountProfileStepResult, 0, len(result.Steps))
	for _, step := range result.Steps {
		steps = append(steps, &traderv1.AccountProfileStepResult{
			Step: step.Step, Status: step.Status, Code: step.Code, Message: step.Message,
		})
	}
	return &traderv1.ApplyAccountProfileResponse{
		Result: &traderv1.AccountProfileResult{
			TradingAccountId: result.TradingAccountID,
			ProductName:      result.ProductName,
			AccountName:      result.AccountName,
			Exchange:         result.Exchange,
			OverallStatus:    result.OverallStatus,
			Steps:            steps,
		},
	}, nil
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
	order = s.orderWithFillSummary(ctx, order)
	return &traderv1.GetOrderResponse{Order: traderOrderToProto(order)}, nil
}

func (s *TraderServer) orderWithFillSummary(ctx context.Context, order trader.Order) trader.Order {
	items := s.ordersWithFillSummaries(ctx, []trader.Order{order})
	if len(items) == 0 {
		return order
	}
	return items[0]
}

func (s *TraderServer) ordersWithFillSummaries(ctx context.Context, items []trader.Order) []trader.Order {
	attacher, ok := s.service.(interface {
		AttachOrderFillSummaries(context.Context, []trader.Order) []trader.Order
	})
	if !ok {
		return items
	}
	return attacher.AttachOrderFillSummaries(ctx, items)
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
		TargetNotional:                    request.GetTargetNotional(),
		ExecutionMode:                     request.GetExecutionMode(),
		MakerLeg:                          request.GetMakerLeg(),
		RunMode:                           request.GetRunMode(),
		EntryDirection:                    request.GetEntryDirection(),
		LegALeverage:                      request.GetLegALeverage(),
		LegBLeverage:                      request.GetLegBLeverage(),
		ExitPolicy:                        request.GetExitPolicy(),
		ExitAnnualizedRate:                request.GetExitAnnualizedRate(),
		ExitAfterSeconds:                  int(request.GetExitAfterSeconds()),
		EarlyExitFunding8hAnnualizedFloor: request.GetEarlyExitFunding_8HAnnualizedFloor(),
		EarlyExitLiquidationDistancePct:   request.GetEarlyExitLiquidationDistancePct(),
	})
	if err != nil {
		return nil, mapTraderError(err)
	}
	return &traderv1.CreateArbitrageCombinationResponse{
		Combination: arbitrageCombinationToProto(item),
	}, nil
}

func (s *TraderServer) UpdateArbitrageCombination(
	ctx context.Context,
	request *traderv1.UpdateArbitrageCombinationRequest,
) (*traderv1.UpdateArbitrageCombinationResponse, error) {
	service, ok := s.service.(traderArbitrageConfigService)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "arbitrage parameter update is unavailable")
	}
	item, err := service.UpdateArbitrageCombination(ctx, trader.UpdateArbitrageInput{
		Token: request.GetToken(), CombinationID: request.GetCombinationId(),
		AskThresholdBps:                   request.AskThresholdBps,
		BidThresholdBps:                   request.BidThresholdBps,
		TargetNotional:                    request.TargetNotional,
		ExitAfterSeconds:                  protoInt32Ptr(request.ExitAfterSeconds),
		ExitAnnualizedRate:                request.ExitAnnualizedRate,
		EarlyExitFunding8hAnnualizedFloor: request.EarlyExitFunding_8HAnnualizedFloor,
		EarlyExitLiquidationDistancePct:   request.EarlyExitLiquidationDistancePct,
	})
	if err != nil {
		if errors.Is(err, trader.ErrInvalidArgument) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, mapTraderError(err)
	}
	return &traderv1.UpdateArbitrageCombinationResponse{
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
	response.Combination.FinalMetrics = arbitrageFinalMetricsToProto(item)
	for _, order := range orders {
		response.Orders = append(response.Orders, traderOrderToProto(order))
	}
	for _, execution := range executions {
		response.RecentExecutions = append(response.RecentExecutions, arbitrageExecutionToProto(execution))
	}
	for _, event := range events {
		response.RecentEvents = append(response.RecentEvents, arbitrageEventToProto(event))
	}
	return response, nil
}

func arbitrageExecutionToProto(execution trader.ArbitrageExecution) *traderv1.ArbitrageExecution {
	trigger := execution.TriggerAskSpread
	if execution.Direction == "bid" {
		trigger = execution.TriggerBidSpread
	}
	filled := shopspringdecimal.Min(
		decimalOrZero(execution.LegAFilledQuantity),
		decimalOrZero(execution.LegBFilledQuantity),
	).String()
	return &traderv1.ArbitrageExecution{
		Id: execution.ID, Direction: execution.Direction, Status: execution.Status,
		TriggerSpreadBps: trigger, TargetBaseQuantity: execution.TargetBaseQuantity,
		FilledBaseQuantity: filled, DeltaNotional: execution.DeltaNotional,
		ErrorMessage:      execution.ErrorMessage,
		CreatedAt:         optionalTraderTimestamp(execution.CreatedAt),
		UpdatedAt:         optionalTraderTimestamp(execution.UpdatedAt),
		RequestedNotional: execution.RequestedNotional,
		PositionEffect:    execution.PositionEffect, ReduceOnly: execution.ReduceOnly,
	}
}

func arbitrageEventToProto(event trader.ArbitrageEvent) *traderv1.ArbitrageEvent {
	result := &traderv1.ArbitrageEvent{
		Id: event.ID, Type: event.Type, Message: event.Message,
		ExecutionId: event.ExecutionID, CreatedAt: optionalTraderTimestamp(event.CreatedAt),
	}
	if event.PayloadObject {
		if payload, err := structpb.NewStruct(event.Payload); err == nil {
			result.Payload = payload
		}
	}
	return result
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

func (s *TraderServer) PreflightArbitrageCombination(
	ctx context.Context,
	request *traderv1.PreflightArbitrageCombinationRequest,
) (*traderv1.PreflightArbitrageCombinationResponse, error) {
	service, ok := s.service.(traderArbitrageReadService)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "arbitrage preflight is unavailable")
	}
	result, err := service.PreflightArbitrageCombination(ctx, trader.CreateArbitrageInput{
		Token:                request.GetToken(),
		LegATradingAccountID: request.GetLegATradingAccountId(),
		LegAInstrumentID:     request.GetLegAInstrumentId(),
		LegBTradingAccountID: request.GetLegBTradingAccountId(),
		LegBInstrumentID:     request.GetLegBInstrumentId(),
		AskThresholdBps:      request.GetAskThresholdBps(),
		BidThresholdBps:      request.GetBidThresholdBps(),
		TargetNotional:       request.GetTargetNotional(),
		ExecutionMode:        request.GetExecutionMode(), MakerLeg: request.GetMakerLeg(),
		RunMode: request.GetRunMode(), EntryDirection: request.GetEntryDirection(),
		LegALeverage: request.GetLegALeverage(), LegBLeverage: request.GetLegBLeverage(),
		ExitPolicy: request.GetExitPolicy(), ExitAnnualizedRate: request.GetExitAnnualizedRate(),
		ExitAfterSeconds:                  int(request.GetExitAfterSeconds()),
		EarlyExitFunding8hAnnualizedFloor: request.GetEarlyExitFunding_8HAnnualizedFloor(),
		EarlyExitLiquidationDistancePct:   request.GetEarlyExitLiquidationDistancePct(),
	})
	if err != nil {
		return nil, mapTraderError(err)
	}
	warnings := make([]*traderv1.PreflightWarning, 0, len(result.Warnings))
	for _, warning := range result.Warnings {
		warnings = append(warnings, preflightWarningToProto(warning))
	}
	return &traderv1.PreflightArbitrageCombinationResponse{
		Executable: result.Executable, CheckedAt: optionalTraderTimestamp(result.CheckedAt),
		ExpiresAt: optionalTraderTimestamp(result.ExpiresAt),
		LegA:      preflightLegToProto(result.LegA), LegB: preflightLegToProto(result.LegB),
		Warnings: warnings, Code: result.Code, Message: result.Message,
		Leg: result.Leg, Exchange: result.Exchange,
	}, nil
}

func (s *TraderServer) PreflightIncreaseTarget(
	ctx context.Context,
	request *traderv1.PreflightIncreaseTargetRequest,
) (*traderv1.PreflightIncreaseTargetResponse, error) {
	service, ok := s.service.(interface {
		PreflightIncreaseTarget(context.Context, string, string, string) (trader.IncreaseTargetPreflight, error)
	})
	if !ok {
		return nil, status.Error(codes.Unimplemented, "increase preflight is unavailable")
	}
	result, err := service.PreflightIncreaseTarget(
		ctx, request.GetToken(), request.GetCombinationId(), request.GetNewTargetNotional(),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	warnings := make([]*traderv1.PreflightWarning, 0, len(result.Warnings))
	for _, warning := range result.Warnings {
		warnings = append(warnings, preflightWarningToProto(warning))
	}
	return &traderv1.PreflightIncreaseTargetResponse{
		Executable: result.Executable, CurrentTargetNotional: result.CurrentTargetNotional,
		NewTargetNotional: result.NewTargetNotional, RequestedTargetDelta: result.RequestedTargetDelta,
		TargetNotionalUnit: result.TargetNotionalUnit,
		LegA:               increaseLegToProto(result.LegA), LegB: increaseLegToProto(result.LegB),
		Warnings: warnings, CheckedAt: optionalTraderTimestamp(result.CheckedAt),
		ExpiresAt: optionalTraderTimestamp(result.ExpiresAt),
		Code:      result.Code, Message: result.Message, Leg: result.Leg, Exchange: result.Exchange,
	}, nil
}

func increaseLegToProto(leg trader.IncreaseTargetLeg) *traderv1.PreflightIncreaseLeg {
	return &traderv1.PreflightIncreaseLeg{
		AdditionalNotional: leg.AdditionalNotional, IncrementalQuantity: leg.IncrementalQuantity,
		IncrementalQuantityUnit: leg.IncrementalQuantityUnit,
		IncrementalMargin:       preflightDecimalToProto(leg.IncrementalMargin),
		EstimatedFee:            preflightDecimalToProto(leg.EstimatedFee),
		AvailableFunds:          preflightDecimalToProto(leg.AvailableFunds),
		MinQuantity:             preflightDecimalToProto(leg.MinQuantity),
		MinNotional:             preflightDecimalToProto(leg.MinNotional),
		QuantityStep:            preflightDecimalToProto(leg.QuantityStep),
	}
}

func (s *TraderServer) ListArbitrageCombinationOrders(
	ctx context.Context,
	request *traderv1.ListArbitrageCombinationOrdersRequest,
) (*traderv1.ListArbitrageCombinationOrdersResponse, error) {
	service, ok := s.service.(traderArbitrageReadService)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "arbitrage history is unavailable")
	}
	items, next, err := service.ListArbitrageCombinationOrders(
		ctx, request.GetQuery().GetToken(), request.GetQuery().GetCombinationId(),
		historyQueryFromProto(request.GetQuery()),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListArbitrageCombinationOrdersResponse{NextCursor: next}
	for _, item := range items {
		response.Items = append(response.Items, traderOrderToProto(item))
	}
	return response, nil
}

func (s *TraderServer) ListArbitrageCombinationExecutions(
	ctx context.Context,
	request *traderv1.ListArbitrageCombinationExecutionsRequest,
) (*traderv1.ListArbitrageCombinationExecutionsResponse, error) {
	service, ok := s.service.(traderArbitrageReadService)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "arbitrage history is unavailable")
	}
	items, next, err := service.ListArbitrageCombinationExecutions(
		ctx, request.GetQuery().GetToken(), request.GetQuery().GetCombinationId(),
		historyQueryFromProto(request.GetQuery()),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListArbitrageCombinationExecutionsResponse{NextCursor: next}
	for _, item := range items {
		response.Items = append(response.Items, arbitrageExecutionToProto(item))
	}
	return response, nil
}

func (s *TraderServer) ListArbitrageCombinationEvents(
	ctx context.Context,
	request *traderv1.ListArbitrageCombinationEventsRequest,
) (*traderv1.ListArbitrageCombinationEventsResponse, error) {
	service, ok := s.service.(traderArbitrageReadService)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "arbitrage history is unavailable")
	}
	items, next, err := service.ListArbitrageCombinationEvents(
		ctx, request.GetQuery().GetToken(), request.GetQuery().GetCombinationId(),
		historyQueryFromProto(request.GetQuery()),
	)
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.ListArbitrageCombinationEventsResponse{NextCursor: next}
	for _, item := range items {
		response.Items = append(response.Items, arbitrageEventToProto(item))
	}
	return response, nil
}

func preflightDecimalToProto(value trader.PreflightDecimal) *traderv1.PreflightDecimal {
	status := value.Status
	if status == "" {
		status = "unknown"
	}
	return &traderv1.PreflightDecimal{Value: value.Value, Status: status, Unit: value.Unit}
}

func preflightWarningToProto(warning trader.PreflightWarning) *traderv1.PreflightWarning {
	return &traderv1.PreflightWarning{
		Code: warning.Code, Message: warning.Message, Leg: warning.Leg, Exchange: warning.Exchange,
		Details: warning.Details,
	}
}

func preflightLegToProto(value trader.PreflightLeg) *traderv1.PreflightLeg {
	return &traderv1.PreflightLeg{
		AvailableFunds:      preflightDecimalToProto(value.AvailableFunds),
		RequiredMargin:      preflightDecimalToProto(value.RequiredMargin),
		EstimatedFee:        preflightDecimalToProto(value.EstimatedFee),
		MinQuantity:         preflightDecimalToProto(value.MinQuantity),
		MinNotional:         preflightDecimalToProto(value.MinNotional),
		QuantityStep:        preflightDecimalToProto(value.QuantityStep),
		RoundedBaseQuantity: preflightDecimalToProto(value.RoundedBaseQuantity),
	}
}

func historyQueryFromProto(
	request *traderv1.ListArbitrageCombinationHistoryRequest,
) trader.ArbitrageHistoryQuery {
	return trader.ArbitrageHistoryQuery{
		Limit: int(request.GetLimit()), Cursor: request.GetCursor(),
		From: protoTraderTime(request.GetFrom()), To: protoTraderTime(request.GetTo()),
		ExecutionID: request.GetExecutionId(),
	}
}

type traderArbitrageCountService interface {
	GetArbitrageCountsByOwnerInternal(context.Context, string) ([]trader.OwnerArbitrageCount, error)
}

func (s *TraderServer) GetArbitrageCountsByOwnerInternal(
	ctx context.Context,
	request *traderv1.GetArbitrageCountsByOwnerInternalRequest,
) (*traderv1.GetArbitrageCountsByOwnerInternalResponse, error) {
	counter, ok := s.service.(traderArbitrageCountService)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "method GetArbitrageCountsByOwnerInternal not implemented")
	}
	items, err := counter.GetArbitrageCountsByOwnerInternal(ctx, request.GetServiceToken())
	if err != nil {
		return nil, mapTraderError(err)
	}
	response := &traderv1.GetArbitrageCountsByOwnerInternalResponse{
		Items: make([]*traderv1.OwnerArbitrageCounts, 0, len(items)),
	}
	for _, item := range items {
		response.Items = append(response.Items, &traderv1.OwnerArbitrageCounts{
			OwnerUsername: item.OwnerUsername,
			Running:       item.Running,
			Closing:       item.Closing,
			Closed:        item.Closed,
			Failed:        item.Failed,
		})
	}
	return response, nil
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
		LastReconciledAt: optionalTraderTimestamp(item.LastReconciledAt),
		LastFilledAt:     optionalTraderTimestamp(item.LastFilledAt),
		ActualFees:       orderFeesToProto(item.ActualFees),
		LiquidityRole:    orderLiquidityRoleProto(item.LiquidityRole),
		SyncState:        item.SyncState,
		TwapJobId:        item.TwapJobID, TwapSliceIndex: int32(item.TwapSliceIndex),
		TwapAttemptIndex:     int32(item.TwapAttemptIndex),
		ArbitrageExecutionId: item.ArbitrageExecutionID,
		ArbitrageLeg:         item.ArbitrageLeg, ArbitrageRole: item.ArbitrageRole,
	}
}

func orderFeesToProto(fees []trader.OrderActualFee) []*traderv1.OrderFee {
	if len(fees) == 0 {
		return nil
	}
	out := make([]*traderv1.OrderFee, 0, len(fees))
	for _, fee := range fees {
		out = append(out, &traderv1.OrderFee{Amount: fee.Amount, Currency: fee.Currency})
	}
	return out
}

func orderLiquidityRoleProto(role string) string {
	switch role {
	case "maker", "taker", "mixed":
		return role
	default:
		return "unknown"
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
	expectedA, expectedB := "", ""
	if !item.VenueBaselineCapturedAt.IsZero() &&
		!item.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC()) {
		expectedA = decimalOrZero(item.LegAVenueBaselineBasePosition).
			Add(decimalOrZero(item.LegABasePosition)).String()
		expectedB = decimalOrZero(item.LegBVenueBaselineBasePosition).
			Add(decimalOrZero(item.LegBBasePosition)).String()
	}
	return &traderv1.ArbitrageCombination{
		Id: item.ID, IdempotencyKey: item.IdempotencyKey,
		LegA: leg(item.LegA), LegB: leg(item.LegB),
		AskThresholdBps: item.AskThresholdBps, BidThresholdBps: item.BidThresholdBps,
		TargetNotional: item.TargetNotional,
		ExecutionMode:  item.ExecutionMode,
		MakerLeg:       item.MakerLeg, Status: item.Status,
		PositionNotional:              item.PositionNotional,
		CumulativeTurnoverNotional:    item.CumulativeTurnoverNotional,
		GrossTurnoverNotional:         item.GrossTurnoverNotional,
		RuntimeState:                  item.RuntimeState,
		LegABasePosition:              item.LegABasePosition,
		LegBBasePosition:              item.LegBBasePosition,
		CarryBaseQuantity:             item.CarryBaseQuantity,
		LegAVenueBasePosition:         item.LegAVenueBasePosition,
		LegBVenueBasePosition:         item.LegBVenueBasePosition,
		LegAPositionDifference:        item.LegAPositionDifference,
		LegBPositionDifference:        item.LegBPositionDifference,
		LastPositionReconciledAt:      optionalTraderTimestamp(item.LastPositionReconciledAt),
		LegAAverageEntryPrice:         item.LegAAverageEntryPrice,
		LegBAverageEntryPrice:         item.LegBAverageEntryPrice,
		AverageEntrySpreadBps:         item.AverageEntrySpreadBps,
		LegAUnrealizedPnl:             item.LegAUnrealizedPnl,
		LegBUnrealizedPnl:             item.LegBUnrealizedPnl,
		RealizedSpreadPnl:             item.RealizedSpreadPnl,
		EstimatedFundingPnl:           item.EstimatedFundingPnl,
		CombinedPositionAnnualized:    item.CombinedPositionAnnualized,
		FundingHistoryComplete:        item.FundingHistoryComplete,
		LegAVenueBaselineBasePosition: item.LegAVenueBaselineBasePosition,
		LegBVenueBaselineBasePosition: item.LegBVenueBaselineBasePosition,
		VenueBaselineCapturedAt:       optionalTraderTimestamp(item.VenueBaselineCapturedAt),
		LegAExpectedBasePosition:      expectedA,
		LegBExpectedBasePosition:      expectedB,
		LegAVenueNotional:             item.LegAVenueNotional,
		LegBVenueNotional:             item.LegBVenueNotional,
		LegAVenueValuationPrice:       item.LegAVenueValuationPrice,
		LegBVenueValuationPrice:       item.LegBVenueValuationPrice,
		LegAVenueValuationAt:          optionalTraderTimestamp(item.LegAVenueValuationAt),
		LegBVenueValuationAt:          optionalTraderTimestamp(item.LegBVenueValuationAt),
		CurrentAskSpreadBps:           item.CurrentAskSpreadBps,
		CurrentBidSpreadBps:           item.CurrentBidSpreadBps,
		MarketDataStale:               item.MarketDataStale, ErrorMessage: item.ErrorMessage,
		ConsecutiveFailures:                int32(item.ConsecutiveFailures),
		NextRetryAt:                        optionalTraderTimestamp(item.NextRetryAt),
		PositionUncertain:                  item.PositionUncertain,
		CreatedAt:                          optionalTraderTimestamp(item.CreatedAt),
		UpdatedAt:                          optionalTraderTimestamp(item.UpdatedAt),
		ClosedAt:                           optionalTraderTimestamp(item.ClosedAt),
		RunMode:                            item.RunMode,
		EntryDirection:                     item.EntryDirection,
		LegALeverage:                       item.LegALeverage,
		LegBLeverage:                       item.LegBLeverage,
		ExitPolicy:                         item.ExitPolicy,
		ExitAnnualizedRate:                 item.ExitAnnualizedRate,
		ExitAfterSeconds:                   int32(item.ExitAfterSeconds),
		TargetReachedAt:                    optionalTraderTimestamp(item.TargetReachedAt),
		ScheduledExitAt:                    optionalTraderTimestamp(item.ScheduledExitAt),
		OneShotPhase:                       item.OneShotPhase,
		EarlyExitFunding_8HAnnualizedFloor: item.EarlyExitFunding8hAnnualizedFloor,
		EarlyExitLiquidationDistancePct:    item.EarlyExitLiquidationDistancePct,
		LegALiquidationDistancePct:         item.LegALiquidationDistancePct,
		LegBLiquidationDistancePct:         item.LegBLiquidationDistancePct,
	}
}

func arbitrageFinalMetricsToProto(item trader.ArbitrageCombination) *traderv1.ArbitrageFinalMetrics {
	return &traderv1.ArbitrageFinalMetrics{
		Status:                     item.FinalMetricsStatus,
		CompletedAt:                optionalTraderTimestamp(item.FinalMetricsCompletedAt),
		RetryCount:                 int32(item.FinalMetricsRetryCount),
		LastError:                  item.FinalMetricsLastError,
		RealizedSpreadPnl:          item.RealizedSpreadPnl,
		EstimatedFundingPnl:        item.EstimatedFundingPnl,
		EstimatedTradingFee:        item.EstimatedTradingFee,
		CombinedPositionAnnualized: item.CombinedPositionAnnualized,
		MetricsQuality:             item.MetricsQuality,
		HoldingSeconds:             item.HoldingSeconds,
		NotionalExposureSeconds:    item.NotionalExposureSeconds,
		CalculatedAt:               optionalTraderTimestamp(item.MetricsCalculatedAt),
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
	var createErr *trader.ArbitrageCreateError
	if errors.As(err, &createErr) {
		st := status.New(arbitrageCreateGRPCCode(createErr.Code), createErr.Message)
		detail := &traderv1.ArbitrageCreateFailure{
			Code: createErr.Code, Message: createErr.Message, Leg: createErr.Leg,
			Details: createErr.Details,
		}
		if withDetails, detailErr := st.WithDetails(detail); detailErr == nil {
			return withDetails.Err()
		}
		return st.Err()
	}
	message := strings.TrimSpace(err.Error())
	switch {
	case errors.Is(err, trader.ErrInvalidServiceToken):
		return status.Error(codes.Unauthenticated, "invalid service token")
	case errors.Is(err, trader.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, "invalid order request")
	case errors.Is(err, trader.ErrNotFound), errors.Is(err, trader.ErrInstrumentUnavailable):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, trader.ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists, "idempotency key conflict")
	case errors.Is(err, trader.ErrActiveArbitrageInstrumentConflict):
		detail := strings.TrimPrefix(
			err.Error(),
			trader.ErrActiveArbitrageInstrumentConflict.Error()+": ",
		)
		return status.Error(
			codes.AlreadyExists,
			"active_arbitrage_instrument_conflict: "+detail,
		)
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
	case errors.Is(err, trader.ErrArbitrageConfigInvalid):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, trader.ErrMarketDataStale):
		return status.Error(codes.Unavailable, "market data is stale")
	case errors.Is(err, trader.ErrRiskLimit):
		return status.Error(codes.FailedPrecondition, "arbitrage risk limit exceeded")
	case errors.Is(err, trader.ErrPositionMode):
		return status.Error(codes.FailedPrecondition, err.Error())
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
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "trader request timed out")
	case strings.Contains(message, "unauthenticated") || strings.Contains(message, "invalid session"):
		return status.Error(codes.Unauthenticated, "invalid session")
	default:
		return status.Error(codes.Internal, "trader request failed")
	}
}

func arbitrageCreateGRPCCode(code string) codes.Code {
	switch code {
	case "invalid_leverage":
		return codes.InvalidArgument
	case "insufficient_margin", "insufficient_spot_balance", "position_capacity_exceeded":
		return codes.FailedPrecondition
	case "leverage_apply_failed", "venue_unavailable":
		return codes.Unavailable
	default:
		return codes.FailedPrecondition
	}
}

func protoInt32Ptr(value *int32) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}
