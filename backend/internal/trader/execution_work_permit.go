package trader

import (
	"context"
	"sync"
	"sync/atomic"
)

type workPermitClass int

const (
	workPermitNewExecution workPermitClass = iota
	workPermitUrgent
)

type permitBank int

const (
	permitNone permitBank = iota
	permitGeneral
	permitReserved
)

type workPermitPool struct {
	general  chan struct{}
	reserved chan struct{}
	inUse    atomic.Int64
}

func workPermitLimits(workers int) (normalLimit, urgentReserved int) {
	if workers <= 0 {
		workers = 16
	}
	if workers == 1 {
		return 1, 0
	}
	urgentReserved = workers / 4
	if urgentReserved < 1 {
		urgentReserved = 1
	}
	if urgentReserved > workers-1 {
		urgentReserved = workers - 1
	}
	normalLimit = workers - urgentReserved
	if normalLimit < 1 {
		normalLimit = 1
	}
	return normalLimit, urgentReserved
}

func newWorkPermitPool(workers int) *workPermitPool {
	normal, reserved := workPermitLimits(workers)
	return &workPermitPool{
		general:  make(chan struct{}, normal),
		reserved: make(chan struct{}, reserved),
	}
}

func (p *workPermitPool) InUse() int64 {
	if p == nil {
		return 0
	}
	return p.inUse.Load()
}

func (p *workPermitPool) tryAcquire(class workPermitClass) (permitBank, bool) {
	if p == nil {
		return permitNone, true
	}
	if class == workPermitUrgent {
		select {
		case p.reserved <- struct{}{}:
			p.inUse.Add(1)
			return permitReserved, true
		default:
		}
		select {
		case p.general <- struct{}{}:
			p.inUse.Add(1)
			return permitGeneral, true
		default:
			return permitNone, false
		}
	}
	select {
	case p.general <- struct{}{}:
		p.inUse.Add(1)
		return permitGeneral, true
	default:
		return permitNone, false
	}
}

func (p *workPermitPool) acquire(ctx context.Context, class workPermitClass) (permitBank, error) {
	if p == nil {
		return permitNone, nil
	}
	if class == workPermitUrgent {
		select {
		case p.reserved <- struct{}{}:
			p.inUse.Add(1)
			return permitReserved, nil
		default:
		}
		select {
		case p.reserved <- struct{}{}:
			p.inUse.Add(1)
			return permitReserved, nil
		case p.general <- struct{}{}:
			p.inUse.Add(1)
			return permitGeneral, nil
		case <-ctx.Done():
			return permitNone, ctx.Err()
		}
	}
	select {
	case p.general <- struct{}{}:
		p.inUse.Add(1)
		return permitGeneral, nil
	case <-ctx.Done():
		return permitNone, ctx.Err()
	}
}

func (p *workPermitPool) release(bank permitBank) {
	if p == nil {
		return
	}
	switch bank {
	case permitReserved:
		<-p.reserved
	case permitGeneral:
		<-p.general
	default:
		return
	}
	p.inUse.Add(-1)
}

type executionWorkPermit struct {
	pool *workPermitPool
	mu   sync.Mutex
	held bool
	bank permitBank
}

func newExecutionWorkPermit(pool *workPermitPool) *executionWorkPermit {
	return &executionWorkPermit{pool: pool}
}

func (p *executionWorkPermit) Held() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held
}

func (p *executionWorkPermit) TryAcquire(class workPermitClass) bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held {
		return true
	}
	bank, ok := p.pool.tryAcquire(class)
	if !ok {
		return false
	}
	p.bank = bank
	p.held = true
	return true
}

func (p *executionWorkPermit) Acquire(ctx context.Context, class workPermitClass) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.held {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	bank, err := p.pool.acquire(ctx, class)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.held {
		p.mu.Unlock()
		p.pool.release(bank)
		return nil
	}
	p.bank = bank
	p.held = true
	p.mu.Unlock()
	return nil
}

func (p *executionWorkPermit) Yield() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.held {
		return
	}
	p.pool.release(p.bank)
	p.bank = permitNone
	p.held = false
}

func (p *executionWorkPermit) Close() {
	p.Yield()
}
