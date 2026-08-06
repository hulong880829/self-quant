package rpc

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	fundingv1 "selfquant/backend/gen/funding/v1"
	"selfquant/backend/internal/funding"
)

type FundingServer struct {
	fundingv1.UnimplementedFundingServiceServer
	repository *funding.Repository
}

func NewFundingServer(repository *funding.Repository) *FundingServer {
	return &FundingServer{repository: repository}
}

func (s *FundingServer) ListFundingRates(ctx context.Context, _ *fundingv1.ListFundingRatesRequest) (*fundingv1.ListFundingRatesResponse, error) {
	rates, total, err := s.repository.List(ctx, funding.Query{
		Limit: 10000,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	response := &fundingv1.ListFundingRatesResponse{
		Items:           make([]*fundingv1.FundingRate, 0, len(rates)),
		Total:           int32(total),
		SnapshotVersion: strconv.FormatInt(now.UnixMilli(), 10),
		ServerTime:      timestamppb.New(now),
	}
	for _, item := range rates {
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
			Stale:                  item.SourceUpdatedAt.IsZero() || now.Sub(item.SourceUpdatedAt) > 2*time.Minute,
		}
		if item.NextRate != nil {
			value := decimal(*item.NextRate)
			rate.NextFundingRate = &value
		}
		for _, point := range item.History {
			rate.History = append(rate.History, &fundingv1.FundingHistoryPoint{
				Rate:      decimal(point.Rate),
				SettledAt: timestamppb.New(point.SettledAt),
			})
		}
		response.Items = append(response.Items, rate)
	}
	return response, nil
}

func decimal(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
