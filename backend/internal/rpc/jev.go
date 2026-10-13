package rpc

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	d "github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	jevv1 "selfquant/backend/gen/jev/v1"
	"selfquant/backend/internal/jev"
)

type jevAPI interface {
	GetSnapshot(context.Context, string, string) (*jev.Snapshot, bool, error)
	ListPositions(ctx context.Context, limit int, cursor, ifNoneMatch, strategyVersion string) ([]jev.Position, string, string, bool, error)
	ListDecisions(ctx context.Context, limit int, cursor, ifNoneMatch, strategyVersion string) ([]jev.Decision, string, string, bool, error)
	ListTrades(ctx context.Context, limit int, cursor, ifNoneMatch, strategyVersion string) ([]jev.Trade, string, string, bool, error)
	GetEquityCurve(ctx context.Context, rangeValue, ifNoneMatch, strategyVersion string) ([]jev.EquityPoint, string, string, bool, error)
}

type JevServer struct {
	jevv1.UnimplementedJevServiceServer
	service jevAPI
}

func NewJevServer(service jevAPI) *JevServer {
	return &JevServer{service: service}
}

func (s *JevServer) GetSnapshot(
	ctx context.Context,
	request *jevv1.GetSnapshotRequest,
) (*jevv1.GetSnapshotResponse, error) {
	snapshot, notModified, err := s.service.GetSnapshot(ctx, request.GetIfNoneMatch(), request.GetStrategyVersion())
	if err != nil {
		return nil, mapJevError(err)
	}
	if notModified {
		return &jevv1.GetSnapshotResponse{NotModified: true, Etag: snapshot.ETag, SnapshotVersion: snapshotVersion(snapshot)}, nil
	}
	return snapshotProto(snapshot), nil
}

func (s *JevServer) ListPositions(
	ctx context.Context,
	request *jevv1.ListPositionsRequest,
) (*jevv1.ListPositionsResponse, error) {
	items, next, etag, notModified, err := s.service.ListPositions(
		ctx, int(request.GetLimit()), request.GetCursor(), request.GetIfNoneMatch(), request.GetStrategyVersion(),
	)
	if err != nil {
		return nil, mapJevError(err)
	}
	response := &jevv1.ListPositionsResponse{NextCursor: next, Etag: etag, NotModified: notModified}
	for _, item := range items {
		response.Items = append(response.Items, positionProto(item))
	}
	return response, nil
}

func (s *JevServer) ListDecisions(
	ctx context.Context,
	request *jevv1.ListDecisionsRequest,
) (*jevv1.ListDecisionsResponse, error) {
	items, next, etag, notModified, err := s.service.ListDecisions(
		ctx, int(request.GetLimit()), request.GetCursor(), request.GetIfNoneMatch(), request.GetStrategyVersion(),
	)
	if err != nil {
		return nil, mapJevError(err)
	}
	response := &jevv1.ListDecisionsResponse{NextCursor: next, Etag: etag, NotModified: notModified}
	for _, item := range items {
		response.Items = append(response.Items, decisionProto(item))
	}
	return response, nil
}

func (s *JevServer) ListTrades(
	ctx context.Context,
	request *jevv1.ListTradesRequest,
) (*jevv1.ListTradesResponse, error) {
	items, next, etag, notModified, err := s.service.ListTrades(
		ctx, int(request.GetLimit()), request.GetCursor(), request.GetIfNoneMatch(), request.GetStrategyVersion(),
	)
	if err != nil {
		return nil, mapJevError(err)
	}
	response := &jevv1.ListTradesResponse{NextCursor: next, Etag: etag, NotModified: notModified}
	for _, item := range items {
		response.Items = append(response.Items, tradeProto(item))
	}
	return response, nil
}

