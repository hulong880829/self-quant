package funding

import (
	"sync/atomic"
	"time"
)

type TradefiSnapshot struct {
	Rows      []TradefiRow
	Version   string
	UpdatedAt time.Time
}

type TradefiSnapshotStore struct {
	snapshot atomic.Pointer[TradefiSnapshot]
}

func NewTradefiSnapshotStore() *TradefiSnapshotStore {
	return &TradefiSnapshotStore{}
}

func (s *TradefiSnapshotStore) Replace(rows []TradefiRow, now time.Time) {
	copied := append([]TradefiRow(nil), rows...)
	now = now.UTC()
	s.snapshot.Store(&TradefiSnapshot{
		Rows: copied, UpdatedAt: now, Version: now.Format("20060102T150405.000000000"),
	})
}

func (s *TradefiSnapshotStore) Get() TradefiSnapshot {
	if s == nil {
		return TradefiSnapshot{}
	}
	current := s.snapshot.Load()
	if current == nil {
		return TradefiSnapshot{}
	}
	copied := *current
	copied.Rows = append([]TradefiRow(nil), current.Rows...)
	return copied
}
