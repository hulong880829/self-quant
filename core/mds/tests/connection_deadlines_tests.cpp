#include "mds/service/connection_deadlines.h"
#include "mds/service/snapshot_recovery.h"

#include <array>
#include <cassert>
#include <chrono>
#include <limits>

int main() {
  using namespace std::chrono_literals;
  using Deadlines = mds::service::ConnectionDeadlines;
  using Expiration = Deadlines::Expiration;

  using mds::service::RecoveryStreamReady;
  assert(RecoveryStreamReady(true, false, false, false, true, false, false,
                             false));
  assert(!RecoveryStreamReady(true, false, true, false, true, false, false,
                              false));
  assert(!RecoveryStreamReady(true, false, false, true, true, true, true,
                              false));
  assert(!RecoveryStreamReady(true, true, false, true, true, true, true,
                              true));
  assert(!RecoveryStreamReady(true, true, false, true, true, true, false,
                              false));
  assert(RecoveryStreamReady(true, true, false, true, true, true, true,
                             false));
  assert(RecoveryStreamReady(false, false, false, false, false, false, false,
                             true));

  const auto start = Deadlines::Clock::time_point{1s};
  using mds::service::ContinuousRecoveryExpired;
  assert(!ContinuousRecoveryExpired({}, start + 1s, 100ms));
  assert(!ContinuousRecoveryExpired(start, start + 99ms, 100ms));
  assert(ContinuousRecoveryExpired(start, start + 100ms, 100ms));

  mds::service::ClientMessageBudget client_messages;
  for (std::size_t index = 0; index < 150; ++index) {
    assert(client_messages.allow(start, 150));
    client_messages.record(start);
  }
  assert(!client_messages.allow(start + 59s, 150));
  assert(client_messages.retry_at(start + 59s, 150) == start + 60s);
  assert(client_messages.allow(start + 60s, 150));

  mds::service::ConnectionAttemptBudget connection_attempts;
  for (std::size_t index = 0;
       index <
       mds::service::ConnectionAttemptBudget::kLimitPerMinute;
       ++index) {
    assert(connection_attempts.allow(start));
    connection_attempts.record(start);
  }
  assert(!connection_attempts.allow(start + 59s));
  assert(connection_attempts.retry_at(start + 59s) == start + 60s);
  assert(connection_attempts.allow(start + 60s));

  Deadlines deadlines;
  assert(!deadlines.reached_live_once());
  assert(deadlines.expiration(start, true, false) == Expiration::None);

  deadlines.subscriptions_ready(start, 100ms);
  assert(deadlines.expiration(start + 99ms, true, false) ==
         Expiration::None);
  assert(deadlines.expiration(start + 100ms, true, false) ==
         Expiration::Startup);
  assert(deadlines.expiration(start + 1s, false, false) ==
         Expiration::None);

  deadlines.mark_live();
  assert(deadlines.reached_live_once());
  assert(deadlines.expiration(start + 1s, true, true) ==
         Expiration::None);
  assert(deadlines.expiration(start + 1s, true, false) ==
         Expiration::None);

  const auto recovery_start = start + 2s;
  deadlines.begin_recovery(recovery_start, 100ms);
  deadlines.begin_recovery(recovery_start + 50ms, 100ms);
  assert(deadlines.expiration(recovery_start + 99ms, true, false) ==
         Expiration::None);
  assert(deadlines.expiration(recovery_start + 100ms, true, false) ==
         Expiration::Recovery);

  deadlines.mark_live();
  assert(deadlines.expiration(recovery_start + 1s, true, false) ==
         Expiration::None);

  deadlines.reset_connection();
  assert(deadlines.reached_live_once());
  deadlines.subscriptions_ready(recovery_start + 2s, 100ms);
  assert(deadlines.expiration(recovery_start + 2s + 99ms, true, false) ==
         Expiration::None);
  assert(deadlines.expiration(recovery_start + 2s + 100ms, true, false) ==
         Expiration::Recovery);

  deadlines.mark_live();
  deadlines.begin_recovery(recovery_start + 4s, 100ms);
  deadlines.subscriptions_ready(recovery_start + 4s + 50ms, 100ms);
  assert(deadlines.expiration(recovery_start + 4s + 100ms, true, false) ==
         Expiration::None);
  assert(deadlines.expiration(recovery_start + 4s + 150ms, true, false) ==
         Expiration::Recovery);

  using WsRecovery = mds::service::WsSnapshotRecovery;
  using DeltaDecision =
      mds::service::WsPreSnapshotDeltaDecision;
  WsRecovery ws_recovery;
  assert(ws_recovery.pre_snapshot_delta() == DeltaDecision::Fail);
  assert(mds::service::ShouldIgnorePreSnapshotDelta(
      utils::md::Venue::Bybit, ws_recovery.pre_snapshot_delta()));
  assert(!mds::service::ShouldIgnorePreSnapshotDelta(
      utils::md::Venue::Binance, ws_recovery.pre_snapshot_delta()));
  assert(!ws_recovery.waiting_snapshot());
  ws_recovery.snapshot_ready();
  assert(ws_recovery.pre_snapshot_delta() == DeltaDecision::Fail);
  ws_recovery.begin_recovery();
  assert(ws_recovery.waiting_snapshot());
  assert(ws_recovery.pre_snapshot_delta() ==
         DeltaDecision::IgnoreDuringRecovery);
  ws_recovery.snapshot_ready();
  assert(!ws_recovery.waiting_snapshot());
  assert(ws_recovery.pre_snapshot_delta() == DeltaDecision::Fail);
  ws_recovery.begin_recovery();
  ws_recovery.reset_connection();
  assert(!ws_recovery.waiting_snapshot());
  assert(ws_recovery.pre_snapshot_delta() == DeltaDecision::Fail);

  Deadlines bybit_startup;
  bybit_startup.subscriptions_ready(start, 100ms);
  assert(bybit_startup.expiration(start + 99ms, true, false) ==
         Expiration::None);
  assert(bybit_startup.expiration(start + 100ms, true, false) ==
         Expiration::Startup);
  bybit_startup.mark_live();
  assert(bybit_startup.expiration(start + 100ms, true, true) ==
         Expiration::None);

  using Bridge = mds::service::SnapshotBridgeValidator;
  using Decision = mds::service::SnapshotBridgeDecision;
  Bridge overlapping(100);
  assert(overlapping.Observe(98, 105, true) ==
         Decision::Accepted);
  assert(overlapping.Observe(106, 110, true, 105) ==
         Decision::Accepted);

  Bridge exact(100);
  assert(exact.Observe(101, 105, true) == Decision::Accepted);

  Bridge futures_previous(100);
  assert(futures_previous.Observe(110, 115, true, 100) ==
         Decision::Accepted);

  Bridge stale(100);
  assert(stale.Observe(90, 100, true) == Decision::Stale);
  assert(stale.Observe(101, 105, true) == Decision::Accepted);

  Bridge initial_gap(100);
  assert(initial_gap.Observe(102, 105, true) ==
         Decision::SnapshotTooOld);

  Bridge strict_gap(100);
  assert(strict_gap.Observe(98, 105, true) == Decision::Accepted);
  assert(strict_gap.Observe(104, 110, true, 103) ==
         Decision::Gap);

  Bridge non_strict(100);
  assert(non_strict.Observe(98, 105, false) ==
         Decision::Accepted);
  assert(non_strict.Observe(104, 110, false) ==
         Decision::Accepted);

  Bridge invalid(100);
  assert(invalid.Observe(105, 104, true) == Decision::Invalid);

  mds::service::LaggingSnapshotRetries retries;
  for (std::uint8_t attempt = 0;
       attempt < mds::service::LaggingSnapshotRetries::kMaximumAttempts;
       ++attempt) {
    assert(retries.allow_retry());
  }
  assert(!retries.allow_retry());
  retries.reset();
  assert(retries.attempts() == 0);
  assert(retries.allow_retry());

  struct DeltaRange {
    std::uint64_t first;
    std::uint64_t final;
  };
  constexpr std::array<DeltaRange, 3> production_pending{{
      {11300470672288ULL, 11300470686196ULL},
      {11300470686197ULL, 11300470700104ULL},
      {11300470700105ULL, 11300470714012ULL},
  }};
  Bridge production_stale_snapshot(11300470300002ULL);
  assert(production_stale_snapshot.Observe(
             production_pending.front().first,
             production_pending.front().final, false, 0) ==
         Decision::SnapshotTooOld);
  mds::service::LaggingSnapshotRetries production_retries;
  assert(mds::service::ShouldRetryLaggingSnapshot(
      Decision::SnapshotTooOld, true, production_retries));
  Bridge production_newer_snapshot(11300470680000ULL);
  for (const auto &delta : production_pending) {
    assert(production_newer_snapshot.Observe(
               delta.first, delta.final, false, 0) ==
           Decision::Accepted);
  }
  assert(production_newer_snapshot.sequence() ==
         production_pending.back().final);

  mds::service::LaggingSnapshotRetries disabled_venue_retries;
  assert(!mds::service::ShouldRetryLaggingSnapshot(
      Decision::SnapshotTooOld, false, disabled_venue_retries));
  assert(disabled_venue_retries.attempts() == 0);
  mds::service::LaggingSnapshotRetries non_lagging_retries;
  assert(!mds::service::ShouldRetryLaggingSnapshot(
      Decision::Gap, true, non_lagging_retries));
  assert(!mds::service::ShouldRetryLaggingSnapshot(
      Decision::Invalid, true, non_lagging_retries));
  assert(non_lagging_retries.attempts() == 0);

  Deadlines retry_deadlines;
  const auto retry_start = Deadlines::Clock::time_point{20s};
  constexpr auto retry_timeout = 10s;
  retry_deadlines.subscriptions_ready(retry_start, retry_timeout);
  retry_deadlines.mark_live();
  retry_deadlines.begin_recovery(retry_start, retry_timeout);
  mds::service::LaggingSnapshotRetries bounded_retries;
  auto retry_time = retry_start;
  for (std::uint8_t attempt = 0;
       attempt <
       mds::service::LaggingSnapshotRetries::kMaximumAttempts;
       ++attempt) {
    retry_time += 1s;
    assert(mds::service::ShouldRetryLaggingSnapshot(
        Decision::SnapshotTooOld, true, bounded_retries));
    assert(retry_deadlines.continue_recovery(retry_time, retry_timeout));
  }
  assert(retry_deadlines.expiration(
             retry_time + retry_timeout - 1ms, true, false) ==
         Expiration::None);
  assert(!mds::service::ShouldRetryLaggingSnapshot(
      Decision::SnapshotTooOld, true, bounded_retries));
  assert(retry_deadlines.expiration(
             retry_time + retry_timeout, true, false) ==
         Expiration::Recovery);

  Deadlines independent_recovery;
  const auto independent_start = Deadlines::Clock::time_point{50s};
  independent_recovery.subscriptions_ready(independent_start, 1s);
  independent_recovery.mark_live();
  independent_recovery.begin_recovery(independent_start, 30s);
  assert(independent_recovery.expiration(
             independent_start + 1s, true, false) ==
         Expiration::None);
  assert(independent_recovery.expiration(
             independent_start + 30s, true, false) ==
         Expiration::Recovery);
  assert(independent_recovery.continue_recovery(
      independent_start + 20s, 30s));
  assert(independent_recovery.expiration(
             independent_start + 49s, true, false) ==
         Expiration::None);
  assert(independent_recovery.expiration(
             independent_start + 50s, true, false) ==
         Expiration::Recovery);

  constexpr std::size_t symbol_count = 6;
  Deadlines round_robin_deadlines;
  const auto round_robin_start =
      Deadlines::Clock::time_point{100s};
  round_robin_deadlines.subscriptions_ready(
      round_robin_start, retry_timeout);
  round_robin_deadlines.mark_live();
  round_robin_deadlines.begin_recovery(
      round_robin_start, retry_timeout);
  std::array<mds::service::LaggingSnapshotRetries, symbol_count>
      symbol_retries;
  std::array<bool, symbol_count> needs_snapshot{};
  needs_snapshot.fill(true);
  std::size_t snapshot_cursor = 0;
  auto snapshot_time = round_robin_start;
  for (std::size_t request = 0;
       request < symbol_count * 2; ++request) {
    const auto selected = mds::service::NextRoundRobin(
        symbol_count, snapshot_cursor,
        [&needs_snapshot](std::size_t index) {
          return needs_snapshot[index];
        });
    assert(selected && *selected == request % symbol_count);
    snapshot_cursor = (*selected + 1) % symbol_count;
    snapshot_time += 2s;
    assert(mds::service::ShouldRetryLaggingSnapshot(
        Decision::SnapshotTooOld, true,
        symbol_retries[*selected]));
    assert(round_robin_deadlines.continue_recovery(
        snapshot_time, retry_timeout));
    assert(round_robin_deadlines.expiration(
               snapshot_time, true, false) ==
           Expiration::None);
  }

  mds::service::SubscriptionBudget budget;
  const auto budget_start =
      mds::service::SubscriptionBudget::Clock::time_point{200s};
  assert(budget.allow(budget_start, 2));
  budget.record(budget_start);
  budget.record(budget_start + 1s);
  assert(!budget.allow(budget_start + 2s, 2));
  assert(budget.retry_at(budget_start + 2s, 2) ==
         budget_start + 1h);
  assert(!budget.allow(budget_start + 1h - 1ms, 2));
  assert(budget.allow(budget_start + 1h, 2));
  assert(budget.size() == 1);

  mds::service::SubscriptionBudget hyperliquid_budget;
  for (std::size_t cycle = 0; cycle < 4; ++cycle) {
    for (std::size_t symbol = 0; symbol < 100; ++symbol) {
      assert(hyperliquid_budget.allow(budget_start, 480));
      hyperliquid_budget.record(budget_start);
    }
  }
  assert(hyperliquid_budget.size() == 400);
  for (std::size_t symbol = 0; symbol < 80; ++symbol) {
    assert(hyperliquid_budget.allow(budget_start, 480));
    hyperliquid_budget.record(budget_start);
  }
  assert(!hyperliquid_budget.allow(budget_start, 480));

  constexpr auto maximum =
      std::numeric_limits<std::uint64_t>::max();
  Bridge boundary(maximum - 1);
  assert(boundary.Observe(maximum, maximum, true) ==
         Decision::Accepted);
  assert(boundary.Observe(maximum, maximum, true) ==
         Decision::Stale);

  const std::array eligible{true, true, true};
  const auto first = mds::service::NextRoundRobin(
      eligible.size(), 0,
      [&eligible](std::size_t index) { return eligible[index]; });
  assert(first && *first == 0);
  const auto second = mds::service::NextRoundRobin(
      eligible.size(), *first + 1,
      [&eligible](std::size_t index) { return eligible[index]; });
  assert(second && *second == 1);
  const auto third = mds::service::NextRoundRobin(
      eligible.size(), *second + 1,
      [&eligible](std::size_t index) { return eligible[index]; });
  assert(third && *third == 2);

  const std::array none{false, false};
  assert(!mds::service::NextRoundRobin(
      none.size(), 0,
      [&none](std::size_t index) { return none[index]; }));
  return 0;
}
