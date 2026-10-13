package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"selfquant/backend/internal/database"
	"selfquant/backend/internal/trader"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "arbitrage-final-metrics-repair: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 || strings.TrimSpace(os.Args[1]) == "" {
		return fmt.Errorf("usage: arbitrage-final-metrics-repair <combination-id>")
	}
	combinationID := strings.TrimSpace(os.Args[1])
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		databaseURL = "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	repository := trader.NewRepository(pool)
	updated, err := repository.RepairFinalArbitragePositionMetrics(ctx, combinationID)
	if err != nil {
		return err
	}
	fmt.Printf(
		"repaired combination=%s realized=%s annualized=%s position_notional=%s quality_ready=true\n",
		updated.ID,
		updated.RealizedSpreadPnl,
		updated.CombinedPositionAnnualized,
		updated.PositionNotional,
	)
	return nil
}
