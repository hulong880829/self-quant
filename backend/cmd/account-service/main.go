package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	accountv1 "selfquant/backend/gen/account/v1"
	"selfquant/backend/internal/account"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/polymarket"
	"selfquant/backend/internal/polymarketauth"
	accountrpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.AccountFromEnv()
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.Level(config.LoggerLevel(cfg.LogLevel)),
	}))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	tokens, err := account.NewTokenService(cfg.TokenSecret, cfg.TokenTTL)
	if err != nil {
		logger.Error("token service startup failed", "error", err)
		os.Exit(1)
	}
	cipher, err := account.NewCredentialCipher(cfg.CredentialsKey)
	if err != nil {
		logger.Error("credentials cipher startup failed", "error", err)
		os.Exit(1)
	}
	service := account.NewService(account.NewRepository(pool), tokens).
		WithTrading(account.NewTradingRepository(pool), cipher).
		WithPolymarket(
			account.NewPolymarketCredentialRepository(pool),
			polymarketauth.NewClient(cfg.PolymarketCLOBURL, cfg.HTTPTimeout),
		).
		WithInstrumentCatalog(account.NewInstrumentCatalog(pool, 10*time.Minute)).
		WithSnapshots(
			portfolio.NewRegistry(&http.Client{Timeout: cfg.HTTPTimeout}, map[string]string{
				"binance": cfg.BinancePortfolioURL, "okx": cfg.OKXAccountURL,
				"bitget": cfg.BitgetAccountURL, "bybit": cfg.BybitAccountURL,
				"gate": cfg.GateAccountURL,
			}),
			polymarket.NewDataClient(cfg.PolymarketDataURL, cfg.HTTPTimeout),
			polymarket.NewCLOBClient(cfg.PolymarketCLOBURL, cfg.HTTPTimeout),
			cfg.AccountSnapshotTTL,
		).
		WithInternalReports(cfg.ReportInternalToken, account.NewTradeFillRepository(pool)).
		WithInternalTrader(cfg.TraderInternalToken).
		WithAICredentials(account.NewAICredentialRepository(pool))

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	accountv1.RegisterAccountServiceServer(server, accountrpc.NewAccountServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}

	go func() {
		logger.Info("account service listening", "address", cfg.GRPCAddress)
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
	logger.Info("account service shut down")
}
