package funding

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"selfquant/backend/internal/exchange"
	"selfquant/backend/internal/funding/ibkr"
)

const (
	tradefiLookupInterval = 2 * time.Second
	tradefiLookupAttempts = 3
	tradefiActiveTTL      = 24 * time.Hour
	tradefiNegativeTTL    = 6 * time.Hour
)

type ContractLookup interface {
	Lookup(context.Context, ibkr.Query) ([]ibkr.Contract, error)
}

type ibCatalog interface {
	ListIBInstrumentResolutions(context.Context) ([]IBResolution, error)
	UpsertIBInstrumentResolution(context.Context, IBResolution) (IBResolution, error)
}

type queuedJob struct {
	key         string
	ref         exchange.UnderlyingReference
	refs        map[string]exchange.UnderlyingReference
	instruments []exchange.Instrument
}

type TradefiService struct {
	catalog    ibCatalog
	lookup     ContractLookup
	funding    *SnapshotStore
	snapshots  *TradefiSnapshotStore
	logger     *slog.Logger
	interval   time.Duration
	attempts   int
	mu         sync.Mutex
	cache      map[string]IBResolution
	lastLookup time.Time
	async      bool
	wake       chan struct{}
	queue      []queuedJob
	inflight   map[string]bool
	follow     map[string]queuedJob
	tradable   map[string]bool
}

func NewTradefiService(
	catalog ibCatalog,
	funding *SnapshotStore,
	lookup ContractLookup,
	snapshots *TradefiSnapshotStore,
	logger *slog.Logger,
) *TradefiService {
	if snapshots == nil {
		snapshots = NewTradefiSnapshotStore()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if funding == nil {
		funding = NewSnapshotStore()
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &TradefiService{
		catalog: catalog, lookup: lookup, funding: funding, snapshots: snapshots,
		logger: logger, interval: tradefiLookupInterval, attempts: tradefiLookupAttempts,
		cache: make(map[string]IBResolution), wake: make(chan struct{}, 1),
		inflight: map[string]bool{}, follow: map[string]queuedJob{}, tradable: map[string]bool{},
	}
}

func (s *TradefiService) Snapshots() *TradefiSnapshotStore { return s.snapshots }

func (s *TradefiService) SetTradableVenues(venues map[string]bool) {
	if s == nil {
		return
	}
	next := map[string]bool{}
	for name, enabled := range venues {
		if enabled {
			next[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	s.mu.Lock()
	s.tradable = next
	s.mu.Unlock()
}

func (s *TradefiService) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.async = true
	s.mu.Unlock()
	go s.loop(ctx)
}

func (s *TradefiService) Restore(ctx context.Context) error {
	if s == nil || s.catalog == nil {
		return nil
	}
	items, err := s.catalog.ListIBInstrumentResolutions(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	legacy := make([]IBResolution, 0)
	for _, item := range items {
		if item.PairMatches == nil {
			item.PairMatches = map[string]PairMatch{}
		}
		if item.ConID != nil && *item.ConID > 0 {
			s.remember(item)
			continue
		}
		legacy = append(legacy, item)
	}
	for _, item := range legacy {
		if existing, exists := s.cache[item.Symbol]; exists && existing.Symbol != item.Symbol {
			continue
		}
		s.remember(item)
	}
	return nil
}

func (s *TradefiService) Reconcile(ctx context.Context, instruments []exchange.Instrument) error {
	if s == nil || s.lookup == nil {
		return nil
	}
	grouped := map[string][]exchange.Instrument{}
	counts := map[string]int{}
	for _, item := range instruments {
		if item.ContractType != exchange.ContractTypePerpetual || !strings.EqualFold(item.Status, "active") {
			continue
		}
		ref := item.Underlying.Normalized()
		if ref.AssetClass == "" {
			continue
		}
		counts[item.Exchange+"/"+ref.AssetClass]++
		if !ref.Routable() {
			continue
		}
		grouped[ref.CacheKey()] = append(grouped[ref.CacheKey()], item)
	}
	for key, items := range grouped {
		refs := map[string]exchange.UnderlyingReference{}
		for _, item := range items {
			refs[pairKey(item.Exchange, item.ExchangeSymbol)] = item.Underlying.Normalized()
		}
		s.enqueue(queuedJob{key: key, ref: items[0].Underlying.Normalized(), refs: refs, instruments: items})
	}
	if !s.worker() {
		s.drain(ctx)
	}
	s.logSummary(counts)
	return nil
}

func (s *TradefiService) worker() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.async
}

func (s *TradefiService) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.drain(ctx)
		}
	}
}

func (s *TradefiService) enqueue(job queuedJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[job.key] {
		s.follow[job.key] = mergeJob(s.follow[job.key], job)
		return
	}
	for index := range s.queue {
		if s.queue[index].key == job.key {
			s.queue[index] = mergeJob(s.queue[index], job)
			return
		}
	}
	s.queue = append(s.queue, job)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func mergeJob(current, extra queuedJob) queuedJob {
	if current.key == "" {
		return extra
	}
	current.instruments = append(current.instruments, extra.instruments...)
	if current.refs == nil {
		current.refs = map[string]exchange.UnderlyingReference{}
	}
	for key, ref := range extra.refs {
		current.refs[key] = ref
	}
	return current
}

func (s *TradefiService) drain(ctx context.Context) {
	for {
		job, ok := s.pop()
		if !ok {
			s.PublishFunding(s.funding.View().Rates)
			return
		}
		if err := s.resolve(ctx, job); err != nil {
			s.logger.Warn("ib underlying lookup failed", "key", job.key, "error", err)
		}
		s.finish(job.key)
		s.PublishFunding(s.funding.View().Rates)
	}
}

func (s *TradefiService) pop() (queuedJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return queuedJob{}, false
	}
	job := s.queue[0]
	s.queue = s.queue[1:]
	s.inflight[job.key] = true
	return job, true
}

func (s *TradefiService) finish(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	if job, ok := s.follow[key]; ok {
		delete(s.follow, key)
		s.queue = append(s.queue, job)
	}
}

func (s *TradefiService) PublishFunding(rates []Rate) {
	if s == nil {
		return
	}
	s.mu.Lock()
	resolutions := canonicalOwnerResolutions(s.cache)
	s.mu.Unlock()
	s.snapshots.Replace(BuildTradefiRows(resolutions, rates, s.venues()), time.Now().UTC())
}

func (s *TradefiService) venues() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := map[string]bool{}
	for name, enabled := range s.tradable {
		copy[name] = enabled
	}
	return copy
}

