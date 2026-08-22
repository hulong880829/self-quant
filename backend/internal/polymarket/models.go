package polymarket

import (
	"sync"
	"time"
)

const MaxChartPoints = 300

type Market struct {
	ID              string
	ConditionID     string
	Slug            string
	Asset           string
	Period          string
	Title           string
	WindowStart     time.Time
	WindowEnd       time.Time
	UpTokenID       string
	DownTokenID     string
	TickSize        string
	NegativeRisk    bool
	Active          bool
	SourceUpdatedAt time.Time
	GammaOpenPrice  string
}

type PricePoint struct {
	Timestamp      time.Time
	OpenPrice      string
	ChainlinkPrice string
}

type Snapshot struct {
	Market         Market
	OpenPrice      string
	ChainlinkPrice string
	UpBid          string
	UpAsk          string
	DownBid        string
	DownAsk        string
	Series         []PricePoint
	SourceUpdated  time.Time
	Stale          bool
	Version        string
}

type SnapshotEventKind int

const (
	SnapshotEventFull SnapshotEventKind = iota
	SnapshotEventPriceDelta
	SnapshotEventQuotes
)

type SnapshotEvent struct {
	Snapshot    Snapshot
	Kind        SnapshotEventKind
	DeltaPoints []PricePoint
}

// QuotePatch describes a quote-only update for one market.
type QuotePatch struct {
	MarketID      string
	UpBid         string
	UpAsk         string
	DownBid       string
	DownAsk       string
	HasUp         bool
	HasDown       bool
	SourceUpdated time.Time
}

type SnapshotStore struct {
	stateMu   sync.RWMutex
	markets   []Market
	snapshots map[string]Snapshot
	version   uint64

	subMu sync.Mutex
	subs  map[string]map[chan SnapshotEvent]struct{}
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		snapshots: make(map[string]Snapshot),
		subs:      make(map[string]map[chan SnapshotEvent]struct{}),
	}
}

func (s *SnapshotStore) ListMarkets(asset, period string, activeOnly bool) ([]Market, uint64) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	now := time.Now()
	result := make([]Market, 0, len(s.markets))
	for _, market := range s.markets {
		if asset != "" && market.Asset != asset {
			continue
		}
		if period != "" && market.Period != period {
			continue
		}
		if activeOnly && !market.Active {
			continue
		}
		if activeOnly && !market.WindowEnd.After(now) {
			continue
		}
		result = append(result, market)
	}
	return result, s.version
}

func (s *SnapshotStore) Get(marketID string) (Snapshot, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	snapshot, ok := s.snapshots[marketID]
	if !ok {
		return Snapshot{}, false
	}
	return cloneSnapshot(snapshot, true), true
}

func (s *SnapshotStore) ReplaceMarkets(markets []Market) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.markets = append([]Market(nil), markets...)
	activeIDs := make(map[string]struct{}, len(markets))
	for _, market := range markets {
		activeIDs[market.ID] = struct{}{}
		snapshot := s.snapshots[market.ID]
		snapshot.Market = market
		s.snapshots[market.ID] = snapshot
	}
	for id := range s.snapshots {
		if _, ok := activeIDs[id]; !ok {
			delete(s.snapshots, id)
		}
	}
	s.version++
}

// PruneExpired removes snapshots whose market window ended before cutoff.
func (s *SnapshotStore) PruneExpired(before time.Time) []string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	removed := make([]string, 0)
	for id, snapshot := range s.snapshots {
		if !snapshot.Market.WindowEnd.Before(before) {
			continue
		}
		delete(s.snapshots, id)
		removed = append(removed, id)
	}
	if len(removed) > 0 {
		s.version++
	}
	return removed
}

func (s *SnapshotStore) Update(snapshot Snapshot) {
	s.commit(snapshot, SnapshotEventFull, nil, true)
}

func (s *SnapshotStore) UpdatePrice(snapshot Snapshot, delta []PricePoint) {
	s.commit(snapshot, SnapshotEventPriceDelta, delta, true)
}

