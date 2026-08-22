package exchange

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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
	InstID, InstType, InstFamily, State, CtType string
	BaseCcy, QuoteCcy, SettleCcy                string
	CtVal, CtMult, TickSz, LotSz                string
}

func parseOKXInstruments(items []okxInstrument, contractType string) []Instrument {
	result := make([]Instrument, 0, len(items))
	for _, item := range items {
		expectedType := "SWAP"
		if contractType == ContractTypeSpot {
			expectedType = "SPOT"
		}
		if item.State != "live" || item.InstType != expectedType {
			continue
		}
		baseAsset, quoteAsset, valid := okxInstrumentAssets(item, contractType)
		if !valid {
			continue
		}
		contractSize, _ := parseFloat(item.CtVal)
		tick, _ := parseFloat(item.TickSz)
		step, _ := parseFloat(item.LotSz)
		interval := 8.0
		if contractType == ContractTypeSpot {
			interval = 0
		}
		model := strings.ToLower(strings.TrimSpace(item.CtType))
		if model == "" {
			model = "linear"
		}
		metadata := instrumentMetadata(item, model, "contracts")
		result = append(result, Instrument{
			Exchange: "okx", ExchangeSymbol: item.InstID,
			BaseAsset: baseAsset, QuoteAsset: quoteAsset,
			GlobalSymbol:  GlobalSymbol(baseAsset, quoteAsset),
			IntervalHours: interval, SettleAsset: item.SettleCcy,
			ContractType: contractType, Status: "active",
			ContractSize: contractSize, PriceTick: tick, QuantityStep: step,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func okxInstrumentAssets(item okxInstrument, contractType string) (string, string, bool) {
	baseAsset := strings.TrimSpace(item.BaseCcy)
	quoteAsset := strings.TrimSpace(item.QuoteCcy)
	if contractType == ContractTypePerpetual {
		family := strings.TrimSpace(item.InstFamily)
		separator := strings.LastIndex(family, "-")
		if separator <= 0 || separator == len(family)-1 {
			return "", "", false
		}
		baseAsset = strings.TrimSpace(family[:separator])
		quoteAsset = strings.TrimSpace(family[separator+1:])
	}
	return baseAsset, quoteAsset, baseAsset != "" && quoteAsset != ""
}

func (o *OKX) SyncInstruments(ctx context.Context, contractType string) ([]Instrument, error) {
	instType := "SWAP"
	if contractType == ContractTypeSpot {
		instType = "SPOT"
	} else if contractType != ContractTypePerpetual {
		return nil, fmt.Errorf("okx: unsupported contract type %q", contractType)
	}
	var payload okxEnvelope[okxInstrument]
	if err := o.client.get(ctx, "/api/v5/public/instruments", url.Values{"instType": {instType}}, &payload); err != nil {
		return nil, err
	}
	if payload.Code != "0" {
		return nil, fmt.Errorf("okx: %s", payload.Msg)
	}
	return parseOKXInstruments(payload.Data, contractType), nil
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
		// OKX semantics: fundingTime is the imminent settlement; nextFundingTime is
		// the following one. Interval is derived from the gap between them.
		value := item.FundingTime
		if value == "" {
			value = item.NextFundingTime
		}
		ts, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, err
		}
		interval := intervals[item.InstID]
		if !settled && item.FundingTime != "" && item.NextFundingTime != "" {
			currentTimestamp, currentErr := strconv.ParseInt(item.FundingTime, 10, 64)
			nextTimestamp, nextErr := strconv.ParseInt(item.NextFundingTime, 10, 64)
			if currentErr == nil && nextErr == nil && nextTimestamp > currentTimestamp {
				interval = float64(nextTimestamp-currentTimestamp) / float64(time.Hour/time.Millisecond)
			}
		}
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

type okxTicker struct {
	InstID, Last, Open24h, Vol24h, VolCcy24h, VolCcyQuote24h, Ts string
}

type okxOpenInterest struct {
	InstID, OI, OICcy, OIUsd, Ts string
}

// applyOKXMarketData maps OKX SWAP ticker/OI fields into normalized funding fields.
// For linear swaps, vol24h is contracts, volCcy24h is base-coin volume, and
// volCcyQuote24h (when present) is quote-currency turnover; otherwise quote
// turnover is derived as base volume * last price.
func applyOKXMarketData(rate *FundingRate, ticker okxTicker, oi okxOpenInterest) {
	last, _ := parseFloat(ticker.Last)
	volumeBase, _ := parseFloat(ticker.VolCcy24h)
	turnover, _ := parseFloat(ticker.VolCcyQuote24h)
	if turnover <= 0 && volumeBase > 0 && last > 0 {
		turnover = volumeBase * last
	}
	rate.LastPrice = last
	rate.Volume24hBase = volumeBase
	rate.Turnover24hUSD = turnover
	open24h, _ := parseFloat(ticker.Open24h)
	if open24h != 0 {
		rate.PriceChange24h = (last - open24h) / open24h
	}
	rate.OpenInterestContracts, _ = parseFloat(oi.OI)
	rate.OpenInterestBase, _ = parseFloat(oi.OICcy)
	rate.OpenInterestNotionalUSD, _ = parseFloat(oi.OIUsd)
}

func (o *OKX) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
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
	result := make([]FundingRate, 0, len(instruments))
	var firstErr error
	for index, instrument := range instruments {
		var payload okxEnvelope[okxFunding]
		err := o.client.get(ctx, "/api/v5/public/funding-rate", url.Values{
			"instId": {instrument.ExchangeSymbol},
		}, &payload)
		if err == nil && payload.Code != "0" {
			err = fmt.Errorf("okx %s: %s", instrument.ExchangeSymbol, payload.Msg)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			rates, parseErr := parseOKXFunding(payload.Data, false, intervals)
			if parseErr != nil {
				if firstErr == nil {
					firstErr = parseErr
				}
			} else if len(rates) > 0 {
				applyOKXMarketData(
					&rates[0],
					tickers[instrument.ExchangeSymbol],
					openInterest[instrument.ExchangeSymbol],
				)
				result = append(result, rates...)
			}
		}
		if err := waitAfterBatch(ctx, index+1); err != nil {
			return nil, err
		}
	}
	if len(result) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}

func (o *OKX) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 10000)
	result := make([]FundingRate, 0, wanted)
	var after string
	for len(result) < wanted {
		query := url.Values{"instId": {instrument.ExchangeSymbol}, "limit": {strconv.Itoa(min(100, wanted-len(result)))}}
		if after != "" {
			query.Set("after", after)
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
		after = payload.Data[len(payload.Data)-1].FundingTime
		if strings.TrimSpace(after) == "" {
			break
		}
	}
	return result, nil
}
