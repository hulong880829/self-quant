package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	accountv1 "selfquant/backend/gen/account/v1"
	adminv1 "selfquant/backend/gen/admin/v1"
	aiv1 "selfquant/backend/gen/ai/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
	jevv1 "selfquant/backend/gen/jev/v1"
	polymarketv1 "selfquant/backend/gen/polymarket/v1"
	reportv1 "selfquant/backend/gen/report/v1"
	spreadv1 "selfquant/backend/gen/spread/v1"
	traderv1 "selfquant/backend/gen/trader/v1"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/gateway"
)

func main() {
	cfg := config.GatewayFromEnv()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.Level(config.LoggerLevel(cfg.LogLevel))}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	accountConn, err := grpc.NewClient(
		cfg.AccountGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("account gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer accountConn.Close()

	polymarketConn, err := grpc.NewClient(
		cfg.PolymarketGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
	)
	if err != nil {
		logger.Error("polymarket gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer polymarketConn.Close()

	reportConn, err := grpc.NewClient(
		cfg.ReportGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("report gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer reportConn.Close()

	traderConn, err := grpc.NewClient(
		cfg.TraderGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("trader gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer traderConn.Close()

	aiConn, err := grpc.NewClient(
		cfg.AIGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("ai gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer aiConn.Close()

	spreadConn, err := grpc.NewClient(
		cfg.SpreadGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8*1024*1024)),
	)
	if err != nil {
		logger.Error("spread gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer spreadConn.Close()

	var jevClient jevv1.JevServiceClient
	if strings.TrimSpace(cfg.JevGRPCAddress) != "" {
		jevConn, err := grpc.NewClient(
			cfg.JevGRPCAddress,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8*1024*1024)),
		)
		if err != nil {
			logger.Warn("jev gRPC client skipped", "error", err)
		} else {
			defer jevConn.Close()
			jevClient = jevv1.NewJevServiceClient(jevConn)
		}
	}

	var adminClient adminv1.AdminServiceClient
	if strings.TrimSpace(cfg.AdminGRPCAddress) != "" {
		adminConn, err := grpc.NewClient(
			cfg.AdminGRPCAddress,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			logger.Warn("admin gRPC client skipped", "error", err)
		} else {
			defer adminConn.Close()
			adminClient = adminv1.NewAdminServiceClient(adminConn)
		}
	}

	server := &http.Server{
		Addr: cfg.HTTPAddress,
		Handler: gateway.NewRouter(
			fundingv1.NewFundingServiceClient(fundingConn),
			accountv1.NewAccountServiceClient(accountConn),
			grpc_health_v1.NewHealthClient(fundingConn),
			gateway.Options{
				SessionCookieName:    cfg.SessionCookieName,
				SessionCookieSecure:  cfg.SessionCookieSecure,
				SessionCookieMaxAge:  cfg.SessionCookieMaxAge,
				GatewayInternalToken: cfg.GatewayInternalToken,
				CORSOrigin:           cfg.CORSOrigin,
				Polymarket:           polymarketv1.NewPolymarketServiceClient(polymarketConn),
				Report:               reportv1.NewReportServiceClient(reportConn),
				Trader:               traderv1.NewTraderServiceClient(traderConn),
				AI:                   aiv1.NewAIServiceClient(aiConn),
				Spread:               spreadv1.NewSpreadServiceClient(spreadConn),
				Jev:                  jevClient,
				Admin:                adminClient,
				BrokerJWTPublicKey:   cfg.BrokerJWTPublicKey,
				BrokerAuthCacheTTL:   cfg.BrokerAuthCacheTTL,
				AccountHealth:        grpc_health_v1.NewHealthClient(accountConn),
				PolymarketHealth:     grpc_health_v1.NewHealthClient(polymarketConn),
				ReportHealth:         grpc_health_v1.NewHealthClient(reportConn),
				TraderHealth:         grpc_health_v1.NewHealthClient(traderConn),
				AIHealth:             grpc_health_v1.NewHealthClient(aiConn),
				SpreadHealth:         grpc_health_v1.NewHealthClient(spreadConn),
			},
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Streaming responses manage their own backpressure and cancellation.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		logger.Info("API gateway listening", "address", cfg.HTTPAddress)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP graceful shutdown failed", "error", err)
	}
	logger.Info("API gateway shut down")
}
