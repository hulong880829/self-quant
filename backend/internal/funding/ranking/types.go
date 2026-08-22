package ranking

import (
	"errors"
	"strings"
	"time"

	"selfquant/backend/internal/funding"
)

var (
	ErrInvalidPeriod = errors.New("invalid ranking period")
	ErrInsufficient  = errors.New("insufficient ranking data")
)

type Period string

const (
	Period1h  Period = "1h"
	Period4h  Period = "4h"
	Period8h  Period = "8h"
	Period24h Period = "24h"
)

var Periods = []Period{Period1h, Period4h, Period8h, Period24h}

func ParsePeriod(value string) (Period, error) {
	switch Period(strings.ToLower(strings.TrimSpace(value))) {
	case Period1h, Period4h, Period8h, Period24h:
		return Period(strings.ToLower(strings.TrimSpace(value))), nil
	default:
		return "", ErrInvalidPeriod
	}
}

func (p Period) Horizon() time.Duration {
	switch p {
	case Period1h:
		return time.Hour
	case Period4h:
		return 4 * time.Hour
	case Period8h:
		return 8 * time.Hour
	case Period24h:
		return 24 * time.Hour
	default:
		return 0
	}
}

func (p Period) Lookback() time.Duration {
	switch p {
	case Period1h:
		return 24 * time.Hour
	case Period4h:
		return 3 * 24 * time.Hour
	case Period8h:
		return 5 * 24 * time.Hour
	case Period24h:
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

func (p Period) SimulationStep() time.Duration {
	switch p {
	case Period1h:
		return time.Minute
	case Period4h:
		return 5 * time.Minute
	case Period8h:
		return 10 * time.Minute
	case Period24h:
		return 30 * time.Minute
	default:
		return 0
	}
}

type Quote struct {
	TS     time.Time
	Symbol string
	Venue  string
	Bid    float64
	Ask    float64
}

func (q Quote) Mid() float64 { return (q.Bid + q.Ask) / 2 }

type Leg struct {
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

type Opportunity struct {
	Rank                       int
	GlobalSymbol               string
	BaseAsset                  string
	QuoteAsset                 string
	Period                     Period
	Long                       Leg
	Short                      Leg
	CurrentMidSpreadBPS        float64
	CurrentExecutableSpreadBPS float64
	TargetSpreadBPS            float64
	PeriodExpectedReturn       float64
	FundingExpectedAnnualized  float64
	SpreadExpectedAnnualized   float64
	CombinedExpectedAnnualized float64
	FirstPassageProbability    float64
	ProfitProbability          float64
	ExpectedExitMinutes        float64
	P5Return                   float64
	MinPositionNotionalUSD     float64
	MinTurnover24hUSD          float64
	Coverage                   float64
	Confidence                 float64
	ModelState                 string
	UpdatedAt                  time.Time
	Stale                      bool
}

type Snapshot struct {
	Period       Period
	Items        []Opportunity
	Version      string
	CalculatedAt time.Time
	Stale        bool
}

func legFromRate(rate funding.Rate) Leg {
	effective := rate.Rate
	if rate.NextRate != nil {
		effective = *rate.NextRate
	}
	return Leg{
		Exchange: rate.Exchange, ExchangeSymbol: rate.ExchangeSymbol,
		EffectiveRate: effective, IntervalHours: rate.IntervalHours,
		NextFundingAt: rate.FundingTime, PositionNotionalUSD: rate.PositionNotionalUSD,
		Turnover24hUSD: rate.Turnover24hUSD, LastPrice: rate.LastPrice,
		SourceUpdatedAt: rate.SourceUpdatedAt,
	}
}