func (s *SnapshotStore) UpdateQuotes(snapshot Snapshot) {
	s.PatchQuotes(snapshot.Market.ID, func(current *Snapshot) bool {
		changed := false
		if snapshot.UpBid != "" && current.UpBid != snapshot.UpBid {
			current.UpBid = snapshot.UpBid
			changed = true
		}
		if snapshot.UpAsk != "" && current.UpAsk != snapshot.UpAsk {
			current.UpAsk = snapshot.UpAsk
			changed = true
		}
		if snapshot.DownBid != "" && current.DownBid != snapshot.DownBid {
			current.DownBid = snapshot.DownBid
			changed = true
		}
		if snapshot.DownAsk != "" && current.DownAsk != snapshot.DownAsk {
			current.DownAsk = snapshot.DownAsk
			changed = true
		}
		if !changed {
			return false
		}
		if !snapshot.SourceUpdated.IsZero() {
			current.SourceUpdated = snapshot.SourceUpdated
		} else {
			current.SourceUpdated = time.Now().UTC()
		}
		current.Stale = false
		return true
	})
}

// PatchQuotes applies a quote-only mutation without copying Series.
func (s *SnapshotStore) PatchQuotes(
	marketID string,
	mutate func(*Snapshot) bool,
) bool {
	s.stateMu.Lock()
	existing, ok := s.snapshots[marketID]
	if !ok {
		s.stateMu.Unlock()
		return false
	}
	next := existing
	if !mutate(&next) {
		s.stateMu.Unlock()
		return false
	}
	next.Version = time.Now().UTC().Format(time.RFC3339Nano)
	s.snapshots[marketID] = next
	s.version++
	published := cloneSnapshot(next, false)
	s.stateMu.Unlock()
	s.publish(marketID, SnapshotEvent{
		Snapshot: published, Kind: SnapshotEventQuotes,
	})
	return true
}

// PatchQuoteBatch applies many quote-only mutations under one write lock.
func (s *SnapshotStore) PatchQuoteBatch(patches []QuotePatch) int {
	if len(patches) == 0 {
		return 0
	}
	type pendingEvent struct {
		marketID string
		event    SnapshotEvent
	}
	events := make([]pendingEvent, 0, len(patches))
	changedCount := 0
	s.stateMu.Lock()
	for _, patch := range patches {
		existing, ok := s.snapshots[patch.MarketID]
		if !ok {
			continue
		}
		next := existing
		changed := false
		if patch.HasUp {
			if patch.UpBid != "" && next.UpBid != patch.UpBid {
				next.UpBid = patch.UpBid
				changed = true
			}
			if patch.UpAsk != "" && next.UpAsk != patch.UpAsk {
				next.UpAsk = patch.UpAsk
				changed = true
			}
		}
		if patch.HasDown {
			if patch.DownBid != "" && next.DownBid != patch.DownBid {
				next.DownBid = patch.DownBid
				changed = true
			}
			if patch.DownAsk != "" && next.DownAsk != patch.DownAsk {
				next.DownAsk = patch.DownAsk
				changed = true
			}
		}
		if !changed {
			continue
		}
		if !patch.SourceUpdated.IsZero() {
			next.SourceUpdated = patch.SourceUpdated
		} else {
			next.SourceUpdated = time.Now().UTC()
		}
		next.Stale = false
		next.Version = time.Now().UTC().Format(time.RFC3339Nano)
		s.snapshots[patch.MarketID] = next
		s.version++
		changedCount++
		events = append(events, pendingEvent{
			marketID: patch.MarketID,
			event: SnapshotEvent{
				Snapshot: cloneSnapshot(next, false),
				Kind:     SnapshotEventQuotes,
			},
		})
	}
	s.stateMu.Unlock()
	for _, item := range events {
		s.publish(item.marketID, item.event)
	}
	return changedCount
}

