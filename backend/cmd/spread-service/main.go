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
	spreadv1 "selfquant/backend/gen/spread/v1"
	"selfquant/backend/internal/config"
	spreadrpc "selfquant/backend/internal/rpc"
	"selfquant/backend/internal/spread"
)

func main() {
	cfg, err := config.SpreadFromEnv()
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.Level(config.LoggerLevel(cfg.LogLevel)),
	}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	repository, err := spread.Open(ctx, cfg)
	if err != nil {
		logger.Error("spread clickhouse startup failed", "error", err)
		os.Exit(1)
	}
	defer repository.Close()
	service := spread.NewService(repository, cfg.CacheTTL, int64(cfg.MaxConcurrentQuery))

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("spread gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	spreadv1.RegisterSpreadServiceServer(server, spreadrpc.NewSpreadServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info(
			"spread service listening",
			"address", cfg.GRPCAddress,
			"clickhouse", cfg.ClickHouseAddr,
			"database", cfg.ClickHouseDatabase,
			"table", cfg.ClickHouseTable,
		)
		if serveErr := server.Serve(listener); serveErr != nil {
			logger.Error("spread gRPC server stopped", "error", serveErr)
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
	logger.Info("spread service shut down")
}