func (s *JevServer) GetEquityCurve(
	ctx context.Context,
	request *jevv1.GetEquityCurveRequest,
) (*jevv1.GetEquityCurveResponse, error) {
	rangeValue := strings.ToLower(strings.TrimSpace(request.GetRange()))
	if rangeValue == "" {
		rangeValue = "24h"
	}
	switch rangeValue {
	case "24h", "7d", "30d", "all":
	default:
		return nil, status.Error(codes.InvalidArgument, "range is invalid")
	}
	points, resolved, etag, notModified, err := s.service.GetEquityCurve(ctx, rangeValue, request.GetIfNoneMatch(), request.GetStrategyVersion())
	if err != nil {
		return nil, mapJevError(err)
	}
	response := &jevv1.GetEquityCurveResponse{NotModified: notModified, Etag: etag, Range: resolved}
	for _, point := range points {
		response.Points = append(response.Points, &jevv1.EquityPoint{
			Ts: protoTime(ptrTime(point.TS)), Equity: decimalString(point.Equity), NetPnl: decimalString(point.NetPnL),
		})
	}
	return response, nil
}

func snapshotProto(snapshot *jev.Snapshot) *jevv1.GetSnapshotResponse {
	return &jevv1.GetSnapshotResponse{
		Etag: snapshot.ETag, SnapshotVersion: snapshotVersion(snapshot), Status: snapshot.Status,
		DegradedReasons: snapshot.DegradedReasons, LastSuccessfulAt: protoTime(snapshot.LastSuccessfulAt),
		LastDecisionAt: protoTime(snapshot.LastDecisionAt), LastMarketDataAt: protoTime(snapshot.LastMarketDataAt),
		ServerTime: protoTime(ptrTime(snapshot.ServerTime)), Model: snapshot.Model,
		ModelRequested: snapshot.ModelRequested, PromptVersion: snapshot.PromptVersion,
		PolicyVersion: snapshot.PolicyVersion, Simulated: snapshot.Simulated, Equity: decimalString(snapshot.Equity),
		NetPnl: decimalString(snapshot.NetPnL), FundingPnl: decimalString(snapshot.FundingPnL),
		RealizedSpreadPnl:   decimalString(snapshot.RealizedSpreadPnL),
		UnrealizedSpreadPnl: decimalString(snapshot.UnrealizedSpreadPnL),
		CumulativeCost:      decimalString(snapshot.CumulativeCost), MaxDrawdown: decimalString(snapshot.MaxDrawdown),
		InitialEquity:         decimalString(snapshot.InitialEquity),
		PaperRoundTripCostBps: decimalString(snapshot.PaperRoundTripCostBPS),
		UsdtUsdcOneToOne:      snapshot.USDTUSDCOneToOne, SingleEntryEmptyReason: snapshot.SingleEntryEmptyReason,
		CrossEntryEmptyReason: snapshot.CrossEntryEmptyReason, StrategySingleStatus: snapshot.StrategySingleStatus,
		StrategyCrossStatus: snapshot.StrategyCrossStatus, OpenPositionCount: int32(snapshot.OpenPositionCount),
		EntryCandidateCount: int32(snapshot.EntryCandidateCount), TypesafeConfigured: snapshot.TypeSafeConfigured,
		SingleFilter: singleFilterProto(snapshot.SingleFilter), CrossFilter: crossFilterProto(snapshot.CrossFilter),
		DecisionJobs: int32(snapshot.DecisionJobs), TypesafeRequests: int32(snapshot.TypeSafeRequests),
		TypesafeSuccess: int32(snapshot.TypeSafeSuccess), TypesafeError: int32(snapshot.TypeSafeError),
		ModelStatus: snapshot.ModelStatus, LastModelCallAt: protoTime(snapshot.LastModelCallAt),
		StrategyVersion: snapshot.StrategyVersion, LifecycleEquity: decimalString(snapshot.LifecycleEquity),
		LifecycleNetPnl: decimalString(snapshot.LifecycleNetPnL), LifecycleFundingPnl: decimalString(snapshot.LifecycleFundingPnL),
		LifecycleRealizedSpreadPnl:   decimalString(snapshot.LifecycleRealizedSpreadPnL),
		LifecycleUnrealizedSpreadPnl: decimalString(snapshot.LifecycleUnrealizedSpreadPnL),
		LifecycleCumulativeCost:      decimalString(snapshot.LifecycleCumulativeCost),
		AvailableStrategyVersions:    snapshot.AvailableStrategyVersions,
	}
}

