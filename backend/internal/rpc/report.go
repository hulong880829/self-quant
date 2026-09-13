package rpc

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	reportv1 "selfquant/backend/gen/report/v1"
	"selfquant/backend/internal/report"
)

type ReportServer struct {
	reportv1.UnimplementedReportServiceServer
	service *report.Service
}

func NewReportServer(service *report.Service) *ReportServer {
	return &ReportServer{service: service}
}

func (s *ReportServer) ListProducts(
	ctx context.Context,
	request *reportv1.ListProductsRequest,
) (*reportv1.ListProductsResponse, error) {
	items, err := s.service.ListProducts(ctx, request.GetToken())
	if err != nil {
		return nil, reportError(err)
	}
	response := &reportv1.ListProductsResponse{
		Items: make([]*reportv1.Product, 0, len(items)),
	}
	for _, item := range items {
		response.Items = append(response.Items, productProto(item))
	}
	return response, nil
}

func (s *ReportServer) GetProductDetail(
	ctx context.Context,
	request *reportv1.GetProductDetailRequest,
) (*reportv1.GetProductDetailResponse, error) {
	item, err := s.service.ProductDetail(ctx, request.GetToken(), request.GetProductId())
	if err != nil {
		return nil, reportError(err)
	}
	response := &reportv1.GetProductDetailResponse{
		Product:         productProto(item.Product),
		RecentCashFlows: cashFlowsProto(item.RecentCashFlows),
		Daily:           make([]*reportv1.DailySnapshot, 0, len(item.Daily)),
	}
	for _, daily := range item.Daily {
		response.Daily = append(response.Daily, dailyProto(daily))
	}
	if item.LatestSnapshot != nil {
		response.LatestSnapshot = dailyProto(*item.LatestSnapshot)
	}
	return response, nil
}

func (s *ReportServer) ListDailySnapshots(
	ctx context.Context,
	request *reportv1.ListDailySnapshotsRequest,
) (*reportv1.ListDailySnapshotsResponse, error) {
	items, err := s.service.ListDailySnapshots(
		ctx, request.GetToken(), request.GetProductId(),
		report.DateRange{
			From:  request.GetFromDate(),
			To:    request.GetToDate(),
			Limit: int(request.GetLimit()),
		},
	)
	if err != nil {
		return nil, reportError(err)
	}
	response := &reportv1.ListDailySnapshotsResponse{
		Items: make([]*reportv1.DailySnapshot, 0, len(items)),
	}
	for _, item := range items {
		response.Items = append(response.Items, dailyProto(item))
	}
	return response, nil
}

func (s *ReportServer) ListCashFlows(
	ctx context.Context,
	request *reportv1.ListCashFlowsRequest,
) (*reportv1.ListCashFlowsResponse, error) {
	items, err := s.service.ListCashFlows(
		ctx, request.GetToken(), request.GetProductId(),
		report.DateRange{
			From:  request.GetFromDate(),
			To:    request.GetToDate(),
			Limit: int(request.GetLimit()),
		},
	)
	if err != nil {
		return nil, reportError(err)
	}
	return &reportv1.ListCashFlowsResponse{Items: cashFlowsProto(items)}, nil
}

func (s *ReportServer) CreateCashFlow(
	ctx context.Context,
	request *reportv1.CreateCashFlowRequest,
) (*reportv1.CreateCashFlowResponse, error) {
	var occurredAt time.Time
	if request.GetOccurredAt() != nil && request.GetOccurredAt().IsValid() {
		occurredAt = request.GetOccurredAt().AsTime()
	}
	item, err := s.service.CreateCashFlow(
		ctx, request.GetToken(), request.GetProductId(), "",
		occurredAt,
		request.GetAmountUsd(), request.GetFlowType(), request.GetNote(),
		request.GetConfirmed(),
	)
	if err != nil {
		return nil, reportError(err)
	}
	return &reportv1.CreateCashFlowResponse{
		CashFlow:        cashFlowProto(item),
		RecomputeStatus: item.RecomputeStatus,
	}, nil
}

