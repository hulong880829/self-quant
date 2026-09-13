package trader

import (
	"context"
	"sync"
	"time"
)

type snapshotRequest struct {
	write      arbitrageMarketSnapshotWrite
	runtime    *arbitrageRuntime
	generation uint64
}

type snapshotBatcher struct {
	scheduler *ArbitrageScheduler
	mu        sync.Mutex
	pending   map[string]snapshotRequest
	wake      chan struct{}
	inFlight  bool
}

func newSnapshotBatcher(scheduler *ArbitrageScheduler) *snapshotBatcher {
	return &snapshotBatcher{
		scheduler: scheduler,
		pending:   make(map[string]snapshotRequest),
		wake:      make(chan struct{}, 1),
	}
}

func (b *snapshotBatcher) Submit(req snapshotRequest) {
	if b == nil || req.runtime == nil || req.write.CombinationID == "" {
		return
	}
	b.mu.Lock()
	b.pending[req.write.CombinationID] = req
	b.mu.Unlock()
	signalLatest(b.wake)
}

func (b *snapshotBatcher) Remove(id string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

func (b *snapshotBatcher) Run(ctx context.Context) {
	if b == nil || b.scheduler == nil {
		return
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
			b.mu.Lock()
			if b.inFlight || len(b.pending) == 0 || timerC != nil {
				b.mu.Unlock()
				continue
			}
			timer = time.NewTimer(arbitrageSnapshotMergeWindow)
			timerC = timer.C
			b.mu.Unlock()
		case <-timerC:
			timerC = nil
			b.flush(ctx)
			b.mu.Lock()
			if !b.inFlight && len(b.pending) > 0 {
				timer = time.NewTimer(arbitrageSnapshotMergeWindow)
				timerC = timer.C
			}
			b.mu.Unlock()
		}
	}
}

func (b *snapshotBatcher) flush(ctx context.Context) {
	s := b.scheduler
	store, ok := s.store.(arbitrageSnapshotBatchStore)
	if !ok {
		return
	}
	b.mu.Lock()
	if b.inFlight || len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	batch := make([]snapshotRequest, 0, arbitrageSnapshotBatchSize)
	writes := make([]arbitrageMarketSnapshotWrite, 0, arbitrageSnapshotBatchSize)
	for id, req := range b.pending {
		if len(batch) >= arbitrageSnapshotBatchSize {
			break
		}
		batch = append(batch, req)
		writes = append(writes, req.write)
		delete(b.pending, id)
	}
	b.inFlight = true
	b.mu.Unlock()

	byID := make(map[string]snapshotRequest, len(batch))
	for _, req := range batch {
		byID[req.write.CombinationID] = req
	}

	timeout := s.controlSummaryTimeout()
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	items, err := store.BatchUpdateArbitrageMarketSnapshots(queryCtx, writes)
	cancel()

	if err != nil {
		for _, req := range batch {
			b.deliver(req, snapshotFlushNotice{
				generation: req.generation,
				seq:        req.write.Sequence,
				failed:     true,
			})
		}
	} else {
		seen := make(map[string]struct{}, len(items))
		for _, item := range items {
			req, ok := byID[item.CombinationID]
			if !ok {
				continue
			}
			seen[item.CombinationID] = struct{}{}
			notice := snapshotFlushNotice{
				generation: req.generation,
				seq:        req.write.Sequence,
				item:       item,
			}
			if item.Applied {
				notice.applied = true
			} else {
				notice.skipped = true
			}
			b.deliver(req, notice)
		}
		for _, req := range batch {
			if _, ok := seen[req.write.CombinationID]; ok {
				continue
			}
			b.deliver(req, snapshotFlushNotice{
				generation: req.generation,
				seq:        req.write.Sequence,
				failed:     true,
			})
		}
	}

	b.mu.Lock()
	b.inFlight = false
	b.mu.Unlock()
}

func (b *snapshotBatcher) deliver(req snapshotRequest, notice snapshotFlushNotice) {
	s := b.scheduler
	if !s.runtimeMatches(req.write.CombinationID, req.runtime, req.generation) {
		return
	}
	req.runtime.snapshotNotice.Store(&notice)
	signalLatest(req.runtime.snapshotNotify)
}
