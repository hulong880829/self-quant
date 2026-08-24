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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	accountv1 "selfquant/backend/gen/account/v1"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	traderrpc "selfquant/backend/internal/rpc"
	"selfquant/backend/internal/trader"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
	"selfquant/backend/internal/trader/orderstream"
)

func main() {
	cfg, err := config.TraderFromEnv()
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
		logger.Error("trader database startup failed", "error", err)
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

	repository := trader.NewRepository(pool)
	catalog := trader.NewInstrumentCatalog(pool, 10*time.Minute)
	credentialProvider := trader.NewAccountCredentialProvider(accountv1.NewAccountServiceClient(accountConn))
	venues := exchange.NewRegistry(&http.Client{Timeout: cfg.HTTPTimeout}, map[string]string{
		"binance": cfg.BinanceURL, "okx": cfg.OKXURL, "bybit": cfg.BybitURL,
		"bitget": cfg.BitgetURL, "gate": cfg.GateURL,
	})
	service := trader.NewService(
		repository,
		catalog,
		credentialProvider,
		venues,
		cfg.HTTPTimeout,
		logger,
	)
	service.ConfigureTwap(repository)
	service.ConfigureArbitrage(repository)
	reconciler := trader.NewReconciler(
		repository, catalog, credentialProvider, venues, cfg.InternalToken,
		cfg.ReconcileInterval, cfg.HTTPTimeout, cfg.ReconcileBatchSize,
		cfg.ReconcileWorkers, logger,
	)
	twapScheduler := trader.NewTwapScheduler(
		repository, repository, service, catalog, credentialProvider, venues,
		cfg.InternalToken, cfg.TwapScheduleInterval, cfg.TwapScheduleLease,
		cfg.HTTPTimeout, cfg.TwapScheduleBatch, cfg.TwapScheduleWorkers, logger,
	)
	go twapScheduler.Run(ctx)
	twapCleanup := trader.NewTwapCleanup(
		repository, cfg.TwapRetention, cfg.TwapCleanupInterval,
		cfg.TwapCleanupBatch, cfg.TwapCleanupMaxBatches, logger,
	)
	go twapCleanup.Run(ctx)
	marketData, err := marketdata.New(marketdata.Options{
		StaleAfter: cfg.ArbitrageBBOStale,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("arbitrage market data startup failed", "error", err)
		os.Exit(1)
	}
	defer marketData.Close()
	var orderStreams *orderstream.Manager
	if cfg.ArbitrageOrderStreamEnabled {
		urls := orderstream.DefaultURLs()
		overrideOrderWSURL(&urls.Binance, cfg.BinanceOrderWSURL)
		overrideOrderWSURL(&urls.OKX, cfg.OKXOrderWSURL)
		overrideOrderWSURL(&urls.Bybit, cfg.BybitOrderWSURL)
		overrideOrderWSURL(&urls.Bitget, cfg.BitgetOrderWSURL)
		overrideOrderWSURL(&urls.Gate, cfg.GateOrderWSURL)
		orderStreams, err = orderstream.New(orderstream.Options{
			URLs: urls, HTTPClient: &http.Client{Timeout: cfg.HTTPTimeout},
			Heartbeat: cfg.OrderStreamHeartbeat, StaleAfter: cfg.OrderStreamStale,
			ListenKeyRefresh: cfg.OrderStreamListenKeyRefresh,
			ReconnectInitial: cfg.OrderStreamReconnectInitial,
			ReconnectMax:     cfg.OrderStreamReconnectMax,
			IdleTimeout:      cfg.OrderStreamSessionIdle,
		})
		if err != nil {
			logger.Error("arbitrage private order stream startup failed", "error", err)
			os.Exit(1)
		}
		defer orderStreams.Close()
	}
	reconciler.ConfigureOrderStreams(orderStreams, cfg.OrderStreamRESTAudit)
	go reconciler.Run(ctx)
	arbitrageExecutor := trader.NewArbitrageExecutor(
		repository, repository, service, catalog, credentialProvider, venues, marketData,
		cfg.InternalToken, cfg.ArbitrageObserverInterval, cfg.ArbitrageRepriceTicks,
		cfg.ArbitrageIOCProtectionTicks, cfg.ArbitrageIOCRetries, cfg.HTTPTimeout, logger,
	)
	arbitrageExecutor.ConfigureOrderStreams(orderStreams, cfg.OrderStreamRESTAudit)
	arbitrageScheduler := trader.NewArbitrageScheduler(
		repository, marketData, arbitrageExecutor,
		cfg.ArbitrageScheduleInterval, cfg.ArbitrageScheduleLease,
		cfg.ArbitrageScheduleBatch, cfg.ArbitrageScheduleWorkers,
		cfg.ArbitrageMaxActiveAccount, cfg.ArbitrageMaxActiveVenue,
		cfg.ArbitrageDryRun, logger,
	)
	go arbitrageScheduler.Run(ctx)
	arbitrageMetrics := trader.NewArbitrageMetricsReporter(
		arbitrageScheduler, marketData, orderStreams, arbitrageExecutor, time.Minute, logger,
	)
	go arbitrageMetrics.Run(ctx)
	arbitrageCleanup := trader.NewArbitrageCleanup(
		repository, cfg.ArbitrageRetention, cfg.ArbitrageCleanupInterval,
		cfg.ArbitrageCleanupBatch, cfg.ArbitrageCleanupMaxBatches, logger,
	)
	go arbitrageCleanup.Run(ctx)

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("trader gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	traderv1.RegisterTraderServiceServer(server, traderrpc.NewTraderServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info("trader service listening", "address", cfg.GRPCAddress)
		if serveErr := server.Serve(listener); serveErr != nil {
			logger.Error("trader gRPC server stopped", "error", serveErr)
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
	logger.Info("trader service shut down")
}

func overrideOrderWSURL(urls *orderstream.VenueURLs, endpoint string) {
	if endpoint == "" {
		return
	}
	urls.SpotWS = endpoint
	urls.PerpetualWS = endpoint
}
