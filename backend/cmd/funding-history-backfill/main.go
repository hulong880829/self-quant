package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"selfquant/backend/internal/config"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/exchange"
	"selfquant/backend/internal/funding"
)

const (
	exitOK      = 0
	exitPartial = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	exchangesFlag := flag.String("exchanges", "", "comma-separated registered and enabled exchanges")
	phaseFlag := flag.String("phase", "recent", "recent, deep, or both")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	requested := funding.ParseExchangeList(*exchangesFlag)
	phase := strings.ToLower(strings.TrimSpace(*phaseFlag))
	if phase == "" {
		phase = "recent"
	}
	if phase != "recent" && phase != "deep" && phase != "both" {
		logger.Error("invalid backfill phase", "phase", phase)
		return exitUsage
	}

	cfg, err := config.FundingFromEnv()
	if err != nil {
		logger.Error("funding configuration failed", "error", err)
		return exitUsage
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.Level(config.LoggerLevel(cfg.LogLevel)),
	}))

	adapters, err := funding.SelectEnabledAdapters(
		funding.FundingAdapters(cfg), cfg.EnabledExchanges, requested,
	)
	if err != nil {
		logger.Error("backfill exchange selection failed", "error", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		return exitPartial
	}
	defer pool.Close()

	repository := funding.NewRepository(pool)
	names := namesOf(adapters)
	instruments, err := repository.ListActivePerpetualInstruments(ctx, names)
	if err != nil {
		logger.Error("list instruments failed", "error", err)
		return exitPartial
	}
	zeroExchanges := funding.ZeroContractExchanges(names, instruments)
	if len(instruments) == 0 {
		logger.Error("no active perpetual instruments for requested exchanges", "exchanges", names)
		return exitPartial
	}
	for _, name := range zeroExchanges {
		logger.Error("exchange has no active perpetual instruments", "exchange", name)
	}

	synchronizer := funding.NewSynchronizer(repository, adapters, nil, logger).
		WithLock(funding.NewHistoryLock(pool))

	var summaries []funding.ExchangeHistorySummary
	switch phase {
	case "recent":
		summaries, _, err = synchronizer.BackfillRecent(ctx, adapters)
	case "deep":
		summaries, _, err = synchronizer.BackfillDeep(ctx, adapters)
	case "both":
		var recentResults map[int64]funding.HistorySyncResult
		summaries, recentResults, err = synchronizer.BackfillRecent(ctx, adapters)
		if ctx.Err() == nil {
			deepSummaries, _, deepErr := synchronizer.BackfillDeepEligible(ctx, adapters, recentResults)
			summaries = append(summaries, deepSummaries...)
			err = errors.Join(err, deepErr)
		}
	}
	if ctx.Err() != nil {
		logger.Error("backfill interrupted", "error", ctx.Err())
		return exitPartial
	}
	if err != nil {
		logger.Error("backfill completed with contract errors", "error", err)
	}
	if funding.SummariesIndicateFailure(summaries, zeroExchanges) {
		return exitPartial
	}
	return exitOK
}

func namesOf(adapters []exchange.Adapter) []string {
	names := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		names = append(names, adapter.Name())
	}
	return names
}
