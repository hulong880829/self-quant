package trader

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWorkPermitLimits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		workers  int
		normal   int
		reserved int
	}{
		{1, 1, 0},
		{2, 1, 1},
		{8, 6, 2},
		{16, 12, 4},
	}
	for _, test := range cases {
		normal, reserved := workPermitLimits(test.workers)
		if normal != test.normal || reserved != test.reserved {
			t.Fatalf("workers=%d normal=%d reserved=%d, want %d/%d",
				test.workers, normal, reserved, test.normal, test.reserved)
		}
	}
}

func TestWorkPermitNewExecutionDoesNotUseReserved(t *testing.T) {
	t.Parallel()
	pool := newWorkPermitPool(8)
	held := make([]*executionWorkPermit, 0, 6)
	for range 6 {
		permit := newExecutionWorkPermit(pool)
		if !permit.TryAcquire(workPermitNewExecution) {
			t.Fatal("normal permit should be available")
		}
		held = append(held, permit)
	}
	blocked := newExecutionWorkPermit(pool)
	if blocked.TryAcquire(workPermitNewExecution) {
		t.Fatal("new execution must not take reserved permits")
	}
	urgent := newExecutionWorkPermit(pool)
	if !urgent.TryAcquire(workPermitUrgent) {
		t.Fatal("urgent should use reserved permits")
	}
	if pool.InUse() != 7 {
		t.Fatalf("in use=%d", pool.InUse())
	}
	for _, permit := range held {
		permit.Close()
	}
	urgent.Close()
	if pool.InUse() != 0 {
		t.Fatalf("leaked permits=%d", pool.InUse())
	}
}

func TestWorkPermitYieldAndReacquire(t *testing.T) {
	t.Parallel()
	pool := newWorkPermitPool(1)
	permit := newExecutionWorkPermit(pool)
	if !permit.TryAcquire(workPermitNewExecution) || pool.InUse() != 1 {
		t.Fatal("expected held permit")
	}
	permit.Yield()
	if permit.Held() || pool.InUse() != 0 {
		t.Fatal("yield should release the pool token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := permit.Acquire(ctx, workPermitUrgent); err != nil || !permit.Held() {
		t.Fatalf("reacquire: held=%t err=%v", permit.Held(), err)
	}
	permit.Close()
	permit.Close()
	if pool.InUse() != 0 {
		t.Fatalf("double close leaked %d", pool.InUse())
	}
}

func TestWorkPermitAcquireCanceledContext(t *testing.T) {
	t.Parallel()
	pool := newWorkPermitPool(1)
	held := newExecutionWorkPermit(pool)
	if !held.TryAcquire(workPermitUrgent) {
		t.Fatal("hold the only permit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiting := newExecutionWorkPermit(pool)
	if err := waiting.Acquire(ctx, workPermitUrgent); err == nil {
		t.Fatal("canceled acquire should fail")
	}
	if waiting.Held() || pool.InUse() != 1 {
		t.Fatalf("canceled acquire leaked: held=%t inUse=%d", waiting.Held(), pool.InUse())
	}
	held.Close()
}

func TestWorkPermitAcquireIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	pool := newWorkPermitPool(8)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			permit := newExecutionWorkPermit(pool)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := permit.Acquire(ctx, workPermitUrgent); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			permit.Yield()
			if err := permit.Acquire(ctx, workPermitUrgent); err != nil {
				t.Errorf("reacquire: %v", err)
				return
			}
			permit.Close()
		}()
	}
	wg.Wait()
	if pool.InUse() != 0 {
		t.Fatalf("leaked permits=%d", pool.InUse())
	}
}
