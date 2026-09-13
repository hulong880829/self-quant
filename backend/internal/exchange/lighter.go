package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	defaultLighterFundingAPIURL = "https://mainnet.zklighter.elliot.ai"
	lighterHistoryPageSize      = 750
	lighterRequestInterval      = time.Second
)

type Lighter struct{ client client }

func NewLighter(timeout time.Duration, baseURL string) *Lighter {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultLighterFundingAPIURL
	}
	c := newClient(baseURL, timeout)
	c.limiter.interval = lighterRequestInterval
	return &Lighter{client: c}
}

func (l *Lighter) Name() string { return "lighter" }

func (l *Lighter) SupportedContractTypes() []string {
	return []string{ContractTypePerpetual}
}

type lighterOrderBookDetails struct {
	Code             json.Number `json:"code"`
	OrderBookDetails []struct {
		Symbol                 string           `json:"symbol"`
		MarketID               json.Number      `json:"market_id"`
		MarketType             string           `json:"market_type"`
		Status                 string           `json:"status"`
		Multiplier             string           `json:"multiplier"`
		SupportedSizeDecimals  int              `json:"supported_size_decimals"`
		SupportedPriceDecimals int              `json:"supported_price_decimals"`
		MarkPrice              string           `json:"mark_price"`
		IndexPrice             string           `json:"index_price"`
		LastTradePrice         lighterFlexFloat `json:"last_trade_price"`
		DailyBaseTokenVolume   lighterFlexFloat `json:"daily_base_token_volume"`
		DailyQuoteTokenVolume  lighterFlexFloat `json:"daily_quote_token_volume"`
		DailyPriceChange       lighterFlexFloat `json:"daily_price_change"`
		OpenInterest           lighterFlexFloat `json:"open_interest"`
		MinBaseAmount          string           `json:"min_base_amount"`
		MinQuoteAmount         string           `json:"min_quote_amount"`
	} `json:"order_book_details"`
}

type lighterFundingRates struct {
	Code         json.Number `json:"code"`
	FundingRates []struct {
		MarketID json.Number      `json:"market_id"`
		Exchange string           `json:"exchange"`
		Symbol   string           `json:"symbol"`
		Rate     lighterFlexFloat `json:"rate"`
	} `json:"funding_rates"`
}

type lighterFundings struct {
	Code       json.Number `json:"code"`
	Resolution string      `json:"resolution"`
	Fundings   []struct {
		Timestamp json.Number `json:"timestamp"`
		Value     string      `json:"value"`
		Rate      string      `json:"rate"`
		Direction string      `json:"direction"`
	} `json:"fundings"`
}

type lighterFlexFloat float64

func (f *lighterFlexFloat) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*f = 0
		return nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			*f = 0
			return nil
		}
		parsed, err := parseFloat(text)
		if err != nil {
			return err
		}
		*f = lighterFlexFloat(parsed)
		return nil
	}
	var parsed float64
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	*f = lighterFlexFloat(parsed)
	return nil
}

func lighterHourlyFromEightHour(eightHour float64) (float64, error) {
	hourly, _ := decimal.NewFromFloat(eightHour).Div(decimal.NewFromInt(8)).Float64()
	if hourly != hourly {
		return 0, fmt.Errorf("lighter hourly conversion produced NaN")
	}
	return hourly, nil
}

func lighterSettledHourlyRate(rate string, direction string) (float64, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(rate))
	if err != nil {
		return 0, err
	}
	// /fundings.rate is a percent-style string (0.0012 = 0.0012%), matching
	// InterestRate/8. Convert to a decimal ratio. Do not divide by 8 again.
	hourly := parsed.Div(decimal.NewFromInt(100))
	if strings.EqualFold(strings.TrimSpace(direction), "short") && hourly.IsPositive() {
		hourly = hourly.Neg()
	}
	value, _ := hourly.Float64()
	return value, nil
}

func nextHourBoundary(now time.Time) time.Time {
	utc := now.UTC()
	truncated := utc.Truncate(time.Hour)
	if utc.Equal(truncated) {
		return truncated.Add(time.Hour)
	}
	return truncated.Add(time.Hour)
}

