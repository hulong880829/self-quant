package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	fundingv1 "selfquant/backend/gen/funding/v1"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/funding"
	"selfquant/backend/internal/funding/ranking"
	fundingrpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.FundingFromEnv()
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.Level(config.LoggerLevel(cfg.LogLevel))}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go logRuntimeMemory(ctx, logger, time.Minute)

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	repository := funding.NewRepository(pool)
	snapshots := funding.NewSnapshotStore()
	rankingSnapshots := ranking.NewSnapshotStore()
	adapters := funding.EnabledFundingAdapters(cfg)
	synchronizer := funding.NewSynchronizer(repository, adapters, snapshots, logger).
		WithLock(funding.NewHistoryLock(pool))
	if err := synchronizer.Hydrate(ctx); err != nil {
		logger.Error("funding cache startup failed", "error", err)
		os.Exit(1)
	}
	rankingPersistence := ranking.NewPostgresSnapshotPersistence(pool)
	for _, period := range ranking.Periods {
		persisted, loadErr := rankingPersistence.Load(
			ctx, period, ranking.CurrentModelVersion,
		)
		if loadErr != nil {
			logger.Warn(
				"funding ranking last-good load failed",
				"period", period, "error", loadErr,
			)
			continue
		}
		if persisted != nil {
			if restoreErr := rankingSnapshots.Restore(*persisted); restoreErr != nil {
				logger.Warn(
					"funding ranking last-good restore failed",
					"period", period, "error", restoreErr,
				)
			}
		}
	}
	snapshot := snapshots.View()
	snapshotBytes := funding.EstimateSnapshotBytes(snapshot.Rates)
	logger.Info(
		"funding snapshot capacity",
		"rows", len(snapshot.Rates), "bytes", snapshotBytes,
		"row_limit", 10000, "byte_limit", 16<<20,
	)
	if !cfg.Development &&
		(cfg.PublishedExchanges["aster"] || cfg.PublishedExchanges["lighter"] ||
			cfg.SpreadEnabledExchanges["aster"] || cfg.SpreadEnabledExchanges["lighter"]) &&
		funding.SnapshotCapacityExceeded(snapshot.Rates) {
		logger.Error(
			"funding snapshot exceeds production capacity; refuse to publish aster/lighter",
			"rows", len(snapshot.Rates), "bytes", snapshotBytes,
		)
		os.Exit(1)
	}
	fundingVersions := make(chan string, 1)
	notifyFundingVersion := func() {
		version := snapshots.View().Version
		if version == "" {
			return
		}
		select {
		case fundingVersions <- version:
		default:
			select {
			case <-fundingVersions:
			default:
			}
			select {
			case fundingVersions <- version:
			default:
			}
		}
	}
	go runSynchronization(
		ctx, synchronizer, cfg.SyncInterval, cfg.InstrumentSyncInterval,
		notifyFundingVersion, logger,
	)
	if cfg.RankingEnabled {
		snapshotWrites := make(chan ranking.Period, len(ranking.Periods))
		go runSnapshotWriter(
			ctx, rankingPersistence, rankingSnapshots, snapshotWrites, logger,
		)
		go runRankingSupervisor(
			ctx, cfg, repository, rankingSnapshots, snapshots, fundingVersions,
			snapshotWrites, logger,
		)
	}

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxSendMsgSize(16*1024*1024),
	)
	fundingv1.RegisterFundingServiceServer(
		server,
		fundingrpc.NewFundingServer(
			snapshots, repository, 2*cfg.SyncInterval, rankingSnapshots,
		).WithPublication(cfg.PublishedExchanges, cfg.SpreadEnabledExchanges),
	)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}

	go func() {
		logger.Info("funding service listening", "address", cfg.GRPCAddress)
		if err := server.Serve(listener); err != nil {
			logger.Error("gRPC server stopped", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	graceful := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(graceful)
	}()
	select {
	case <-graceful:
	case <-time.After(10 * time.Second):
		server.Stop()
	}
	logger.Info("funding service shut down")
}

func logRuntimeMemory(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	logStats := func() {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		logger.Info(
			"funding runtime memory",
			"rss_bytes", currentRSSBytes(),
			"heap_alloc_bytes", stats.HeapAlloc,
			"heap_inuse_bytes", stats.HeapInuse,
			"heap_sys_bytes", stats.HeapSys,
			"heap_objects", stats.HeapObjects,
			"next_gc_bytes", stats.NextGC,
			"gc_cycles", stats.NumGC,
			"gc_pause_total_ns", stats.PauseTotalNs,
			"memory_limit_bytes", debug.SetMemoryLimit(-1),
		)
	}
	logStats()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logStats()
		}
	}
}

func currentRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

func rankingRates(rates []funding.Rate, allowed map[string]bool) []funding.Rate {
	return funding.FilterRatesByExchange(rates, allowed)
}

func runRankingSupervisor(
	ctx context.Context,
	cfg config.Funding,
	fundingRepository *funding.Repository,
	rankingSnapshots *ranking.SnapshotStore,
	fundingSnapshots *funding.SnapshotStore,
	fundingVersions <-chan string,
	snapshotWrites chan<- ranking.Period,
	logger *slog.Logger,
) {
	retryDelay := 5 * time.Second
	for {
		rankingRepository, err := ranking.Open(ctx, ranking.RepositoryConfig{
			Address: cfg.ClickHouseAddr, Database: cfg.ClickHouseDatabase,
			Table: cfg.ClickHouseTable, User: cfg.ClickHouseUser, Password: cfg.ClickHousePassword,
			TLS: cfg.ClickHouseTLS, TLSSkipVerify: cfg.ClickHouseTLSSkip,
			QueryTimeout: cfg.RankingQueryTimeout, PoolSize: cfg.RankingPoolSize,
			UseMinuteTable: cfg.RankingMinuteEnabled,
		})
		if err == nil {
			historyCache := ranking.NewHistoryCache(
				rankingRepository, 7*24*time.Hour, cfg.RankingHistoryMaxRows,
			)
			engine := ranking.NewEngine(
				historyCache, fundingRepository, rankingSnapshots, cfg.RankingRateMaxAge,
			)
			runRanking(
				ctx, engine, historyCache, rankingSnapshots, fundingSnapshots,
				fundingVersions, snapshotWrites, cfg.RankingEnabledExchanges,
				cfg.RankingInterval, cfg.RankingHistoryIncrement,
				cfg.RankingTimeout, logger,
			)
			_ = historyCache.Close()
			return
		}
		for _, period := range ranking.Periods {
			if !rankingSnapshots.View(period).LastSuccessfulAt.IsZero() {
				rankingSnapshots.MarkStale(period)
			} else {
				rankingSnapshots.MarkUnavailable(period)
			}
		}
		logger.Warn(
			"opportunity ranking waiting for clickhouse",
			"retry_in", retryDelay, "error", err,
		)
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		retryDelay = min(2*retryDelay, 2*time.Minute)
	}
}

func runSnapshotWriter(
	ctx context.Context,
	persistence ranking.SnapshotPersistence,
	snapshots *ranking.SnapshotStore,
	requests <-chan ranking.Period,
	logger *slog.Logger,
) {
	type pendingWrite struct {
		attempt int
		due     time.Time
	}
	pending := make(map[ranking.Period]pendingWrite, len(ranking.Periods))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case period := <-requests:
			if _, exists := pending[period]; !exists {
				pending[period] = pendingWrite{due: time.Now()}
			}
		case now := <-ticker.C:
			for period, state := range pending {
				if now.Before(state.due) {
					continue
				}
				snapshot := snapshots.Get(period)
				writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := persistence.Upsert(
					writeCtx, snapshot, ranking.CurrentModelVersion,
				)
				cancel()
				if err == nil {
					delete(pending, period)
					continue
				}
				state.attempt++
				delay := time.Duration(1<<min(state.attempt, 6)) * time.Second
				state.due = now.Add(delay)
				pending[period] = state
				logger.Warn(
					"funding ranking last-good persistence failed",
					"period", period, "retry_in", delay, "error", err,
				)
			}
		}
	}
}

