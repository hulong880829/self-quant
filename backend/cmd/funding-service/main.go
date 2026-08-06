package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
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
	synchronizer := funding.NewSynchronizer(repository, adapters, logger)
	go runSynchronization(
		ctx, synchronizer, cfg.SyncInterval, cfg.InstrumentSyncInterval, logger,
	)

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxSendMsgSize(16*1024*1024),
	)
	fundingv1.RegisterFundingServiceServer(server, fundingrpc.NewFundingServer(repository))
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

func runSynchronization(
	ctx context.Context,
	synchronizer *funding.Synchronizer,
	interval time.Duration,
	instrumentInterval time.Duration,
	logger *slog.Logger,
) {
	if err := synchronizer.SyncAll(ctx, true); err != nil {
		logger.Warn("initial synchronization completed with errors", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	instrumentTicker := time.NewTicker(instrumentInterval)
	defer instrumentTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := synchronizer.SyncAll(ctx, false); err != nil {
				logger.Warn("scheduled synchronization completed with errors", "error", err)
			}
		case <-instrumentTicker.C:
			if err := synchronizer.SyncAll(ctx, true); err != nil {
				logger.Warn("instrument/history synchronization completed with errors", "error", err)
			}
		}
	}
}
