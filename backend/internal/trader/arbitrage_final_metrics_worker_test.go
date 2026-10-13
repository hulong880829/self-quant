package trader

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

type finalMetricsFakeStore struct {
	mu       sync.Mutex
	items    map[string]ArbitrageCombination
	claimed  map[string]time.Time
	missing  map[string]int
	persists int
	retries  int
	releases int
}

func (s *finalMetricsFakeStore) ClaimDueFinalMetrics(_ context.Context, limit int) ([]ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	claimed := make([]ArbitrageCombination, 0)
	for id, item := range s.items {
		if len(claimed) >= limit {
			break
		}
		if item.FinalMetricsStatus == finalMetricsCompleted || !item.FinalMetricsRetryFinite {
			continue
		}
		if !item.FinalMetricsNextRetryAt.IsZero() && item.FinalMetricsNextRetryAt.After(now) {
			continue
		}
		if !combinationLooksFlat(item) && item.Status != "closing" {
			continue
		}
		until, ok := s.claimed[id]
		if ok && until.After(now) {
			continue
		}
		item.FinalMetricsNextRetryAt = now.Add(finalMetricsClaimLease)
		item.FinalMetricsRetryFinite = true
		s.items[id] = item
		s.claimed[id] = item.FinalMetricsNextRetryAt
		claimed = append(claimed, item)
	}
	return claimed, nil
}

func (s *finalMetricsFakeStore) PersistFinalArbitragePositionMetrics(
	_ context.Context, id string, expectedVersion int64, excludeResidual bool, _ time.Time,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if excludeResidual {
		return ArbitrageCombination{}, errors.New("exclude residual is not supported")
	}
	item := s.items[id]
	if item.Version != expectedVersion || item.FinalMetricsStatus == finalMetricsCompleted {
		return item, ErrArbitrageConflict
	}
	if s.missing[id] > 0 {
		return item, ErrFinalMetricsNotReady
	}
	s.persists++
	item.FinalMetricsStatus = finalMetricsCompleted
	item.FinalMetricsRetryFinite = false
	item.FinalMetricsCompletedAt = time.Now().UTC()
	s.items[id] = item
	delete(s.claimed, id)
	return item, nil
}

func (s *finalMetricsFakeStore) RecordFinalMetricsRetry(
	_ context.Context, id string, expectedVersion int64, expectedLeaseUntil time.Time, message string,
) (ArbitrageCombination, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok || item.Version != expectedVersion ||
		!item.FinalMetricsNextRetryAt.Equal(expectedLeaseUntil) ||
		item.FinalMetricsStatus == finalMetricsCompleted ||
		(item.FinalMetricsStatus != finalMetricsPending && item.FinalMetricsStatus != finalMetricsPartial) {
		return ArbitrageCombination{}, false, nil
	}
	s.retries++
	item.FinalMetricsStatus = finalMetricsPartial
	item.FinalMetricsLastError = message
	item.FinalMetricsRetryCount++
	item.FinalMetricsNextRetryAt = time.Now().UTC().Add(time.Minute)
	item.FinalMetricsRetryFinite = true
	s.items[id] = item
	s.claimed[id] = item.FinalMetricsNextRetryAt
	return item, true, nil
}

func (s *finalMetricsFakeStore) ReleaseFinalMetricsClaim(
	_ context.Context, id string, expectedVersion int64, expectedLeaseUntil time.Time,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok || item.Version != expectedVersion ||
		!item.FinalMetricsNextRetryAt.Equal(expectedLeaseUntil) ||
		(item.FinalMetricsStatus != finalMetricsPending && item.FinalMetricsStatus != finalMetricsPartial) {
		return false, nil
	}
	s.releases++
	item.FinalMetricsNextRetryAt = time.Now().UTC()
	s.items[id] = item
	delete(s.claimed, id)
	return true, nil
}

func (s *finalMetricsFakeStore) ListMissingArbitrageOrderAverages(
	_ context.Context, ids []string,
) ([]missingArbitrageAverageOrder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orders := make([]missingArbitrageAverageOrder, 0)
	for _, id := range ids {
		for n := 0; n < s.missing[id]; n++ {
			orders = append(orders, missingArbitrageAverageOrder{CombinationID: id})
		}
	}
	return orders, nil
}

type countingCompensator struct {
	mu    sync.Mutex
	calls int
	ids   []string
}

func (c *countingCompensator) Compensate(_ context.Context, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.ids = append(c.ids, ids...)
}

func TestFinalMetricsWorkerCompletesFlatClosedOnce(t *testing.T) {
	store := &finalMetricsFakeStore{
		items: map[string]ArbitrageCombination{
			"combo": {
				ID: "combo", Status: "closed", Version: 3,
				FinalMetricsStatus: finalMetricsPending, FinalMetricsRetryFinite: true,
				LegABasePosition: "0", LegBBasePosition: "0", CarryBaseQuantity: "0",
			},
		},
		claimed: map[string]time.Time{},
		missing: map[string]int{},
	}
	compensator := &countingCompensator{}
	worker := NewArbitrageFinalMetricsWorker(store, compensator, nil)
	worker.runOnce(context.Background())
	worker.runOnce(context.Background())
	if store.persists != 1 || compensator.calls != 1 {
		t.Fatalf("persists=%d compensate=%d", store.persists, compensator.calls)
	}
	if store.items["combo"].FinalMetricsStatus != finalMetricsCompleted {
		t.Fatalf("status=%s", store.items["combo"].FinalMetricsStatus)
	}
}