func runRanking(
	ctx context.Context,
	engine *ranking.Engine,
	cache *ranking.HistoryCache,
	rankingSnapshots *ranking.SnapshotStore,
	fundingSnapshots *funding.SnapshotStore,
	fundingVersions <-chan string,
	snapshotWrites chan<- ranking.Period,
	rankingEnabled map[string]bool,
	interval time.Duration,
	incrementInterval time.Duration,
	timeout time.Duration,
	logger *slog.Logger,
) {
	for _, period := range ranking.Periods {
		if rankingSnapshots.View(period).LastSuccessfulAt.IsZero() {
			rankingSnapshots.MarkWarming(period)
		}
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if incrementInterval <= 0 {
		incrementInterval = 5 * time.Minute
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runExact := func(version string) bool {
		rates := rankingRates(fundingSnapshots.View().Rates, rankingEnabled)
		if len(rates) == 0 {
			return false
		}
		historyCtx, computeCtx, cancelHistory, cancelCompute := rankingTimeoutContexts(ctx, timeout)
		started := time.Now()
		err := cache.Update(historyCtx, rates, time.Now().UTC(), 7*24*time.Hour)
		cancelHistory()
		if err != nil {
			cancelCompute()
			if !historyUpdateShouldWarn(err) {
				return false
			}
			for _, period := range ranking.Periods {
				markRankingFailure(rankingSnapshots, period)
			}
			logger.Warn(
				"opportunity ranking history update failed",
				"funding_version", version, "error", err,
			)
			return false
		}
		generation := cache.Generation()
		computation, err := engine.Compute(computeCtx, ranking.Periods, rates, time.Now().UTC())
		cancelCompute()
		if err != nil {
			for _, period := range ranking.Periods {
				markRankingFailure(rankingSnapshots, period)
			}
			logger.Warn(
				"opportunity ranking refresh failed",
				"funding_version", version, "history_generation", generation.ID,
				"error", err,
			)
			return false
		}
		if fundingSnapshots.View().Version != version ||
			cache.Generation().ID != generation.ID {
			logger.Info(
				"opportunity ranking result superseded before publish",
				"funding_version", version, "history_generation", generation.ID,
			)
			return false
		}
		engine.Publish(computation.Periods, time.Now().UTC())
		for period, periodErr := range computation.Errors {
			markRankingFailure(rankingSnapshots, period)
			logger.Warn(
				"opportunity ranking period rejected",
				"period", period, "error", periodErr,
			)
		}
		if len(computation.Periods) == 0 {
			return false
		}
		for _, period := range ranking.Periods {
			if rankingSnapshots.View(period).Status != ranking.SnapshotReady {
				continue
			}
			select {
			case snapshotWrites <- period:
			default:
			}
		}
		logger.Info(
			"opportunity ranking refresh completed",
			"funding_version", version, "history_generation", generation.ID,
			"rows", generation.Rows, "slots", generation.Slots,
			"tail_slots", generation.TailSlots, "replay_slots", generation.ReplaySlots,
			"bytes", generation.Bytes, "candidates", generation.CandidateCount,
			"data_through", generation.DataThrough, "duration", time.Since(started),
		)
		return len(computation.Errors) == 0
	}
	updateTail := func() {
		if !incrementalHistoryAllowed(cache.Generation().ID) {
			return
		}
		rates := rankingRates(fundingSnapshots.View().Rates, rankingEnabled)
		if len(rates) == 0 {
			return
		}
		updateCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := cache.Update(updateCtx, rates, time.Now().UTC(), 7*24*time.Hour); err != nil {
			if !historyUpdateShouldWarn(err) {
				return
			}
			logger.Warn("opportunity history incremental refresh failed", "error", err)
		}
	}
	fallbackTicker := time.NewTicker(interval)
	defer fallbackTicker.Stop()
	incrementTicker := time.NewTicker(incrementInterval)
	defer incrementTicker.Stop()
	var lastAttemptedVersion string
	var lastSuccessfulVersion string
	for {
		select {
		case <-ctx.Done():
			return
		case version := <-fundingVersions:
			if version == "" ||
				(version == lastAttemptedVersion && version == lastSuccessfulVersion) {
				continue
			}
			lastAttemptedVersion = version
			if runExact(version) {
				lastSuccessfulVersion = version
			}
		case <-incrementTicker.C:
			updateTail()
		case <-fallbackTicker.C:
			version := fundingSnapshots.View().Version
			if version != "" && version != lastSuccessfulVersion {
				lastAttemptedVersion = version
				if runExact(version) {
					lastSuccessfulVersion = version
				}
			}
		}
	}
}

func historyUpdateShouldWarn(err error) bool {
	return err != nil && !errors.Is(err, ranking.ErrHistoryRetryDeferred)
}

func incrementalHistoryAllowed(generationID uint64) bool {
	return generationID != 0
}

func rankingTimeoutContexts(
	parent context.Context, timeout time.Duration,
) (history context.Context, compute context.Context, cancelHistory context.CancelFunc, cancelCompute context.CancelFunc) {
	history, cancelHistory = context.WithTimeout(parent, timeout)
	compute, cancelCompute = context.WithTimeout(parent, timeout)
	return history, compute, cancelHistory, cancelCompute
}

func markRankingFailure(snapshots *ranking.SnapshotStore, period ranking.Period) {
	if snapshots.View(period).LastSuccessfulAt.IsZero() {
		snapshots.MarkWarming(period)
		return
	}
	snapshots.MarkStale(period)
}

func runSynchronization(
	ctx context.Context,
	synchronizer *funding.Synchronizer,
	interval time.Duration,
	instrumentInterval time.Duration,
	onCurrentSynced func(),
	logger *slog.Logger,
) {
	newInstruments, err := synchronizer.SyncInstruments(ctx)
	if err != nil {
		logger.Warn("initial instrument synchronization completed with errors", "error", err)
	}
	if err := synchronizer.SyncCurrent(ctx); err != nil {
		logger.Warn("initial current funding synchronization completed with errors", "error", err)
	} else if onCurrentSynced != nil {
		onCurrentSynced()
	}
	var group sync.WaitGroup
	group.Add(5)
	go func() {
		defer group.Done()
		synchronizer.BackfillNewInstruments(ctx, newInstruments)
	}()
	go func() {
		defer group.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := synchronizer.SyncCurrent(ctx); err != nil {
					logger.Warn("scheduled current synchronization completed with errors", "error", err)
				} else if onCurrentSynced != nil {
					onCurrentSynced()
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		if err := synchronizer.SyncIncremental(ctx); err != nil {
			logger.Warn("initial funding history incremental synchronization completed with errors", "error", err)
		}
		if err := synchronizer.CleanupHistory(ctx); err != nil {
			logger.Warn("initial funding history cleanup failed", "error", err)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := synchronizer.SyncIncremental(ctx); err != nil {
					logger.Warn("scheduled funding history incremental synchronization completed with errors", "error", err)
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		ticker := time.NewTicker(instrumentInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				inserted, syncErr := synchronizer.SyncInstruments(ctx)
				if syncErr != nil {
					logger.Warn("instrument synchronization completed with errors", "error", syncErr)
				}
				if len(inserted) > 0 {
					synchronizer.BackfillNewInstruments(ctx, inserted)
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := synchronizer.ReconcileRecentHistory(ctx); err != nil {
					logger.Warn("recent funding history reconciliation failed", "error", err)
				}
				if err := synchronizer.CleanupHistory(ctx); err != nil {
					logger.Warn("funding history cleanup failed", "error", err)
				}
			}
		}
	}()
	group.Wait()
}
