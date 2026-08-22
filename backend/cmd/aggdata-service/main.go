package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"selfquant/backend/internal/aggdata"
	"selfquant/backend/internal/config"
)

func main() {
	cfg, err := config.AggDataFromEnv()
	if err != nil {
		slog.Error("invalid aggdata configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(
		os.Stdout,
		&slog.HandlerOptions{Level: slog.Level(config.LoggerLevel(cfg.LogLevel))},
	))
	catalog, err := aggdata.NewCatalog(cfg.RecordingDir)
	if err != nil {
		logger.Error("catalog startup failed", "error", err)
		os.Exit(1)
	}
	if _, err := catalog.Refresh(); err != nil && !errors.Is(err, aggdata.ErrManifestMissing) {
		logger.Error("manifest startup failed", "error", err)
		os.Exit(1)
	}
	store, err := aggdata.NewStoreWithFairPrice(aggdata.FairPriceConfig{
		Enabled:             cfg.FairPriceEnabled,
		DepthK:              cfg.FairPriceDepth,
		LambdaPerBP:         cfg.FairPriceLambdaPerBP,
		ImbalanceAlpha:      cfg.FairPriceImbalanceAlpha,
		EWMATau:             cfg.FairPriceEWMATau,
		ImpactNotional:      cfg.FairPriceImpactNotional,
		VenueDominanceRatio: cfg.FairPriceVenueDominance,
	})
	if err != nil {
		logger.Error("fair price startup failed", "error", err)
		os.Exit(1)
	}
	if current := catalog.Snapshot(); current != nil {
		store.Reconcile(current)
	}
	history := aggdata.NewHistory(catalog.Root(), cfg.HistoryWorkers)
	handler := aggdata.NewServer(
		catalog, store, history, logger, cfg.BrowserToken, cfg.AllowedOrigins,
		cfg.MaxClients, cfg.MaxSubscriptions, cfg.HistoryResolution, cfg.StreamInterval,
		cfg.FairPriceStreamInterval, cfg.FairPriceHeartbeat,
	)
	server := &http.Server{
		Addr:              cfg.HTTPAddress,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go handler.LogFairPriceStats(ctx)
	if cfg.FairPriceEnabled && cfg.FairPricePersistEnabled {
		recorder, recorderErr := aggdata.NewFairPriceRecorder(
			store,
			logger,
			aggdata.FairPriceRecorderConfig{
				DatabaseURL:      cfg.DatabaseURL,
				SampleInterval:   cfg.FairPriceSampleInterval,
				Retention:        cfg.FairPriceRetention,
				CleanupInterval:  cfg.FairPriceCleanupInterval,
				DeleteBatch:      cfg.FairPriceDeleteBatch,
				DeleteMaxBatches: cfg.FairPriceDeleteMaxBatches,
				OperationTimeout: cfg.FairPriceDBTimeout,
			},
		)
		if recorderErr != nil {
			logger.Error("invalid fair price recorder configuration", "error", recorderErr)
			os.Exit(1)
		}
		handler.SetFairPriceHistory(recorder)
		go recorder.Run(ctx)
	}
	manifestChanged := make(chan struct{}, 1)
	go catalog.Watch(ctx, cfg.ManifestRefresh, manifestChanged)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-manifestChanged:
				if current := catalog.Snapshot(); current != nil {
					store.Reconcile(current)
					logger.Info("recording manifest updated", "markets", len(store.Markets()))
				}
			}
		}
	}()
	gateway := aggdata.NewGatewayClient(
		cfg.MDSGatewayURL, cfg.MDSGatewayToken, store, logger,
	)
	go gateway.Run(ctx)
	go func() {
		logger.Info("aggdata service listening", "address", cfg.HTTPAddress)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("aggdata HTTP server failed", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("aggdata HTTP shutdown failed", "error", err)
	}
	logger.Info("aggdata service shut down")
}
