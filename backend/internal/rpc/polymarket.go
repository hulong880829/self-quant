package rpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	polymarketv1 "selfquant/backend/gen/polymarket/v1"
	"selfquant/backend/internal/polymarket"
)

type PolymarketServer struct {
	polymarketv1.UnimplementedPolymarketServiceServer
	service   *polymarket.Service
	snapshots *polymarket.SnapshotStore
}

func NewPolymarketServer(
	service *polymarket.Service,
	snapshots *polymarket.SnapshotStore,
) *PolymarketServer {
	return &PolymarketServer{service: service, snapshots: snapshots}
}

func (s *PolymarketServer) ListMarkets(
	_ context.Context,
	request *polymarketv1.ListMarketsRequest,
) (*polymarketv1.ListMarketsResponse, error) {
	markets, version := s.snapshots.ListMarkets(
		request.GetAsset(), request.GetPeriod(), request.GetActiveOnly(),
	)
	items := make([]*polymarketv1.Market, 0, len(markets))
	for _, market := range markets {
		items = append(items, marketToProto(market))
	}
	return &polymarketv1.ListMarketsResponse{
		Items: items, ServerTime: timestamppb.Now(),
		SnapshotVersion: fmt.Sprintf("%d", version),
	}, nil
}

func (s *PolymarketServer) GetMarketSnapshot(
	_ context.Context,
	request *polymarketv1.GetMarketSnapshotRequest,
) (*polymarketv1.GetMarketSnapshotResponse, error) {
	snapshot, ok := s.snapshots.Get(request.GetMarketId())
	if !ok {
		return nil, status.Error(codes.NotFound, "market not found")
	}
	return &polymarketv1.GetMarketSnapshotResponse{
		Snapshot:   snapshotToProto(snapshot, snapshot.Series, true),
		ServerTime: timestamppb.Now(),
	}, nil
}

func (s *PolymarketServer) StreamMarketSnapshots(
	request *polymarketv1.StreamMarketSnapshotsRequest,
	stream polymarketv1.PolymarketService_StreamMarketSnapshotsServer,
) error {
	marketID := request.GetMarketId()
	initial, ok := s.snapshots.Get(marketID)
	if !ok {
		return status.Error(codes.NotFound, "market not found")
	}
	fullSeries := polymarket.DownsampleSeries(initial.Series, polymarket.MaxChartPoints)
	if err := stream.Send(&polymarketv1.StreamMarketSnapshotsResponse{
		Snapshot:   snapshotToProto(initial, fullSeries, true),
		ServerTime: timestamppb.Now(),
	}); err != nil {
		return err
	}
	updates, cancel := s.snapshots.Subscribe(marketID)
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case event := <-updates:
			proto := streamSnapshotEvent(event)
			if err := stream.Send(&polymarketv1.StreamMarketSnapshotsResponse{
				Snapshot: proto, ServerTime: timestamppb.Now(),
			}); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := stream.Send(&polymarketv1.StreamMarketSnapshotsResponse{
				ServerTime: timestamppb.Now(),
			}); err != nil {
				return err
			}
		}
	}
}

func streamSnapshotEvent(event polymarket.SnapshotEvent) *polymarketv1.MarketSnapshot {
	switch event.Kind {
	case polymarket.SnapshotEventFull:
		return snapshotToProto(
			event.Snapshot,
			polymarket.DownsampleSeries(event.Snapshot.Series, polymarket.MaxChartPoints),
			true,
		)
	case polymarket.SnapshotEventPriceDelta:
		return snapshotToProto(event.Snapshot, event.DeltaPoints, true)
	default:
		return snapshotToProto(event.Snapshot, nil, false)
	}
}

func (s *PolymarketServer) GetAccountSummary(
	ctx context.Context,
	request *polymarketv1.GetAccountSummaryRequest,
) (*polymarketv1.GetAccountSummaryResponse, error) {
	summary, err := s.service.GetAccountSummary(
		ctx, request.GetToken(), request.GetTradingAccountId(),
	)
	if err != nil {
		return nil, mapPolymarketError(err)
	}
	return &polymarketv1.GetAccountSummaryResponse{
		Summary: &polymarketv1.AccountSummary{
			TradingAccountId: summary.TradingAccountID,
			AccountName:      summary.AccountName, WalletAddress: summary.WalletAddress,
			AvailableBalance: summary.AvailableBalance,
			PositionValue:    summary.PositionValue, TotalAssets: summary.TotalAssets,
			SourceUpdatedAt: timestamppb.New(summary.SourceUpdatedAt), Stale: summary.Stale,
		},
	}, nil
}

