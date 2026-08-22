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
	reportv1 "selfquant/backend/gen/report/v1"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/report"
	reportrpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.ReportFromEnv()
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewJSONHandler(
		os.Stdout,
		&slog.HandlerOptions{Level: slog.Level(config.LoggerLevel(cfg.LogLevel))},
	))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("report database startup failed", "error", err)
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
	accountClient := accountv1.NewAccountServiceClient(accountConn)
	location, _ := time.LoadLocation(cfg.Timezone)
	repository := report.NewRepository(pool)
	service := report.NewService(repository, accountClient, location)
	metrics := &report.Metrics{}

	source, err := report.NewGRPCAccountSource(accountClient, cfg.InternalToken)
	if err != nil {
		logger.Error("report account source unavailable", "error", err)
		os.Exit(1)
	}
	scheduler := report.NewScheduler(repository, source, metrics, logger, location)
	go scheduler.Run(ctx)

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("report gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	reportv1.RegisterReportServiceServer(server, reportrpc.NewReportServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info("report service listening", "address", cfg.GRPCAddress, "timezone", cfg.Timezone)
		if serveErr := server.Serve(listener); serveErr != nil {
			logger.Error("report gRPC server stopped", "error", serveErr)
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
	logger.Info("report service shut down", "metrics", metrics.Snapshot())
}
