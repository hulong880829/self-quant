package ranking

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"selfquant/backend/internal/funding"
)

const (
	defaultHistoryRows = 12_000_000
	quoteBytesEstimate = 24
)

var recordedRankingVenues = map[string]struct{}{
	"binance": {}, "okx": {}, "bybit": {}, "bitget": {}, "gate": {},
}

type HistoryGeneration struct {
	ID          uint64
	Series      map[string]map[string][]MinuteQuote
	Rows        int
	Bytes       int64
	From        time.Time
	DataThrough time.Time
	BuiltAt     time.Time
	Degraded    bool
	LastError   string
}

type MinuteQuote struct {
	Minute int64
	Bid    float64
	Ask    float64
}

type HistoryCache struct {
	source    HistoryStore
	retention time.Duration
	maxRows   int
	updateMu  sync.Mutex
	current   atomic.Pointer[HistoryGeneration]
}

type warmHistoryStore interface {
	QueryWarm(context.Context, time.Time, time.Time, []string, []string) ([]Quote, error)
}

func NewHistoryCache(source HistoryStore, retention time.Duration, maxRows int) *HistoryCache {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if maxRows <= 0 {
		maxRows = defaultHistoryRows
	}
	return &HistoryCache{source: source, retention: retention, maxRows: maxRows}
}

func (c *HistoryCache) Close() error {
	if c == nil || c.source == nil {
		return nil
	}
	return c.source.Close()
}

func (c *HistoryCache) Generation() *HistoryGeneration {
	if generation := c.current.Load(); generation != nil {
		return generation
	}
	return &HistoryGeneration{}
}

func (c *HistoryCache) Update(
	ctx context.Context, rates []funding.Rate, now time.Time, lookback time.Duration,
) error {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	if lookback <= 0 || lookback > c.retention {
		lookback = c.retention
	}
	symbols, venues := cacheUniverse(rates)
	if len(symbols) == 0 || len(venues) == 0 {
		return fmt.Errorf("%w: no recorded cross-venue perpetual universe", ErrInsufficient)
	}
	previous := c.current.Load()
	from := now.Add(-lookback)
	fullLoad := previous == nil || previous.Rows == 0 || previous.From.IsZero() ||
		previous.From.After(from.Add(time.Minute))
	if !fullLoad && previous != nil && !previous.DataThrough.IsZero() {
		from = previous.DataThrough.Add(-time.Minute)
	}
	to := now.Truncate(time.Minute)
	var quotes []Quote
	var err error
	if warmer, ok := c.source.(warmHistoryStore); ok && fullLoad {
		quotes, err = warmer.QueryWarm(ctx, from, to, symbols, venues)
	} else {
		quotes, err = c.source.Query(ctx, from, to, symbols, venues)
	}
	if err != nil {
		c.publishDegraded(previous, now, err)
		return err
	}
	coverageFrom := from
	if previous != nil && !previous.From.IsZero() && previous.From.Before(coverageFrom) {
		coverageFrom = previous.From
	}
	next := mergeHistory(
		previous, quotes, now.Add(-c.retention), coverageFrom, now, symbols, venues,
	)
	if next.Rows > c.maxRows {
		err = fmt.Errorf("history cache row limit exceeded: %d > %d", next.Rows, c.maxRows)
		c.publishDegraded(previous, now, err)
		return err
	}
	c.current.Store(next)
	return nil
}

func (c *HistoryCache) publishDegraded(previous *HistoryGeneration, now time.Time, err error) {
	next := &HistoryGeneration{ID: 1, BuiltAt: now.UTC(), Degraded: true, LastError: err.Error()}
	if previous != nil {
		*next = *previous
		next.ID++
		next.BuiltAt = now.UTC()
		next.Degraded = true
		next.LastError = err.Error()
	}
	c.current.Store(next)
}