func (s *PolymarketServer) ListPositions(
	ctx context.Context,
	request *polymarketv1.ListPositionsRequest,
) (*polymarketv1.ListPositionsResponse, error) {
	positions, stale, err := s.service.ListPositions(
		ctx, request.GetToken(), request.GetTradingAccountId(), false,
	)
	if err != nil {
		return nil, mapPolymarketError(err)
	}
	items := make([]*polymarketv1.Position, 0, len(positions))
	for _, position := range positions {
		items = append(items, positionToProto(position))
	}
	return &polymarketv1.ListPositionsResponse{
		Items: items, ServerTime: timestamppb.Now(), Stale: stale,
	}, nil
}

func (s *PolymarketServer) ListOpenOrders(
	ctx context.Context,
	request *polymarketv1.ListOpenOrdersRequest,
) (*polymarketv1.ListOpenOrdersResponse, error) {
	orders, stale, err := s.service.ListOpenOrders(
		ctx, request.GetToken(), request.GetTradingAccountId(), false,
	)
	if err != nil {
		return nil, mapPolymarketError(err)
	}
	items := make([]*polymarketv1.OpenOrder, 0, len(orders))
	for _, order := range orders {
		items = append(items, openOrderToProto(order))
	}
	return &polymarketv1.ListOpenOrdersResponse{
		Items: items, ServerTime: timestamppb.Now(), Stale: stale,
	}, nil
}

func (s *PolymarketServer) PlaceOrder(
	ctx context.Context,
	request *polymarketv1.PlaceOrderRequest,
) (*polymarketv1.PlaceOrderResponse, error) {
	order, err := s.service.PlaceOrder(ctx, polymarket.PlaceOrderInput{
		Token: request.GetToken(), TradingAccountID: request.GetTradingAccountId(),
		MarketID: request.GetMarketId(), Outcome: request.GetOutcome(),
		Side: request.GetSide(), Amount: request.GetAmount(),
		AmountUnit: request.GetAmountUnit(), IdempotencyKey: request.GetIdempotencyKey(),
		ExecutionType: request.GetExecutionType(), LimitPrice: request.GetLimitPrice(),
	})
	if err != nil && order.ID == "" {
		return nil, mapPolymarketError(err)
	}
	return &polymarketv1.PlaceOrderResponse{Order: orderToProto(order)}, nil
}

func (s *PolymarketServer) CancelOrder(
	ctx context.Context,
	request *polymarketv1.CancelOrderRequest,
) (*polymarketv1.CancelOrderResponse, error) {
	result, err := s.service.CancelOrder(
		ctx, request.GetToken(), request.GetTradingAccountId(), request.GetOrderId(),
	)
	if err != nil {
		return nil, mapPolymarketError(err)
	}
	return &polymarketv1.CancelOrderResponse{
		OrderId: result.OrderID, Status: result.Status, Message: result.Message,
	}, nil
}

func (s *PolymarketServer) GetOrder(
	ctx context.Context,
	request *polymarketv1.GetOrderRequest,
) (*polymarketv1.GetOrderResponse, error) {
	order, err := s.service.GetOrder(ctx, request.GetToken(), request.GetOrderId())
	if err != nil {
		return nil, mapPolymarketError(err)
	}
	return &polymarketv1.GetOrderResponse{Order: orderToProto(order)}, nil
}

func (s *PolymarketServer) StreamAccountEvents(
	request *polymarketv1.StreamAccountEventsRequest,
	stream polymarketv1.PolymarketService_StreamAccountEventsServer,
) error {
	events, unsubscribe, err := s.service.SubscribeAccountEvents(
		stream.Context(), request.GetToken(), request.GetTradingAccountId(),
	)
	if err != nil {
		return mapPolymarketError(err)
	}
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-heartbeat.C:
			if err := stream.Send(&polymarketv1.AccountEvent{
				Type: "heartbeat", ServerTime: timestamppb.Now(),
			}); err != nil {
				return err
			}
		case event := <-events:
			if err := stream.Send(accountEventToProto(event)); err != nil {
				return err
			}
		}
	}
}

func marketToProto(market polymarket.Market) *polymarketv1.Market {
	return &polymarketv1.Market{
		Id: market.ID, ConditionId: market.ConditionID, Slug: market.Slug,
		Asset: market.Asset, Period: market.Period, Title: market.Title,
		WindowStart: timestamppb.New(market.WindowStart),
		WindowEnd:   timestamppb.New(market.WindowEnd),
		UpTokenId:   market.UpTokenID, DownTokenId: market.DownTokenID,
		TickSize: market.TickSize, NegativeRisk: market.NegativeRisk, Active: market.Active,
	}
}

