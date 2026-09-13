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
	Name               string      `json:"name"`
	InDelisting        bool        `json:"in_delisting"`
	FundingRate        string      `json:"funding_rate"`
	FundingNextApply   int64       `json:"funding_next_apply"`
	FundingInterval    int64       `json:"funding_interval"`
	QuantoMultiplier   string      `json:"quanto_multiplier"`
	OrderPriceRound    string      `json:"order_price_round"`
	OrderSizeRound     string      `json:"order_size_round"`
	OrderSizeMin       bybitString `json:"order_size_min"`
	OrderSizeMax       bybitString `json:"order_size_max"`
	MarketOrderSizeMax bybitString `json:"market_order_size_max"`
	EnableDecimal      bool        `json:"enable_decimal"`
}

type gateSpotPair struct {
	ID              string `json:"id"`
	Base            string `json:"base"`
	Quote           string `json:"quote"`
	TradeStatus     string `json:"trade_status"`
	Precision       int    `json:"precision"`
	AmountPrecision int    `json:"amount_precision"`
	MinBaseAmount   string `json:"min_base_amount"`
	MinQuoteAmount  string `json:"min_quote_amount"`
}

func parseGateSpotInstruments(items []gateSpotPair) []Instrument {
	result := make([]Instrument, 0, len(items))
	for _, item := range items {
		if item.TradeStatus != "tradable" {
			continue
		}
		metadata, _ := json.Marshal(item)
		minQuantity, minQuantityStatus := knownConstraint(item.MinBaseAmount)
		minNotional, minNotionalStatus := knownConstraint(item.MinQuoteAmount)
		marketStep, marketStepStatus := knownDecimalConstraint(
			strconv.FormatFloat(
				precisionStep(strconv.Itoa(item.AmountPrecision)), 'f', -1, 64,
			),
		)
		marketMinQuantity, marketMinQuantityStatus := knownDecimalConstraint(item.MinBaseAmount)
		marketMinNotional, marketMinNotionalStatus := knownDecimalConstraint(item.MinQuoteAmount)
		result = append(result, Instrument{
			Exchange: "gate", ExchangeSymbol: item.ID,
			BaseAsset: item.Base, QuoteAsset: item.Quote,
			GlobalSymbol: GlobalSymbol(item.Base, item.Quote),
			SettleAsset:  item.Quote, ContractType: ContractTypeSpot,
			Status: "active", ContractSize: 1,
			PriceTick:    precisionStep(strconv.Itoa(item.Precision)),
			QuantityStep: precisionStep(strconv.Itoa(item.AmountPrecision)),
			MinQuantity:  minQuantity, MinNotional: minNotional,
			MinQuantityStatus: minQuantityStatus, MinNotionalStatus: minNotionalStatus,
			MaxQuantityStatus:  ConstraintNotApplicable,
			MarketQuantityStep: marketStep, MarketQuantityStepStatus: marketStepStatus,
			MarketMinQuantity: marketMinQuantity, MarketMinQuantityStatus: marketMinQuantityStatus,
			MarketMaxQuantityStatus: ConstraintNotApplicable,
			MarketMinNotional:       marketMinNotional, MarketMinNotionalStatus: marketMinNotionalStatus,
			Metadata: metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func parseGateInstruments(items []gateContract) []Instrument {
	return parseGateSettlementInstruments(items, "USDT")
}

func parseGateSettlementInstruments(items []gateContract, settle string) []Instrument {
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
		minQuantity, minQuantityStatus := knownConstraint(string(item.OrderSizeMin))
		maxQuantity, maxQuantityStatus := maximumDecimalConstraint(string(item.OrderSizeMax))
		marketMaxQuantity, marketMaxQuantityStatus := maximumDecimalConstraint(
			string(item.MarketOrderSizeMax),
		)
		venueStep := item.OrderSizeRound
		if venueStep == "" {
			venueStep = "1"
		}
		marketStep, marketStepStatus := knownDecimalConstraint(venueStep)
		marketMinQuantity, marketMinQuantityStatus := knownDecimalConstraint(
			string(item.OrderSizeMin),
		)
		model := "linear"
		if !strings.EqualFold(settle, "USDT") {
			model = "inverse"
		}
		metadata := instrumentMetadata(item, model, "contracts")
		result = append(result, Instrument{
			Exchange: "gate", ExchangeSymbol: item.Name,
			BaseAsset: base, QuoteAsset: quote, GlobalSymbol: GlobalSymbol(base, quote),
			IntervalHours: interval, SettleAsset: strings.ToUpper(settle),
			ContractType: "perpetual", Status: "active", ContractSize: size,
			PriceTick: tick, QuantityStep: mustParsePositiveFloat(venueStep, 1),
			MinQuantity: minQuantity, MinQuantityStatus: minQuantityStatus,
			MinNotionalStatus: ConstraintNotApplicable,
			MaxQuantity:       maxQuantity, MaxQuantityStatus: maxQuantityStatus,
			MarketQuantityStep: marketStep, MarketQuantityStepStatus: marketStepStatus,
			MarketMinQuantity: marketMinQuantity, MarketMinQuantityStatus: marketMinQuantityStatus,
			MarketMaxQuantity: marketMaxQuantity, MarketMaxQuantityStatus: marketMaxQuantityStatus,
			MarketMinNotionalStatus: ConstraintNotApplicable,
			Metadata:                metadata, SourceUpdatedAt: time.Now().UTC(),
		})
	}
	return result
}

func (g *Gate) SyncInstruments(ctx context.Context, contractType string) ([]Instrument, error) {
	if contractType == ContractTypeSpot {
		var payload []gateSpotPair
		if err := g.client.get(ctx, "/api/v4/spot/currency_pairs", nil, &payload); err != nil {
			return nil, err
		}
		return parseGateSpotInstruments(payload), nil
	}
	if contractType != ContractTypePerpetual {
		return nil, fmt.Errorf("gate: unsupported contract type %q", contractType)
	}
	var all []Instrument
	for _, settle := range []string{"usdt", "btc"} {
		var payload []gateContract
		if err := g.client.get(ctx, "/api/v4/futures/"+settle+"/contracts", nil, &payload); err != nil {
			return nil, err
		}
		all = append(all, parseGateSettlementInstruments(payload, settle)...)
	}
	return all, nil
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
		contractSize, _ := parseFloat(item.QuantoMultiplier)
		var oiBase, oiNotional float64
		if contractSize > 0 {
			oiBase = oi * contractSize
			oiNotional = oiBase * markPrice
		}
		volume, _ := parseFloat(ticker.Volume24hBase)
		turnover, _ := parseFloat(ticker.Volume24hQuote)
		change, _ := parseFloat(ticker.ChangePercentage)
		result = append(result, FundingRate{
			Exchange: "gate", ExchangeSymbol: item.Name, Rate: rate,
			FundingTime: seconds(item.FundingNextApply), IntervalHours: interval,
			NextRate: nextRate, MarkPrice: markPrice, IndexPrice: indexPrice,
			LastPrice: lastPrice, OpenInterestContracts: oi,
			OpenInterestBase: oiBase, OpenInterestNotionalUSD: oiNotional,
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
	wanted := clampLimit(limit, 10000)
	result := make([]FundingRate, 0, wanted)
	from := since.Unix()
	if since.IsZero() {
		from = time.Now().AddDate(-1, 0, 0).Unix()
	}
	now := time.Now().UTC()
	// Gate rejects a start time at or beyond its 180-day boundary. Keep one
	// day of margin so request latency cannot move the boundary past `from`.
	earliest := now.Add(-179 * 24 * time.Hour).Unix()
	if from < earliest {
		from = earliest
	}
	interval := time.Duration(instrument.IntervalHours * float64(time.Hour))
	if interval <= 0 {
		interval = 8 * time.Hour
	}
	// Gate rejects broad from/to ranges and caps a page at 100 records.
	// Keep each time window below 90 expected settlements.
	window := 90 * interval
	for len(result) < wanted && from < now.Unix() {
		to := min(from+int64(window/time.Second), now.Unix())
		query := url.Values{
			"contract": {instrument.ExchangeSymbol},
			"limit":    {"100"},
			"from":     {strconv.FormatInt(from, 10)},
			"to":       {strconv.FormatInt(to, 10)},
		}
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
			if len(result) == wanted {
				break
			}
		}
		from = to + 1
	}
	return result, nil
}
