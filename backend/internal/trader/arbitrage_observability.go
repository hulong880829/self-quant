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
	sql       interface{ SQLStats() RepositorySQLStats }
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

func (r *ArbitrageMetricsReporter) ConfigureRepositorySQLStats(
	stats interface{ SQLStats() RepositorySQLStats },
) {
	r.sql = stats
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
			var sql RepositorySQLStats
			if r.sql != nil {
				sql = r.sql.SQLStats()
			}
			r.logger.Info("arbitrage metrics",
				"active_combinations", scheduler.ActiveCombinations,
				"active_accounts", scheduler.ActiveAccounts,
				"active_venues", scheduler.ActiveVenues,
				"triggers_total", scheduler.Triggers,
				"claims_total", scheduler.Claims,
				"stale_bbo_reads_total", scheduler.StaleBBOReads,
				"signal_stale_rejected_total", scheduler.SignalStaleRejected,
				"capacity_rejected_total", scheduler.CapacityRejected,
				"bbo_wakeups_total", scheduler.BBOWakeups,
				"bbo_evaluations_total", scheduler.BBOEvaluations,
				"scheduler_evaluations_total", scheduler.Evaluations,
				"control_wakeups_total", scheduler.ControlWakeups,
				"stale_wakeups_total", scheduler.StaleWakeups,
				"retry_wakeups_total", scheduler.RetryWakeups,
				"control_probes_total", scheduler.ControlProbes,
				"control_full_reloads_total", scheduler.FullReloads,
				"market_snapshot_writes_total", scheduler.SnapshotWrites,
				"execution_slots_in_use", scheduler.ExecutionSlotsInUse,
				"work_permits_in_use", scheduler.WorkPermitsInUse,
				"wss_active_streams", market.ActiveStreams,
				"wss_references", market.References,
				"wss_connects_total", market.Connects,
				"wss_reconnects_total", market.Reconnects,
				"wss_disconnects_total", market.Disconnects,
				"wss_read_timeouts_total", market.ReadTimeouts,
				"wss_subscription_rejections_total", market.SubscriptionRejections,
				"wss_subscription_ack_timeouts_total", market.SubscriptionAckTimeouts,
				"bbo_updates_total", market.Updates,
				"market_stale_reads_total", market.StaleReads,
				"market_parser_errors_total", market.ParserErrors,
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
				"order_stream_heartbeat_write_failed_total", orders.HeartbeatWriteFailed,
				"order_stream_events_applied_total", executor.StreamEvents,
				"order_stream_rest_fallbacks_total", executor.RESTFallbacks,
				"order_stream_last_lag_ms", executor.LastStreamLagMS,
				"order_stream_last_hedge_latency_ms", executor.LastHedgeLatencyMS,
				"funding_sql_calls_total", sql.FundingSyncCalls,
				"funding_sql_candidates_total", sql.FundingCandidates,
				"funding_sql_writes_total", sql.FundingWrites,
				"funding_sql_noops_total", sql.FundingNoops,
				"funding_sql_errors_total", sql.FundingErrors,
				"funding_sql_duration_ms_total", sql.FundingDurationMS,
				"lease_sql_calls_total", sql.LeaseCalls,
				"lease_sql_returned_total", sql.LeaseReturned,
				"lease_sql_writes_total", sql.LeaseWrites,
				"lease_sql_errors_total", sql.LeaseErrors,
				"lease_sql_duration_ms_total", sql.LeaseDurationMS,
				"arbitrage_replay_loads_total", sql.ReplayLoads,
				"arbitrage_replay_order_rows_total", sql.ReplayOrderRows,
				"arbitrage_replay_fill_rows_total", sql.ReplayFillRows,
				"arbitrage_replay_duration_ms_total", sql.ReplayDurationMS,
			)
		}
	}
}
