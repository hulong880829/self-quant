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
	EffectiveRate       float64
	IntervalHours       float64
	NextFundingAt       time.Time
	PositionNotionalUSD float64
	Turnover24hUSD      float64
	LastPrice           float64
	SourceUpdatedAt     time.Time
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
}

// BuildSpreads creates one directional spread for each distinct exchange pair
// sharing the exact same global symbol.
func BuildSpreads(rates []Rate) []Spread {
	groups := make(map[string][]Rate)
	for _, rate := range rates {
		if rate.GlobalSymbol == "" || rate.Exchange == "" || rate.IntervalHours <= 0 {
			continue
		}
		groups[rate.GlobalSymbol] = append(groups[rate.GlobalSymbol], rate)
	}

	symbols := make([]string, 0, len(groups))
	for symbol := range groups {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)

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
		QuoteAsset:   longRate.QuoteAsset,
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
		EffectiveRate:       effective,
		IntervalHours:       rate.IntervalHours,
		NextFundingAt:       rate.FundingTime,
		PositionNotionalUSD: rate.PositionNotionalUSD,
		Turnover24hUSD:      rate.Turnover24hUSD,
		LastPrice:           rate.LastPrice,
		SourceUpdatedAt:     rate.SourceUpdatedAt,
	}
}

func olderTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
