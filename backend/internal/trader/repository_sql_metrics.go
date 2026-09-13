package trader

import "sync/atomic"

type RepositorySQLStats struct {
	FundingSyncCalls  uint64
	FundingCandidates uint64
	FundingWrites     uint64
	FundingNoops      uint64
	FundingErrors     uint64
	FundingDurationMS uint64
	LeaseCalls        uint64
	LeaseReturned     uint64
	LeaseWrites       uint64
	LeaseErrors       uint64
	LeaseDurationMS   uint64
	ReplayLoads       uint64
	ReplayOrderRows   uint64
	ReplayFillRows    uint64
	ReplayDurationMS  uint64
}

type repositorySQLMetrics struct {
	fundingSyncCalls  atomic.Uint64
	fundingCandidates atomic.Uint64
	fundingWrites     atomic.Uint64
	fundingNoops      atomic.Uint64
	fundingErrors     atomic.Uint64
	fundingDurationMS atomic.Uint64
	leaseCalls        atomic.Uint64
	leaseReturned     atomic.Uint64
	leaseWrites       atomic.Uint64
	leaseErrors       atomic.Uint64
	leaseDurationMS   atomic.Uint64
	replayLoads       atomic.Uint64
	replayOrderRows   atomic.Uint64
	replayFillRows    atomic.Uint64
	replayDurationMS  atomic.Uint64
}

func (r *Repository) recordArbitrageReplay(replay arbitrageReplay) {
	r.sqlMetrics.replayLoads.Add(1)
	r.sqlMetrics.replayOrderRows.Add(uint64(replay.orderRowCount))
	r.sqlMetrics.replayFillRows.Add(uint64(replay.fillRowCount))
	r.sqlMetrics.replayDurationMS.Add(uint64(replay.loadDuration.Milliseconds()))
}

func (r *Repository) SQLStats() RepositorySQLStats {
	return RepositorySQLStats{
		FundingSyncCalls:  r.sqlMetrics.fundingSyncCalls.Load(),
		FundingCandidates: r.sqlMetrics.fundingCandidates.Load(),
		FundingWrites:     r.sqlMetrics.fundingWrites.Load(),
		FundingNoops:      r.sqlMetrics.fundingNoops.Load(),
		FundingErrors:     r.sqlMetrics.fundingErrors.Load(),
		FundingDurationMS: r.sqlMetrics.fundingDurationMS.Load(),
		LeaseCalls:        r.sqlMetrics.leaseCalls.Load(),
		LeaseReturned:     r.sqlMetrics.leaseReturned.Load(),
		LeaseWrites:       r.sqlMetrics.leaseWrites.Load(),
		LeaseErrors:       r.sqlMetrics.leaseErrors.Load(),
		LeaseDurationMS:   r.sqlMetrics.leaseDurationMS.Load(),
		ReplayLoads:       r.sqlMetrics.replayLoads.Load(),
		ReplayOrderRows:   r.sqlMetrics.replayOrderRows.Load(),
		ReplayFillRows:    r.sqlMetrics.replayFillRows.Load(),
		ReplayDurationMS:  r.sqlMetrics.replayDurationMS.Load(),
	}
}
