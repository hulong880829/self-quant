package aggdata

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type LiveSample struct {
	WallNS        uint64
	RawBPS        float64
	GatedBPS      float64
	RawMin        float64
	RawMax        float64
	GatedMin      float64
	GatedMax      float64
	WindowSamples uint64
}

type sampleRing struct {
	mu     sync.RWMutex
	values []LiveSample
	next   int
	full   bool
	hour   time.Time
}

// The MDS gateway publishes at 50ms by default. Keep every BBO image for the
// complete current UTC hour so history queries do not lose the first ~42
// minutes before the recorder finalizes the hour's shard.
const currentHourSampleCapacity = 20 * 60 * 60

func newSampleRing(capacity int) *sampleRing {
	return &sampleRing{values: make([]LiveSample, capacity)}
}

func (r *sampleRing) add(sample LiveSample) {
	hour := time.Unix(0, int64(sample.WallNS)).UTC().Truncate(time.Hour)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.hour.Equal(hour) {
		r.hour = hour
		r.next = 0
		r.full = false
	}
	r.values[r.next] = sample
	r.next = (r.next + 1) % len(r.values)
	if r.next == 0 {
		r.full = true
	}
}

func (r *sampleRing) snapshot() []LiveSample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := r.next
	start := 0
	if r.full {
		count = len(r.values)
		start = r.next
	}
	result := make([]LiveSample, count)
	for index := range result {
		result[index] = r.values[(start+index)%len(r.values)]
	}
	return result
}

type liveMarket struct {
	catalog   catalogMarket
	bbo       snapshotSlot
	book      snapshotSlot
	fair      atomic.Pointer[FairPriceSnapshot]
	fairState atomic.Pointer[fairModelState]
	ring      *sampleRing
}

type Store struct {
	mu                  sync.RWMutex
	markets             map[string]*liveMarket
	symbolIndex         map[string][]string
	identityIndex       map[string]string
	topicMap            map[uint16]*liveMarket
	mappedSegments      map[string]uint16
	learningSegment     string
	fairEngine          *fairPriceEngine
	fairComputed        atomic.Uint64
	fairInvalid         atomic.Uint64
	fairCrossed         atomic.Uint64
	fairUncrossFailed   atomic.Uint64
	fairDegraded        atomic.Uint64
	unmappedFrames      atomic.Uint64
	mappingsPreserved   atomic.Uint64
	mappingsInvalidated atomic.Uint64
	topicRelearns       atomic.Uint64
	staleAfterNS        atomic.Int64
}

func NewStore() *Store {
	return &Store{
		markets:        make(map[string]*liveMarket),
		symbolIndex:    make(map[string][]string),
		identityIndex:  make(map[string]string),
		topicMap:       make(map[uint16]*liveMarket),
		mappedSegments: make(map[string]uint16),
	}
}

func NewStoreWithFairPrice(config FairPriceConfig) (*Store, error) {
	store := NewStore()
	if !config.Enabled {
		return store, nil
	}
	engine, err := newFairPriceEngine(config)
	if err != nil {
		return nil, err
	}
	store.fairEngine = engine
	return store, nil
}

func (s *Store) SetStaleAfter(duration time.Duration) {
	s.staleAfterNS.Store(duration.Nanoseconds())
}

func (s *Store) SnapshotFresh(snapshot *Snapshot, now time.Time) bool {
	return snapshot != nil && snapshot.Ready && s.wallFresh(snapshot.WallNS, now)
}

func (s *Store) FairSnapshotFresh(snapshot *FairPriceSnapshot, now time.Time) bool {
	return snapshot != nil && snapshot.Ready && s.wallFresh(snapshot.WallNS, now)
}

func (s *Store) wallFresh(wallNS uint64, now time.Time) bool {
	staleAfter := s.staleAfterNS.Load()
	if staleAfter <= 0 {
		return true
	}
	nowNS := now.UnixNano()
	return wallNS <= uint64(nowNS) && nowNS-int64(wallNS) <= staleAfter
}

