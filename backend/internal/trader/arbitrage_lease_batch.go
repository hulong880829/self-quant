package trader

import (
	"context"
	"sync"
	"time"
)

type leaseRequest struct {
	id         string
	runtime    *arbitrageRuntime
	generation uint64
}

type leaseBatcher struct {
	scheduler *ArbitrageScheduler
	mu        sync.Mutex
	pending   map[string]leaseRequest
	wake      chan struct{}
	inFlight  bool
}

func newLeaseBatcher(scheduler *ArbitrageScheduler) *leaseBatcher {
	return &leaseBatcher{
		scheduler: scheduler,
		pending:   make(map[string]leaseRequest),
		wake:      make(chan struct{}, 1),
	}
}

func (b *leaseBatcher) Submit(runtime *arbitrageRuntime) {
	if b == nil || runtime == nil {
		return
	}
	b.mu.Lock()
	b.pending[runtime.combination.ID] = leaseRequest{
		id: runtime.combination.ID, runtime: runtime, generation: runtime.generation,
	}
	b.mu.Unlock()
	signalLatest(b.wake)
}

func (b *leaseBatcher) Remove(id string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

func (b *leaseBatcher) Run(ctx context.Context) {
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
			timer = time.NewTimer(arbitrageLeaseMergeWindow)
			timerC = timer.C
			b.mu.Unlock()
		case <-timerC:
			timerC = nil
			b.flush(ctx)
			b.mu.Lock()
			if !b.inFlight && len(b.pending) > 0 {
				timer = time.NewTimer(arbitrageLeaseMergeWindow)
				timerC = timer.C
			}
			b.mu.Unlock()
		}
	}
}

func (b *leaseBatcher) flush(ctx context.Context) {
	s := b.scheduler
	store, ok := s.store.(arbitrageLeaseBatchStore)
	if !ok {
		return
	}
	b.mu.Lock()
	if b.inFlight || len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	batch := make([]leaseRequest, 0, arbitrageLeaseBatchSize)
	for id, req := range b.pending {
		if len(batch) >= arbitrageLeaseBatchSize {
			break
		}
		batch = append(batch, req)
		delete(b.pending, id)
	}
	b.inFlight = true
	b.mu.Unlock()

	ids := make([]string, 0, len(batch))
	for _, req := range batch {
		ids = append(ids, req.id)
	}
	timeout := s.controlSummaryTimeout()
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	renewed, err := store.RenewArbitrageLeases(queryCtx, ids, s.lease)
	cancel()

	for _, req := range batch {
		notice := &leaseRenewNotice{generation: req.generation}
		if err != nil {
			notice.failed = true
		} else {
			notice.renewed = renewed[req.id]
		}
		if !s.runtimeMatches(req.id, req.runtime, req.generation) {
			continue
		}
		req.runtime.leaseNotice.Store(notice)
		signalLatest(req.runtime.leaseNotify)
	}

	b.mu.Lock()
	b.inFlight = false
	b.mu.Unlock()
}
