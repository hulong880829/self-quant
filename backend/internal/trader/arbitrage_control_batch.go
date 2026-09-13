package trader

import (
	"context"
	"time"
)

type controlProbeSnapshot struct {
	ready       bool
	queryFailed bool
	missing     bool
	summary     arbitrageControlSummary
}

type leaseRenewNotice struct {
	generation uint64
	failed     bool
	renewed    bool
}

type snapshotFlushNotice struct {
	generation uint64
	seq        uint64
	applied    bool
	skipped    bool
	failed     bool
	item       arbitrageMarketSnapshotBatchItem
}

func chunkStrings(ids []string, size int) [][]string {
	if size <= 0 || len(ids) == 0 {
		if len(ids) == 0 {
			return nil
		}
		return [][]string{ids}
	}
	chunks := make([][]string, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

func signalLatest(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *ArbitrageScheduler) controlSummaryTimeout() time.Duration {
	if s == nil {
		return 2 * time.Second
	}
	timeout := 2 * s.controlInterval
	if timeout < 2*time.Second {
		return 2 * time.Second
	}
	return timeout
}

type controlSummaryBatcher struct {
	scheduler *ArbitrageScheduler
}

func newControlSummaryBatcher(scheduler *ArbitrageScheduler) *controlSummaryBatcher {
	return &controlSummaryBatcher{scheduler: scheduler}
}

func (b *controlSummaryBatcher) Run(ctx context.Context) {
	if b == nil || b.scheduler == nil {
		return
	}
	interval := b.scheduler.controlInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flush(ctx)
		}
	}
}

func (b *controlSummaryBatcher) flush(ctx context.Context) {
	s := b.scheduler
	store, ok := s.store.(arbitrageControlSummaryBatchStore)
	if !ok {
		return
	}
	s.mu.Lock()
	type replica struct {
		id         string
		runtime    *arbitrageRuntime
		generation uint64
	}
	replicas := make([]replica, 0, len(s.runtimes))
	ids := make([]string, 0, len(s.runtimes))
	for id, runtime := range s.runtimes {
		replicas = append(replicas, replica{id: id, runtime: runtime, generation: runtime.generation})
		ids = append(ids, id)
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	byID := make(map[string]replica, len(replicas))
	for _, item := range replicas {
		byID[item.id] = item
	}
	timeout := s.controlSummaryTimeout()
	for _, chunk := range chunkStrings(ids, arbitrageControlSummaryBatchSize) {
		queryCtx, cancel := context.WithTimeout(ctx, timeout)
		batch, err := store.ListArbitrageControlSummaries(queryCtx, chunk)
		cancel()
		if err != nil {
			for _, id := range chunk {
				item, ok := byID[id]
				if !ok {
					continue
				}
				s.storeControlProbe(item.id, item.runtime, item.generation, controlProbeSnapshot{
					ready: true, queryFailed: true,
				})
			}
			continue
		}
		for _, id := range chunk {
			item, ok := byID[id]
			if !ok {
				continue
			}
			if summary, found := batch.Found[id]; found {
				s.storeControlProbe(item.id, item.runtime, item.generation, controlProbeSnapshot{
					ready: true, summary: summary,
				})
				continue
			}
			s.storeControlProbe(item.id, item.runtime, item.generation, controlProbeSnapshot{
				ready: true, missing: true,
			})
		}
	}
}

func (s *ArbitrageScheduler) storeControlProbe(
	id string,
	runtime *arbitrageRuntime,
	generation uint64,
	probe controlProbeSnapshot,
) {
	if runtime == nil || !s.runtimeMatches(id, runtime, generation) {
		return
	}
	runtime.controlProbe.Store(probe)
}

func (s *ArbitrageScheduler) runtimeMatches(
	id string,
	runtime *arbitrageRuntime,
	generation uint64,
) bool {
	if s == nil || runtime == nil {
		return false
	}
	s.mu.Lock()
	current := s.runtimes[id]
	s.mu.Unlock()
	return current == runtime && runtime.generation == generation
}