func canonicalOwnerResolutions(cache map[string]IBResolution) []IBResolution {
	result := make([]IBResolution, 0)
	for cacheKey, item := range cache {
		if cacheKey != item.Symbol {
			continue
		}
		result = append(result, cloneIBResolution(item))
	}
	return result
}

func cloneIBResolution(item IBResolution) IBResolution {
	clone := item
	if item.ConID != nil {
		conID := *item.ConID
		clone.ConID = &conID
	}
	if item.PairMatches != nil {
		clone.PairMatches = make(map[string]PairMatch, len(item.PairMatches))
		for key, match := range item.PairMatches {
			clone.PairMatches[key] = match
		}
	}
	if item.Aliases != nil {
		clone.Aliases = append([]string(nil), item.Aliases...)
	}
	return clone
}

func (s *TradefiService) fresh(symbol string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.cache[symbol]
	return ok && !item.NextVerify.IsZero() && item.NextVerify.After(now)
}

func (s *TradefiService) resolve(ctx context.Context, job queuedJob) error {
	now := time.Now().UTC()
	if current, ok := s.cached(job.key); ok && !current.NextVerify.IsZero() && current.NextVerify.After(now) {
		if !current.Active {
			return nil
		}
		s.attachPairs(&current, job, now)
		return s.persist(ctx, current)
	}
	if previous, ok := s.cached(job.key); ok && previous.ConID != nil && *previous.ConID > 0 {
		contracts, err := s.lookupConID(ctx, *previous.ConID)
		if err != nil {
			return err
		}
		resolution := resolutionFromContracts(job.ref, contracts, now)
		if resolution.ConID == nil || *resolution.ConID != *previous.ConID || !compatibleRecheck(previous, resolution) {
			previous.Active = false
			previous.Status = "inactive"
			previous.LastVerified = now
			previous.NextVerify = now.Add(tradefiNegativeTTL)
			previous.PairMatches = mergePairMatches(previous.PairMatches, nil)
			s.attachPairs(&previous, job, now)
			return s.persist(ctx, previous)
		}
		resolution.Symbol = previous.Symbol
		resolution.Aliases = appendAlias(previous.Aliases, job.key)
		resolution.PairMatches = previous.PairMatches
		s.attachPairs(&resolution, job, now)
		return s.persist(ctx, resolution)
	}
	contracts, err := s.lookupContracts(ctx, job.ref)
	if err != nil {
		return err
	}
	resolution := resolutionFromContracts(job.ref, contracts, now)
	if resolution.ConID != nil && *resolution.ConID > 0 {
		if previous, ok := s.cached(job.key); ok {
			resolution.PairMatches = previous.PairMatches
			resolution.Aliases = previous.Aliases
		}
		resolution.Aliases = appendAlias(resolution.Aliases, job.key)
		resolution.Symbol = "conid:" + strconv.FormatInt(*resolution.ConID, 10)
	}
	s.attachPairs(&resolution, job, now)
	return s.persist(ctx, resolution)
}

