package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type Binance struct{ client client }

func NewBinance(timeout time.Duration) *Binance {
	return &Binance{client: newClient("https://fapi.binance.com", timeout)}
}

func (b *Binance) Name() string { return "binance" }

type binanceExchangeInfo struct {
	Symbols []struct {
		Symbol, BaseAsset, QuoteAsset, MarginAsset, Status, ContractType string
		Filters                                                          []struct {
			FilterType string `json:"filterType"`
			TickSize   string `json:"tickSize"`
			StepSize   string `json:"stepSize"`
		} `json:"filters"`
	}
}

func parseBinanceInstruments(payload binanceExchangeInfo) []Instrument {
	result := make([]Instrument, 0, len(payload.Symbols))
	for _, item := range payload.Symbols {
		if item.Status != "TRADING" || item.ContractType != "PERPETUAL" {
			continue
		}
		var tick, step float64
		for _, filter := range item.Filters {
			if filter.FilterType == "PRICE_FILTER" {
				tick, _ = parseFloat(filter.TickSize)
			}
			if filter.FilterType == "LOT_SIZE" {
				step, _ = parseFloat(filter.StepSize)
			}
		}
		metadata, _ := json.Marshal(item)
		result = append(result, Instrument{
			Exchange: "binance", ExchangeSymbol: item.Symbol,
			BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset,
			GlobalSymbol:  GlobalSymbol(item.BaseAsset, item.QuoteAsset),
			IntervalHours: 8, SettleAsset: item.MarginAsset,
			ContractType: "perpetual", Status: "active",
			ContractSize: 1, PriceTick: tick, QuantityStep: step,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (b *Binance) SyncInstruments(ctx context.Context) ([]Instrument, error) {
	var payload binanceExchangeInfo
	if err := b.client.get(ctx, "/fapi/v1/exchangeInfo", nil, &payload); err != nil {
		return nil, err
	}
	return parseBinanceInstruments(payload), nil
}

type binancePremium struct {
	Symbol          string `json:"symbol"`
	LastFundingRate string `json:"lastFundingRate"`
	NextFundingTime int64  `json:"nextFundingTime"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	Time            int64  `json:"time"`
}

type binanceTicker struct {
	Symbol             string `json:"symbol"`
	LastPrice          string `json:"lastPrice"`
	Volume             string `json:"volume"`
	QuoteVolume        string `json:"quoteVolume"`
	PriceChangePercent string `json:"priceChangePercent"`
}

func parseBinanceCurrent(payload []binancePremium, tickers map[string]binanceTicker) ([]FundingRate, error) {
	result := make([]FundingRate, 0, len(payload))
	for _, item := range payload {
		rate, err := parseFloat(item.LastFundingRate)
		if err != nil {
			return nil, fmt.Errorf("%s funding rate: %w", item.Symbol, err)
		}
		markPrice, _ := parseFloat(item.MarkPrice)
		indexPrice, _ := parseFloat(item.IndexPrice)
		ticker := tickers[item.Symbol]
		lastPrice, _ := parseFloat(ticker.LastPrice)
		volume, _ := parseFloat(ticker.Volume)
		turnover, _ := parseFloat(ticker.QuoteVolume)
		change, _ := parseFloat(ticker.PriceChangePercent)
		result = append(result, FundingRate{
			Exchange: "binance", ExchangeSymbol: item.Symbol, Rate: rate,
			FundingTime: milliseconds(item.NextFundingTime), IntervalHours: 8,
			MarkPrice: markPrice, IndexPrice: indexPrice, LastPrice: lastPrice,
			Volume24hBase: volume, Turnover24hUSD: turnover,
			PriceChange24h: change / 100, SourceUpdatedAt: milliseconds(item.Time),
		})
	}
	return result, nil
}

func (b *Binance) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
	var payload []binancePremium
	if err := b.client.get(ctx, "/fapi/v1/premiumIndex", nil, &payload); err != nil {
		return nil, err
	}
	var tickerPayload []binanceTicker
	if err := b.client.get(ctx, "/fapi/v1/ticker/24hr", nil, &tickerPayload); err != nil {
		return nil, err
	}
	tickers := make(map[string]binanceTicker, len(tickerPayload))
	for _, item := range tickerPayload {
		tickers[item.Symbol] = item
	}
	rates, err := parseBinanceCurrent(payload, tickers)
	if err != nil {
		return nil, err
	}
	bySymbol := make(map[string]*FundingRate, len(rates))
	for index := range rates {
		bySymbol[rates[index].ExchangeSymbol] = &rates[index]
	}
	type openInterest struct {
		Symbol       string `json:"symbol"`
		OpenInterest string `json:"openInterest"`
	}
	jobs := make(chan Instrument)
	var workers sync.WaitGroup
	var lock sync.Mutex
	for range 6 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for instrument := range jobs {
				var response openInterest
				if err := b.client.get(ctx, "/fapi/v1/openInterest", url.Values{"symbol": {instrument.ExchangeSymbol}}, &response); err != nil {
					continue
				}
				value, _ := parseFloat(response.OpenInterest)
				lock.Lock()
				if rate := bySymbol[instrument.ExchangeSymbol]; rate != nil {
					rate.OpenInterestContracts = value
					rate.OpenInterestBase = value
					rate.OpenInterestNotionalUSD = value * rate.MarkPrice
				}
				lock.Unlock()
			}
		}()
	}
	for _, instrument := range instruments {
		jobs <- instrument
	}
	close(jobs)
	workers.Wait()
	return rates, nil
}

type binanceHistory struct {
	Symbol      string `json:"symbol"`
	FundingRate string `json:"fundingRate"`
	FundingTime int64  `json:"fundingTime"`
}

func (b *Binance) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 5000)
	result := make([]FundingRate, 0, wanted)
	start := since.UnixMilli()
	for len(result) < wanted {
		pageSize := min(1000, wanted-len(result))
		query := url.Values{"symbol": {instrument.ExchangeSymbol}, "limit": {strconv.Itoa(pageSize)}}
		if start > 0 {
			query.Set("startTime", strconv.FormatInt(start, 10))
		}
		var page []binanceHistory
		if err := b.client.get(ctx, "/fapi/v1/fundingRate", query, &page); err != nil {
			return nil, err
		}
		for _, item := range page {
			rate, err := parseFloat(item.FundingRate)
			if err != nil {
				return nil, err
			}
			result = append(result, FundingRate{
				Exchange: "binance", ExchangeSymbol: item.Symbol, Rate: rate,
				FundingTime: milliseconds(item.FundingTime), Settled: true,
				IntervalHours:   instrument.IntervalHours,
				SourceUpdatedAt: milliseconds(item.FundingTime),
			})
		}
		if len(page) < pageSize {
			break
		}
		start = page[len(page)-1].FundingTime + 1
	}
	return result, nil
}