func (c *HistoryCache) Query(
	ctx context.Context, from, to time.Time, symbols, venues []string,
) ([]Quote, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index, _, err := c.SnapshotRange(from, to, symbols, venues)
	if err != nil {
		return nil, err
	}
	var result []Quote
	for symbol, byVenue := range index {
		for venue, quotes := range byVenue {
			for _, quote := range quotes {
				result = append(result, Quote{
					TS: time.Unix(quote.Minute*60, 0).UTC(), Symbol: symbol, Venue: venue,
					Bid: quote.Bid, Ask: quote.Ask,
				})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Symbol != result[j].Symbol {
			return result[i].Symbol < result[j].Symbol
		}
		if result[i].Venue != result[j].Venue {
			return result[i].Venue < result[j].Venue
		}
		return result[i].TS.Before(result[j].TS)
	})
	return result, nil
}

func (c *HistoryCache) SnapshotRange(
	from, to time.Time, symbols, venues []string,
) (map[string]map[string][]MinuteQuote, *HistoryGeneration, error) {
	generation := c.current.Load()
	if generation == nil || generation.Rows == 0 {
		return nil, generation, fmt.Errorf("%w: history cache is warming", ErrInsufficient)
	}
	symbolSet := stringSet(symbols)
	venueSet := stringSet(venues)
	result := make(map[string]map[string][]MinuteQuote, len(symbolSet))
	fromMinute, toMinute := from.Unix()/60, to.Unix()/60
	for symbol, byVenue := range generation.Series {
		if _, ok := symbolSet[symbol]; !ok {
			continue
		}
		for venue, quotes := range byVenue {
			if _, ok := venueSet[venue]; !ok {
				continue
			}
			start := sort.Search(
				len(quotes), func(i int) bool { return quotes[i].Minute >= fromMinute },
			)
			end := sort.Search(
				len(quotes), func(i int) bool { return quotes[i].Minute >= toMinute },
			)
			if start == end {
				continue
			}
			if result[symbol] == nil {
				result[symbol] = make(map[string][]MinuteQuote)
			}
			result[symbol][venue] = quotes[start:end]
		}
	}
	return result, generation, nil
}

func mergeHistory(
	previous *HistoryGeneration, updates []Quote, cutoff, coverageFrom, now time.Time,
	symbols, venues []string,
) *HistoryGeneration {
	allowedSymbols := stringSet(symbols)
	allowedVenues := stringSet(venues)
	pending := make(map[string]map[string]map[int64]Quote)
	for _, quote := range updates {
		if quote.TS.Before(cutoff) {
			continue
		}
		if _, ok := allowedSymbols[quote.Symbol]; !ok {
			continue
		}
		if _, ok := allowedVenues[quote.Venue]; !ok {
			continue
		}
		if pending[quote.Symbol] == nil {
			pending[quote.Symbol] = make(map[string]map[int64]Quote)
		}
		if pending[quote.Symbol][quote.Venue] == nil {
			pending[quote.Symbol][quote.Venue] = make(map[int64]Quote)
		}
		pending[quote.Symbol][quote.Venue][quote.TS.Unix()/60] = quote
	}
	series := make(map[string]map[string][]MinuteQuote, len(allowedSymbols))
	count := 0
	var lastMinute int64
	if previous != nil {
		for symbol, byVenue := range previous.Series {
			if _, ok := allowedSymbols[symbol]; !ok {
				continue
			}
			nextByVenue := make(map[string][]MinuteQuote, len(byVenue))
			for venue, quotes := range byVenue {
				if _, ok := allowedVenues[venue]; !ok {
					continue
				}
				start := sort.Search(
					len(quotes), func(i int) bool { return quotes[i].Minute >= cutoff.Unix()/60 },
				)
				quotes = quotes[start:]
				if byMinute := pending[symbol][venue]; len(byMinute) > 0 {
					quotes = mergeMinuteSeries(quotes, byMinute)
					delete(pending[symbol], venue)
				}
				if len(quotes) > 0 {
					nextByVenue[venue] = quotes
					count += len(quotes)
					if quotes[len(quotes)-1].Minute > lastMinute {
						lastMinute = quotes[len(quotes)-1].Minute
					}
				}
			}
			if len(nextByVenue) > 0 {
				series[symbol] = nextByVenue
			}
		}
	}
	for symbol, byVenue := range pending {
		for venue, byMinute := range byVenue {
			if len(byMinute) == 0 {
				continue
			}
			quotes := mergeMinuteSeries(nil, byMinute)
			if series[symbol] == nil {
				series[symbol] = make(map[string][]MinuteQuote)
			}
			series[symbol][venue] = quotes
			count += len(quotes)
			if quotes[len(quotes)-1].Minute > lastMinute {
				lastMinute = quotes[len(quotes)-1].Minute
			}
		}
	}
	if coverageFrom.Before(cutoff) {
		coverageFrom = cutoff
	}
	id := uint64(1)
	if previous != nil {
		id = previous.ID + 1
	}
	return &HistoryGeneration{
		ID: id, Series: series, Rows: count, Bytes: int64(count * quoteBytesEstimate),
		From: coverageFrom.UTC(), DataThrough: time.Unix(lastMinute*60, 0).UTC(),
		BuiltAt: now.UTC(),
	}
}

func mergeMinuteSeries(existing []MinuteQuote, updates map[int64]Quote) []MinuteQuote {
	incoming := make([]MinuteQuote, 0, len(updates))
	for minute, quote := range updates {
		incoming = append(incoming, MinuteQuote{Minute: minute, Bid: quote.Bid, Ask: quote.Ask})
	}
	sort.Slice(incoming, func(i, j int) bool { return incoming[i].Minute < incoming[j].Minute })
	result := make([]MinuteQuote, 0, len(existing)+len(incoming))
	left, right := 0, 0
	for left < len(existing) && right < len(incoming) {
		existingMinute := existing[left].Minute
		incomingMinute := incoming[right].Minute
		switch {
		case existingMinute < incomingMinute:
			result = append(result, existing[left])
			left++
		case existingMinute > incomingMinute:
			result = append(result, incoming[right])
			right++
		default:
			result = append(result, incoming[right])
			left++
			right++
		}
	}
	result = append(result, existing[left:]...)
	result = append(result, incoming[right:]...)
	return result
}

func cacheUniverse(rates []funding.Rate) ([]string, []string) {
	venuesBySymbol := make(map[string]map[string]struct{})
	for _, rate := range rates {
		if _, ok := recordedRankingVenues[rate.Exchange]; !ok || rate.GlobalSymbol == "" {
			continue
		}
		if venuesBySymbol[rate.GlobalSymbol] == nil {
			venuesBySymbol[rate.GlobalSymbol] = make(map[string]struct{})
		}
		venuesBySymbol[rate.GlobalSymbol][rate.Exchange] = struct{}{}
	}
	var symbols, venues []string
	for symbol, symbolVenues := range venuesBySymbol {
		if len(symbolVenues) < 2 {
			continue
		}
		symbols = append(symbols, symbol)
		for venue := range symbolVenues {
			venues = append(venues, venue)
		}
	}
	return uniqueSorted(symbols), uniqueSorted(venues)
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
