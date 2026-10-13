package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	fundingv1 "selfquant/backend/gen/funding/v1"
	jevv1 "selfquant/backend/gen/jev/v1"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/jev"
	jevrpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.JevFromEnv()
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.Level(config.LoggerLevel(cfg.LogLevel)),
	}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.OpenWithOptions(ctx, cfg.DatabaseURL, database.OpenOptions{ApplicationName: "jev-service"})
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	quotes, err := jev.OpenQuotes(ctx, cfg)
	if err != nil {
		logger.Error("clickhouse startup failed", "error", err)
		os.Exit(1)
	}
	defer quotes.Close()

	fundingConn, err := grpc.NewClient(
		cfg.FundingGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
	)
	if err != nil {
		logger.Error("funding gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer fundingConn.Close()

	store := jev.NewRepository(pool)
	service := jev.NewService(cfg, store)
	engine := jev.NewEngine(
		cfg, store, jev.NewFundingClient(fundingv1.NewFundingServiceClient(fundingConn)),
		quotes, jev.NewPostgresSpots(pool), jev.NewTypeSafeClient(cfg, nil), logger,
	)
	engine.SetSnapshotHandler(service.SetSnapshot)
	if err := engine.RecoverAndStart(ctx); err != nil {
		logger.Error("jev recovery failed", "error", err)
		os.Exit(1)
	}
	go jev.NewRetentionWorker(store, store, logger).RunLoop(ctx)
	go engine.RunLoop(ctx)

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("jev gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	jevv1.RegisterJevServiceServer(server, jevrpc.NewJevServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info(
			"jev service listening",
			"address", cfg.GRPCAddress,
			"typesafe_configured", strings.TrimSpace(cfg.TypeSafeAPIKey) != "",
			"enabled", cfg.Enabled,
		)
		if serveErr := server.Serve(listener); serveErr != nil {
			logger.Error("jev gRPC server stopped", "error", serveErr)
			stop()
		}
	}()
	<-ctx.Done()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		server.Stop()
	}
	logger.Info("jev service shut down")
}