func (s *Store) Reconcile(catalog *catalogSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previousMappings := s.mappedSegments
	previousLearning := s.learningSegment
	next := make(map[string]*liveMarket, len(catalog.Markets))
	index := make(map[string][]string, len(catalog.Markets))
	identityIndex := make(map[string]string, len(catalog.Markets))
	topicMap := make(map[uint16]*liveMarket)
	mappedSegments := make(map[string]uint16)
	activeSegments := make(map[string]struct{})
	var preserved uint64
	for _, market := range catalog.Markets {
		key := market.Identity.key()
		existing := s.markets[key]
		reconciled := &liveMarket{
			catalog: market,
			ring:    newSampleRing(currentHourSampleCapacity),
		}
		if existing != nil {
			reconciled.ring = existing.ring
			if segment := market.Segments[KindBBO]; segment != "" &&
				segment == existing.catalog.Segments[KindBBO] {
				reconciled.bbo.value.Store(existing.bbo.value.Load())
			}
			if segment := market.Segments[KindBook]; segment != "" &&
				segment == existing.catalog.Segments[KindBook] {
				reconciled.book.value.Store(existing.book.value.Load())
				reconciled.fair.Store(existing.fair.Load())
				reconciled.fairState.Store(existing.fairState.Load())
			}
		}
		for kind, segment := range market.Segments {
			if segment == "" {
				continue
			}
			activeSegments[segment] = struct{}{}
			if existing == nil || existing.catalog.Segments[kind] != segment {
				continue
			}
			topicID, mapped := previousMappings[segment]
			if !mapped {
				continue
			}
			topicMap[topicID] = reconciled
			mappedSegments[segment] = topicID
			preserved++
		}
		next[key] = reconciled
		canonical := canonicalSymbol(market.Symbol)
		index[canonical] = append(index[canonical], key)
		identityIndex[marketIdentityIndexKey(market.Profile, market.Symbol)] = key
	}
	s.markets = next
	s.symbolIndex = index
	s.identityIndex = identityIndex
	s.topicMap = topicMap
	s.mappedSegments = mappedSegments
	s.learningSegment = ""
	if _, active := activeSegments[previousLearning]; active {
		s.learningSegment = previousLearning
	}
	s.mappingsPreserved.Add(preserved)
	if invalidated := uint64(len(previousMappings)); invalidated > preserved {
		invalidated -= preserved
		s.mappingsInvalidated.Add(invalidated)
	}
}

func (s *Store) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topicMap = make(map[uint16]*liveMarket)
	s.mappedSegments = make(map[string]uint16)
	s.learningSegment = ""
	for _, market := range s.markets {
		markNotReady(&market.bbo)
		markNotReady(&market.book)
		markFairNotReady(market, "gateway_disconnected", 0, 0, 0)
		market.fairState.Store(nil)
	}
}

func markNotReady(slot *snapshotSlot) {
	previous := slot.value.Load()
	if previous == nil || !previous.Ready {
		return
	}
	copy := *previous
	copy.Ready = false
	copy.RingEpoch = 0
	copy.RingSequence = 0
	copy.Generation = 0
	copy.Browser20, copy.Browser50 = nil, nil
	slot.value.Store(&copy)
}

func markFairNotReady(
	market *liveMarket,
	reason string,
	ringEpoch, ringSequence, generation uint64,
) {
	previous := market.fair.Load()
	if previous == nil && ringEpoch == 0 && ringSequence == 0 && generation == 0 {
		return
	}
	value := &FairPriceSnapshot{
		Profile:      market.catalog.Profile,
		Symbol:       market.catalog.Symbol,
		Ready:        false,
		ResetReason:  reason,
		RingEpoch:    ringEpoch,
		RingSequence: ringSequence,
		Generation:   generation,
	}
	if previous != nil {
		copy := *previous
		copy.Ready = false
		copy.ResetReason = reason
		copy.JSON = nil
		if ringEpoch != 0 || ringSequence != 0 || generation != 0 {
			copy.RingEpoch = ringEpoch
			copy.RingSequence = ringSequence
			copy.Generation = generation
		}
		value = &copy
	}
	market.fair.Store(value)
}

