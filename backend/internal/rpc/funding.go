package rpc

import (
	"context"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	fundingv1 "selfquant/backend/gen/funding/v1"
	"selfquant/backend/internal/funding"
	"selfquant/backend/internal/funding/ranking"
)

type historyRepository interface {
	ListHistory(context.Context, string, string, int) ([]funding.HistoryPoint, error)
}

type FundingServer struct {
	fundingv1.UnimplementedFundingServiceServer
	snapshots  *funding.SnapshotStore
	repository historyRepository
	staleAfter time.Duration
	rankings   *ranking.SnapshotStore
}

func NewFundingServer(
	snapshots *funding.SnapshotStore,
	repository historyRepository,
	staleAfter time.Duration,
	rankings ...*ranking.SnapshotStore,
) *FundingServer {
	var rankingStore *ranking.SnapshotStore
	if len(rankings) > 0 {
		rankingStore = rankings[0]
	}
	return &FundingServer{
		snapshots: snapshots, repository: repository, staleAfter: staleAfter,
		rankings: rankingStore,
	}
}

func (s *FundingServer) ListFundingRates(_ context.Context, _ *fundingv1.ListFundingRatesRequest) (*fundingv1.ListFundingRatesResponse, error) {
	snapshot := s.snapshots.Get()
	now := time.Now().UTC()
	response := &fundingv1.ListFundingRatesResponse{
		Items:           make([]*fundingv1.FundingRate, 0, len(snapshot.Rates)),
		Total:           int32(snapshot.Total),
		SnapshotVersion: snapshot.Version,
		ServerTime:      timestamppb.New(now),
	}
	for _, item := range snapshot.Rates {
		rate := &fundingv1.FundingRate{
			Exchange:               item.Exchange,
			ExchangeSymbol:         item.ExchangeSymbol,
			GlobalSymbol:           item.GlobalSymbol,
			BaseAsset:              item.BaseAsset,
			QuoteAsset:             item.QuoteAsset,
			PositionQuantity:       decimal(item.PositionQuantity),
			PositionNotionalUsd:    decimal(item.PositionNotionalUSD),
			Volume_24HBase:         decimal(item.Volume24hBase),
			Turnover_24HUsd:        decimal(item.Turnover24hUSD),
			FundingRate:            decimal(item.Rate),
			AnnualizedRate:         decimal(item.AnnualizedRate),
			FundingIntervalSeconds: int32(item.IntervalHours * 3600),
			NextFundingAt:          timestamppb.New(item.FundingTime),
			MarkPrice:              decimal(item.MarkPrice),
			IndexPrice:             decimal(item.IndexPrice),
			LastPrice:              decimal(item.LastPrice),
			PriceChange_24H:        decimal(item.PriceChange24h),
			Cumulative_24H:         decimal(item.Cumulative24h),
			Cumulative_7D:          decimal(item.Cumulative7d),
			SourceUpdatedAt:        timestamppb.New(item.SourceUpdatedAt),
			Stale:                  item.SourceUpdatedAt.IsZero() || now.Sub(item.SourceUpdatedAt) > s.staleAfter,
		}
		if item.NextRate != nil {
			value := decimal(*item.NextRate)
			rate.NextFundingRate = &value
		}
		response.Items = append(response.Items, rate)
	}
	return response, nil
}

func (s *FundingServer) ListFundingSpreads(
	_ context.Context,
	_ *fundingv1.ListFundingSpreadsRequest,
) (*fundingv1.ListFundingSpreadsResponse, error) {
	snapshot := s.snapshots.Get()
	now := time.Now().UTC()
	spreads := funding.BuildSpreads(snapshot.Rates)
	response := &fundingv1.ListFundingSpreadsResponse{
		Items:           make([]*fundingv1.FundingSpread, 0, len(spreads)),
		Total:           int32(len(spreads)),
		SnapshotVersion: snapshot.Version,
		ServerTime:      timestamppb.New(now),
	}
	for _, item := range spreads {
		longLeg := spreadLegToProto(item.Long, now, s.staleAfter)
		shortLeg := spreadLegToProto(item.Short, now, s.staleAfter)
		response.Items = append(response.Items, &fundingv1.FundingSpread{
			GlobalSymbol:           item.GlobalSymbol,
			BaseAsset:              item.BaseAsset,
			QuoteAsset:             item.QuoteAsset,
			LongLeg:                longLeg,
			ShortLeg:               shortLeg,
			SingleSpreadAnnualized: decimal(item.SingleAnnualized),
			Spread_24HAnnualized:   decimal(item.Annualized24h),
			Spread_7DAnnualized:    decimal(item.Annualized7d),
			MinPositionNotionalUsd: decimal(item.MinPositionNotionalUSD),
			MinTurnover_24HUsd:     decimal(item.MinTurnover24hUSD),
			UpdatedAt:              timestamppb.New(item.UpdatedAt),
			Stale:                  longLeg.Stale || shortLeg.Stale,
		})
	}
	return response, nil
}

