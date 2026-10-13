package jev

import (
	"context"
	"errors"
	"testing"
)

func TestCycleLeaseUnlockFailureDoesNotReleaseToPool(t *testing.T) {
	released := false
	destroyed := false
	lease := &heldCycleLock{
		unlock: func(ctx context.Context) (bool, error) {
			if ctx.Err() != nil {
				t.Fatal("cleanup context should not already be canceled")
			}
			return false, errors.New("unlock query failed")
		},
		releaseToPool: func() { released = true },
		destroy:       func() { destroyed = true },
	}
	if err := lease.Release(); err == nil {
		t.Fatal("expected unlock error")
	}
	if released {
		t.Fatal("still-locked connection must not return to the pool")
	}
	if !destroyed {
		t.Fatal("unlock failure must Hijack+Close")
	}
}

func TestCycleLeaseUnlockFalseDoesNotReleaseToPool(t *testing.T) {
	released := false
	destroyed := false
	lease := &heldCycleLock{
		unlock:        func(context.Context) (bool, error) { return false, nil },
		releaseToPool: func() { released = true },
		destroy:       func() { destroyed = true },
	}
	if err := lease.Release(); err == nil {
		t.Fatal("expected unlock false error")
	}
	if released || !destroyed {
		t.Fatalf("released=%v destroyed=%v", released, destroyed)
	}
}

func TestCycleLeaseReleaseUsesLiveBackgroundContext(t *testing.T) {
	var sawLive bool
	lease := &heldCycleLock{
		unlock: func(ctx context.Context) (bool, error) {
			if ctx.Err() != nil {
				t.Fatal("cleanup context should not already be canceled")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("cleanup context must have a timeout")
			}
			sawLive = true
			return true, nil
		},
		releaseToPool: func() {},
		destroy:       func() { t.Fatal("successful unlock must not destroy the connection") },
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !sawLive {
		t.Fatal("unlock was not called")
	}
}

func TestCycleLeaseReleaseIsIdempotent(t *testing.T) {
	calls := 0
	lease := &heldCycleLock{
		unlock: func(context.Context) (bool, error) {
			calls++
			return true, nil
		},
		releaseToPool: func() {},
		destroy:       func() { t.Fatal("destroy") },
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unlock calls=%d", calls)
	}
}

func TestMemoryCycleLockBusySkipsSecondAcquire(t *testing.T) {
	store := NewMemoryStore()
	first, ok, err := store.AcquireCycleLock(context.Background())
	if err != nil || !ok || first == nil {
		t.Fatalf("first lock ok=%v err=%v", ok, err)
	}
	second, ok, err := store.AcquireCycleLock(context.Background())
	if err != nil || ok || second != nil {
		t.Fatalf("second lock ok=%v err=%v lease=%v", ok, err, second)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	third, ok, err := store.AcquireCycleLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("after release ok=%v err=%v", ok, err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
}