func (s *Store) ActiveSegments() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []string
	for _, market := range s.markets {
		for _, segment := range market.catalog.Segments {
			result = append(result, segment)
		}
	}
	sort.Strings(result)
	return result
}

func (s *Store) Apply(frame GatewayFrame) error {
	s.mu.Lock()
	market := s.topicMap[frame.TopicID]
	if frame.Kind == KindReset {
		if market != nil {
			s.applyReset(market, frame)
		}
		s.mu.Unlock()
		return nil
	}
	if market == nil {
		var err error
		market, err = s.identify(frame)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if market == nil {
			s.unmappedFrames.Add(1)
			s.mu.Unlock()
			return nil
		}
		s.topicMap[frame.TopicID] = market
		if segment := market.catalog.Segments[frame.Kind]; segment != "" {
			s.mappedSegments[segment] = frame.TopicID
			if s.learningSegment == segment {
				s.learningSegment = ""
			}
		}
	}
	s.mu.Unlock()

	previous := marketSnapshot(market, frame.Kind)
	if previous != nil {
		if frame.RingEpoch == previous.RingEpoch {
			if frame.RingSequence < previous.RingSequence {
				return fmt.Errorf("regressive SQGW sequence for topic %d", frame.TopicID)
			}
			if frame.RingSequence == previous.RingSequence {
				return nil
			}
			if frame.Generation <= previous.Generation {
				return fmt.Errorf("non-increasing SQGW generation for topic %d", frame.TopicID)
			}
		}
	}
	snapshot := &Snapshot{
		Profile:      market.catalog.Profile,
		Symbol:       market.catalog.Symbol,
		Kind:         frame.Kind,
		TopicID:      frame.TopicID,
		RingEpoch:    frame.RingEpoch,
		RingSequence: frame.RingSequence,
		Generation:   frame.Generation,
		WallNS:       frame.WallNS,
		Ready:        true,
		BBO:          frame.BBO,
		Book:         frame.Book,
	}
	var err error
	snapshot.Browser20, err = EncodeBrowserSnapshot(snapshot, 20)
	if err != nil {
		return err
	}
	snapshot.Browser50, err = EncodeBrowserSnapshot(snapshot, 50)
	if err != nil {
		return err
	}
	if frame.Kind == KindBBO {
		market.bbo.value.Store(snapshot)
		market.ring.add(LiveSample{
			WallNS: frame.WallNS, RawBPS: frame.BBO.RawSpreadBPS,
			GatedBPS: frame.BBO.GatedSpreadBPS,
		})
	} else {
		market.book.value.Store(snapshot)
		if s.fairEngine != nil {
			fair, state := s.fairEngine.compute(
				market.catalog.Profile,
				market.catalog.Symbol,
				snapshot,
				market.fairState.Load(),
			)
			market.fair.Store(fair)
			if fair.Ready && state != nil {
				s.fairComputed.Add(1)
				if fair.Crossed {
					s.fairCrossed.Add(1)
				}
				if fair.Degraded {
					s.fairDegraded.Add(1)
				}
				market.fairState.Store(state)
			} else {
				s.fairInvalid.Add(1)
				if fair.ResetReason == "uncross_depth_exhausted" {
					s.fairUncrossFailed.Add(1)
				}
				market.fairState.Store(nil)
			}
		}
	}
	return nil
}