func spreadLegToProto(
	item funding.SpreadLeg,
	now time.Time,
	staleAfter time.Duration,
) *fundingv1.FundingSpreadLeg {
	stale := item.SourceUpdatedAt.IsZero() || now.Sub(item.SourceUpdatedAt) > staleAfter
	return &fundingv1.FundingSpreadLeg{
		Exchange:               item.Exchange,
		ExchangeSymbol:         item.ExchangeSymbol,
		EffectiveFundingRate:   decimal(item.EffectiveRate),
		FundingIntervalSeconds: int32(item.IntervalHours * 3600),
		NextFundingAt:          timestamppb.New(item.NextFundingAt),
		PositionNotionalUsd:    decimal(item.PositionNotionalUSD),
		Turnover_24HUsd:        decimal(item.Turnover24hUSD),
		SourceUpdatedAt:        timestamppb.New(item.SourceUpdatedAt),
		LastPrice:              decimal(item.LastPrice),
		Stale:                  stale,
	}
}

func (s *FundingServer) ListFundingOpportunities(
	ctx context.Context,
	request *fundingv1.ListFundingOpportunitiesRequest,
) (*fundingv1.ListFundingOpportunitiesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	period, err := ranking.ParsePeriod(request.GetPeriod())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "period must be one of 1h, 4h, 8h, 24h")
	}
	minNotional, err := nonNegativeDecimal(request.GetMinLegNotionalUsd())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "min_leg_notional_usd must be non-negative")
	}
	minVolume, err := nonNegativeDecimal(request.GetMinLegVolume_24HUsd())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "min_leg_volume_24h_usd must be non-negative")
	}
	limit := int(request.GetLimit())
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	if s.rankings == nil {
		return &fundingv1.ListFundingOpportunitiesResponse{
			ServerTime: timestamppb.New(now), Status: string(ranking.SnapshotUnavailable), Stale: true,
		}, nil
	}
	snapshot := s.rankings.View(period)
	response := &fundingv1.ListFundingOpportunitiesResponse{
		Items:           make([]*fundingv1.FundingOpportunityRanking, 0, min(limit, len(snapshot.Items))),
		SnapshotVersion: snapshot.Version, Generation: snapshot.Generation,
		ServerTime: timestamppb.New(now), Status: string(snapshot.Status),
		Stale: snapshot.Stale || snapshot.Status != ranking.SnapshotReady,
	}
	if !snapshot.CalculatedAt.IsZero() {
		response.CalculatedAt = timestamppb.New(snapshot.CalculatedAt)
	}
	if !snapshot.LastSuccessfulAt.IsZero() {
		response.LastSuccessfulAt = timestamppb.New(snapshot.LastSuccessfulAt)
	}
	if !snapshot.DataThrough.IsZero() {
		response.DataThrough = timestamppb.New(snapshot.DataThrough)
	}
	for index, item := range snapshot.Items {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		if item.MinPositionNotionalUSD < minNotional || item.MinTurnover24hUSD < minVolume {
			continue
		}
		response.Total++
		if len(response.Items) < limit {
			rank := item.Rank
			if rank <= 0 {
				rank = index + 1
			}
			protoItem := rankingToProto(rank, item, now, s.staleAfter, response.Stale)
			response.Items = append(response.Items, protoItem)
			response.Stale = response.Stale || protoItem.Stale
		}
	}
	if response.Stale && response.Status == string(ranking.SnapshotReady) {
		response.Status = string(ranking.SnapshotStale)
	}
	return response, nil
}

