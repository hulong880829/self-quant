package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/report"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "report-period-repair: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
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
	repository := report.NewRepository(pool)
	location := report.PeriodLocation()
	productName := "funding-arb"

	fmt.Println("== funding-arb cash flows around 2026-08-29 18:17 ==")
	rows, err := pool.Query(ctx, `
		SELECT f.id, f.flow_date::text, f.occurred_at, f.amount_usd::text, f.flow_type
		FROM product_cash_flows f
		JOIN products p ON p.id=f.product_id
		WHERE p.name=$1
		  AND f.occurred_at >= TIMESTAMPTZ '2026-08-29 00:00:00+08'
		  AND f.occurred_at < TIMESTAMPTZ '2026-08-31 00:00:00+08'
		ORDER BY f.occurred_at, f.id`, productName)
	if err != nil {
		return err
	}
	defer rows.Close()
	foundRedemption := false
	for rows.Next() {
		var id int64
		var flowDate, amount, flowType string
		var occurredAt time.Time
		if err := rows.Scan(&id, &flowDate, &occurredAt, &amount, &flowType); err != nil {
			return err
		}
		local := occurredAt.In(location)
		fmt.Printf("id=%d type=%s amount=%s occurred_at=%s flow_date=%s go_report_date=%s\n",
			id, flowType, amount, local.Format(time.RFC3339), flowDate, report.ReportDate(occurredAt))
		if flowType == "redemption" && decimal.RequireFromString(amount).Equal(decimal.RequireFromString("-1500")) {
			foundRedemption = true
			if flowDate != "2026-08-30" {
				return fmt.Errorf("expected -1500 redemption to belong to 2026-08-30, got %s", flowDate)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !foundRedemption {
		fmt.Println("WARNING: no confirmed -1500 redemption found; not inventing cash flows")
	}

	printSnapshot := func(label, date string) {
		item, loadErr := repository.LoadSnapshot(ctx, productName, date)
		if loadErr != nil {
			fmt.Printf("%s %s: %v\n", label, date, loadErr)
			return
		}
		fmt.Printf("%s %s opening=%s closing=%s net_cf=%s pnl=%s return=%s status=%s flows=%d\n",
			label, date, item.OpeningEquityUSD, item.ClosingEquityUSD, item.NetCashFlowUSD,
			item.PnLUSD, item.ReturnRate, item.Status, item.CashFlowCount)
	}

	fmt.Println("== snapshots before recompute ==")
	printSnapshot("before", "2026-08-29")
	printSnapshot("before", "2026-08-30")
	before29, _ := repository.LoadSnapshot(ctx, productName, "2026-08-29")
	before30, err := repository.LoadSnapshot(ctx, productName, "2026-08-30")
	if err != nil {
		return fmt.Errorf("load 2026-08-30 snapshot: %w", err)
	}

	day29 := time.Date(2026, 8, 29, 9, 1, 0, 0, location)
	day30 := time.Date(2026, 8, 30, 9, 1, 0, 0, location)
	now := time.Now().UTC()
	if _, err := repository.RecomputeDate(ctx, before30.ProductID, day29, location, now); err != nil {
		fmt.Printf("recompute 08-29: %v\n", err)
	}
	if _, err := repository.RecomputeDate(ctx, before30.ProductID, day30, location, now); err != nil {
		return fmt.Errorf("recompute 08-30: %w", err)
	}

	fmt.Println("== snapshots after recompute ==")
	printSnapshot("after", "2026-08-29")
	printSnapshot("after", "2026-08-30")
	after29, _ := repository.LoadSnapshot(ctx, productName, "2026-08-29")
	after30, err := repository.LoadSnapshot(ctx, productName, "2026-08-30")
	if err != nil {
		return err
	}
	fmt.Println("== 08-29 / 08-30 diff ==")
	printDiff("08-29", before29, after29)
	printDiff("08-30", before30, after30)
	fmt.Printf("expected 08-30 opening=7174.1791 closing=5525.3778 net_cf=-1500 pnl=-148.8013 dietz~-2.38%%\n")
	fmt.Printf("actual   08-30 opening=%s closing=%s net_cf=%s pnl=%s return=%s\n",
		after30.OpeningEquityUSD, after30.ClosingEquityUSD, after30.NetCashFlowUSD,
		after30.PnLUSD, after30.ReturnRate)

	fmt.Println("== equity anomalies (no auto-created subscriptions) ==")
	anomalies, err := repository.ListEquityAnomalies(ctx, productName, "100")
	if err != nil {
		return err
	}
	if len(anomalies) == 0 {
		fmt.Println("none")
	}
	for _, item := range anomalies {
		note := ""
		if item.ReportDate == "2026-08-20" {
			note = " [checklist only: do not invent a subscription]"
		}
		fmt.Printf("%s opening=%s closing=%s net_cf=%s pnl=%s%s\n",
			item.ReportDate, item.OpeningEquityUSD, item.ClosingEquityUSD,
			item.NetCashFlowUSD, item.PnLUSD, note)
	}
	return nil
}

func printDiff(date string, before, after report.DailySnapshot) {
	fmt.Printf("%s net_cf %s -> %s | pnl %s -> %s | return %s -> %s | status %s -> %s\n",
		date, before.NetCashFlowUSD, after.NetCashFlowUSD,
		before.PnLUSD, after.PnLUSD, before.ReturnRate, after.ReturnRate,
		before.Status, after.Status)
}
