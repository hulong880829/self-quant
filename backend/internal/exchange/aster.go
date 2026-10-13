package exchange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultAsterFundingAPIURL = "https://fapi.asterdex.com"
	asterOpenInterestInterval = 100 * time.Millisecond
	asterOpenInterestWorkers  = 8
)

type Aster struct {
	client   client
	oiClient client
}

func NewAster(timeout time.Duration, baseURL string) *Aster {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAsterFundingAPIURL
	}
	c := newClient(baseURL, timeout)
	c.limiter.interval = 200 * time.Millisecond
	oi := newClient(baseURL, timeout)
	oi.limiter.interval = asterOpenInterestInterval
	return &Aster{client: c, oiClient: oi}
}

func (a *Aster) Name() string { return "aster" }

func (a *Aster) SupportedContractTypes() []string {
	return []string{ContractTypePerpetual}
}

type asterExchangeInfo struct {
	Symbols []struct {
		Symbol, BaseAsset, QuoteAsset, MarginAsset, Status, ContractType string
		UnderlyingType                                                   string   `json:"underlyingType"`
		UnderlyingSubType                                                []string `json:"underlyingSubType"`
		ContractSize                                                     float64  `json:"contractSize"`
		Filters                                                          []struct {
			FilterType  string `json:"filterType"`
			TickSize    string `json:"tickSize"`
			StepSize    string `json:"stepSize"`
			MinQty      string `json:"minQty"`
			MaxQty      string `json:"maxQty"`
			MinNotional string `json:"minNotional"`
			Notional    string `json:"notional"`
		} `json:"filters"`
	} `json:"symbols"`
}

func parseAsterInstruments(payload asterExchangeInfo) []Instrument {
	result := make([]Instrument, 0, len(payload.Symbols))
	for _, item := range payload.Symbols {
		if item.Status != "TRADING" || !strings.EqualFold(item.ContractType, "PERPETUAL") {
			continue
		}
		var tick, step, minQuantity, minNotional float64
		var maxQuantity, marketStep, marketMinQuantity, marketMaxQuantity string
		var minNotionalRaw string
		minQuantityStatus, minNotionalStatus := ConstraintUnknown, ConstraintUnknown
		maxQuantityStatus := ConstraintUnknown
		marketStepStatus, marketMinQuantityStatus := ConstraintUnknown, ConstraintUnknown
		marketMaxQuantityStatus := ConstraintUnknown
		for _, filter := range item.Filters {
			switch filter.FilterType {
			case "PRICE_FILTER":
				tick, _ = parseFloat(filter.TickSize)
			case "LOT_SIZE":
				step, _ = parseFloat(filter.StepSize)
				minQuantity, minQuantityStatus = knownConstraint(filter.MinQty)
				maxQuantity, maxQuantityStatus = maximumDecimalConstraint(filter.MaxQty)
			case "MARKET_LOT_SIZE":
				marketStep, marketStepStatus = knownDecimalConstraint(filter.StepSize)
				marketMinQuantity, marketMinQuantityStatus = knownDecimalConstraint(filter.MinQty)
				marketMaxQuantity, marketMaxQuantityStatus = maximumDecimalConstraint(filter.MaxQty)
			case "MIN_NOTIONAL", "NOTIONAL":
				minNotionalRaw = filter.MinNotional
				if minNotionalRaw == "" {
					minNotionalRaw = filter.Notional
				}
				minNotional, minNotionalStatus = knownConstraint(minNotionalRaw)
			}
		}
		marketMinNotional, marketMinNotionalStatus := knownDecimalConstraint(minNotionalRaw)
		contractSize := item.ContractSize
		if contractSize <= 0 {
			contractSize = 1
		}
		metadata := instrumentMetadata(item, "linear", "base")
		parsed := Instrument{
			Exchange: "aster", ExchangeSymbol: item.Symbol,
			BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset,
			GlobalSymbol:  GlobalSymbol(item.BaseAsset, item.QuoteAsset),
			IntervalHours: 8, SettleAsset: firstNonEmpty(item.MarginAsset, item.QuoteAsset),
			ContractType: ContractTypePerpetual, Status: "active",
			ContractSize: contractSize, PriceTick: tick, QuantityStep: step,
			MinQuantity: minQuantity, MinNotional: minNotional,
			MinQuantityStatus: minQuantityStatus, MinNotionalStatus: minNotionalStatus,
			MaxQuantity: maxQuantity, MaxQuantityStatus: maxQuantityStatus,
			MarketQuantityStep: marketStep, MarketQuantityStepStatus: marketStepStatus,
			MarketMinQuantity: marketMinQuantity, MarketMinQuantityStatus: marketMinQuantityStatus,
			MarketMaxQuantity: marketMaxQuantity, MarketMaxQuantityStatus: marketMaxQuantityStatus,
			MarketMinNotional: marketMinNotional, MarketMinNotionalStatus: marketMinNotionalStatus,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		}
		switch {
		case asterPrelaunch(item.UnderlyingSubType):
			parsed.SetUnderlying(PreMarketReference(item.BaseAsset, "aster.exchangeInfo.underlyingSubType=pre-launch"))
		case asterSubtype(item.UnderlyingSubType, "STOCK"):
			parsed.SetUnderlying(TradfiCandidate(AssetClassStock, item.BaseAsset, item.QuoteAsset, "aster.exchangeInfo.underlyingSubType=STOCK"))
		case asterSubtype(item.UnderlyingSubType, "ETF"):
			parsed.SetUnderlying(TradfiCandidate(AssetClassETF, item.BaseAsset, item.QuoteAsset, "aster.exchangeInfo.underlyingSubType=ETF"))
		case asterSubtype(item.UnderlyingSubType, "Commodities"):
			parsed.SetUnderlying(CommodityReference(item.BaseAsset, "USD", "aster.exchangeInfo.underlyingSubType=Commodities"))
		default:
			parsed.SetUnderlying(CryptoReference(item.BaseAsset, item.QuoteAsset, "aster.perpetual"))
		}
		result = append(result, parsed)
	}
	return result
}

