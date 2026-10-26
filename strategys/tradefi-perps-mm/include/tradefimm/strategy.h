#pragma once

#include <array>
#include <cstdint>
#include <string>

#include "strategyframe/strategyframe.h"
#include "tradefimm/engine.h"
#include "tradefimm/normalize.h"
#include "tradefimm/recovery.h"

namespace tradefimm {

class TradefiPerpsMm {
 public:
  explicit TradefiPerpsMm(int* exit_code = nullptr,
                          bool shadow = false) noexcept;
  void init(strategyframe::StrategyContext& context);
  void on_bbo_update(const strategyframe::BboUpdate& update);
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate& update);
  void on_oms_status(const strategyframe::OmsStatusUpdate& update);
  void on_instrument_catalog(const strategyframe::InstrumentCatalogInfo& catalog);
  void on_open_orders_snapshot(const strategyframe::OpenOrdersSnapshot& snapshot);
  void on_positions_snapshot(const strategyframe::PositionsSnapshot& snapshot);
  void on_balances_snapshot(const strategyframe::BalanceSnapshot& snapshot);
  void on_query_complete(const strategyframe::QueryComplete& complete);
  void on_timer(const strategyframe::TimerEvent&);

  [[nodiscard]] const Engine& engine() const noexcept { return engine_; }
  [[nodiscard]] const Recovery& recovery() const noexcept { return recovery_; }

 private:
  struct ConfiguredLeg {
    Venue venue{Venue::Hyperliquid};
    strategyframe::AccountId account{};
    strategyframe::ProductType product{strategyframe::ProductType::Unknown};
    std::string symbol;
    std::string security;
    bool strict{};
    bool cash{};
    strategyframe::InstrumentId instrument{};
    bool catalog_seen{};
    std::string base;
    std::string quote;
    std::uint8_t price_scale{};
    std::uint8_t quantity_scale{};
    std::int64_t multiplier{1};
    std::uint8_t multiplier_scale{};
    std::int64_t min_native{1};
    std::int64_t lot{1};
    std::int64_t tick{1};
    bool orders_done{};
    bool positions_done{};
    bool balances_done{true};
    bool balance_seen{};
    bool adapter_reconciled{};
    bool unknown_order{};
    std::uint64_t reconcile_generation{};
    std::int64_t position{};
    std::int64_t available_balance{};
    std::array<strategyframe::OrderToken, 8> cleanup_tokens{};
    std::size_t cleanup_token_count{};
    std::array<strategyframe::QueriedOrderView, 8> cleanup_orders{};
    std::size_t cleanup_order_count{};
    int cleanup_attempts{};
    std::uint64_t cleanup_requery_ns{};
    bool cleanup_needed{};
    std::array<std::uint64_t, 4> query_seen_tokens{};
    std::size_t query_seen_token_count{};
    std::uint64_t recv_ns{};
    strategyframe::FixedPoint bid{};
    strategyframe::FixedPoint ask{};
  };

  struct DecimalConfig {
    std::string basis_offset{"0.00"};
    std::string basis_buffer{"0.01"};
    std::string slippage_per_base{"0.01"};
    std::string target_profit_per_base{"0.01"};
    std::string quote_size_base{"1"};
    std::string max_maker_position{"100"};
    std::string max_hedge_position{"100"};
    std::string max_net_delta{"100"};
    std::string delta_tolerance{"0"};
    std::string max_notional_usd{"10000.00"};
    std::string max_unhedged_notional{"100.00"};
    int maker_tick_multiplier{1};
  };

  enum class LegId : std::uint8_t { Maker = 1, Hedge = 2 };

  struct PendingQuery {
    strategyframe::QueryToken token{};
    LegId leg{LegId::Maker};
    strategyframe::QueryComplete::Kind kind{
        strategyframe::QueryComplete::Kind::OpenOrders};
    std::uint64_t generation{};
    bool occupied{};
    bool active{};
  };

  [[nodiscard]] bool load(const strategyframe::StrategyParams& params);
  [[nodiscard]] bool apply_catalogs() noexcept;
  [[nodiscard]] bool price_at(const strategyframe::FixedPoint& price,
                              std::int64_t& out) const noexcept;
  [[nodiscard]] bool native_order(const ConfiguredLeg& leg, std::int64_t base_qty,
                                  std::int64_t calc_price, std::int64_t& quantity,
                                  std::int64_t& order_price) const noexcept;
  [[nodiscard]] bool submit_order_or_shadow(
      strategyframe::OrderRequest& request, SlotId slot,
      std::uint64_t generation, bool buy) noexcept;
  void cancel_order_or_shadow(const strategyframe::OrderView& order) noexcept;
  [[nodiscard]] bool cancel_venue_order_or_shadow(
      ConfiguredLeg& leg,
      const strategyframe::QueriedOrderView& order) noexcept;
  [[nodiscard]] std::string_view next_client_order_id(
      std::string_view role) noexcept;
  void fail(const char* reason) noexcept;
  void refresh_books() noexcept;
  void maybe_quote() noexcept;
  void cancel_maker_orders() noexcept;
  void drive_recovery() noexcept;
  void begin_snapshot_queries(LegId leg_id) noexcept;
  void update_reconcile_barrier() noexcept;
  void submit_deferred_cleanup(ConfiguredLeg& leg) noexcept;
  [[nodiscard]] PendingQuery* find_pending_query(
      strategyframe::QueryToken token) noexcept;
  [[nodiscard]] const PendingQuery* find_pending_query(
      strategyframe::QueryToken token) const noexcept;
  [[nodiscard]] bool remember_query(
      strategyframe::QueryToken token, LegId leg,
      strategyframe::QueryComplete::Kind kind,
      std::uint64_t generation) noexcept;
  [[nodiscard]] bool has_active_query(
      LegId leg, std::uint64_t generation) const noexcept;
  [[nodiscard]] ConfiguredLeg& leg(LegId id) noexcept;
  [[nodiscard]] const ConfiguredLeg& leg(LegId id) const noexcept;
  [[nodiscard]] static std::uint8_t adapter_kind(Venue venue) noexcept;
  [[nodiscard]] ConfiguredLeg* leg_for(strategyframe::InstrumentId id) noexcept;
  [[nodiscard]] bool slot_for_token(std::uint64_t token,
                                    SlotId& slot) const noexcept;

  int* exit_code_{};
  strategyframe::StrategyContext* context_{};
  Engine engine_{};
  Recovery recovery_{};
  ConfiguredLeg maker_{};
  ConfiguredLeg hedge_{};
  PairSpec spec_{};
  DecimalConfig decimals_{};
  std::array<PendingQuery, 32> pending_queries_{};
  std::string strategy_instance_;
  std::uint8_t price_calc_{};
  std::uint8_t quantity_calc_{};
  bool invalid_config_{};
  bool started_{};
  bool unknown_order_{};
  bool quoted_{};
  bool shadow_{};
  bool cold_start_reconciled_{};
  std::uint64_t max_bbo_age_ns_{1'000'000'000ULL};
  std::uint64_t max_hedge_latency_ns_{1'000'000'000ULL};
  std::uint64_t process_epoch_{};
  std::uint64_t cloid_sequence_{1};
  int completed_cycles_{};
  int stop_after_hedge_cycles_{};
  int max_active_maker_orders_{1};
  std::array<char, 64> cloid_{};
};

}  // namespace tradefimm