func snapshotToProto(
	snapshot polymarket.Snapshot,
	series []polymarket.PricePoint,
	includeSeries bool,
) *polymarketv1.MarketSnapshot {
	points := make([]*polymarketv1.PricePoint, 0)
	if includeSeries {
		for _, point := range series {
			points = append(points, pricePointToProto(point))
		}
	}
	return &polymarketv1.MarketSnapshot{
		Market: marketToProto(snapshot.Market), OpenPrice: snapshot.OpenPrice,
		ChainlinkPrice: snapshot.ChainlinkPrice, UpBid: snapshot.UpBid,
		UpAsk: snapshot.UpAsk, DownBid: snapshot.DownBid, DownAsk: snapshot.DownAsk,
		PriceSeries: points, SourceUpdatedAt: timestamppb.New(snapshot.SourceUpdated),
		Stale: snapshot.Stale, Version: snapshot.Version,
	}
}

func pricePointToProto(point polymarket.PricePoint) *polymarketv1.PricePoint {
	return &polymarketv1.PricePoint{
		Timestamp: timestamppb.New(point.Timestamp), OpenPrice: point.OpenPrice,
		ChainlinkPrice: point.ChainlinkPrice,
	}
}

func openOrderToProto(order polymarket.OpenOrder) *polymarketv1.OpenOrder {
	return &polymarketv1.OpenOrder{
		Id: order.ID, ConditionId: order.ConditionID, TokenId: order.TokenID,
		MarketTitle: order.MarketTitle, Outcome: order.Outcome, Side: order.Side,
		Price: order.Price, OriginalSize: order.OriginalSize,
		MatchedSize: order.MatchedSize, RemainingSize: order.RemainingSize,
		Status: order.Status, OrderType: order.OrderType,
		CreatedAt: timestamppb.New(order.CreatedAt),
	}
}

func accountEventToProto(event polymarket.AccountEvent) *polymarketv1.AccountEvent {
	orders := make([]*polymarketv1.OpenOrder, 0, len(event.OpenOrders))
	for _, order := range event.OpenOrders {
		orders = append(orders, openOrderToProto(order))
	}
	result := &polymarketv1.AccountEvent{
		Type: event.Type, OpenOrders: orders,
		PortfolioChanged: event.PortfolioChanged, ServerTime: timestamppb.Now(),
	}
	if event.Order != nil {
		result.Order = openOrderToProto(*event.Order)
	}
	return result
}

func positionToProto(position polymarket.Position) *polymarketv1.Position {
	return &polymarketv1.Position{
		Id: position.ID, TradingAccountId: position.TradingAccountID,
		ConditionId: position.ConditionID, TokenId: position.TokenID,
		Market: position.Market, Outcome: position.Outcome, Size: position.Size,
		AveragePrice: position.AveragePrice, CurrentPrice: position.CurrentPrice,
		InitialValue: position.InitialValue, CurrentValue: position.CurrentValue,
		CashPnl: position.CashPnL, PercentPnl: position.PercentPnL,
		Redeemable:      position.Redeemable,
		SourceUpdatedAt: timestamppb.New(position.SourceUpdatedAt),
	}
}

func orderToProto(order polymarket.Order) *polymarketv1.Order {
	return &polymarketv1.Order{
		Id: order.ID, ClobOrderId: order.CLOBOrderID,
		TradingAccountId: order.TradingAccountID, MarketId: order.MarketID,
		TokenId: order.TokenID, Outcome: order.Outcome, Side: order.Side,
		RequestedAmount: order.RequestedAmount, AmountUnit: order.AmountUnit,
		FilledSize: order.FilledSize, AveragePrice: order.AveragePrice,
		Status: order.Status, ErrorCode: order.ErrorCode,
		CreatedAt: timestamppb.New(order.CreatedAt), UpdatedAt: timestamppb.New(order.UpdatedAt),
		ExecutionType: order.ExecutionType, LimitPrice: order.LimitPrice,
		ClobOrderType: order.CLOBOrderType, ErrorMessage: order.ErrorMessage,
	}
}

func mapPolymarketError(err error) error {
	if status.Code(err) != codes.Unknown {
		return err
	}
	var upstream *polymarket.CLOBError
	if errors.As(err, &upstream) {
		switch upstream.StatusCode {
		case 401, 403:
			return status.Error(codes.Unauthenticated, "polymarket credentials invalid; please rebind")
		case 425, 429:
			return status.Error(codes.ResourceExhausted, "polymarket upstream rate limited")
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "polymarket upstream timed out")
	case errors.Is(err, polymarket.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, polymarket.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return status.Error(codes.NotFound, "resource not found")
	case errors.Is(err, polymarket.ErrMarketClosed):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, polymarket.ErrCancelRejected):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, polymarket.ErrAccountStreamUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, polymarket.ErrInsufficientFunds),
		errors.Is(err, polymarket.ErrInsufficientPosition):
		return status.Error(codes.ResourceExhausted, err.Error())
	default:
		return status.Error(codes.Unavailable, "polymarket upstream unavailable")
	}
}
