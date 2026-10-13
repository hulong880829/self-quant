package jev

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

type MemoryStore struct {
	mu        sync.Mutex
	locked    bool
	runs      map[int64]*Run
	nextRun   int64
	positions map[int64]*Position
	nextPos   int64
	decisions map[int64]*Decision
	nextDec   int64
	trades    map[int64]*Trade
	nextTrade int64
	equity    []EquityPoint
	snapshots map[int64]Snapshot
	nextSnap  int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs:      map[int64]*Run{},
		positions: map[int64]*Position{},
		decisions: map[int64]*Decision{},
		trades:    map[int64]*Trade{},
		snapshots: map[int64]Snapshot{},
	}
}

type memoryCycleLease struct {
	store *MemoryStore
	once  sync.Once
}

func (s *MemoryStore) AcquireCycleLock(context.Context) (CycleLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return nil, false, nil
	}
	s.locked = true
	return &memoryCycleLease{store: s}, true, nil
}

func (l *memoryCycleLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.store.mu.Lock()
		l.store.locked = false
		l.store.mu.Unlock()
	})
	return nil
}

func (s *MemoryStore) EnsureActiveRun(_ context.Context, _ time.Time, initialEquity decimal.Decimal, strategyVersion string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strategyVersion == "" {
		strategyVersion = StrategyVersion
	}
	for _, run := range s.runs {
		if run.Status == "active" && run.StrategyVersion == strategyVersion {
			return *run, nil
		}
	}
	s.nextRun++
	run := &Run{
		ID: s.nextRun, Status: "active", StrategyVersion: strategyVersion,
		InitialEquity: initialEquity, Equity: initialEquity, PeakEquity: initialEquity,
	}
	s.runs[run.ID] = run
	return *run, nil
}

func (s *MemoryStore) ActiveRun(_ context.Context, strategyVersion string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strategyVersion == "" {
		strategyVersion = StrategyVersion
	}
	for _, run := range s.runs {
		if run.Status == "active" && run.StrategyVersion == strategyVersion {
			copy := *run
			return &copy, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ListRuns(context.Context) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, len(s.runs))
	for _, run := range s.runs {
		out = append(out, *run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) SaveRun(_ context.Context, run Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := run
	s.runs[run.ID] = &copy
	return nil
}

func (s *MemoryStore) ListAllPositions(_ context.Context, runID int64) ([]Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Position, 0)
	for _, item := range s.positions {
		if item.RunID == runID {
			out = append(out, *item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (s *MemoryStore) ListOpenPositions(ctx context.Context, runID int64) ([]Position, error) {
	all, err := s.ListAllPositions(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, item := range all {
		if item.Status == PositionOpen {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListOpenPositionsExceptStrategy(_ context.Context, strategyVersion string) ([]Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Position, 0)
	for _, item := range s.positions {
		if item.Status != PositionOpen {
			continue
		}
		run := s.runs[item.RunID]
		if run == nil || run.StrategyVersion == strategyVersion {
			continue
		}
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) ListPositionsPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Position, int64, error) {
	all, err := s.ListAllPositions(ctx, runID)
	if err != nil {
		return nil, 0, err
	}
	return pageByID(all, limit, cursor, func(item Position) int64 { return item.ID })
}

func (s *MemoryStore) InsertPosition(_ context.Context, position Position) (Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if position.StrategyVersion == "" {
		position.StrategyVersion = StrategyVersion
	}
	s.nextPos++
	position.ID = s.nextPos
	copy := position
	s.positions[position.ID] = &copy
	return position, nil
}

func (s *MemoryStore) SavePosition(_ context.Context, position Position) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := position
	s.positions[position.ID] = &copy
	return nil
}

func (s *MemoryStore) InsertDecision(_ context.Context, decision Decision) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if decision.StrategyVersion == "" {
		decision.StrategyVersion = StrategyVersion
	}
	s.nextDec++
	decision.ID = s.nextDec
	copy := decision
	s.decisions[decision.ID] = &copy
	return decision, nil
}

func (s *MemoryStore) SaveDecision(_ context.Context, decision Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := decision
	s.decisions[decision.ID] = &copy
	return nil
}

func (s *MemoryStore) ListPendingDecisions(_ context.Context, runID int64) ([]Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Decision, 0)
	for _, item := range s.decisions {
		if item.RunID == runID && item.ExecutionStatus == ExecPending {
			out = append(out, *item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) ListDecisionsPage(_ context.Context, runID int64, limit int, cursor int64) ([]Decision, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]Decision, 0)
	for _, item := range s.decisions {
		if item.RunID == runID {
			all = append(all, *item)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	return pageByID(all, limit, cursor, func(item Decision) int64 { return item.ID })
}

func (s *MemoryStore) InsertTrade(_ context.Context, trade Trade) (Trade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextTrade++
	trade.ID = s.nextTrade
	copy := trade
	s.trades[trade.ID] = &copy
	return trade, nil
}

func (s *MemoryStore) ListTradesPage(_ context.Context, runID int64, limit int, cursor int64) ([]Trade, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]Trade, 0)
	for _, item := range s.trades {
		if item.RunID == runID {
			trade := *item
			if pos := s.positions[trade.PositionID]; pos != nil {
				trade.StrategyVersion = pos.StrategyVersion
				trade.BaseAsset = pos.BaseAsset
				trade.Kind = pos.Kind
				trade.LongExchange = pos.LongExchange
				trade.LongSymbol = pos.LongSymbol
				trade.LongProduct = pos.LongProduct
				trade.ShortExchange = pos.ShortExchange
				trade.ShortSymbol = pos.ShortSymbol
				trade.ShortProduct = pos.ShortProduct
			}
			all = append(all, trade)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	return pageByID(all, limit, cursor, func(item Trade) int64 { return item.ID })
}

func (s *MemoryStore) InsertEquityPoint(_ context.Context, _ int64, point EquityPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.equity = append(s.equity, point)
	return nil
}

func (s *MemoryStore) ListEquityPoints(_ context.Context, _ int64, from *time.Time) ([]EquityPoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EquityPoint, 0, len(s.equity))
	for _, point := range s.equity {
		if from != nil && point.TS.Before(*from) {
			continue
		}
		out = append(out, point)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

func (s *MemoryStore) PublishSnapshot(_ context.Context, runID int64, snapshot Snapshot) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	if run == nil {
		return Snapshot{}, fmt.Errorf("run missing")
	}
	run.SnapshotVersion++
	snapshot.Version = run.SnapshotVersion
	snapshot.ETag = fmt.Sprintf(`"%d"`, snapshot.Version)
	s.snapshots[runID] = snapshot
	return snapshot, nil
}

func (s *MemoryStore) LatestSnapshot(_ context.Context, runID int64) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.snapshots[runID]
	if !ok {
		return nil, nil
	}
	copy := snapshot
	return &copy, nil
}

func pageByID[T any](items []T, limit int, cursor int64, id func(T) int64) ([]T, int64, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	filtered := items
	if cursor > 0 {
		next := make([]T, 0, len(items))
		for _, item := range items {
			if id(item) < cursor {
				next = append(next, item)
			}
		}
		filtered = next
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	var nextCursor int64
	if len(filtered) == limit && len(items) > 0 {
		nextCursor = id(filtered[len(filtered)-1])
	}
	return filtered, nextCursor, nil
}
