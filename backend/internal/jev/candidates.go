package jev

import (
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

type CandidateInputs struct {
	Now      time.Time
	Rates    []Rate
	Spots    map[string]SpotInstrument
	Quotes   map[string]Quote
	Forecast func(long, short MarketKey) (BasisForecast, bool)
}

type CandidateSet struct {
	Single            []Candidate
	Cross             []Candidate
	SingleEmptyReason string
	CrossEmptyReason  string
	DegradedReasons   []string
	SingleStats       SingleFilterStats
	CrossStats        CrossFilterStats
}

func BuildEntryCandidates(cfg config.Jev, input CandidateInputs) CandidateSet {
	now := input.Now.UTC()
	single, singleStats := selectSingleCandidates(cfg, now, input.Rates, input.Spots, input.Quotes, input.Forecast)
	cross, crossStats := selectCrossCandidates(cfg, now, input.Rates, input.Quotes, input.Forecast)
	return CandidateSet{
		Single:            single,
		Cross:             cross,
		SingleEmptyReason: singleEmptyReason(singleStats),
		CrossEmptyReason:  crossEmptyReason(crossStats),
		SingleStats:       singleStats,
		CrossStats:        crossStats,
	}
}

func SelectFundingUniverse(cfg config.Jev, now time.Time, rates []Rate, spots map[string]SpotInstrument) (single []Candidate, cross []Candidate, singleStats SingleFilterStats, crossStats CrossFilterStats) {
	single, singleStats = selectSingleFunding(cfg, now, rates, spots)
	cross, crossStats = selectCrossFunding(cfg, now, rates)
	return single, cross, singleStats, crossStats
}

func AllEntryCandidates(set CandidateSet) []Candidate {
	out := make([]Candidate, 0, len(set.Single)+len(set.Cross))
	out = append(out, set.Single...)
	out = append(out, set.Cross...)
	return out
}

func rankByFunding(candidates []Candidate) []Candidate {
	out := append([]Candidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].FundingReturn24h.Equal(out[j].FundingReturn24h) {
			return out[i].FundingReturn24h.GreaterThan(out[j].FundingReturn24h)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func rankEntryCandidates(candidates []Candidate) []Candidate {
	out := append([]Candidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ExpectedNetReturn24h.Equal(out[j].ExpectedNetReturn24h) {
			return out[i].ExpectedNetReturn24h.GreaterThan(out[j].ExpectedNetReturn24h)
		}
		ci := decimalOrZero(out[i].BasisForecastConfidence)
		cj := decimalOrZero(out[j].BasisForecastConfidence)
		if !ci.Equal(cj) {
			return ci.GreaterThan(cj)
		}
		pi := decimalOrZero(out[i].BasisReturnP10)
		pj := decimalOrZero(out[j].BasisReturnP10)
		if !pi.Equal(pj) {
			return pi.GreaterThan(pj)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func capEntryCandidates(candidates []Candidate, limit int) []Candidate {
	ranked := rankEntryCandidates(candidates)
	if limit <= 0 || len(ranked) <= limit {
		return ranked
	}
	return ranked[:limit]
}

func selectSingleFunding(cfg config.Jev, now time.Time, rates []Rate, spots map[string]SpotInstrument) ([]Candidate, SingleFilterStats) {
	var stats SingleFilterStats
	stats.Input = len(rates)
	passed := make([]Candidate, 0)
	for _, rate := range rates {
		if !ordinaryCryptoPerp(rate) {
			continue
		}
		stats.ContractPassed++
		if rate.History24hComplete {
			stats.HistoryComplete++
		}
		if rateExpired(rate, now) {
			continue
		}
		if !rate.Cumulative24h.IsPositive() {
			continue
		}
		stats.Positive24hPassed++
		if !rateFresh(rate, now, cfg.RateMaxAge) {
			continue
		}
		stats.FreshPassed++
		if rate.Turnover24hUSD.LessThan(cfg.MinTurnoverUSD) {
			continue
		}
		stats.TurnoverPassed++
		if strings.TrimSpace(rate.BaseAsset) == "" || strings.TrimSpace(rate.QuoteAsset) == "" {
			continue
		}
		spot, ok := matchSpot(rate, spots, cfg.USDTUSDCOneToOne)
		if !ok {
			continue
		}
		stats.SpotMatched++
		candidate := Candidate{
			Key:                  SingleCandidateKey(rate.Exchange, spot.ExchangeSymbol, rate.ExchangeSymbol),
			Kind:                 KindSingle,
			BaseAsset:            strings.ToUpper(rate.BaseAsset),
			QuoteAsset:           strings.ToUpper(rate.QuoteAsset),
			GlobalSymbol:         rate.GlobalSymbol,
			LongExchange:         spot.Exchange,
			LongSymbol:           spot.ExchangeSymbol,
			LongProduct:          ProductSpot,
			LongCanonicalSymbol:  CanonicalSymbol(spot.BaseAsset, spot.QuoteAsset),
			LongQuoteAsset:       strings.ToUpper(spot.QuoteAsset),
			ShortExchange:        rate.Exchange,
			ShortSymbol:          rate.ExchangeSymbol,
			ShortProduct:         ProductPerpetual,
			ShortCanonicalSymbol: CanonicalSymbol(rate.BaseAsset, rate.QuoteAsset),
			ShortQuoteAsset:      strings.ToUpper(rate.QuoteAsset),
			Cumulative24hLong:    decimal.Zero,
			Cumulative24hShort:   rate.Cumulative24h,
			Edge24h:              rate.Cumulative24h,
			FundingReturn24h:     rate.Cumulative24h,
			CurrentFundingLong:   decimal.Zero,
			CurrentFundingShort:  rate.FundingRate,
			NextFundingShort:     rate.NextFundingRate,
			TurnoverLong:         decimal.Zero,
			TurnoverShort:        rate.Turnover24hUSD,
		}
		passed = append(passed, candidate)
	}
	selected := rankByFunding(passed)
	if len(selected) > TopEntryCount {
		selected = selected[:TopEntryCount]
	}
	return selected, stats
}

func selectCrossFunding(cfg config.Jev, now time.Time, rates []Rate) ([]Candidate, CrossFilterStats) {
	var stats CrossFilterStats
	stats.Input = len(rates)
	eligible := make([]Rate, 0)
	for _, rate := range rates {
		if !ordinaryCryptoPerp(rate) {
			continue
		}
		stats.ContractPassed++
		if rate.History24hComplete {
			stats.HistoryComplete++
		}
		if rateExpired(rate, now) {
			continue
		}
		if !rateFresh(rate, now, cfg.RateMaxAge) {
			continue
		}
		stats.FreshPassed++
		if rate.Turnover24hUSD.LessThan(cfg.MinTurnoverUSD) {
			continue
		}
		stats.TurnoverPassed++
		if strings.TrimSpace(rate.BaseAsset) == "" || strings.TrimSpace(rate.QuoteAsset) == "" {
			continue
		}
		eligible = append(eligible, rate)
	}
	groups := map[string][]Rate{}
	for _, rate := range eligible {
		groups[crossGroupKey(rate, cfg.USDTUSDCOneToOne)] = append(groups[crossGroupKey(rate, cfg.USDTUSDCOneToOne)], rate)
	}
	best := make([]Candidate, 0)
	positiveGroups := 0
	for _, members := range groups {
		candidate, ok := bestCrossPair(members)
		if !ok {
			continue
		}
		positiveGroups++
		best = append(best, candidate)
	}
	stats.PositiveEdgePassed = positiveGroups
	selected := rankByFunding(best)
	if len(selected) > TopEntryCount {
		selected = selected[:TopEntryCount]
	}
	return selected, stats
}

func bestCrossPair(members []Rate) (Candidate, bool) {
	var best Candidate
	found := false
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			left, right := members[i], members[j]
			if strings.EqualFold(left.Exchange, right.Exchange) {
				continue
			}
			longRate, shortRate := left, right
			if left.Cumulative24h.GreaterThan(right.Cumulative24h) {
				longRate, shortRate = right, left
			} else if !right.Cumulative24h.GreaterThan(left.Cumulative24h) {
				continue
			}
			edge := shortRate.Cumulative24h.Sub(longRate.Cumulative24h)
			if !edge.IsPositive() {
				continue
			}
			candidate := Candidate{
				Key:                  CrossCandidateKey(longRate.Exchange, longRate.ExchangeSymbol, shortRate.Exchange, shortRate.ExchangeSymbol),
				Kind:                 KindCross,
				BaseAsset:            strings.ToUpper(firstNonEmpty(longRate.BaseAsset, shortRate.BaseAsset)),
				QuoteAsset:           strings.ToUpper(firstNonEmpty(longRate.QuoteAsset, shortRate.QuoteAsset)),
				GlobalSymbol:         firstNonEmpty(longRate.GlobalSymbol, shortRate.GlobalSymbol),
				LongExchange:         longRate.Exchange,
				LongSymbol:           longRate.ExchangeSymbol,
				LongProduct:          ProductPerpetual,
				LongCanonicalSymbol:  CanonicalSymbol(longRate.BaseAsset, longRate.QuoteAsset),
				LongQuoteAsset:       strings.ToUpper(longRate.QuoteAsset),
				ShortExchange:        shortRate.Exchange,
				ShortSymbol:          shortRate.ExchangeSymbol,
				ShortProduct:         ProductPerpetual,
				ShortCanonicalSymbol: CanonicalSymbol(shortRate.BaseAsset, shortRate.QuoteAsset),
				ShortQuoteAsset:      strings.ToUpper(shortRate.QuoteAsset),
				Cumulative24hLong:    longRate.Cumulative24h,
				Cumulative24hShort:   shortRate.Cumulative24h,
				Edge24h:              edge,
				FundingReturn24h:     edge,
				CurrentFundingLong:   longRate.FundingRate,
				CurrentFundingShort:  shortRate.FundingRate,
				NextFundingLong:      longRate.NextFundingRate,
				NextFundingShort:     shortRate.NextFundingRate,
				TurnoverLong:         longRate.Turnover24hUSD,
				TurnoverShort:        shortRate.Turnover24hUSD,
			}
			if !found || candidate.FundingReturn24h.GreaterThan(best.FundingReturn24h) {
				best = candidate
				found = true
			}
		}
	}
	return best, found
}

func FilterByLatestBBO(cfg config.Jev, now time.Time, candidates []Candidate, quotes map[string]Quote, kind string) ([]Candidate, int, int, int) {
	required := RequiredDepthUSD(cfg)
	available, fresh, depth := 0, 0, 0
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		long, longOK := lookupQuote(quotes, candidate.LongExchange, candidate.LongCanonicalSymbol, candidate.LongProduct)
		short, shortOK := lookupQuote(quotes, candidate.ShortExchange, candidate.ShortCanonicalSymbol, candidate.ShortProduct)
		if !longOK || !shortOK || !long.Tradable() || !short.Tradable() {
			continue
		}
		available++
		if !quoteFreshTradable(long, now, cfg.BBOMaxAge) || !quoteFreshTradable(short, now, cfg.BBOMaxAge) {
			continue
		}
		fresh++
		if !directionalDepthOK(long, short, required) {
			continue
		}
		depth++
		candidate.LongQuote = long
		candidate.ShortQuote = short
		candidate.CurrentMidBasisBPS = MidBasisBPS(long, short)
		out = append(out, candidate)
	}
	_ = kind
	return out, available, fresh, depth
}

func FinalizeEntryCandidates(cfg config.Jev, now time.Time, candidates []Candidate, lookup func(long, short MarketKey) (BasisForecast, bool)) ([]Candidate, int, int) {
	basisPassed, paybackPassed := 0, 0
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		forecast, ok := lookup(candidate.LongKey(), candidate.ShortKey())
		if !ok || !forecast.OK {
			continue
		}
		age := now.Sub(forecast.CalculatedAt)
		candidate = attachForecast(candidate, forecast)
		candidate = ApplyLocalReturns(cfg, candidate)
		basisPassed++
		if !EntryFilterOK(cfg, candidate, age) {
			continue
		}
		paybackPassed++
		out = append(out, candidate)
	}
	return rankEntryCandidates(out), basisPassed, paybackPassed
}

func selectSingleCandidates(
	cfg config.Jev,
	now time.Time,
	rates []Rate,
	spots map[string]SpotInstrument,
	quotes map[string]Quote,
	forecast func(long, short MarketKey) (BasisForecast, bool),
) ([]Candidate, SingleFilterStats) {
	funding, stats := selectSingleFunding(cfg, now, rates, spots)
	if quotes == nil {
		stats.Final = len(funding)
		return funding, stats
	}
	filtered, available, fresh, depth := FilterByLatestBBO(cfg, now, funding, quotes, KindSingle)
	stats.BBOAvailable = available
	stats.BBOFreshPassed = fresh
	stats.DepthPassed = depth
	if forecast == nil {
		stats.Final = len(filtered)
		return filtered, stats
	}
	final, basisPassed, paybackPassed := FinalizeEntryCandidates(cfg, now, filtered, forecast)
	stats.BasisForecastPassed = basisPassed
	stats.PaybackPassed = paybackPassed
	stats.Final = len(final)
	return final, stats
}

func selectCrossCandidates(
	cfg config.Jev,
	now time.Time,
	rates []Rate,
	quotes map[string]Quote,
	forecast func(long, short MarketKey) (BasisForecast, bool),
) ([]Candidate, CrossFilterStats) {
	funding, stats := selectCrossFunding(cfg, now, rates)
	if quotes == nil {
		stats.Final = len(funding)
		return funding, stats
	}
	filtered, available, fresh, depth := FilterByLatestBBO(cfg, now, funding, quotes, KindCross)
	stats.BBOAvailable = available
	stats.BBOFreshPassed = fresh
	stats.DepthPassed = depth
	if forecast == nil {
		stats.Final = len(filtered)
		return filtered, stats
	}
	final, basisPassed, paybackPassed := FinalizeEntryCandidates(cfg, now, filtered, forecast)
	stats.BasisForecastPassed = basisPassed
	stats.PaybackPassed = paybackPassed
	stats.Final = len(final)
	return final, stats
}

func singleEmptyReason(stats SingleFilterStats) string {
	if stats.Final > 0 {
		return ""
	}
	switch {
	case stats.Input == 0:
		return "no_rates"
	case stats.ContractPassed == 0:
		return "no_supported_contract"
	case stats.Positive24hPassed == 0:
		return "no_positive_24h_rate"
	case stats.FreshPassed == 0:
		return "rate_stale"
	case stats.TurnoverPassed == 0:
		return "turnover_below_threshold"
	case stats.SpotMatched == 0:
		return "missing_spot_instrument"
	case stats.BBOAvailable == 0:
		return "bbo_unavailable"
	case stats.BBOFreshPassed == 0:
		return "bbo_unavailable"
	case stats.DepthPassed == 0:
		return "bbo_depth_insufficient"
	case stats.BasisForecastPassed == 0:
		return BasisHistoryInsufficient
	case stats.PaybackPassed == 0:
		return "non_positive_expected_net_return_24h"
	default:
		return "non_positive_expected_net_return_24h"
	}
}

func crossEmptyReason(stats CrossFilterStats) string {
	if stats.Final > 0 {
		return ""
	}
	switch {
	case stats.Input == 0:
		return "no_rates"
	case stats.ContractPassed == 0:
		return "no_supported_contract"
	case stats.PositiveEdgePassed == 0:
		return "no_positive_24h_edge"
	case stats.FreshPassed == 0:
		return "rate_stale"
	case stats.TurnoverPassed == 0:
		return "turnover_below_threshold"
	case stats.BBOAvailable == 0:
		return "bbo_unavailable"
	case stats.BBOFreshPassed == 0:
		return "bbo_unavailable"
	case stats.DepthPassed == 0:
		return "bbo_depth_insufficient"
	case stats.BasisForecastPassed == 0:
		return BasisHistoryInsufficient
	case stats.PaybackPassed == 0:
		return "non_positive_expected_net_return_24h"
	default:
		return "non_positive_expected_net_return_24h"
	}
}

func ordinaryCryptoPerp(rate Rate) bool {
	kind := strings.ToUpper(strings.TrimSpace(rate.VenueContractType))
	if kind == "" || kind == VenuePerpetual {
		return true
	}
	return false
}

func rateExpired(rate Rate, now time.Time) bool {
	if rate.NextFundingAt.IsZero() {
		return false
	}
	return !rate.NextFundingAt.After(now)
}

func rateFresh(rate Rate, now time.Time, maxAge time.Duration) bool {
	if rate.Stale || rate.SourceUpdatedAt.IsZero() || maxAge <= 0 {
		return false
	}
	age := now.Sub(rate.SourceUpdatedAt)
	return age >= 0 && age <= maxAge
}

func quoteFreshTradable(quote Quote, now time.Time, maxAge time.Duration) bool {
	return quote.Tradable() && quote.Fresh(now, maxAge)
}

func matchSpot(rate Rate, spots map[string]SpotInstrument, usdtUsdc bool) (SpotInstrument, bool) {
	base := strings.ToUpper(strings.TrimSpace(rate.BaseAsset))
	quote := strings.ToUpper(strings.TrimSpace(rate.QuoteAsset))
	exchange := strings.ToLower(strings.TrimSpace(rate.Exchange))
	if item, ok := spots[spotKey(exchange, base, quote)]; ok {
		return item, true
	}
	if usdtUsdc && (quote == "USDT" || quote == "USDC") {
		alt := "USDC"
		if quote == "USDC" {
			alt = "USDT"
		}
		if item, ok := spots[spotKey(exchange, base, alt)]; ok {
			return item, true
		}
	}
	return SpotInstrument{}, false
}

func lookupQuote(quotes map[string]Quote, exchange, canonical, product string) (Quote, bool) {
	quote, ok := quotes[NewCanonicalMarketKey(exchange, canonical, product).QuoteKey()]
	return quote, ok
}

func SingleCandidateKey(exchange, spotSymbol, perpSymbol string) string {
	return "single:" + strings.ToLower(exchange) + ":" + strings.ToUpper(spotSymbol) + ":" + strings.ToUpper(perpSymbol)
}

func CrossCandidateKey(longEx, longSym, shortEx, shortSym string) string {
	return "cross:" + strings.ToLower(longEx) + ":" + strings.ToUpper(longSym) + ":" +
		strings.ToLower(shortEx) + ":" + strings.ToUpper(shortSym)
}

func rateMapKey(exchange, symbol string) string {
	return strings.ToLower(exchange) + "\x00" + strings.ToUpper(symbol)
}

func spotKey(exchange, base, quote string) string {
	return strings.ToLower(exchange) + "\x00" + strings.ToUpper(base) + "\x00" + strings.ToUpper(quote)
}

func crossGroupKey(rate Rate, usdtUsdc bool) string {
	quote := strings.ToUpper(strings.TrimSpace(rate.QuoteAsset))
	if usdtUsdc && (quote == "USDT" || quote == "USDC") {
		quote = "USDT"
	}
	return strings.ToUpper(strings.TrimSpace(rate.BaseAsset)) + "\x00" + quote
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func candidateKeys(items []Candidate) []MarketKey {
	seen := map[string]MarketKey{}
	for _, item := range items {
		for _, key := range []MarketKey{item.LongKey(), item.ShortKey()} {
			if !key.CanonicalOK() {
				continue
			}
			seen[key.QuoteKey()] = key
		}
	}
	out := make([]MarketKey, 0, len(seen))
	for _, key := range seen {
		out = append(out, key)
	}
	return out
}
