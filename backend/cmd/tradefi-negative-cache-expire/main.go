package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"selfquant/backend/internal/database"
	"selfquant/backend/internal/funding"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tradefi-negative-cache-expire: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dryRun := false
	canonical := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--dry-run":
			dryRun = true
		case "--canonical":
			canonical = true
		default:
			return fmt.Errorf("unknown argument %s", arg)
		}
	}
	if dryRun && !canonical {
		return fmt.Errorf("--dry-run requires --canonical")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	repository := funding.NewRepository(pool)
	if canonical {
		actions, err := repository.MigrateIBCanonical(ctx, dryRun)
		if err != nil {
			return err
		}
		fmt.Print(formatIBCanonicalActions(actions, dryRun))
		return nil
	}
	expiredAt := time.Now().UTC().Add(-time.Second)
	rows, err := repository.ExpireIBNegativeResolutions(ctx, expiredAt)
	if err != nil {
		return err
	}
	fmt.Print(formatIBNegativeCacheExpiry(rows))
	return nil
}

func formatIBCanonicalActions(actions []funding.IBCanonicalAction, dryRun bool) string {
	var builder strings.Builder
	mode := "applied"
	if dryRun {
		mode = "dry-run"
	}
	fmt.Fprintf(&builder, "%s %d ib canonical actions\n", mode, len(actions))
	for _, action := range actions {
		fmt.Fprintf(&builder, "kind=%s symbol=%s target=%s conid=%d\n", action.Kind, action.Symbol, action.Target, action.ConID)
	}
	return builder.String()
}

func formatIBNegativeCacheExpiry(rows []funding.IBNegativeCacheExpiry) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "expired %d ib negative resolutions\n", len(rows))
	for _, row := range rows {
		fmt.Fprintf(
			&builder,
			"symbol=%s status=%s previous_next_verify=%s next_verify=%s\n",
			row.Symbol, row.Status,
			row.PreviousNextVerify.Format(time.RFC3339Nano),
			row.NextVerify.Format(time.RFC3339Nano),
		)
	}
	return builder.String()
}