func compatibleRecheck(previous, next IBResolution) bool {
	if previous.SecType != "" && next.SecType != "" && !strings.EqualFold(previous.SecType, next.SecType) {
		return false
	}
	if previous.UnderlyingCurrency != "" && next.UnderlyingCurrency != "" && !strings.EqualFold(previous.UnderlyingCurrency, next.UnderlyingCurrency) {
		return false
	}
	if previous.PrimaryExchange != "" && next.PrimaryExchange != "" && !strings.EqualFold(previous.PrimaryExchange, next.PrimaryExchange) {
		return false
	}
	if previous.UnderlyingSymbol != "" && next.UnderlyingSymbol != "" && !strings.EqualFold(previous.UnderlyingSymbol, next.UnderlyingSymbol) {
		return false
	}
	return true
}

func (s *TradefiService) lookupConID(ctx context.Context, conID int64) ([]ibkr.Contract, error) {
	var last error
	for attempt := 1; attempt <= s.attempts; attempt++ {
		if err := s.waitInterval(ctx); err != nil {
			return nil, err
		}
		page, err := s.lookup.Lookup(ctx, ibkr.Query{ConID: conID})
		if err == nil {
			return page, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("ib contract lookup failed")
	}
	return nil, last
}

func (s *TradefiService) lookupContracts(ctx context.Context, ref exchange.UnderlyingReference) ([]ibkr.Contract, error) {
	queries := ref.IBQueries()
	var last error
	var contracts []ibkr.Contract
	succeeded := 0
	for _, query := range queries {
		var err error
		var page []ibkr.Contract
		for attempt := 1; attempt <= s.attempts; attempt++ {
			if err = s.waitInterval(ctx); err != nil {
				return nil, err
			}
			page, err = s.lookup.Lookup(ctx, ibkr.Query{
				SecType: query.SecType, Symbol: query.Symbol,
				Currency: query.Currency, Exchange: query.Exchange,
			})
			if err == nil {
				break
			}
			last = err
		}
		if err != nil {
			continue
		}
		succeeded++
		contracts = append(contracts, page...)
	}
	if succeeded != len(queries) {
		if last == nil {
			last = errors.New("ib contract lookup failed")
		}
		return nil, last
	}
	return contracts, nil
}

func (s *TradefiService) attachPairs(resolution *IBResolution, job queuedJob, now time.Time) {
	if resolution.PairMatches == nil {
		resolution.PairMatches = map[string]PairMatch{}
	}
	class := ""
	if resolution.Active {
		class = verifiedClass(job.ref.AssetClass)
	}
	for _, item := range job.instruments {
		key := pairKey(item.Exchange, item.ExchangeSymbol)
		if class == "" {
			resolution.PairMatches[key] = PairMatch{MatchClass: MatchCandidate, Multiplier: 1}
			continue
		}
		own := job.refs[key]
		if own.AssetClass == "" {
			own = item.Underlying.Normalized()
		}
		multiplier := own.Multiplier
		if multiplier <= 0 {
			multiplier = 1
		}
		resolution.PairMatches[key] = PairMatch{
			MatchClass: class, VerifiedAt: now, PerpPriceCurrency: own.PriceCurrency(),
			Multiplier: multiplier,
		}
	}
}

func (s *TradefiService) persist(ctx context.Context, resolution IBResolution) error {
	saved := resolution
	if s.catalog != nil {
		var err error
		saved, err = s.catalog.UpsertIBInstrumentResolution(ctx, resolution)
		if err != nil {
			return err
		}
	}
	s.store(saved)
	return nil
}

func appendAlias(aliases []string, key string) []string {
	for _, alias := range aliases {
		if alias == key {
			return aliases
		}
	}
	return append(aliases, key)
}

func (s *TradefiService) rateFor(item exchange.Instrument) Rate {
	for _, rate := range s.funding.View().Rates {
		if rate.Exchange == item.Exchange && rate.ExchangeSymbol == item.ExchangeSymbol {
			return rate
		}
	}
	return Rate{}
}

func (s *TradefiService) cached(key string) (IBResolution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.cache[key]
	if !ok {
		return IBResolution{}, false
	}
	return cloneIBResolution(item), true
}

func (s *TradefiService) store(item IBResolution) {
	s.mu.Lock()
	s.remember(item)
	s.mu.Unlock()
}

func (s *TradefiService) remember(item IBResolution) {
	stored := cloneIBResolution(item)
	stale := make([]string, 0)
	for key, current := range s.cache {
		if current.Symbol == stored.Symbol {
			stale = append(stale, key)
		}
	}
	for _, key := range stale {
		delete(s.cache, key)
	}
	s.cache[stored.Symbol] = stored
	for _, alias := range stored.Aliases {
		if alias == "" || alias == stored.Symbol {
			continue
		}
		s.cache[alias] = stored
	}
}

func (s *TradefiService) logSummary(counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		s.logger.Info("underlying identity count", "venue_class", key, "count", counts[key])
	}
	verified, ambiguous, missing := 0, 0, 0
	s.mu.Lock()
	for _, item := range s.cache {
		switch item.Status {
		case MatchAmbiguous:
			ambiguous++
		case MatchNotFound:
			missing++
		}
		for _, match := range item.PairMatches {
			switch match.MatchClass {
			case MatchVerifiedListing, MatchVerifiedIdentifier:
				verified++
			}
		}
	}
	s.mu.Unlock()
	s.logger.Info("ib underlying match summary", "verified", verified, "ambiguous", ambiguous, "not_found", missing)
}

