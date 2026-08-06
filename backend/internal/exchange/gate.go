package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Gate struct{ client client }

func NewGate(timeout time.Duration) *Gate {
	return &Gate{client: newClient("https://api.gateio.ws", timeout)}
}
func (g *Gate) Name() string { return "gate" }

type gateContract struct {
	Name             string  `json:"name"`
	InDelisting      bool    `json:"in_delisting"`
	FundingRate      string  `json:"funding_rate"`
	FundingNextApply int64   `json:"funding_next_apply"`
	FundingInterval  int64   `json:"funding_interval"`
	QuantoMultiplier string  `json:"quanto_multiplier"`
	OrderPriceRound  string  `json:"order_price_round"`
	OrderSizeMin     float64 `json:"order_size_min"`
}

func parseGateInstruments(items []gateContract) []Instrument {
	result := make([]Instrument, 0, len(items))
	for _, item := range items {
		parts := strings.Split(item.Name, "_")
		if item.InDelisting || len(parts) < 2 {
			continue
		}
		base, quote := strings.Join(parts[:len(parts)-1], ""), parts[len(parts)-1]
		interval := float64(item.FundingInterval) / 3600
		if interval <= 0 {
			interval = 8
		}
		size, _ := parseFloat(item.QuantoMultiplier)
		tick, _ := parseFloat(item.OrderPriceRound)
		metadata, _ := json.Marshal(item)
		result = append(result, Instrument{
			Exchange: "gate", ExchangeSymbol: item.Name,
			BaseAsset: base, QuoteAsset: quote, GlobalSymbol: GlobalSymbol(base, quote),
			IntervalHours: interval, SettleAsset: "USDT",
			ContractType: "perpetual", Status: "active", ContractSize: size,
			PriceTick: tick, QuantityStep: item.OrderSizeMin,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (g *Gate) SyncInstruments(ctx context.Context) ([]Instrument, error) {
	var payload []gateContract
	if err := g.client.get(ctx, "/api/v4/futures/usdt/contracts", nil, &payload); err != nil {
		return nil, err
	}
	return parseGateInstruments(payload), nil
}

type gateTicker struct {
	Contract              string `json:"contract"`
	Last                  string `json:"last"`
	ChangePercentage      string `json:"change_percentage"`
	FundingRate           string `json:"funding_rate"`
	FundingRateIndicative string `json:"funding_rate_indicative"`
	MarkPrice             string `json:"mark_price"`
	IndexPrice            string `json:"index_price"`
	TotalSize             string `json:"total_size"`
	Volume24hBase         string `json:"volume_24h_base"`
	Volume24hQuote        string `json:"volume_24h_quote"`
}

func parseGateCurrent(items []gateContract, tickers map[string]gateTicker) ([]FundingRate, error) {
	result := make([]FundingRate, 0, len(items))
	for _, item := range items {
		ticker := tickers[item.Name]
		rateValue := ticker.FundingRate
		if rateValue == "" {
			rateValue = item.FundingRate
		}
		if item.InDelisting || rateValue == "" {
			continue
		}
		rate, err := parseFloat(rateValue)
		if err != nil {
			return nil, err
		}
		interval := float64(item.FundingInterval) / 3600
		if interval <= 0 {
			interval = 8
		}
		var nextRate *float64
		if ticker.FundingRateIndicative != "" {
			value, parseErr := parseFloat(ticker.FundingRateIndicative)
			if parseErr == nil {
				nextRate = floatPointer(value)
			}
		}
		lastPrice, _ := parseFloat(ticker.Last)
		markPrice, _ := parseFloat(ticker.MarkPrice)
		indexPrice, _ := parseFloat(ticker.IndexPrice)
		oi, _ := parseFloat(ticker.TotalSize)
		volume, _ := parseFloat(ticker.Volume24hBase)
		turnover, _ := parseFloat(ticker.Volume24hQuote)
		change, _ := parseFloat(ticker.ChangePercentage)
		result = append(result, FundingRate{
			Exchange: "gate", ExchangeSymbol: item.Name, Rate: rate,
			FundingTime: seconds(item.FundingNextApply), IntervalHours: interval,
			NextRate: nextRate, MarkPrice: markPrice, IndexPrice: indexPrice,
			LastPrice: lastPrice, OpenInterestContracts: oi,
			OpenInterestBase: oi, OpenInterestNotionalUSD: oi * markPrice,
			Volume24hBase: volume, Turnover24hUSD: turnover,
			PriceChange24h: change / 100, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result, nil
}

func (g *Gate) FetchCurrent(ctx context.Context, _ []Instrument) ([]FundingRate, error) {
	var payload []gateContract
	if err := g.client.get(ctx, "/api/v4/futures/usdt/contracts", nil, &payload); err != nil {
		return nil, err
	}
	var tickerPayload []gateTicker
	if err := g.client.get(ctx, "/api/v4/futures/usdt/tickers", nil, &tickerPayload); err != nil {
		return nil, err
	}
	tickers := make(map[string]gateTicker, len(tickerPayload))
	for _, ticker := range tickerPayload {
		tickers[ticker.Contract] = ticker
	}
	return parseGateCurrent(payload, tickers)
}

type gateHistory struct {
	Time int64  `json:"t"`
	Rate string `json:"r"`
}

func (g *Gate) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 5000)
	result := make([]FundingRate, 0, wanted)
	from := since.Unix()
	if since.IsZero() {
		from = time.Now().AddDate(0, -6, 0).Unix()
	}
	for len(result) < wanted {
		pageSize := min(1000, wanted-len(result))
		query := url.Values{"contract": {instrument.ExchangeSymbol}, "limit": {strconv.Itoa(pageSize)}, "from": {strconv.FormatInt(from, 10)}}
		var page []gateHistory
		if err := g.client.get(ctx, "/api/v4/futures/usdt/funding_rate", query, &page); err != nil {
			return nil, fmt.Errorf("gate history: %w", err)
		}
		for _, item := range page {
			rate, err := parseFloat(item.Rate)
			if err != nil {
				return nil, err
			}
			result = append(result, FundingRate{
				Exchange: "gate", ExchangeSymbol: instrument.ExchangeSymbol,
				Rate: rate, FundingTime: seconds(item.Time), Settled: true,
				IntervalHours: instrument.IntervalHours, SourceUpdatedAt: seconds(item.Time),
			})
		}
		if len(page) < pageSize {
			break
		}
		from = page[len(page)-1].Time + 1
	}
	return result, nil
}