func singleFilterProto(stats jev.SingleFilterStats) *jevv1.JevSingleFilterStats {
	return &jevv1.JevSingleFilterStats{
		Input: int32(stats.Input), ContractPassed: int32(stats.ContractPassed),
		Positive_24HPassed: int32(stats.Positive24hPassed), FreshPassed: int32(stats.FreshPassed),
		TurnoverPassed: int32(stats.TurnoverPassed), SpotMatched: int32(stats.SpotMatched),
		BboFreshPassed: int32(stats.BBOFreshPassed), DepthPassed: int32(stats.DepthPassed),
		Final: int32(stats.Final), BasisForecastPassed: int32(stats.BasisForecastPassed),
	}
}

func crossFilterProto(stats jev.CrossFilterStats) *jevv1.JevCrossFilterStats {
	return &jevv1.JevCrossFilterStats{
		Input: int32(stats.Input), ContractPassed: int32(stats.ContractPassed),
		PositiveEdgePassed: int32(stats.PositiveEdgePassed), FreshPassed: int32(stats.FreshPassed),
		TurnoverPassed: int32(stats.TurnoverPassed), BboFreshPassed: int32(stats.BBOFreshPassed),
		DepthPassed: int32(stats.DepthPassed), Final: int32(stats.Final),
		BasisForecastPassed: int32(stats.BasisForecastPassed),
	}
}

func positionProto(item jev.Position) *jevv1.JevPosition {
	return &jevv1.JevPosition{
		Id: formatID(item.ID), Kind: item.Kind, Status: item.Status, CandidateKey: item.CandidateKey,
		BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset, GlobalSymbol: item.GlobalSymbol,
		LongExchange: item.LongExchange, LongSymbol: item.LongSymbol, LongProduct: item.LongProduct,
		ShortExchange: item.ShortExchange, ShortSymbol: item.ShortSymbol, ShortProduct: item.ShortProduct,
		SharedBaseQty: decimalString(item.SharedBaseQty), LongQty: decimalString(item.LongQty),
		ShortQty: decimalString(item.ShortQty), LongEntryPrice: decimalString(item.LongEntryPrice),
		ShortEntryPrice: decimalString(item.ShortEntryPrice), LongMarkPrice: decimalString(item.LongMarkPrice),
		ShortMarkPrice: decimalString(item.ShortMarkPrice), RealizedSpreadPnl: decimalString(item.RealizedSpreadPnL),
		UnrealizedSpreadPnl: decimalString(item.UnrealizedSpreadPnL), FundingPnl: decimalString(item.FundingPnL),
		CostPaid: decimalString(item.CostPaid), NetPnl: decimalString(item.NetPnL()),
		ExitPendingReason: item.ExitPendingReason, DegradedReason: item.DegradedReason,
		OpenedAt: protoTime(ptrTime(item.OpenedAt)), ClosedAt: protoTime(item.ClosedAt),
		LastMarkAt: protoTime(item.LastMarkAt), EntryMidBasisBps: optionalDecimalString(item.EntryMidBasisBPS),
		StrategyVersion: item.StrategyVersion, NetIfExit: optionalDecimalString(item.NetIfExit),
		NetIfExitAt: protoTime(item.NetIfExitAt), ExitConfirmCount: int32(item.ExitConfirmCount),
	}
}