func (s *ReportServer) RecomputeProduct(
	ctx context.Context,
	request *reportv1.RecomputeProductRequest,
) (*reportv1.RecomputeProductResponse, error) {
	written, err := s.service.Recompute(
		ctx, request.GetToken(), request.GetProductId(),
		request.GetFromDate(), request.GetToDate(),
	)
	if err != nil {
		return nil, reportError(err)
	}
	return &reportv1.RecomputeProductResponse{SnapshotsWritten: int32(written)}, nil
}

func productProto(item report.Product) *reportv1.Product {
	return &reportv1.Product{
		Id: item.ID, Name: item.Name, BaseCurrency: item.BaseCurrency,
		Timezone: item.Timezone, Active: item.Active, AccountCount: int32(item.AccountCount),
		LatestEquityUsd: item.LatestEquityUSD, LatestPnlUsd: item.LatestPnLUSD,
		LatestReturnRate: item.LatestReturnRate, UpdatedAt: timestamppb.New(item.UpdatedAt),
		DisplayName: item.DisplayName, Category: item.Category, Strategy: item.Strategy,
		InceptionDate: item.InceptionDate,
	}
}

func dailyProto(item report.DailySnapshot) *reportv1.DailySnapshot {
	value := &reportv1.DailySnapshot{
		Id: item.ID, ProductId: item.ProductID, ReportDate: item.ReportDate,
		OpeningEquityUsd: item.OpeningEquityUSD, ClosingEquityUsd: item.ClosingEquityUSD,
		NetCashFlowUsd: item.NetCashFlowUSD, PnlUsd: item.PnLUSD,
		ReturnRate: item.ReturnRate, SampleCount: int32(item.SampleCount),
		Status: item.Status, FinalizedAt: timestamppb.New(item.FinalizedAt),
		AbsoluteReturn: item.AbsoluteReturn, AnnualizedReturn: item.AnnualizedReturn,
		Annualized_7D: item.Annualized7D, Annualized_30D: item.Annualized30D,
		MaxDrawdown: item.MaxDrawdown, Sharpe: item.Sharpe,
		Volume_24HUsd:   item.Volume24hUSD,
		SubscriptionUsd: item.SubscriptionUSD, RedemptionUsd: item.RedemptionUSD,
		CashFlowCount: int32(item.CashFlowCount), PeriodRuleVersion: int32(item.PeriodRuleVersion),
	}
	if !item.PeriodStart.IsZero() {
		value.PeriodStart = timestamppb.New(item.PeriodStart)
	}
	if !item.PeriodEnd.IsZero() {
		value.PeriodEnd = timestamppb.New(item.PeriodEnd)
	}
	return value
}

func cashFlowsProto(items []report.CashFlow) []*reportv1.CashFlow {
	result := make([]*reportv1.CashFlow, 0, len(items))
	for _, item := range items {
		result = append(result, cashFlowProto(item))
	}
	return result
}

func cashFlowProto(item report.CashFlow) *reportv1.CashFlow {
	value := &reportv1.CashFlow{
		Id: item.ID, ProductId: item.ProductID, FlowDate: item.FlowDate,
		AmountUsd: item.AmountUSD, FlowType: item.FlowType, Note: item.Note,
		Confirmed: item.Confirmed, ConfirmedBy: item.ConfirmedBy,
		CreatedBy: item.CreatedBy, CreatedAt: timestamppb.New(item.CreatedAt),
		OccurredAt: timestamppb.New(item.OccurredAt),
	}
	if !item.ConfirmedAt.IsZero() {
		value.ConfirmedAt = timestamppb.New(item.ConfirmedAt)
	}
	return value
}

func reportError(err error) error {
	switch {
	case errors.Is(err, report.ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, "invalid session")
	case errors.Is(err, report.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, report.ErrNotFound):
		return status.Error(codes.NotFound, "product not found")
	case errors.Is(err, report.ErrDuplicate):
		return status.Error(codes.AlreadyExists, "duplicate cash flow")
	default:
		return status.Error(codes.Internal, "report operation failed")
	}
}
