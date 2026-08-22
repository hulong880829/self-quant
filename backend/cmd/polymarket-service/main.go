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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	accountv1 "selfquant/backend/gen/account/v1"
	polymarketv1 "selfquant/backend/gen/polymarket/v1"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/polymarket"
	polymarketrpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.PolymarketFromEnv()
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.Level(config.LoggerLevel(cfg.LogLevel)),
	}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	accountConn, err := grpc.NewClient(
		cfg.AccountGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("account gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer accountConn.Close()

	repository := polymarket.NewRepository(pool)
	snapshots := polymarket.NewSnapshotStore()
	service := polymarket.NewService(
		repository,
		polymarket.NewGammaClient(cfg.GammaURL, cfg.HTTPTimeout),
		polymarket.NewCLOBClient(cfg.CLOBURL, cfg.HTTPTimeout),
		polymarket.NewDataClient(cfg.DataURL, cfg.HTTPTimeout),
		polymarket.NewHistoryClient(
			cfg.ChainlinkHistoryURL, cfg.ChainlinkHistoryToken, cfg.HTTPTimeout,
		),
		polymarket.NewAccountCredentialProvider(accountv1.NewAccountServiceClient(accountConn)),
		snapshots,
		cfg.PositionActiveTTL,
		cfg.PriceSampleInterval,
		logger,
	)
	service.EnableAccountStreams(ctx, cfg.CLOBUserWSURL)
	if err := service.Hydrate(ctx); err != nil {
		logger.Error("polymarket cache hydrate failed", "error", err)
		os.Exit(1)
	}
	marketStream := polymarket.NewMarketStream(cfg.CLOBWSURL, snapshots, logger)
	go func() {
		if err := service.SyncMarkets(ctx); err != nil {
			logger.Warn("initial market sync failed; serving hydrated data", "error", err)
			return
		}
		marketStream.NotifyMarketsChanged()
	}()
	go runSynchronization(ctx, service, repository, marketStream, cfg, logger)
	go polymarket.NewChainlinkStream(cfg.RTDSURL, service, logger).Run(ctx)
	go marketStream.Run(ctx)

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxSendMsgSize(16*1024*1024),
	)
	polymarketv1.RegisterPolymarketServiceServer(
		server, polymarketrpc.NewPolymarketServer(service, snapshots),
	)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info("polymarket service listening", "address", cfg.GRPCAddress)
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
}

func runSynchronization(
	ctx context.Context,
	service *polymarket.Service,
	repository *polymarket.Repository,
	marketStream *polymarket.MarketStream,
	cfg config.Polymarket,
	logger *slog.Logger,
) {
	marketTicker := time.NewTicker(cfg.MarketSyncInterval)
	quoteTicker := time.NewTicker(15 * time.Second)
	cleanupTicker := time.NewTicker(6 * time.Hour)
	defer marketTicker.Stop()
	defer quoteTicker.Stop()
	defer cleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-marketTicker.C:
			if err := service.SyncMarkets(ctx); err != nil {
				logger.Warn("market synchronization failed", "error", err)
			} else {
				marketStream.NotifyMarketsChanged()
			}
		case <-quoteTicker.C:
			if err := service.RefreshQuotes(ctx); err != nil {
				logger.Warn("quote synchronization failed", "error", err)
			}
		case <-cleanupTicker.C:
			cutoff := time.Now().Add(-cfg.PriceRetention)
			if err := repository.CleanupMarketData(ctx, cutoff); err != nil {
				logger.Warn("market data retention cleanup failed", "error", err)
			} else {
				removed := service.PruneMemory(cutoff)
				logger.Info("polymarket memory pruned", "removed_markets", removed)
			}
		}
	}
}