func parseLighterInstruments(payload lighterOrderBookDetails) []Instrument {
	result := make([]Instrument, 0, len(payload.OrderBookDetails))
	for _, item := range payload.OrderBookDetails {
		if !strings.EqualFold(item.Status, "active") {
			continue
		}
		if item.MarketType != "" && !strings.EqualFold(item.MarketType, "perp") {
			continue
		}
		symbol := strings.TrimSpace(item.Symbol)
		if symbol == "" {
			continue
		}
		multiplier, err := parseFloat(item.Multiplier)
		if err != nil || multiplier <= 0 {
			multiplier = 1
		}
		step := 1.0
		for range item.SupportedSizeDecimals {
			step /= 10
		}
		priceTick := 1.0
		for range item.SupportedPriceDecimals {
			priceTick /= 10
		}
		minQuantity, minQuantityStatus := knownConstraint(item.MinBaseAmount)
		minNotional, minNotionalStatus := knownConstraint(item.MinQuoteAmount)
		stepText := strconv.FormatFloat(step, 'f', item.SupportedSizeDecimals, 64)
		metadata, _ := json.Marshal(map[string]any{
			"market_id":                item.MarketID.String(),
			"multiplier":               item.Multiplier,
			"market_type":              item.MarketType,
			"supported_price_decimals": item.SupportedPriceDecimals,
			"supported_size_decimals":  item.SupportedSizeDecimals,
			"contractModel":            "linear",
			"positionSizeUnit":         "base",
		})
		result = append(result, Instrument{
			Exchange: "lighter", ExchangeSymbol: symbol,
			BaseAsset: symbol, QuoteAsset: "USDC",
			GlobalSymbol: GlobalSymbol(symbol, "USDC"), IntervalHours: 1,
			SettleAsset: "USDC", ContractType: ContractTypePerpetual, Status: "active",
			ContractSize: multiplier, PriceTick: priceTick, QuantityStep: step,
			MinQuantity: minQuantity, MinNotional: minNotional,
			MinQuantityStatus: minQuantityStatus, MinNotionalStatus: minNotionalStatus,
			MaxQuantityStatus:  ConstraintNotApplicable,
			MarketQuantityStep: stepText, MarketQuantityStepStatus: ConstraintKnown,
			MarketMinQuantity: item.MinBaseAmount, MarketMinQuantityStatus: minQuantityStatus,
			MarketMaxQuantityStatus: ConstraintNotApplicable,
			MarketMinNotional:       item.MinQuoteAmount, MarketMinNotionalStatus: minNotionalStatus,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (l *Lighter) SyncInstruments(ctx context.Context, contractType string) ([]Instrument, error) {
	if contractType != ContractTypePerpetual {
		return nil, fmt.Errorf("lighter: unsupported contract type %q", contractType)
	}
	var payload lighterOrderBookDetails
	if err := l.client.get(ctx, "/api/v1/orderBookDetails", url.Values{
		"filter": {"perp"},
	}, &payload); err != nil {
		return nil, err
	}
	return parseLighterInstruments(payload), nil
}

func lighterMarketID(instrument Instrument) (int64, error) {
	if len(instrument.Metadata) == 0 {
		return 0, fmt.Errorf("lighter %s: missing market_id metadata", instrument.ExchangeSymbol)
	}
	var metadata struct {
		MarketID json.Number `json:"market_id"`
	}
	if err := json.Unmarshal(instrument.Metadata, &metadata); err != nil {
		return 0, fmt.Errorf("lighter %s market_id: %w", instrument.ExchangeSymbol, err)
	}
	marketID, err := metadata.MarketID.Int64()
	if err != nil {
		return 0, fmt.Errorf("lighter %s market_id: %w", instrument.ExchangeSymbol, err)
	}
	return marketID, nil
}

func parseLighterCurrent(
	payload lighterFundingRates,
	books lighterOrderBookDetails,
	now time.Time,
) ([]FundingRate, error) {
	type marketQuote struct {
		MarkPrice            float64
		IndexPrice           float64
		LastPrice            float64
		Volume24hBase        float64
		Turnover24hUSD       float64
		PriceChange24h       float64
		OpenInterestBase     float64
		OpenInterestNotional float64
	}
	details := make(map[string]marketQuote, len(books.OrderBookDetails))
	for _, item := range books.OrderBookDetails {
		mark, _ := parseFloat(item.MarkPrice)
		index, _ := parseFloat(item.IndexPrice)
		last := float64(item.LastTradePrice)
		if last == 0 {
			last = mark
		}
		details[item.Symbol] = marketQuote{
			MarkPrice: mark, IndexPrice: index, LastPrice: last,
			Volume24hBase:        float64(item.DailyBaseTokenVolume),
			Turnover24hUSD:       float64(item.DailyQuoteTokenVolume),
			PriceChange24h:       float64(item.DailyPriceChange) / 100,
			OpenInterestBase:     float64(item.OpenInterest),
			OpenInterestNotional: float64(item.OpenInterest) * mark,
		}
	}
	result := make([]FundingRate, 0, len(payload.FundingRates))
	seen := make(map[string]struct{}, len(payload.FundingRates))
	for _, item := range payload.FundingRates {
		if !strings.EqualFold(strings.TrimSpace(item.Exchange), "lighter") {
			continue
		}
		symbol := strings.TrimSpace(item.Symbol)
		if symbol == "" {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		hourly, err := lighterHourlyFromEightHour(float64(item.Rate))
		if err != nil {
			return nil, fmt.Errorf("lighter %s: %w", symbol, err)
		}
		market := details[symbol]
		seen[symbol] = struct{}{}
		result = append(result, FundingRate{
			Exchange: "lighter", ExchangeSymbol: symbol, Rate: hourly,
			FundingTime: nextHourBoundary(now), IntervalHours: 1,
			MarkPrice: market.MarkPrice, IndexPrice: market.IndexPrice,
			LastPrice: market.LastPrice, Volume24hBase: market.Volume24hBase,
			Turnover24hUSD: market.Turnover24hUSD, PriceChange24h: market.PriceChange24h,
			OpenInterestBase:        market.OpenInterestBase,
			OpenInterestContracts:   market.OpenInterestBase,
			OpenInterestNotionalUSD: market.OpenInterestNotional,
			SourceUpdatedAt:         now.UTC(),
		})
	}
	return result, nil
}

func (l *Lighter) FetchCurrent(ctx context.Context, instruments []Instrument) ([]FundingRate, error) {
	var payload lighterFundingRates
	if err := l.client.get(ctx, "/api/v1/funding-rates", nil, &payload); err != nil {
		return nil, err
	}
	var books lighterOrderBookDetails
	if err := l.client.get(ctx, "/api/v1/orderBookDetails", url.Values{
		"filter": {"perp"},
	}, &books); err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(instruments))
	for _, instrument := range instruments {
		wanted[instrument.ExchangeSymbol] = struct{}{}
	}
	rates, err := parseLighterCurrent(payload, books, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return rates, nil
	}
	filtered := rates[:0]
	for _, rate := range rates {
		if _, ok := wanted[rate.ExchangeSymbol]; ok {
			filtered = append(filtered, rate)
		}
	}
	return filtered, nil
}

func lighterFundingTimestamp(raw json.Number) (int64, error) {
	timestamp, err := raw.Int64()
	if err != nil {
		return 0, err
	}
	if timestamp > 1e12 {
		timestamp = timestamp / 1000
	}
	return timestamp, nil
}

func (l *Lighter) FetchHistory(
	ctx context.Context, instrument Instrument, since time.Time, limit int,
) ([]FundingRate, error) {
	marketID, err := lighterMarketID(instrument)
	if err != nil {
		return nil, err
	}
	wanted := clampLimit(limit, 9000)
	result := make([]FundingRate, 0, wanted)
	seen := make(map[int64]struct{}, wanted)
	lowerBound := since.UTC().Unix()
	cursorEnd := time.Now().UTC().Unix()
	for len(result) < wanted {
		if cursorEnd <= lowerBound {
			break
		}
		pageSize := min(lighterHistoryPageSize, wanted-len(result))
		query := url.Values{
			"market_id":       {strconv.FormatInt(marketID, 10)},
			"resolution":      {"1h"},
			"start_timestamp": {strconv.FormatInt(lowerBound, 10)},
			"end_timestamp":   {strconv.FormatInt(cursorEnd, 10)},
			"count_back":      {strconv.Itoa(pageSize)},
		}
		var page lighterFundings
		if err := l.client.get(ctx, "/api/v1/fundings", query, &page); err != nil {
			return nil, err
		}
		if len(page.Fundings) == 0 {
			break
		}
		var earliestRaw int64
		for _, item := range page.Fundings {
			timestamp, err := lighterFundingTimestamp(item.Timestamp)
			if err != nil {
				return nil, fmt.Errorf("lighter fundings timestamp: %w", err)
			}
			if earliestRaw == 0 || timestamp < earliestRaw {
				earliestRaw = timestamp
			}
			if timestamp < lowerBound || timestamp > cursorEnd {
				continue
			}
			if _, ok := seen[timestamp]; ok {
				continue
			}
			rate, err := lighterSettledHourlyRate(item.Rate, item.Direction)
			if err != nil {
				return nil, fmt.Errorf("lighter %s funding history: %w", instrument.ExchangeSymbol, err)
			}
			settledAt := seconds(timestamp)
			seen[timestamp] = struct{}{}
			result = append(result, FundingRate{
				Exchange: "lighter", ExchangeSymbol: instrument.ExchangeSymbol,
				Rate: rate, FundingTime: settledAt, Settled: true,
				IntervalHours: 1, SourceUpdatedAt: settledAt,
			})
		}
		if earliestRaw == 0 || earliestRaw <= lowerBound {
			break
		}
		nextEnd := earliestRaw - 1
		if nextEnd >= cursorEnd {
			break
		}
		cursorEnd = nextEnd
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("lighter fundings: empty history for %s: %w",
			instrument.ExchangeSymbol, ErrNoSettledHistory)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].FundingTime.Before(result[j].FundingTime)
	})
	return result, nil
}

var _ Adapter = (*Lighter)(nil)
var _ ContractTypeSupport = (*Lighter)(nil)
