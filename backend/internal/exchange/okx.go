package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OKX struct{ client client }

func NewOKX(timeout time.Duration) *OKX {
	return &OKX{client: newClient("https://www.okx.com", timeout)}
}
func (o *OKX) Name() string { return "okx" }

type okxEnvelope[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []T    `json:"data"`
}
type okxInstrument struct {
	InstID, InstType, State, CtType, BaseCcy, QuoteCcy, SettleCcy string
	CtVal, TickSz, LotSz                                          string
}

func parseOKXInstruments(items []okxInstrument) []Instrument {
	result := make([]Instrument, 0, len(items))
	for _, item := range items {
		if item.State != "live" || item.InstType != "SWAP" || item.CtType != "linear" {
			continue
		}
		contractSize, _ := parseFloat(item.CtVal)
		tick, _ := parseFloat(item.TickSz)
		step, _ := parseFloat(item.LotSz)
		metadata, _ := json.Marshal(item)
		result = append(result, Instrument{
			Exchange: "okx", ExchangeSymbol: item.InstID,
			BaseAsset: item.BaseCcy, QuoteAsset: item.QuoteCcy,
			GlobalSymbol:  GlobalSymbol(item.BaseCcy, item.QuoteCcy),
			IntervalHours: 8, SettleAsset: item.SettleCcy,
			ContractType: "perpetual", Status: "active",
			ContractSize: contractSize, PriceTick: tick, QuantityStep: step,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (o *OKX) SyncInstruments(ctx context.Context) ([]Instrument, error) {
	var payload okxEnvelope[okxInstrument]
	if err := o.client.get(ctx, "/api/v5/public/instruments", url.Values{"instType": {"SWAP"}}, &payload); err != nil {
		return nil, err
	}
	if payload.Code != "0" {
		return nil, fmt.Errorf("okx: %s", payload.Msg)
	}
	return parseOKXInstruments(payload.Data), nil
}

type okxFunding struct {
	InstID, FundingRate, NextFundingRate, FundingTime, NextFundingTime string
}

func parseOKXFunding(items []okxFunding, settled bool, intervals map[string]float64) ([]FundingRate, error) {
	result := make([]FundingRate, 0, len(items))
	for _, item := range items {
		rate, err := parseFloat(item.FundingRate)
		if err != nil {
			return nil, err
		}
		value := item.NextFundingTime
		if settled {
			value = item.FundingTime
		}
		ts, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, err
		}
		interval := intervals[item.InstID]
		if interval <= 0 {
			interval = 8
		}
		var nextRate *float64
		if item.NextFundingRate != "" {
			value, parseErr := parseFloat(item.NextFundingRate)
			if parseErr == nil {
				nextRate = floatPointer(value)
			}
		}
		result = append(result, FundingRate{
			Exchange: "okx", ExchangeSymbol: item.InstID, Rate: rate,
			FundingTime: milliseconds(ts), Settled: settled,
			IntervalHours: interval, NextRate: nextRate,
			SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result, nil
}

func (o *OKX) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
	type okxTicker struct {
		InstID, Last, Open24h, Vol24h, VolCcy24h, Ts string
	}
	type okxOpenInterest struct {
		InstID, OI, OICcy, OIUsd, Ts string
	}
	var tickersPayload okxEnvelope[okxTicker]
	if err := o.client.get(ctx, "/api/v5/market/tickers", url.Values{"instType": {"SWAP"}}, &tickersPayload); err != nil {
		return nil, err
	}
	var oiPayload okxEnvelope[okxOpenInterest]
	if err := o.client.get(ctx, "/api/v5/public/open-interest", url.Values{"instType": {"SWAP"}}, &oiPayload); err != nil {
		return nil, err
	}
	tickers := make(map[string]okxTicker, len(tickersPayload.Data))
	for _, item := range tickersPayload.Data {
		tickers[item.InstID] = item
	}
	openInterest := make(map[string]okxOpenInterest, len(oiPayload.Data))
	for _, item := range oiPayload.Data {
		openInterest[item.InstID] = item
	}
	intervals := make(map[string]float64, len(instruments))
	for _, item := range instruments {
		intervals[item.ExchangeSymbol] = item.IntervalHours
	}
	type resultItem struct {
		rates []FundingRate
		err   error
	}
	jobs := make(chan Instrument)
	results := make(chan resultItem)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for instrument := range jobs {
				var payload okxEnvelope[okxFunding]
				err := o.client.get(ctx, "/api/v5/public/funding-rate", url.Values{"instId": {instrument.ExchangeSymbol}}, &payload)
				if err == nil && payload.Code != "0" {
					err = fmt.Errorf("okx %s: %s", instrument.ExchangeSymbol, payload.Msg)
				}
				if err != nil {
					results <- resultItem{err: err}
					continue
				}
				rates, err := parseOKXFunding(payload.Data, false, intervals)
				if err == nil && len(rates) > 0 {
					ticker := tickers[instrument.ExchangeSymbol]
					oi := openInterest[instrument.ExchangeSymbol]
					rates[0].LastPrice, _ = parseFloat(ticker.Last)
					rates[0].Volume24hBase, _ = parseFloat(ticker.Vol24h)
					rates[0].Turnover24hUSD, _ = parseFloat(ticker.VolCcy24h)
					open24h, _ := parseFloat(ticker.Open24h)
					if open24h != 0 {
						rates[0].PriceChange24h = (rates[0].LastPrice - open24h) / open24h
					}
					rates[0].OpenInterestContracts, _ = parseFloat(oi.OI)
					rates[0].OpenInterestBase, _ = parseFloat(oi.OICcy)
					rates[0].OpenInterestNotionalUSD, _ = parseFloat(oi.OIUsd)
				}
				results <- resultItem{rates: rates, err: err}
			}
		}()
	}
	go func() {
		for _, instrument := range instruments {
			jobs <- instrument
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()
	result := make([]FundingRate, 0, len(instruments))
	var firstErr error
	for item := range results {
		if item.err != nil {
			if firstErr == nil {
				firstErr = item.err
			}
			continue
		}
		result = append(result, item.rates...)
	}
	if len(result) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}

func (o *OKX) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 500)
	result := make([]FundingRate, 0, wanted)
	var before string
	for len(result) < wanted {
		query := url.Values{"instId": {instrument.ExchangeSymbol}, "limit": {strconv.Itoa(min(100, wanted-len(result)))}}
		if before != "" {
			query.Set("before", before)
		}
		var payload okxEnvelope[okxFunding]
		if err := o.client.get(ctx, "/api/v5/public/funding-rate-history", query, &payload); err != nil {
			return nil, err
		}
		if payload.Code != "0" {
			return nil, fmt.Errorf("okx: %s", payload.Msg)
		}
		rates, err := parseOKXFunding(payload.Data, true, map[string]float64{instrument.ExchangeSymbol: instrument.IntervalHours})
		if err != nil {
			return nil, err
		}
		for _, rate := range rates {
			if since.IsZero() || !rate.FundingTime.Before(since) {
				result = append(result, rate)
			}
		}
		if len(payload.Data) < 100 || len(payload.Data) == 0 {
			break
		}
		before = payload.Data[len(payload.Data)-1].FundingTime
		if strings.TrimSpace(before) == "" {
			break
		}
	}
	return result, nil
}
