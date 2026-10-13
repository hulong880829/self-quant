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
	adminv1 "selfquant/backend/gen/admin/v1"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/admin"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	accountrpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.AdminFromEnv()
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

	signer, err := admin.LoadSigner(cfg.JWTPrivateKeyFile, cfg.AccessTokenTTL)
	if err != nil {
		logger.Error("jwt signer startup failed", "error", err)
		os.Exit(1)
	}
	accountConn, err := grpc.NewClient(cfg.AccountGRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("account gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer accountConn.Close()

	traderConn, err := grpc.NewClient(cfg.TraderGRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("trader gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer traderConn.Close()

	service := admin.NewService(
		admin.NewRepository(pool), signer, cfg.GatewayInternalToken, cfg.AdminInternalToken,
	).WithAccountSummary(admin.NewAccountSummaryClient(accountv1.NewAccountServiceClient(accountConn))).
		WithTraderCounts(admin.NewTraderCountClient(traderv1.NewTraderServiceClient(traderConn)))

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	adminv1.RegisterAdminServiceServer(server, accountrpc.NewAdminServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info("admin service listening", "address", cfg.GRPCAddress)
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
	logger.Info("admin service shut down")
}
