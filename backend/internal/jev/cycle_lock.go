package jev

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const cycleLockCleanupTimeout = 3 * time.Second

type CycleLease interface {
	Release() error
}

type heldCycleLock struct {
	mu            sync.Mutex
	done          bool
	unlock        func(ctx context.Context) (bool, error)
	releaseToPool func()
	destroy       func()
}

func (h *heldCycleLock) Release() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return nil
	}
	h.done = true
	cleanup, cancel := context.WithTimeout(context.Background(), cycleLockCleanupTimeout)
	defer cancel()
	unlocked, err := h.unlock(cleanup)
	if err == nil && unlocked {
		h.releaseToPool()
		return nil
	}
	h.destroy()
	if err != nil {
		return fmt.Errorf("cycle lock unlock: %w", err)
	}
	return fmt.Errorf("cycle lock unlock returned false")
}