// PatchChainlinkPrice mutates one market's live Chainlink fields in place.
// mutate may append a Series sample; returned delta is published without copying
// the full Series into the subscriber event payload.
func (s *SnapshotStore) PatchChainlinkPrice(
	marketID string,
	mutate func(*Snapshot) (changed bool, delta []PricePoint),
) (bool, string, []PricePoint) {
	s.stateMu.Lock()
	existing, ok := s.snapshots[marketID]
	if !ok {
		s.stateMu.Unlock()
		return false, "", nil
	}
	next := existing
	changed, delta := mutate(&next)
	if !changed {
		s.stateMu.Unlock()
		return false, existing.OpenPrice, nil
	}
	next.Version = time.Now().UTC().Format(time.RFC3339Nano)
	s.snapshots[marketID] = next
	s.version++
	openPrice := next.OpenPrice
	published := cloneSnapshot(next, false)
	deltaCopy := append([]PricePoint(nil), delta...)
	kind := SnapshotEventPriceDelta
	s.stateMu.Unlock()
	s.publish(marketID, SnapshotEvent{
		Snapshot: published, Kind: kind, DeltaPoints: deltaCopy,
	})
	return true, openPrice, deltaCopy
}

func (s *SnapshotStore) commit(
	snapshot Snapshot,
	kind SnapshotEventKind,
	delta []PricePoint,
	copySeries bool,
) {
	snapshot.Version = time.Now().UTC().Format(time.RFC3339Nano)
	if copySeries {
		snapshot.Series = append([]PricePoint(nil), snapshot.Series...)
	}
	s.stateMu.Lock()
	s.snapshots[snapshot.Market.ID] = snapshot
	s.version++
	includeSeries := kind == SnapshotEventFull
	published := cloneSnapshot(snapshot, includeSeries)
	deltaCopy := append([]PricePoint(nil), delta...)
	s.stateMu.Unlock()
	s.publish(snapshot.Market.ID, SnapshotEvent{
		Snapshot: published, Kind: kind, DeltaPoints: deltaCopy,
	})
}

func (s *SnapshotStore) publish(marketID string, event SnapshotEvent) {
	s.subMu.Lock()
	subscribers := s.subs[marketID]
	if len(subscribers) == 0 {
		s.subMu.Unlock()
		return
	}
	// Copy subscriber set so slow consumers do not hold the state path.
	targets := make([]chan SnapshotEvent, 0, len(subscribers))
	for subscriber := range subscribers {
		targets = append(targets, subscriber)
	}
	s.subMu.Unlock()
	for _, subscriber := range targets {
		select {
		case subscriber <- event:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- event:
			default:
			}
		}
	}
}

func (s *SnapshotStore) Subscribe(marketID string) (<-chan SnapshotEvent, func()) {
	channel := make(chan SnapshotEvent, 1)
	s.subMu.Lock()
	if s.subs[marketID] == nil {
		s.subs[marketID] = make(map[chan SnapshotEvent]struct{})
	}
	s.subs[marketID][channel] = struct{}{}
	s.subMu.Unlock()
	cancel := func() {
		s.subMu.Lock()
		if subscribers := s.subs[marketID]; subscribers != nil {
			delete(subscribers, channel)
			if len(subscribers) == 0 {
				delete(s.subs, marketID)
			}
		}
		s.subMu.Unlock()
	}
	return channel, cancel
}

func cloneSnapshot(snapshot Snapshot, includeSeries bool) Snapshot {
	cloned := snapshot
	if includeSeries {
		cloned.Series = append([]PricePoint(nil), snapshot.Series...)
	} else {
		cloned.Series = nil
	}
	return cloned
}

func trimSeries(points []PricePoint, max int) []PricePoint {
	if max <= 0 || len(points) <= max {
		return points
	}
	return append([]PricePoint(nil), points[len(points)-max:]...)
}

func DownsampleSeries(points []PricePoint, maxPoints int) []PricePoint {
	if maxPoints <= 0 || len(points) <= maxPoints {
		return append([]PricePoint(nil), points...)
	}
	if maxPoints == 1 {
		return []PricePoint{points[len(points)-1]}
	}
	result := make([]PricePoint, maxPoints)
	step := float64(len(points)-1) / float64(maxPoints-1)
	for index := 0; index < maxPoints; index++ {
		sourceIndex := int(float64(index) * step)
		if sourceIndex >= len(points) {
			sourceIndex = len(points) - 1
		}
		result[index] = points[sourceIndex]
	}
	return result
}
