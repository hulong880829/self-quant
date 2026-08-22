package trader

import (
	"context"
	"log/slog"
	"time"

	"selfquant/backend/internal/trader/marketdata"
	"selfquant/backend/internal/trader/orderstream"
)

type ArbitrageMetricsReporter struct {
	scheduler *ArbitrageScheduler
	market    *marketdata.Manager
	orders    *orderstream.Manager
	executor  *ArbitrageExecutor
	interval  time.Duration
	logger    *slog.Logger
}

func NewArbitrageMetricsReporter(
	scheduler *ArbitrageScheduler,
	market *marketdata.Manager,
	orders *orderstream.Manager,
	executor *ArbitrageExecutor,
	interval time.Duration,
	logger *slog.Logger,
) *ArbitrageMetricsReporter {
	if interval <= 0 {
		interval = time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitrageMetricsReporter{
		scheduler: scheduler, market: market, orders: orders, executor: executor,
		interval: interval, logger: logger,
	}
}

func (r *ArbitrageMetricsReporter) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scheduler := r.scheduler.Stats()
			market := r.market.Stats()
			var orders orderstream.Stats
			if r.orders != nil {
				orders = r.orders.Stats()
			}
			var executor ArbitrageExecutorStats
			if r.executor != nil {
				executor = r.executor.Stats()
			}
			r.logger.Info("arbitrage metrics",
				"active_combinations", scheduler.ActiveCombinations,
				"active_accounts", scheduler.ActiveAccounts,
				"active_venues", scheduler.ActiveVenues,
				"triggers_total", scheduler.Triggers,
				"claims_total", scheduler.Claims,
				"stale_bbo_reads_total", scheduler.StaleBBOReads,
				"capacity_rejected_total", scheduler.CapacityRejected,
				"wss_active_streams", market.ActiveStreams,
				"wss_references", market.References,
				"wss_connects_total", market.Connects,
				"wss_reconnects_total", market.Reconnects,
				"bbo_updates_total", market.Updates,
				"market_stale_reads_total", market.StaleReads,
				"order_stream_active_sessions", orders.ActiveSessions,
				"order_stream_healthy_sessions", orders.HealthySessions,
				"order_stream_references", orders.References,
				"order_stream_watchers", orders.Watchers,
				"order_stream_connects_total", orders.Connects,
				"order_stream_reconnects_total", orders.Reconnects,
				"order_stream_updates_total", orders.Updates,
				"order_stream_disconnects_total", orders.Disconnects,
				"order_stream_dropped_total", orders.Dropped,
				"order_stream_auth_failures_total", orders.AuthFailures,
				"order_stream_events_applied_total", executor.StreamEvents,
				"order_stream_rest_fallbacks_total", executor.RESTFallbacks,
				"order_stream_last_lag_ms", executor.LastStreamLagMS,
				"order_stream_last_hedge_latency_ms", executor.LastHedgeLatencyMS,
			)
		}
	}
}
