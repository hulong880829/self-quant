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

type hyperliquidSpotMeta struct {
	Tokens []struct {
		Name       string `json:"name"`
		Index      int    `json:"index"`
		SzDecimals int    `json:"szDecimals"`
	} `json:"tokens"`
	Universe []struct {
		Name        string `json:"name"`
		Tokens      []int  `json:"tokens"`
		IsCanonical bool   `json:"isCanonical"`
	} `json:"universe"`
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

func (h *Hyperliquid) syncSpotInstruments(ctx context.Context) ([]Instrument, error) {
	var raw []json.RawMessage
	if err := h.client.post(ctx, "/info", `{"type":"spotMetaAndAssetCtxs"}`, &raw); err != nil {
		return nil, err
	}
	if len(raw) != 2 {
		return nil, fmt.Errorf("hyperliquid spotMetaAndAssetCtxs: expected 2 elements")
	}
	var meta hyperliquidSpotMeta
	if err := json.Unmarshal(raw[0], &meta); err != nil {
		return nil, err
	}
	return parseHyperliquidSpotInstruments(meta), nil
}

func parseHyperliquidSpotInstruments(meta hyperliquidSpotMeta) []Instrument {
	tokens := make(map[int]struct {
		name string
		step float64
	}, len(meta.Tokens))
	for _, token := range meta.Tokens {
		step := 1.0
		for range token.SzDecimals {
			step /= 10
		}
		tokens[token.Index] = struct {
			name string
			step float64
		}{name: token.Name, step: step}
	}
	result := make([]Instrument, 0, len(meta.Universe))
	for _, pair := range meta.Universe {
		if len(pair.Tokens) != 2 {
			continue
		}
		base, baseOK := tokens[pair.Tokens[0]]
		quote, quoteOK := tokens[pair.Tokens[1]]
		if !baseOK || !quoteOK {
			continue
		}
		metadata, _ := json.Marshal(pair)
		result = append(result, Instrument{
			Exchange: "hyperliquid", ExchangeSymbol: pair.Name,
			BaseAsset: base.name, QuoteAsset: quote.name,
			GlobalSymbol: GlobalSymbol(base.name, quote.name),
			SettleAsset:  quote.name, ContractType: ContractTypeSpot,
			Status: "active", ContractSize: 1, QuantityStep: base.step,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (h *Hyperliquid) SyncInstruments(ctx context.Context, contractType string) ([]Instrument, error) {
	if contractType == ContractTypeSpot {
		return h.syncSpotInstruments(ctx)
	}
	if contractType != ContractTypePerpetual {
		return nil, fmt.Errorf("hyperliquid: unsupported contract type %q", contractType)
	}
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
	wanted := clampLimit(limit, 10000)
	result := make([]FundingRate, 0, wanted)
	start := since.UnixMilli()
	if since.IsZero() {
		start = time.Now().AddDate(-1, 0, 0).UnixMilli()
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