func asterPrelaunch(values []string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), "pre-launch") || strings.Contains(strings.ToLower(value), "prelaunch") {
			return true
		}
	}
	return false
}

func asterSubtype(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func (a *Aster) SyncInstruments(ctx context.Context, contractType string) ([]Instrument, error) {
	if contractType != ContractTypePerpetual {
		return nil, fmt.Errorf("aster: unsupported contract type %q", contractType)
	}
	var payload asterExchangeInfo
	if err := a.client.get(ctx, "/fapi/v3/exchangeInfo", nil, &payload); err != nil {
		return nil, err
	}
	return parseAsterInstruments(payload), nil
}

type asterPremium struct {
	Symbol          string `json:"symbol"`
	LastFundingRate string `json:"lastFundingRate"`
	NextFundingTime int64  `json:"nextFundingTime"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	Time            int64  `json:"time"`
}

type asterTicker struct {
	Symbol             string `json:"symbol"`
	LastPrice          string `json:"lastPrice"`
	Volume             string `json:"volume"`
	QuoteVolume        string `json:"quoteVolume"`
	PriceChangePercent string `json:"priceChangePercent"`
}

type asterFundingInfo struct {
	Symbol               string `json:"symbol"`
	FundingIntervalHours int    `json:"fundingIntervalHours"`
}

type asterOpenInterest struct {
	Symbol       string `json:"symbol"`
	OpenInterest string `json:"openInterest"`
	Time         int64  `json:"time"`
}

func asterFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func applyAsterOpenInterest(rate *FundingRate, oi asterOpenInterest) error {
	if !strings.EqualFold(strings.TrimSpace(oi.Symbol), rate.ExchangeSymbol) {
		return fmt.Errorf("aster %s openInterest: symbol mismatch %q", rate.ExchangeSymbol, oi.Symbol)
	}
	if rate.MarkPrice <= 0 || !asterFinite(rate.MarkPrice) {
		return fmt.Errorf("aster %s openInterest: mark price %v", rate.ExchangeSymbol, rate.MarkPrice)
	}
	value, err := parseFloat(oi.OpenInterest)
	if err != nil {
		return fmt.Errorf("aster %s openInterest: %w", rate.ExchangeSymbol, err)
	}
	if value < 0 || !asterFinite(value) {
		return fmt.Errorf("aster %s openInterest: invalid value %v", rate.ExchangeSymbol, value)
	}
	notional := value * rate.MarkPrice
	if !asterFinite(notional) {
		return fmt.Errorf("aster %s openInterest: invalid notional %v", rate.ExchangeSymbol, notional)
	}
	rate.OpenInterestContracts = value
	rate.OpenInterestBase = value
	rate.OpenInterestNotionalUSD = notional
	return nil
}

func parseAsterCurrent(
	payload []asterPremium,
	tickers map[string]asterTicker,
	intervals map[string]float64,
) ([]FundingRate, error) {
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
		interval := intervals[item.Symbol]
		if interval <= 0 {
			interval = 8
		}
		result = append(result, FundingRate{
			Exchange: "aster", ExchangeSymbol: item.Symbol, Rate: rate,
			FundingTime: milliseconds(item.NextFundingTime), IntervalHours: interval,
			MarkPrice: markPrice, IndexPrice: indexPrice, LastPrice: lastPrice,
			Volume24hBase: volume, Turnover24hUSD: turnover,
			PriceChange24h: change / 100, SourceUpdatedAt: milliseconds(item.Time),
		})
	}
	return result, nil
}

func (a *Aster) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
	var payload []asterPremium
	if err := a.client.get(ctx, "/fapi/v3/premiumIndex", nil, &payload); err != nil {
		return nil, err
	}
	var tickerPayload []asterTicker
	if err := a.client.get(ctx, "/fapi/v3/ticker/24hr", nil, &tickerPayload); err != nil {
		return nil, err
	}
	tickers := make(map[string]asterTicker, len(tickerPayload))
	for _, item := range tickerPayload {
		tickers[item.Symbol] = item
	}
	intervals := make(map[string]float64, len(instruments))
	for _, instrument := range instruments {
		intervals[instrument.ExchangeSymbol] = instrument.IntervalHours
	}
	var fundingInfoPayload []asterFundingInfo
	if err := a.client.get(ctx, "/fapi/v3/fundingInfo", nil, &fundingInfoPayload); err == nil {
		for _, item := range fundingInfoPayload {
			if item.FundingIntervalHours > 0 {
				intervals[item.Symbol] = float64(item.FundingIntervalHours)
			}
		}
	}
	wanted := make(map[string]struct{}, len(instruments))
	for _, instrument := range instruments {
		wanted[instrument.ExchangeSymbol] = struct{}{}
	}
	if len(wanted) > 0 {
		filtered := payload[:0]
		for _, item := range payload {
			if _, ok := wanted[item.Symbol]; ok {
				filtered = append(filtered, item)
			}
		}
		payload = filtered
	}
	rates, err := parseAsterCurrent(payload, tickers, intervals)
	if err != nil {
		return nil, err
	}
	if len(instruments) == 0 {
		return rates, nil
	}
	rates, err = a.applyOpenInterest(ctx, rates)
	if err != nil {
		return nil, err
	}
	return rates, nil
}

func isAsterInactiveInstrumentError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "HTTP 400") && strings.Contains(text, `"code":-4108`)
}

func (a *Aster) applyOpenInterest(ctx context.Context, rates []FundingRate) ([]FundingRate, error) {
	if len(rates) == 0 {
		return nil, nil
	}
	inactive := make([]bool, len(rates))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(asterOpenInterestWorkers)
	for i := range rates {
		i := i
		group.Go(func() error {
			var response asterOpenInterest
			if err := a.oiClient.get(groupCtx, "/fapi/v3/openInterest", url.Values{
				"symbol": {rates[i].ExchangeSymbol},
			}, &response); err != nil {
				if isAsterInactiveInstrumentError(err) {
					inactive[i] = true
					return nil
				}
				return fmt.Errorf("aster %s openInterest: %w", rates[i].ExchangeSymbol, err)
			}
			return applyAsterOpenInterest(&rates[i], response)
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	total := len(rates)
	kept := make([]FundingRate, 0, total)
	skippedSymbols := make([]string, 0, 8)
	skipped := 0
	for i := range rates {
		if !inactive[i] {
			kept = append(kept, rates[i])
			continue
		}
		skipped++
		if len(skippedSymbols) < 8 {
			skippedSymbols = append(skippedSymbols, rates[i].ExchangeSymbol)
		}
	}
	if skipped == 0 {
		return kept, nil
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("aster openInterest: all %d instruments inactive", total)
	}
	slog.Default().Warn("aster current funding skipped inactive instruments",
		"skipped", skipped,
		"symbols", skippedSymbols,
		"code", -4108,
		"total", total,
	)
	return kept, nil
}

type asterHistory struct {
	Symbol      string `json:"symbol"`
	FundingRate string `json:"fundingRate"`
	FundingTime int64  `json:"fundingTime"`
}

func (a *Aster) FetchHistory(
	ctx context.Context, instrument Instrument, since time.Time, limit int,
) ([]FundingRate, error) {
	wanted := clampLimit(limit, 10000)
	result := make([]FundingRate, 0, wanted)
	start := since.UnixMilli()
	for len(result) < wanted {
		pageSize := min(1000, wanted-len(result))
		query := url.Values{
			"symbol": {instrument.ExchangeSymbol},
			"limit":  {strconv.Itoa(pageSize)},
		}
		if start > 0 {
			query.Set("startTime", strconv.FormatInt(start, 10))
		}
		var page []asterHistory
		if err := a.client.get(ctx, "/fapi/v3/fundingRate", query, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			rate, err := parseFloat(item.FundingRate)
			if err != nil {
				return nil, fmt.Errorf("aster %s funding history: %w", item.Symbol, err)
			}
			interval := instrument.IntervalHours
			if interval <= 0 {
				interval = 8
			}
			result = append(result, FundingRate{
				Exchange: "aster", ExchangeSymbol: item.Symbol, Rate: rate,
				FundingTime: milliseconds(item.FundingTime), Settled: true,
				IntervalHours: interval, SourceUpdatedAt: milliseconds(item.FundingTime),
			})
		}
		if len(page) < pageSize {
			break
		}
		start = page[len(page)-1].FundingTime + 1
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("aster fundingRate: empty history for %s: %w",
			instrument.ExchangeSymbol, ErrNoSettledHistory)
	}
	return result, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var _ Adapter = (*Aster)(nil)
var _ ContractTypeSupport = (*Aster)(nil)
