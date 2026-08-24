package trader

import (
	"context"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/marketdata"
)

type executorFailStore struct {
	arbitrageStore
	mu        sync.Mutex
	execution ArbitrageExecution
	combo     ArbitrageCombination
	failures  int
}

func (s *executorFailStore) UpdateArbitrageExecution(
	_ context.Context, execution ArbitrageExecution,
) (ArbitrageExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execution = execution
	return execution, nil
}

func (s *executorFailStore) RecordArbitrageFailure(
	_ context.Context, _, message string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.combo.ErrorMessage = message
	s.combo.ConsecutiveFailures++
	return s.combo, nil
}

func (s *executorFailStore) GetArbitrageCombinationByOwner(
	_ context.Context, _, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.combo, nil
}

func (s *executorFailStore) UpdateArbitrageCombinationRuntime(
	_ context.Context, item ArbitrageCombination,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.combo = item
	return item, nil
}

func (s *executorFailStore) GetActiveArbitrageExecution(
	_ context.Context, _ string,
) (ArbitrageExecution, error) {
	return ArbitrageExecution{}, ErrNotFound
}

func (s *executorFailStore) AppendArbitrageEvent(
	context.Context, string, string, string, map[string]any,
) error {
	return nil
}

func TestArbitrageExecutorFailureKeepsCombinationRunning(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61734", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "unknown",
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combo, ArbitrageExecution{ID: "exec-1", Direction: "ask"},
		marketdata.BBO{}, marketdata.BBO{},
	)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.combo.Status != "running" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if store.execution.Status != "failed" || store.failures != 1 || store.combo.ErrorMessage == "" {
		t.Fatalf("execution=%+v failures=%d combo=%+v", store.execution, store.failures, store.combo)
	}
}

func TestArbitrageExecutorCloseDoesNotRequireFailedStatus(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61735", OwnerUsername: "admin",
		Status: "closing",
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
}