func rankingToProto(
	rank int,
	item ranking.Opportunity,
	now time.Time,
	staleAfter time.Duration,
	snapshotStale bool,
) *fundingv1.FundingOpportunityRanking {
	longLeg := rankingLegToProto(item.Long, now, staleAfter)
	shortLeg := rankingLegToProto(item.Short, now, staleAfter)
	return &fundingv1.FundingOpportunityRanking{
		Rank: int32(rank), GlobalSymbol: item.GlobalSymbol,
		BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset, Period: string(item.Period),
		LongLeg:                    longLeg,
		ShortLeg:                   shortLeg,
		CurrentMidSpreadBps:        decimal(item.CurrentMidSpreadBPS),
		CurrentExecutableSpreadBps: decimal(item.CurrentExecutableSpreadBPS),
		TargetSpreadBps:            decimal(item.TargetSpreadBPS),
		PeriodExpectedReturn:       decimal(item.PeriodExpectedReturn),
		FundingExpectedAnnualized:  decimal(item.FundingExpectedAnnualized),
		SpreadExpectedAnnualized:   decimal(item.SpreadExpectedAnnualized),
		CombinedExpectedAnnualized: decimal(item.CombinedExpectedAnnualized),
		FirstPassageProbability:    decimal(item.FirstPassageProbability),
		ProfitProbability:          decimal(item.ProfitProbability),
		ExpectedExitMinutes:        decimal(item.ExpectedExitMinutes),
		P5Return:                   decimal(item.P5Return),
		MinPositionNotionalUsd:     decimal(item.MinPositionNotionalUSD),
		MinTurnover_24HUsd:         decimal(item.MinTurnover24hUSD),
		Coverage:                   decimal(item.Coverage), Confidence: decimal(item.Confidence),
		ModelState: item.ModelState, UpdatedAt: timestamppb.New(item.UpdatedAt),
		Stale: snapshotStale || item.Stale || longLeg.Stale || shortLeg.Stale,
	}
}

func rankingLegToProto(
	item ranking.Leg,
	now time.Time,
	staleAfter time.Duration,
) *fundingv1.FundingSpreadLeg {
	return &fundingv1.FundingSpreadLeg{
		Exchange: item.Exchange, ExchangeSymbol: item.ExchangeSymbol,
		EffectiveFundingRate:   decimal(item.EffectiveRate),
		FundingIntervalSeconds: int32(item.IntervalHours * 3600),
		NextFundingAt:          timestamppb.New(item.NextFundingAt),
		PositionNotionalUsd:    decimal(item.PositionNotionalUSD),
		Turnover_24HUsd:        decimal(item.Turnover24hUSD),
		SourceUpdatedAt:        timestamppb.New(item.SourceUpdatedAt),
		LastPrice:              decimal(item.LastPrice),
		Stale:                  item.SourceUpdatedAt.IsZero() || now.Sub(item.SourceUpdatedAt) > staleAfter,
	}
}

func nonNegativeDecimal(value string) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		return 0, status.Error(codes.InvalidArgument, "invalid non-negative decimal")
	}
	return parsed, nil
}

func (s *FundingServer) GetFundingHistory(
	ctx context.Context,
	request *fundingv1.GetFundingHistoryRequest,
) (*fundingv1.GetFundingHistoryResponse, error) {
	exchangeName := strings.TrimSpace(request.GetExchange())
	exchangeSymbol := strings.TrimSpace(request.GetExchangeSymbol())
	if exchangeName == "" || exchangeSymbol == "" {
		return nil, status.Error(codes.InvalidArgument, "exchange and exchange_symbol are required")
	}
	limit := int(request.GetLimit())
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	points, err := s.repository.ListHistory(ctx, exchangeName, exchangeSymbol, limit)
	if err != nil {
		return nil, status.Error(codes.Internal, "list funding history")
	}
	response := &fundingv1.GetFundingHistoryResponse{
		Items: make([]*fundingv1.FundingHistoryPoint, 0, len(points)),
	}
	for _, point := range points {
		response.Items = append(response.Items, &fundingv1.FundingHistoryPoint{
			Rate: decimal(point.Rate), SettledAt: timestamppb.New(point.SettledAt),
		})
	}
	return response, nil
}

func decimal(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