func (s *Store) identify(frame GatewayFrame) (*liveMarket, error) {
	base, quote, _, _ := frameIdentity(frame)
	var candidates []*liveMarket
	for _, market := range s.markets {
		if _, active := market.catalog.Segments[frame.Kind]; !active {
			continue
		}
		if canonicalSymbol(market.catalog.Symbol) != canonicalSymbol(base+quote) {
			continue
		}
		candidates = append(candidates, market)
	}
	if s.learningSegment != "" {
		learning, err := parseSegment(s.learningSegment)
		if err == nil && streamKind(learning.Stream) == frame.Kind {
			for _, candidate := range candidates {
				if candidate.catalog.Identity.key() == learning.key() {
					return candidate, nil
				}
			}
		}
		return nil, nil
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return nil, nil
}

func (s *Store) BeginLearning(segment string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.learningSegment = segment
}

func (s *Store) SegmentMapped(segment string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, mapped := s.mappedSegments[segment]
	return mapped
}

func (s *Store) RecordTopicRelearn() {
	s.topicRelearns.Add(1)
}

func canonicalSymbol(value string) string {
	var output strings.Builder
	for _, character := range strings.ToUpper(value) {
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func frameIdentity(frame GatewayFrame) (string, string, [8]uint8, uint8) {
	if frame.BBO != nil {
		return frame.BBO.Base, frame.BBO.Quote, frame.BBO.VenueSlotIDs, frame.BBO.MemberCount
	}
	return frame.Book.Base, frame.Book.Quote, frame.Book.VenueSlotIDs, frame.Book.MemberCount
}

func profileMatchesSlots(profile string, slots [8]uint8, count uint8) bool {
	lower := strings.ToLower(profile)
	for _, id := range slots[:count] {
		if !strings.Contains(lower, venueName(id)) {
			return false
		}
	}
	return true
}

func marketSnapshot(market *liveMarket, kind Kind) *Snapshot {
	switch kind {
	case KindBBO:
		return market.bbo.value.Load()
	case KindBook:
		return market.book.value.Load()
	default:
		return nil
	}
}

func (s *Store) applyReset(market *liveMarket, frame GatewayFrame) {
	previous := market.bbo.value.Load()
	if previous != nil && previous.TopicID == frame.TopicID {
		copy := *previous
		copy.Ready = false
		copy.Generation = frame.Generation
		copy.RingEpoch = frame.RingEpoch
		copy.RingSequence = frame.RingSequence
		copy.Browser20, copy.Browser50 = nil, nil
		market.bbo.value.Store(&copy)
	}
	previous = market.book.value.Load()
	if previous != nil && previous.TopicID == frame.TopicID {
		copy := *previous
		copy.Ready = false
		copy.Generation = frame.Generation
		copy.RingEpoch = frame.RingEpoch
		copy.RingSequence = frame.RingSequence
		copy.Browser20, copy.Browser50 = nil, nil
		market.book.value.Store(&copy)
		markFairNotReady(
			market, "orderbook_reset",
			frame.RingEpoch, frame.RingSequence, frame.Generation,
		)
		market.fairState.Store(nil)
	}
}

func (s *Store) Lookup(symbol string) (*liveMarket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := s.symbolIndex[canonicalSymbol(symbol)]
	if len(keys) > 1 {
		return nil, fmt.Errorf("symbol %q exists in multiple profiles", symbol)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("unknown symbol %q", symbol)
	}
	return s.markets[keys[0]], nil
}

func (s *Store) LookupProfile(profile, symbol string) (*liveMarket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.identityIndex[marketIdentityIndexKey(profile, symbol)]
	if !ok {
		return nil, fmt.Errorf("unknown market %q/%q", profile, symbol)
	}
	return s.markets[key], nil
}

func marketIdentityIndexKey(profile, symbol string) string {
	return strings.ToLower(strings.TrimSpace(profile)) + "\x00" + canonicalSymbol(symbol)
}

func (s *Store) MarketKey(market *liveMarket) string {
	return market.catalog.Identity.key()
}

func (s *Store) LookupKey(key string) (*liveMarket, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	market, ok := s.markets[key]
	return market, ok
}

func (s *Store) FairSnapshots() []*FairPriceSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*FairPriceSnapshot, 0, len(s.markets))
	for _, market := range s.markets {
		if snapshot := market.fair.Load(); snapshot != nil && snapshot.Ready {
			result = append(result, snapshot)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Profile == result[j].Profile {
			return result[i].Symbol < result[j].Symbol
		}
		return result[i].Profile < result[j].Profile
	})
	return result
}

func (s *Store) FairPriceEnabled() bool {
	return s.fairEngine != nil
}

func (s *Store) FairPriceModelID() string {
	if s.fairEngine == nil {
		return ""
	}
	return s.fairEngine.modelID
}

type FairPriceStats struct {
	Computed            uint64
	Invalid             uint64
	Crossed             uint64
	UncrossFailed       uint64
	Degraded            uint64
	UnmappedFrames      uint64
	MappingsPreserved   uint64
	MappingsInvalidated uint64
	TopicRelearns       uint64
}

func (s *Store) FairPriceStats() FairPriceStats {
	return FairPriceStats{
		Computed:            s.fairComputed.Load(),
		Invalid:             s.fairInvalid.Load(),
		Crossed:             s.fairCrossed.Load(),
		UncrossFailed:       s.fairUncrossFailed.Load(),
		Degraded:            s.fairDegraded.Load(),
		UnmappedFrames:      s.unmappedFrames.Load(),
		MappingsPreserved:   s.mappingsPreserved.Load(),
		MappingsInvalidated: s.mappingsInvalidated.Load(),
		TopicRelearns:       s.topicRelearns.Load(),
	}
}

func (s *Store) Markets() []Market {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Market, 0, len(s.markets))
	for _, market := range s.markets {
		value := Market{
			Profile: market.catalog.Profile, Symbol: market.catalog.Symbol,
			HasBBO:       market.catalog.Segments[KindBBO] != "" || hasShard(market.catalog.Shards, "aggbbo"),
			HasOrderBook: market.catalog.Segments[KindBook] != "" || hasShard(market.catalog.Shards, "aggorderbook"),
			Recording:    market.catalog.Recording,
		}
		if snapshot := market.bbo.value.Load(); snapshot != nil && snapshot.BBO != nil {
			fillMarketFromIdentity(&value, snapshot.BBO.Base, snapshot.BBO.Quote,
				snapshot.BBO.VenueSlotIDs, snapshot.BBO.MemberCount,
				snapshot.BBO.PriceScale, snapshot.BBO.QuantityScale, snapshot)
		} else if snapshot := market.book.value.Load(); snapshot != nil && snapshot.Book != nil {
			fillMarketFromIdentity(&value, snapshot.Book.Base, snapshot.Book.Quote,
				snapshot.Book.VenueSlotIDs, snapshot.Book.MemberCount,
				snapshot.Book.PriceScale, snapshot.Book.QuantityScale, snapshot)
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Symbol < result[j].Symbol })
	return result
}