func TestFinalMetricsWorkerClaimPreventsSecondGetOrder(t *testing.T) {
	store := &finalMetricsFakeStore{
		items: map[string]ArbitrageCombination{
			"combo": {
				ID: "combo", Status: "closed", Version: 1,
				FinalMetricsStatus: finalMetricsPending, FinalMetricsRetryFinite: true,
				LegABasePosition: "0", LegBBasePosition: "0", CarryBaseQuantity: "0",
			},
		},
		claimed: map[string]time.Time{},
		missing: map[string]int{"combo": 30},
	}
	first := &countingCompensator{}
	second := &countingCompensator{}
	NewArbitrageFinalMetricsWorker(store, first, nil).runOnce(context.Background())
	NewArbitrageFinalMetricsWorker(store, second, nil).runOnce(context.Background())
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("first=%d second=%d", first.calls, second.calls)
	}
	if store.items["combo"].Status != "closed" || store.persists != 0 {
		t.Fatalf("item=%+v persists=%d", store.items["combo"], store.persists)
	}
}

func TestFinalMetricsWorkerSkipsInfinityAndDust(t *testing.T) {
	store := &finalMetricsFakeStore{
		items: map[string]ArbitrageCombination{
			"dust": {
				ID: "dust", Status: "closed", Version: 1,
				FinalMetricsStatus: finalMetricsPartial, FinalMetricsRetryFinite: false,
				LegABasePosition: "1", LegBBasePosition: "0", CarryBaseQuantity: "1",
			},
			"done": {
				ID: "done", Status: "closed", Version: 1,
				FinalMetricsStatus: finalMetricsCompleted, FinalMetricsRetryFinite: false,
				LegABasePosition: "0", LegBBasePosition: "0", CarryBaseQuantity: "0",
			},
		},
		claimed: map[string]time.Time{},
		missing: map[string]int{},
	}
	compensator := &countingCompensator{}
	NewArbitrageFinalMetricsWorker(store, compensator, nil).runOnce(context.Background())
	if compensator.calls != 0 || store.persists != 0 {
		t.Fatalf("calls=%d persists=%d", compensator.calls, store.persists)
	}
}

func TestFinalMetricsWorkerUsesFakeGetOrderForThirtyZeroAverages(t *testing.T) {
	orders := make([]missingArbitrageAverageOrder, 0, 30)
	results := make(map[string]exchange.Result, 30)
	for index := 0; index < 30; index++ {
		order := sampleMissingAverageOrder(
			"zero-average-"+string(rune('a'+index/26))+string(rune('a'+index%26)),
			"binance", 1, 10,
		)
		order.CombinationID = "combo"
		orders = append(orders, order)
		results[order.ClientOrderID] = exchange.Result{
			AveragePrice: "100", FilledQuantity: "2", VenueOrderID: order.VenueOrderID,
		}
	}
	store := &finalMetricsFakeStore{
		items: map[string]ArbitrageCombination{
			"combo": {
				ID: "combo", Status: "closed", Version: 4,
				FinalMetricsStatus: finalMetricsPending, FinalMetricsRetryFinite: true,
				LegABasePosition: "0", LegBBasePosition: "0", CarryBaseQuantity: "0",
			},
		},
		claimed: map[string]time.Time{},
		missing: map[string]int{},
	}
	missing := &stubMissingAverageStore{orders: orders}
	adapter := &countingGetOrderAdapter{results: results}
	compensator := newTestFillAverageCompensator(
		t, missing, nil, map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10)}, nil,
	)
	worker := NewArbitrageFinalMetricsWorker(store, compensator, nil)
	claimed, err := store.ClaimDueFinalMetrics(context.Background(), 20)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%d err=%v", len(claimed), err)
	}
	if adapter.getOrderCalls() != 0 || store.items["combo"].Status != "closed" ||
		store.items["combo"].FinalMetricsStatus != finalMetricsPending {
		t.Fatal("close state queried the venue before the worker attempt")
	}
	second := NewArbitrageFinalMetricsWorker(store, compensator, nil)
	second.runOnce(context.Background())
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("leased row GetOrder=%d", adapter.getOrderCalls())
	}
	worker.process(context.Background(), claimed[0])
	if adapter.getOrderCalls() != 30 {
		t.Fatalf("GetOrder=%d", adapter.getOrderCalls())
	}
	if store.items["combo"].FinalMetricsStatus != finalMetricsCompleted ||
		store.items["combo"].Status != "closed" || store.persists != 1 {
		t.Fatalf("item=%+v persists=%d", store.items["combo"], store.persists)
	}
	worker.runOnce(context.Background())
	if adapter.getOrderCalls() != 30 {
		t.Fatalf("completed row GetOrder=%d", adapter.getOrderCalls())
	}
}

func TestOneShotDustKeepsCompleted(t *testing.T) {
	completedAt := time.Now().UTC()
	store := &executorFailStore{combo: ArbitrageCombination{
		ID: "combo", Status: "running", RunMode: "one_shot", OneShotPhase: "exiting",
		Version: 2, LegABasePosition: "0", LegBBasePosition: "-1", CarryBaseQuantity: "-1",
		FinalMetricsStatus: finalMetricsCompleted, FinalMetricsCompletedAt: completedAt,
		FinalMetricsLastError: "",
	}}
	updated, applied, err := store.MarkOneShotExitedWithDust(
		context.Background(), store.combo.ID, store.combo.Version, "0", "-1", "-1",
	)
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	if updated.OneShotPhase != "exited" || updated.FinalMetricsStatus != finalMetricsCompleted ||
		!updated.FinalMetricsCompletedAt.Equal(completedAt) {
		t.Fatalf("updated=%+v", updated)
	}
}