func (s *TradefiService) waitInterval(ctx context.Context) error {
	s.mu.Lock()
	var wait time.Duration
	if !s.lastLookup.IsZero() {
		if elapsed := time.Since(s.lastLookup); elapsed < s.interval {
			wait = s.interval - elapsed
		}
	}
	s.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	s.mu.Lock()
	s.lastLookup = time.Now()
	s.mu.Unlock()
	return nil
}

func normalizeStockSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

type TradefiRow struct {
	StockSymbol         string
	ConID               int64
	PrimaryExchange     string
	LocalSymbol         string
	TradingClass        string
	VerifiedAt          time.Time
	Exchange            string
	ExchangeSymbol      string
	BaseAsset           string
	QuoteAsset          string
	FundingRate         float64
	AnnualizedRate      float64
	IntervalHours       float64
	NextFundingAt       time.Time
	Volume24hBase       float64
	Turnover24hUSD      float64
	PositionQuantity    float64
	PositionNotionalUSD float64
	NextRate            *float64
	Cumulative24h       float64
	Cumulative7d        float64
	History24hComplete  *bool
	History7dComplete   *bool
	SourceUpdatedAt     time.Time
	PriceMultiplier     float64
}

func IsStale(sourceUpdatedAt, now time.Time, staleAfter time.Duration) bool {
	return sourceUpdatedAt.IsZero() || now.Sub(sourceUpdatedAt) > staleAfter
}

func BuildTradefiRows(resolutions []IBResolution, rates []Rate, tradable map[string]bool) []TradefiRow {
	active := make([]IBResolution, 0, len(resolutions))
	now := time.Now().UTC()
	for _, item := range resolutions {
		if item.Active && item.Status == "active" && item.ConID != nil && *item.ConID > 0 && item.NextVerify.After(now) {
			active = append(active, item)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Symbol < active[j].Symbol })
	rows := make([]TradefiRow, 0)
	for _, resolution := range active {
		for _, rate := range rates {
			match, ok := resolution.PairMatches[pairKey(rate.Exchange, rate.ExchangeSymbol)]
			venue := strings.ToLower(rate.Exchange)
			if !ok || !verifiedMatch(match.MatchClass) || !priceCompatible(match.PerpPriceCurrency, resolution.UnderlyingCurrency) ||
				!exchange.ExecutionAdapter(venue) || !tradable[venue] || IsStale(rate.SourceUpdatedAt, now, 15*time.Minute) {
				continue
			}
			multiplier := match.Multiplier
			if multiplier <= 0 {
				multiplier = 1
			}
			rows = append(rows, TradefiRow{
				StockSymbol: resolution.UnderlyingSymbol, ConID: *resolution.ConID,
				PrimaryExchange: resolution.PrimaryExchange, LocalSymbol: resolution.LocalSymbol,
				TradingClass: resolution.TradingClass, VerifiedAt: resolution.LastVerified,
				Exchange: rate.Exchange, ExchangeSymbol: rate.ExchangeSymbol,
				BaseAsset: rate.BaseAsset, QuoteAsset: rate.QuoteAsset,
				FundingRate: rate.Rate, AnnualizedRate: rate.AnnualizedRate,
				IntervalHours: rate.IntervalHours, NextFundingAt: rate.FundingTime,
				Volume24hBase: rate.Volume24hBase, Turnover24hUSD: rate.Turnover24hUSD,
				PositionQuantity: rate.PositionQuantity, PositionNotionalUSD: rate.PositionNotionalUSD,
				NextRate: rate.NextRate, Cumulative24h: rate.Cumulative24h, Cumulative7d: rate.Cumulative7d,
				History24hComplete: rate.History24hComplete, History7dComplete: rate.History7dComplete,
				SourceUpdatedAt: rate.SourceUpdatedAt, PriceMultiplier: multiplier,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].StockSymbol != rows[j].StockSymbol {
			return rows[i].StockSymbol < rows[j].StockSymbol
		}
		if rows[i].Exchange != rows[j].Exchange {
			return rows[i].Exchange < rows[j].Exchange
		}
		return rows[i].ExchangeSymbol < rows[j].ExchangeSymbol
	})
	return rows
}