func (s *Store) Ready() bool {
	return s.ReadyAt(time.Now())
}

func (s *Store) ReadyAt(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.markets) == 0 {
		return false
	}
	for _, market := range s.markets {
		if market.catalog.Segments[KindBBO] != "" {
			snapshot := market.bbo.value.Load()
			if !s.SnapshotFresh(snapshot, now) {
				return false
			}
		}
		if market.catalog.Segments[KindBook] != "" {
			snapshot := market.book.value.Load()
			if !s.SnapshotFresh(snapshot, now) {
				return false
			}
		}
	}
	return true
}

func (s *Store) StaleTopicCount(now time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, market := range s.markets {
		for kind, segment := range market.catalog.Segments {
			if segment != "" && !s.SnapshotFresh(marketSnapshot(market, kind), now) {
				count++
			}
		}
	}
	return count
}

func fillMarketFromIdentity(
	market *Market, base, quote string, slots [8]uint8, count uint8,
	priceScale, quantityScale uint8, snapshot *Snapshot,
) {
	market.Base, market.Quote = base, quote
	market.PriceScale, market.QuantityScale = priceScale, quantityScale
	for _, id := range slots[:count] {
		market.Venues = append(market.Venues, venueName(id))
	}
	market.Live = snapshot.Ready
	market.UpdatedNS = snapshot.WallNS
}

func hasShard(shards []string, stream string) bool {
	for _, shard := range shards {
		if strings.HasPrefix(filepathBase(shard), stream+".") || filepathBase(shard) == stream+".sqrec.zst" {
			return true
		}
	}
	return false
}

func filepathBase(path string) string {
	index := strings.LastIndexByte(path, '/')
	return path[index+1:]
}
