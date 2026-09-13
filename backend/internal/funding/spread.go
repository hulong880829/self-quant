package funding

import (
	"math"
	"sort"
	"time"
)

// SpreadLeg is one side of a cross-exchange funding spread.
type SpreadLeg struct {
	Exchange            string
	ExchangeSymbol      string
	GlobalSymbol        string
	BaseAsset           string
	QuoteAsset          string
	EffectiveRate       float64
	IntervalHours       float64
	NextFundingAt       time.Time
	PositionNotionalUSD float64
	Turnover24hUSD      float64
	LastPrice           float64
	SourceUpdatedAt     time.Time
	History24hComplete  *bool
	History7dComplete   *bool
	VenueContractType   string
}

// Spread is a directional cross-exchange funding opportunity.
type Spread struct {
	GlobalSymbol           string
	BaseAsset              string
	QuoteAsset             string
	Long                   SpreadLeg
	Short                  SpreadLeg
	SingleAnnualized       float64
	Annualized24h          float64
	Annualized7d           float64
	MinPositionNotionalUSD float64
	MinTurnover24hUSD      float64
	UpdatedAt              time.Time
	History24hComplete     *bool
	History7dComplete      *bool
}

// BuildSpreads creates one directional spread for each compatible exchange pair.
func BuildSpreads(rates []Rate) []Spread {
	groups := make(map[string][]Rate)
	for _, rate := range rates {
		if rate.GlobalSymbol == "" || rate.Exchange == "" || rate.IntervalHours <= 0 {
			continue
		}
		for _, symbol := range PairingSymbols(rate) {
			groups[symbol] = append(groups[symbol], rate)
		}
	}

	symbols := make([]string, 0, len(groups))
	for symbol := range groups {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)

	seen := make(map[string]struct{})
	var spreads []Spread
	for _, symbol := range symbols {
		group := groups[symbol]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].Exchange == group[j].Exchange {
				return group[i].ExchangeSymbol < group[j].ExchangeSymbol
			}
			return group[i].Exchange < group[j].Exchange
		})

		unique := make([]Rate, 0, len(group))
		for _, rate := range group {
			if len(unique) == 0 || unique[len(unique)-1].Exchange != rate.Exchange {
				unique = append(unique, rate)
			}
		}
		for i := 0; i < len(unique); i++ {
			for j := i + 1; j < len(unique); j++ {
				if !PairableRates(unique[i], unique[j]) {
					continue
				}
				key := canonicalSpreadKey(unique[i], unique[j])
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				spreads = append(spreads, buildSpread(symbol, unique[i], unique[j]))
			}
		}
	}
	return spreads
}

func buildSpread(symbol string, first, second Rate) Spread {
	firstEffective := effectiveRate(first)
	secondEffective := effectiveRate(second)
	longRate, shortRate := first, second
	longEffective, shortEffective := firstEffective, secondEffective
	if firstEffective/first.IntervalHours > secondEffective/second.IntervalHours {
		longRate, shortRate = second, first
		longEffective, shortEffective = secondEffective, firstEffective
	}

	return Spread{
		GlobalSymbol: symbol,
		BaseAsset:    longRate.BaseAsset,
		QuoteAsset:   PairingQuoteAsset(longRate),
		Long:         spreadLeg(longRate, longEffective),
		Short:        spreadLeg(shortRate, shortEffective),
		SingleAnnualized: (shortEffective/shortRate.IntervalHours -
			longEffective/longRate.IntervalHours) * 24 * 365,
		Annualized24h: (shortRate.Cumulative24h - longRate.Cumulative24h) * 365,
		Annualized7d:  (shortRate.Cumulative7d - longRate.Cumulative7d) * 365 / 7,
		MinPositionNotionalUSD: math.Min(
			longRate.PositionNotionalUSD,
			shortRate.PositionNotionalUSD,
		),
		MinTurnover24hUSD: math.Min(longRate.Turnover24hUSD, shortRate.Turnover24hUSD),
		UpdatedAt:         olderTime(longRate.SourceUpdatedAt, shortRate.SourceUpdatedAt),
		History24hComplete: combineCoverage(
			longRate.History24hComplete, shortRate.History24hComplete,
		),
		History7dComplete: combineCoverage(
			longRate.History7dComplete, shortRate.History7dComplete,
		),
	}
}

func effectiveRate(rate Rate) float64 {
	if rate.NextRate != nil {
		return *rate.NextRate
	}
	return rate.Rate
}

func spreadLeg(rate Rate, effective float64) SpreadLeg {
	return SpreadLeg{
		Exchange:            rate.Exchange,
		ExchangeSymbol:      rate.ExchangeSymbol,
		GlobalSymbol:        rate.GlobalSymbol,
		BaseAsset:           rate.BaseAsset,
		QuoteAsset:          rate.QuoteAsset,
		EffectiveRate:       effective,
		IntervalHours:       rate.IntervalHours,
		NextFundingAt:       rate.FundingTime,
		PositionNotionalUSD: rate.PositionNotionalUSD,
		Turnover24hUSD:      rate.Turnover24hUSD,
		LastPrice:           rate.LastPrice,
		SourceUpdatedAt:     rate.SourceUpdatedAt,
		History24hComplete:  rate.History24hComplete,
		History7dComplete:   rate.History7dComplete,
		VenueContractType:   rate.VenueContractType,
	}
}

func combineCoverage(left, right *bool) *bool {
	if left == nil && right == nil {
		return nil
	}
	ok := (left == nil || *left) && (right == nil || *right)
	return boolPtr(ok)
}

func boolPtr(value bool) *bool { return &value }

func olderTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
