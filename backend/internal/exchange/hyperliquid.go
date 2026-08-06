package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type Hyperliquid struct{ client client }

func NewHyperliquid(timeout time.Duration) *Hyperliquid {
	return &Hyperliquid{client: newClient("https://api.hyperliquid.xyz", timeout)}
}
func (h *Hyperliquid) Name() string { return "hyperliquid" }

type hyperliquidMeta struct {
	Universe []struct {
		Name       string `json:"name"`
		IsDelisted bool   `json:"isDelisted"`
		SzDecimals int    `json:"szDecimals"`
	} `json:"universe"`
}
type hyperliquidContext struct {
	Funding      string `json:"funding"`
	MarkPx       string `json:"markPx"`
	OraclePx     string `json:"oraclePx"`
	OpenInterest string `json:"openInterest"`
	DayNtlVlm    string `json:"dayNtlVlm"`
	PrevDayPx    string `json:"prevDayPx"`
}

func (h *Hyperliquid) metaAndContexts(ctx context.Context) (hyperliquidMeta, []hyperliquidContext, error) {
	var raw []json.RawMessage
	if err := h.client.post(ctx, "/info", `{"type":"metaAndAssetCtxs"}`, &raw); err != nil {
		return hyperliquidMeta{}, nil, err
	}
	if len(raw) != 2 {
		return hyperliquidMeta{}, nil, fmt.Errorf("hyperliquid metaAndAssetCtxs: expected 2 elements")
	}
	var meta hyperliquidMeta
	var contexts []hyperliquidContext
	if err := json.Unmarshal(raw[0], &meta); err != nil {
		return meta, nil, err
	}
	if err := json.Unmarshal(raw[1], &contexts); err != nil {
		return meta, nil, err
	}
	return meta, contexts, nil
}

func parseHyperliquidInstruments(meta hyperliquidMeta) []Instrument {
	result := make([]Instrument, 0, len(meta.Universe))
	for _, item := range meta.Universe {
		if item.IsDelisted {
			continue
		}
		step := 1.0
		for range item.SzDecimals {
			step /= 10
		}
		metadata, _ := json.Marshal(item)
		result = append(result, Instrument{
			Exchange: "hyperliquid", ExchangeSymbol: item.Name,
			BaseAsset: item.Name, QuoteAsset: "USDC",
			GlobalSymbol: GlobalSymbol(item.Name, "USDC"), IntervalHours: 1,
			SettleAsset: "USDC", ContractType: "perpetual", Status: "active",
			ContractSize: 1, QuantityStep: step, Metadata: metadata,
			SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (h *Hyperliquid) SyncInstruments(ctx context.Context) ([]Instrument, error) {
	meta, _, err := h.metaAndContexts(ctx)
	if err != nil {
		return nil, err
	}
	return parseHyperliquidInstruments(meta), nil
}

func parseHyperliquidCurrent(meta hyperliquidMeta, contexts []hyperliquidContext, now time.Time) ([]FundingRate, error) {
	result := make([]FundingRate, 0, len(contexts))
	next := now.UTC().Truncate(time.Hour).Add(time.Hour)
	for i, item := range meta.Universe {
		if i >= len(contexts) || item.IsDelisted {
			continue
		}
		rate, err := parseFloat(contexts[i].Funding)
		if err != nil {
			return nil, err
		}
		markPrice, _ := parseFloat(contexts[i].MarkPx)
		indexPrice, _ := parseFloat(contexts[i].OraclePx)
		oi, _ := parseFloat(contexts[i].OpenInterest)
		turnover, _ := parseFloat(contexts[i].DayNtlVlm)
		previous, _ := parseFloat(contexts[i].PrevDayPx)
		var change float64
		if previous != 0 {
			change = (markPrice - previous) / previous
		}
		result = append(result, FundingRate{
			Exchange: "hyperliquid", ExchangeSymbol: item.Name, Rate: rate,
			FundingTime: next, IntervalHours: 1, MarkPrice: markPrice,
			IndexPrice: indexPrice, LastPrice: markPrice,
			OpenInterestContracts: oi, OpenInterestBase: oi,
			OpenInterestNotionalUSD: oi * markPrice, Turnover24hUSD: turnover,
			PriceChange24h: change, SourceUpdatedAt: now.UTC(),
		})
	}
	return result, nil
}

func (h *Hyperliquid) FetchCurrent(ctx context.Context, _ []Instrument) ([]FundingRate, error) {
	meta, contexts, err := h.metaAndContexts(ctx)
	if err != nil {
		return nil, err
	}
	return parseHyperliquidCurrent(meta, contexts, time.Now())
}

type hyperliquidHistory struct {
	Coin        string `json:"coin"`
	FundingRate string `json:"fundingRate"`
	Time        int64  `json:"time"`
}

func (h *Hyperliquid) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 5000)
	result := make([]FundingRate, 0, wanted)
	start := since.UnixMilli()
	if since.IsZero() {
		start = time.Now().AddDate(0, -6, 0).UnixMilli()
	}
	for len(result) < wanted {
		body := fmt.Sprintf(`{"type":"fundingHistory","coin":%q,"startTime":%d,"endTime":%d}`, instrument.ExchangeSymbol, start, time.Now().UnixMilli())
		var page []hyperliquidHistory
		if err := h.client.post(ctx, "/info", body, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			rate, err := strconv.ParseFloat(item.FundingRate, 64)
			if err != nil {
				return nil, err
			}
			result = append(result, FundingRate{
				Exchange: "hyperliquid", ExchangeSymbol: item.Coin, Rate: rate,
				FundingTime: milliseconds(item.Time), Settled: true, IntervalHours: 1,
				SourceUpdatedAt: milliseconds(item.Time),
			})
			if len(result) == wanted {
				break
			}
		}
		if len(page) < 500 {
			break
		}
		start = page[len(page)-1].Time + 1
	}
	return result, nil
}