func decisionProto(item jev.Decision) *jevv1.JevDecision {
	positionID := ""
	if item.PositionID != nil {
		positionID = formatID(*item.PositionID)
	}
	return &jevv1.JevDecision{
		Id: formatID(item.ID), PositionId: positionID, CandidateKey: item.CandidateKey, Kind: item.Kind,
		Decision: item.Decision, PolicyStatus: item.PolicyStatus, ExecutionStatus: item.ExecutionStatus,
		EnterProbability: decimalString(item.EnterProbability), ExitProbability: decimalString(item.ExitProbability),
		Confidence: decimalString(item.Confidence), ExpectedNetReturn_24H: optionalDecimalString(item.ExpectedNetReturn24h),
		Rationale: item.Rationale, PromptVersion: item.PromptVersion, PolicyVersion: item.PolicyVersion,
		ModelRequested: item.ModelRequested, ModelActual: item.ModelActual, ErrorMessage: item.ErrorMessage,
		DecisionAt: protoTime(ptrTime(item.DecisionAt)), ExecutionStartedAt: protoTime(item.ExecutionStartedAt),
		FillDeadline:             protoTime(item.FillDeadline),
		FundingReturn_24H:        optionalDecimalString(item.FundingReturn24h),
		HoldingHorizonNetReturn:  optionalDecimalString(item.HoldingHorizonNetReturn),
		ExpectedPaybackMinutes:   optionalDecimalString(item.ExpectedPaybackMinutes),
		CurrentMidBasisBps:       optionalDecimalString(item.CurrentMidBasisBPS),
		EntryMidBasisBps:         optionalDecimalString(item.EntryMidBasisBPS),
		BasisChangeSinceEntryBps: optionalDecimalString(item.BasisChangeSinceEntryBPS),
		ExpectedBasisReturn_24H:  optionalDecimalString(item.ExpectedBasisReturn24h),
		BasisReturnP10:           optionalDecimalString(item.BasisReturnP10),
		BasisProfitProbability:   optionalDecimalString(item.BasisProfitProbability),
		BasisForecastConfidence:  optionalDecimalString(item.BasisForecastConfidence),
		BasisSampleCount:         optionalInt32(item.BasisSampleCount),
		BasisModelVersion:        optionalStringValue(item.BasisModelVersion),
		StrategyVersion:          item.StrategyVersion,
		ParseErrorType:           item.ParseErrorType,
	}
}

func tradeProto(item jev.Trade) *jevv1.JevTrade {
	decisionID := ""
	if item.DecisionID != nil {
		decisionID = formatID(*item.DecisionID)
	}
	return &jevv1.JevTrade{
		Id: formatID(item.ID), PositionId: formatID(item.PositionID), DecisionId: decisionID, Side: item.Side,
		SharedBaseQty: decimalString(item.SharedBaseQty), LongFillPrice: decimalString(item.LongFillPrice),
		ShortFillPrice: decimalString(item.ShortFillPrice), Notional: decimalString(item.Notional),
		Cost: decimalString(item.Cost), FilledAt: protoTime(ptrTime(item.FilledAt)),
		StrategyVersion: item.StrategyVersion, BaseAsset: item.BaseAsset, Kind: item.Kind,
		LongExchange: item.LongExchange, LongSymbol: item.LongSymbol, LongProduct: item.LongProduct,
		ShortExchange: item.ShortExchange, ShortSymbol: item.ShortSymbol, ShortProduct: item.ShortProduct,
	}
}

func optionalDecimalString(value *d.Decimal) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(value.String())
}

func optionalInt32(value *int) *int32 {
	if value == nil {
		return nil
	}
	out := int32(*value)
	return &out
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func formatID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func protoTime(value *time.Time) *timestamppb.Timestamp {
	if value == nil || value.IsZero() {
		return nil
	}
	return timestamppb.New(value.UTC())
}

func ptrTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func snapshotVersion(snapshot *jev.Snapshot) string {
	if snapshot == nil {
		return ""
	}
	return strings.Trim(snapshot.ETag, `"`)
}

func mapJevError(err error) error {
	switch {
	case errors.Is(err, jev.ErrNoSnapshot), errors.Is(err, jev.ErrUnavailable):
		return status.Error(codes.FailedPrecondition, "jev snapshot unavailable")
	case errors.Is(err, jev.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, "invalid argument")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.Error(codes.DeadlineExceeded, "jev query timed out")
	default:
		return status.Error(codes.Unavailable, "jev query failed")
	}
}
