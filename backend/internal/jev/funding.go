package jev

import (
	"context"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	fundingv1 "selfquant/backend/gen/funding/v1"
)

type FundingClient struct {
	api fundingv1.FundingServiceClient
}

func NewFundingClient(api fundingv1.FundingServiceClient) *FundingClient {
	return &FundingClient{api: api}
}

func (c *FundingClient) ListRates(ctx context.Context) ([]Rate, error) {
	response, err := c.api.ListFundingRates(ctx, &fundingv1.ListFundingRatesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]Rate, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		out = append(out, protoRate(item))
	}
	return out, nil
}

func (c *FundingClient) ListHistory(ctx context.Context, exchange, symbol string, limit int) ([]HistoryPoint, error) {
	response, err := c.api.GetFundingHistory(ctx, &fundingv1.GetFundingHistoryRequest{
		Exchange: exchange, ExchangeSymbol: symbol, Limit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]HistoryPoint, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		point := HistoryPoint{Rate: parseDecimalString(item.GetRate())}
		if ts := item.GetSettledAt(); ts != nil && ts.IsValid() {
			point.SettledAt = ts.AsTime().UTC()
		}
		out = append(out, point)
	}
	return out, nil
}

func protoRate(item *fundingv1.FundingRate) Rate {
	rate := Rate{
		Exchange:               strings.ToLower(item.GetExchange()),
		ExchangeSymbol:         strings.ToUpper(item.GetExchangeSymbol()),
		GlobalSymbol:           item.GetGlobalSymbol(),
		BaseAsset:              strings.ToUpper(item.GetBaseAsset()),
		QuoteAsset:             strings.ToUpper(item.GetQuoteAsset()),
		FundingRate:            parseDecimalString(item.GetFundingRate()),
		Cumulative24h:          parseDecimalString(item.GetCumulative_24H()),
		AnnualizedRate:         parseDecimalString(item.GetAnnualizedRate()),
		Turnover24hUSD:         parseDecimalString(item.GetTurnover_24HUsd()),
		PositionNotionalUSD:    parseDecimalString(item.GetPositionNotionalUsd()),
		FundingIntervalSeconds: item.GetFundingIntervalSeconds(),
		History24hComplete:     item.History_24HComplete != nil && item.GetHistory_24HComplete(),
		VenueContractType:      item.GetVenueContractType(),
		Stale:                  item.GetStale(),
	}
	if ts := item.GetSourceUpdatedAt(); ts != nil && ts.IsValid() {
		rate.SourceUpdatedAt = ts.AsTime().UTC()
	}
	if ts := item.GetNextFundingAt(); ts != nil && ts.IsValid() {
		rate.NextFundingAt = ts.AsTime().UTC()
	}
	if item.NextFundingRate != nil {
		value := parseDecimalString(item.GetNextFundingRate())
		rate.NextFundingRate = &value
	}
	return rate
}

func parseDecimalString(value string) decimal.Decimal {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return decimal.Zero
	}
	return parsed
}

func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

func DownsampleEquity(points []EquityPoint, maxPoints int) []EquityPoint {
	if maxPoints <= 0 || len(points) <= maxPoints {
		return points
	}
	stride := (len(points) + maxPoints - 1) / maxPoints
	out := make([]EquityPoint, 0, maxPoints)
	for i := 0; i < len(points); i += stride {
		out = append(out, points[i])
	}
	last := points[len(points)-1]
	if len(out) == 0 || !out[len(out)-1].TS.Equal(last.TS) {
		out = append(out, last)
	}
	return out
}

func EquityFrom(rangeValue string, now time.Time) *time.Time {
	switch strings.ToLower(strings.TrimSpace(rangeValue)) {
	case "24h":
		from := now.Add(-24 * time.Hour)
		return &from
	case "7d":
		from := now.Add(-7 * 24 * time.Hour)
		return &from
	case "30d":
		from := now.Add(-30 * 24 * time.Hour)
		return &from
	default:
		return nil
	}
}
