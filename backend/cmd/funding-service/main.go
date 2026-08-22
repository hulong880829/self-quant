package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
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
	"selfquant/backend/internal/exchange"
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

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	repository := funding.NewRepository(pool)
	snapshots := funding.NewSnapshotStore()
	rankingSnapshots := ranking.NewSnapshotStore()
	availableAdapters := []exchange.Adapter{
		exchange.NewBinance(cfg.HTTPTimeout), exchange.NewOKX(cfg.HTTPTimeout),
		exchange.NewBybit(cfg.HTTPTimeout), exchange.NewBitget(cfg.HTTPTimeout),
		exchange.NewGate(cfg.HTTPTimeout), exchange.NewHyperliquid(cfg.HTTPTimeout),
	}
	adapters := make([]exchange.Adapter, 0, len(availableAdapters))
	for _, adapter := range availableAdapters {
		if cfg.EnabledExchanges[adapter.Name()] {
			adapters = append(adapters, adapter)
		}
	}
	synchronizer := funding.NewSynchronizer(repository, adapters, snapshots, logger)
	if err := synchronizer.Hydrate(ctx); err != nil {
		logger.Error("funding cache startup failed", "error", err)
		os.Exit(1)
	}
	go runSynchronization(
		ctx, synchronizer, cfg.SyncInterval, cfg.InstrumentSyncInterval, logger,
	)
	if cfg.RankingEnabled {
		rankingRepository, openErr := ranking.Open(ctx, ranking.RepositoryConfig{
			Address: cfg.ClickHouseAddr, Database: cfg.ClickHouseDatabase,
			Table: cfg.ClickHouseTable, User: cfg.ClickHouseUser, Password: cfg.ClickHousePassword,
			TLS: cfg.ClickHouseTLS, TLSSkipVerify: cfg.ClickHouseTLSSkip,
			QueryTimeout: cfg.RankingQueryTimeout, PoolSize: cfg.RankingPoolSize,
		})
		if openErr != nil {
			logger.Warn("opportunity ranking disabled because clickhouse is unavailable", "error", openErr)
		} else {
			defer rankingRepository.Close()
			rankingEngine := ranking.NewEngine(
				rankingRepository, rankingSnapshots, 2*cfg.SyncInterval,
			)
			go runRanking(
				ctx, rankingEngine, snapshots, cfg.Ranking1hInterval,
				cfg.RankingSlowInterval, logger,
			)
		}
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
		),
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

func runRanking(
	ctx context.Context,
	engine *ranking.Engine,
	fundingSnapshots *funding.SnapshotStore,
	fastInterval time.Duration,
	slowInterval time.Duration,
	logger *slog.Logger,
) {
	var refreshMu sync.Mutex
	refresh := func(periods []ranking.Period) {
		refreshMu.Lock()
		defer refreshMu.Unlock()
		snapshot := fundingSnapshots.Get()
		if len(snapshot.Rates) == 0 {
			return
		}
		if err := engine.Refresh(ctx, periods, snapshot.Rates, time.Now().UTC()); err != nil {
			logger.Warn("opportunity ranking refresh failed", "periods", periods, "error", err)
		}
	}
	refresh(ranking.Periods)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		ticker := time.NewTicker(fastInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh([]ranking.Period{ranking.Period1h})
			}
		}
	}()
	go func() {
		defer group.Done()
		ticker := time.NewTicker(slowInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh([]ranking.Period{ranking.Period4h, ranking.Period8h, ranking.Period24h})
			}
		}
	}()
	group.Wait()
}

func runSynchronization(
	ctx context.Context,
	synchronizer *funding.Synchronizer,
	interval time.Duration,
	instrumentInterval time.Duration,
	logger *slog.Logger,
) {
	if err := synchronizer.SyncInstruments(ctx); err != nil {
		logger.Warn("initial instrument synchronization completed with errors", "error", err)
	}
	if err := synchronizer.SyncCurrent(ctx); err != nil {
		logger.Warn("initial current funding synchronization completed with errors", "error", err)
	}
	var group sync.WaitGroup
	group.Add(4)
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
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		if err := synchronizer.SyncHistory(ctx, true); err != nil {
			logger.Warn("initial funding history synchronization completed with errors", "error", err)
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
				if err := synchronizer.SyncHistory(ctx, false); err != nil {
					logger.Warn("scheduled funding history synchronization completed with errors", "error", err)
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
				if err := synchronizer.SyncInstruments(ctx); err != nil {
					logger.Warn("instrument synchronization completed with errors", "error", err)
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
