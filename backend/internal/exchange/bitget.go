package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type Bitget struct{ client client }

func NewBitget(timeout time.Duration) *Bitget {
	return &Bitget{client: newClient("https://api.bitget.com", timeout)}
}
func (b *Bitget) Name() string { return "bitget" }

type bitgetEnvelope[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data T      `json:"data"`
}
type bitgetInstrument struct {
	Symbol, BaseCoin, QuoteCoin, SymbolStatus, SymbolType, FundingRateInterval string
	SettleCoin, SizeMultiplier, PriceEndStep, MinTradeNum                      string
}

func parseBitgetInstruments(items []bitgetInstrument) []Instrument {
	result := make([]Instrument, 0, len(items))
	for _, item := range items {
		if item.SymbolStatus != "normal" || item.SymbolType != "perpetual" {
			continue
		}
		hours, _ := strconv.ParseFloat(item.FundingRateInterval, 64)
		if hours <= 0 {
			hours = 8
		}
		size, _ := parseFloat(item.SizeMultiplier)
		tick, _ := parseFloat(item.PriceEndStep)
		step, _ := parseFloat(item.MinTradeNum)
		metadata, _ := json.Marshal(item)
		result = append(result, Instrument{
			Exchange: "bitget", ExchangeSymbol: item.Symbol,
			BaseAsset: item.BaseCoin, QuoteAsset: item.QuoteCoin,
			GlobalSymbol:  GlobalSymbol(item.BaseCoin, item.QuoteCoin),
			IntervalHours: hours, SettleAsset: item.SettleCoin,
			ContractType: "perpetual", Status: "active", ContractSize: size,
			PriceTick: tick, QuantityStep: step, Metadata: metadata,
			SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (b *Bitget) SyncInstruments(ctx context.Context) ([]Instrument, error) {
	var all []Instrument
	for _, product := range []string{"USDT-FUTURES", "USDC-FUTURES"} {
		var payload bitgetEnvelope[[]bitgetInstrument]
		if err := b.client.get(ctx, "/api/v2/mix/market/contracts", url.Values{"productType": {product}}, &payload); err != nil {
			return nil, err
		}
		if payload.Code != "00000" {
			return nil, fmt.Errorf("bitget: %s", payload.Msg)
		}
		all = append(all, parseBitgetInstruments(payload.Data)...)
	}
	return all, nil
}

type bitgetTicker struct {
	Symbol, FundingRate, NextSettleTime                                            string
	LastPr, IndexPrice, MarkPrice, BaseVolume, QuoteVolume, HoldingAmount, OpenUtc string
}

func parseBitgetCurrent(items []bitgetTicker, intervals map[string]float64) ([]FundingRate, error) {
	result := make([]FundingRate, 0, len(items))
	for _, item := range items {
		rate, err := parseFloat(item.FundingRate)
		if err != nil {
			return nil, err
		}
		interval := intervals[item.Symbol]
		if interval <= 0 {
			interval = 8
		}
		nextFundingAt := time.Now().UTC().Truncate(time.Duration(interval) * time.Hour).Add(time.Duration(interval) * time.Hour)
		if item.NextSettleTime != "" {
			ts, parseErr := strconv.ParseInt(item.NextSettleTime, 10, 64)
			if parseErr != nil {
				return nil, parseErr
			}
			nextFundingAt = milliseconds(ts)
		}
		lastPrice, _ := parseFloat(item.LastPr)
		markPrice, _ := parseFloat(item.MarkPrice)
		indexPrice, _ := parseFloat(item.IndexPrice)
		volume, _ := parseFloat(item.BaseVolume)
		turnover, _ := parseFloat(item.QuoteVolume)
		oi, _ := parseFloat(item.HoldingAmount)
		openPrice, _ := parseFloat(item.OpenUtc)
		var change float64
		if openPrice != 0 {
			change = (lastPrice - openPrice) / openPrice
		}
		result = append(result, FundingRate{
			Exchange: "bitget", ExchangeSymbol: item.Symbol, Rate: rate,
			FundingTime: nextFundingAt, IntervalHours: interval,
			MarkPrice: markPrice, IndexPrice: indexPrice, LastPrice: lastPrice,
			OpenInterestContracts: oi, OpenInterestBase: oi,
			OpenInterestNotionalUSD: oi * markPrice, Volume24hBase: volume,
			Turnover24hUSD: turnover, PriceChange24h: change,
			SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result, nil
}

func (b *Bitget) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
	var all []FundingRate
	intervals := make(map[string]float64, len(instruments))
	for _, instrument := range instruments {
		intervals[instrument.ExchangeSymbol] = instrument.IntervalHours
	}
	for _, product := range []string{"USDT-FUTURES", "USDC-FUTURES"} {
		var payload bitgetEnvelope[[]bitgetTicker]
		if err := b.client.get(ctx, "/api/v2/mix/market/tickers", url.Values{"productType": {product}}, &payload); err != nil {
			return nil, err
		}
		rates, err := parseBitgetCurrent(payload.Data, intervals)
		if err != nil {
			return nil, err
		}
		all = append(all, rates...)
	}
	return all, nil
}

type bitgetHistory struct {
	Symbol, FundingRate, FundingTime string
}

func (b *Bitget) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 500)
	result := make([]FundingRate, 0, wanted)
	page := 1
	product := "USDT-FUTURES"
	if instrument.QuoteAsset == "USDC" {
		product = "USDC-FUTURES"
	}
	for len(result) < wanted {
		var payload bitgetEnvelope[[]bitgetHistory]
		query := url.Values{"symbol": {instrument.ExchangeSymbol}, "productType": {product}, "pageSize": {strconv.Itoa(min(100, wanted-len(result)))}, "pageNo": {strconv.Itoa(page)}}
		if err := b.client.get(ctx, "/api/v2/mix/market/history-fund-rate", query, &payload); err != nil {
			return nil, err
		}
		if len(payload.Data) == 0 {
			break
		}
		for _, item := range payload.Data {
			rate, err := parseFloat(item.FundingRate)
			if err != nil {
				return nil, err
			}
			ts, err := strconv.ParseInt(item.FundingTime, 10, 64)
			if err != nil {
				return nil, err
			}
			t := milliseconds(ts)
			if since.IsZero() || !t.Before(since) {
				result = append(result, FundingRate{
					Exchange: "bitget", ExchangeSymbol: instrument.ExchangeSymbol,
					Rate: rate, FundingTime: t, Settled: true,
					IntervalHours: instrument.IntervalHours, SourceUpdatedAt: t,
				})
			}
		}
		if len(payload.Data) < 100 {
			break
		}
		page++
	}
	return result, nil
}
