package ai

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	fundingv1 "selfquant/backend/gen/funding/v1"
)

var assetTokenPattern = regexp.MustCompile(`\b[A-Z0-9]{2,12}\b`)

type ContextProvider interface {
	Name() string
	Build(context.Context, string) (string, error)
}

type fundingSource interface {
	ListFundingRates(
		context.Context,
		*fundingv1.ListFundingRatesRequest,
		...grpc.CallOption,
	) (*fundingv1.ListFundingRatesResponse, error)
}

type FundingContextProvider struct {
	client        fundingSource
	maxSymbolRows int
	maxRows       int
	minTurnover   float64
	maxChars      int
}

func NewFundingContextProvider(
	client fundingSource,
	maxSymbolRows int,
	maxRows int,
	minTurnover float64,
	maxChars int,
) *FundingContextProvider {
	return &FundingContextProvider{
		client: client, maxSymbolRows: maxSymbolRows, maxRows: maxRows,
		minTurnover: minTurnover, maxChars: maxChars,
	}
}

func (p *FundingContextProvider) Name() string {
	return "funding"
}

func (p *FundingContextProvider) Build(ctx context.Context, question string) (string, error) {
	response, err := p.client.ListFundingRates(ctx, &fundingv1.ListFundingRatesRequest{})
	if err != nil {
		return "", fmt.Errorf("load funding context: %w", err)
	}
	fresh := make([]*fundingv1.FundingRate, 0, len(response.GetItems()))
	assets := make(map[string]struct{})
	for _, item := range response.GetItems() {
		if item == nil || item.GetStale() {
			continue
		}
		fresh = append(fresh, item)
		assets[strings.ToUpper(strings.TrimSpace(item.GetBaseAsset()))] = struct{}{}
	}
	matchedAssets := questionAssets(question, assets)
	selected := selectFundingRows(
		fresh, matchedAssets, p.maxSymbolRows, p.maxRows, p.minTurnover,
	)
	serverTime := ""
	if response.GetServerTime() != nil {
		serverTime = response.GetServerTime().AsTime().UTC().Format(time.RFC3339)
	}
	header := fmt.Sprintf(
		"只读资金费快照；snapshot_version=%s；server_time=%s。"+
			"仅供投研参考，不构成交易建议，不允许执行交易。\n",
		response.GetSnapshotVersion(), serverTime,
	)
	if len(selected) == 0 {
		return truncateContext(
			header+"当前无新鲜且满足筛选条件的资金费数据，请明确告知用户此限制，不要编造实时数据。",
			p.maxChars,
		), nil
	}
	var builder strings.Builder
	builder.WriteString(header)
	for _, item := range selected {
		line := fmt.Sprintf(
			"exchange=%s symbol=%s base=%s funding_rate=%s annualized_rate=%s "+
				"next_funding_at=%s turnover_24h_usd=%s source_updated_at=%s\n",
			item.GetExchange(), item.GetGlobalSymbol(), item.GetBaseAsset(),
			item.GetFundingRate(), item.GetAnnualizedRate(),
			protoTime(item.GetNextFundingAt()), item.GetTurnover_24HUsd(),
			protoTime(item.GetSourceUpdatedAt()),
		)
		if builder.Len()+len(line) > p.maxChars {
			break
		}
		builder.WriteString(line)
	}
	return truncateContext(builder.String(), p.maxChars), nil
}

func questionAssets(question string, available map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for _, token := range assetTokenPattern.FindAllString(strings.ToUpper(question), -1) {
		if _, ok := available[token]; ok {
			result[token] = struct{}{}
		}
	}
	return result
}

func selectFundingRows(
	items []*fundingv1.FundingRate,
	matchedAssets map[string]struct{},
	maxSymbolRows int,
	maxRows int,
	minTurnover float64,
) []*fundingv1.FundingRate {
	selected := make([]*fundingv1.FundingRate, 0, len(items))
	for _, item := range items {
		if len(matchedAssets) > 0 {
			if _, ok := matchedAssets[strings.ToUpper(item.GetBaseAsset())]; !ok {
				continue
			}
		} else if decimalFloat(item.GetTurnover_24HUsd()) < minTurnover {
			continue
		}
		selected = append(selected, item)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		return math.Abs(decimalFloat(selected[i].GetFundingRate())) >
			math.Abs(decimalFloat(selected[j].GetFundingRate()))
	})
	limit := maxRows
	if len(matchedAssets) > 0 {
		limit = maxSymbolRows
	}
	if len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}

func decimalFloat(value string) float64 {
	parsed, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return parsed
}

func protoTime(value *timestamppb.Timestamp) string {
	if value == nil {
		return ""
	}
	return value.AsTime().UTC().Format(time.RFC3339)
}

func truncateContext(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}
