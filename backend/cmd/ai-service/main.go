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
	aiv1 "selfquant/backend/gen/ai/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
	"selfquant/backend/internal/ai"
	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	airpc "selfquant/backend/internal/rpc"
)

func main() {
	cfg, err := config.AIFromEnv()
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
		logger.Error("database setup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	conversations := ai.NewConversationRepository(
		pool, cfg.ConversationRetention, cfg.ConversationLease,
		cfg.MaxConversationsPerUser, cfg.MaxRawMessagesPerConversation,
	)

	accountConn, err := grpc.NewClient(
		cfg.AccountGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("account gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer accountConn.Close()
	fundingConn, err := grpc.NewClient(
		cfg.FundingGRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		logger.Error("funding gRPC client failed", "error", err)
		os.Exit(1)
	}
	defer fundingConn.Close()

	openRouter := ai.NewOpenRouterProvider(
		&http.Client{Timeout: cfg.HTTPTimeout},
		cfg.OpenRouterBaseURL,
		cfg.OpenRouterModel,
		cfg.OpenRouterReferer,
		cfg.OpenRouterTitle,
		cfg.MaxOutputTokens,
	)
	service := ai.NewService(
		ai.NewAccountCredentialProvider(accountv1.NewAccountServiceClient(accountConn)),
		ai.NewProviderRegistry(openRouter),
		ai.NewFundingContextProvider(
			fundingv1.NewFundingServiceClient(fundingConn),
			cfg.ContextMaxSymbolRows,
			cfg.ContextMaxRows,
			cfg.ContextMinTurnover,
			cfg.ContextMaxChars,
		),
		conversations,
		ai.ServiceConfig{
			MaxPromptChars:         cfg.MaxInputChars,
			InputTokenBudget:       cfg.ChatInputTokenBudget,
			RecentMessagePairs:     cfg.RecentMessagePairs,
			SummaryTriggerTokens:   cfg.SummaryTriggerTokens,
			SummaryMaxChars:        cfg.SummaryMaxChars,
			SummaryMaxOutputTokens: cfg.SummaryMaxOutputTokens,
		},
	)
	cleanupWorker := ai.NewCleanupWorker(
		conversations, cfg.CleanupInterval, cfg.CleanupBatch,
		cfg.CleanupMaxBatches, logger,
	)
	go cleanupWorker.Run(ctx)

	listener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("ai gRPC listen failed", "error", err)
		os.Exit(1)
	}
	server := grpc.NewServer()
	aiv1.RegisterAIServiceServer(server, airpc.NewAIServer(service))
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if cfg.Development {
		reflection.Register(server)
	}
	go func() {
		logger.Info("ai service listening", "address", cfg.GRPCAddress)
		if serveErr := server.Serve(listener); serveErr != nil {
			logger.Error("ai gRPC server stopped", "error", serveErr)
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
	logger.Info("ai service shut down")
}
