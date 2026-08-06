package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type Bybit struct{ client client }

func NewBybit(timeout time.Duration) *Bybit {
	return &Bybit{client: newClient("https://api.bybit.com", timeout)}
}
func (b *Bybit) Name() string { return "bybit" }

type bybitEnvelope[T any] struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		List           []T    `json:"list"`
		NextPageCursor string `json:"nextPageCursor"`
	} `json:"result"`
}

type bybitString string

func (value *bybitString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*value = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*value = bybitString(text)
		return nil
	}
	*value = bybitString(data)
	return nil
}

type bybitInstrument struct {
	Symbol, ContractType, Status, BaseCoin, QuoteCoin string
	FundingInterval                                   bybitString
	SettleCoin                                        string
	PriceFilter                                       struct {
		TickSize string `json:"tickSize"`
	} `json:"priceFilter"`
	LotSizeFilter struct {
		QtyStep string `json:"qtyStep"`
	} `json:"lotSizeFilter"`
}

func parseBybitInstruments(items []bybitInstrument) []Instrument {
	result := make([]Instrument, 0, len(items))
	for _, item := range items {
		if item.Status != "Trading" || item.ContractType != "LinearPerpetual" {
			continue
		}
		minutes, _ := strconv.ParseFloat(string(item.FundingInterval), 64)
		if minutes <= 0 {
			minutes = 480
		}
		tick, _ := parseFloat(item.PriceFilter.TickSize)
		step, _ := parseFloat(item.LotSizeFilter.QtyStep)
		metadata, _ := json.Marshal(item)
		result = append(result, Instrument{
			Exchange: "bybit", ExchangeSymbol: item.Symbol,
			BaseAsset: item.BaseCoin, QuoteAsset: item.QuoteCoin,
			GlobalSymbol:  GlobalSymbol(item.BaseCoin, item.QuoteCoin),
			IntervalHours: minutes / 60, SettleAsset: item.SettleCoin,
			ContractType: "perpetual", Status: "active", ContractSize: 1,
			PriceTick: tick, QuantityStep: step, Metadata: metadata,
			SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (b *Bybit) SyncInstruments(ctx context.Context) ([]Instrument, error) {
	var all []Instrument
	cursor := ""
	for {
		query := url.Values{"category": {"linear"}, "limit": {"1000"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var payload bybitEnvelope[bybitInstrument]
		if err := b.client.get(ctx, "/v5/market/instruments-info", query, &payload); err != nil {
			return nil, err
		}
		if payload.RetCode != 0 {
			return nil, fmt.Errorf("bybit: %s", payload.RetMsg)
		}
		all = append(all, parseBybitInstruments(payload.Result.List)...)
		if payload.Result.NextPageCursor == "" || payload.Result.NextPageCursor == cursor {
			break
		}
		cursor = payload.Result.NextPageCursor
	}
	return all, nil
}

type bybitTicker struct {
	Symbol, FundingRate, NextFundingTime                    string
	LastPrice, MarkPrice, IndexPrice, Price24hPcnt          string
	OpenInterest, OpenInterestValue, Turnover24h, Volume24h string
	FundingIntervalHour                                     bybitString
}

func parseBybitCurrent(items []bybitTicker, intervals map[string]float64) ([]FundingRate, error) {
	result := make([]FundingRate, 0, len(items))
	for _, item := range items {
		if item.FundingRate == "" || item.NextFundingTime == "" {
			continue
		}
		rate, err := parseFloat(item.FundingRate)
		if err != nil {
			return nil, err
		}
		ts, err := strconv.ParseInt(item.NextFundingTime, 10, 64)
		if err != nil {
			return nil, err
		}
		interval := intervals[item.Symbol]
		if parsed, parseErr := parseFloat(string(item.FundingIntervalHour)); parseErr == nil && parsed > 0 {
			interval = parsed
		}
		if interval == 0 {
			interval = 8
		}
		lastPrice, _ := parseFloat(item.LastPrice)
		markPrice, _ := parseFloat(item.MarkPrice)
		indexPrice, _ := parseFloat(item.IndexPrice)
		priceChange, _ := parseFloat(item.Price24hPcnt)
		oi, _ := parseFloat(item.OpenInterest)
		oiValue, _ := parseFloat(item.OpenInterestValue)
		turnover, _ := parseFloat(item.Turnover24h)
		volume, _ := parseFloat(item.Volume24h)
		result = append(result, FundingRate{
			Exchange: "bybit", ExchangeSymbol: item.Symbol, Rate: rate,
			FundingTime: milliseconds(ts), IntervalHours: interval,
			MarkPrice: markPrice, IndexPrice: indexPrice, LastPrice: lastPrice,
			OpenInterestContracts: oi, OpenInterestBase: oi,
			OpenInterestNotionalUSD: oiValue, Volume24hBase: volume,
			Turnover24hUSD: turnover, PriceChange24h: priceChange,
			SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result, nil
}

func (b *Bybit) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
	var payload bybitEnvelope[bybitTicker]
	if err := b.client.get(ctx, "/v5/market/tickers", url.Values{"category": {"linear"}}, &payload); err != nil {
		return nil, err
	}
	if payload.RetCode != 0 {
		return nil, fmt.Errorf("bybit: %s", payload.RetMsg)
	}
	intervals := make(map[string]float64, len(instruments))
	for _, item := range instruments {
		intervals[item.ExchangeSymbol] = item.IntervalHours
	}
	return parseBybitCurrent(payload.Result.List, intervals)
}

type bybitHistory struct {
	Symbol, FundingRate, FundingRateTimestamp string
}

func (b *Bybit) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 1000)
	result := make([]FundingRate, 0, wanted)
	end := time.Now().UnixMilli()
	for len(result) < wanted {
		query := url.Values{"category": {"linear"}, "symbol": {instrument.ExchangeSymbol}, "limit": {strconv.Itoa(min(200, wanted-len(result)))}, "endTime": {strconv.FormatInt(end, 10)}}
		var payload bybitEnvelope[bybitHistory]
		if err := b.client.get(ctx, "/v5/market/funding/history", query, &payload); err != nil {
			return nil, err
		}
		if payload.RetCode != 0 {
			return nil, fmt.Errorf("bybit: %s", payload.RetMsg)
		}
		if len(payload.Result.List) == 0 {
			break
		}
		for _, item := range payload.Result.List {
			rate, err := parseFloat(item.FundingRate)
			if err != nil {
				return nil, err
			}
			ts, err := strconv.ParseInt(item.FundingRateTimestamp, 10, 64)
			if err != nil {
				return nil, err
			}
			t := milliseconds(ts)
			if since.IsZero() || !t.Before(since) {
				result = append(result, FundingRate{
					Exchange: "bybit", ExchangeSymbol: item.Symbol, Rate: rate,
					FundingTime: t, Settled: true, IntervalHours: instrument.IntervalHours,
					SourceUpdatedAt: t,
				})
			}
			end = ts - 1
		}
		if len(payload.Result.List) < 200 || (!since.IsZero() && milliseconds(end).Before(since)) {
			break
		}
	}
	return result, nil
}
