#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <string_view>

#include "strategyframe/config.h"
#include "strategyframe/managers.h"

namespace strategyframe {

struct RuntimeMetrics {
  std::uint64_t market_updates{};
  std::uint64_t execution_updates{};
  std::uint64_t timer_events{};
  std::uint64_t duplicate_updates{};
  std::uint64_t out_of_order_updates{};
  std::uint64_t reconcile_count{};
  std::uint64_t callback_failures{};
  std::uint64_t callback_samples{};
  std::uint64_t callback_total_ns{};
  std::uint64_t callback_max_ns{};
  std::uint64_t tick_to_callback_samples{};
  std::uint64_t tick_to_callback_total_cycles{};
  std::uint64_t tick_to_callback_max_cycles{};
  std::uint64_t execution_dispatch_samples{};
  std::uint64_t execution_dispatch_total_ns{};
  std::uint64_t execution_dispatch_max_ns{};
  std::uint64_t order_call_samples{};
  std::uint64_t order_call_total_ns{};
  std::uint64_t order_call_max_ns{};
  std::uint64_t command_queue_high_water{};
  std::uint64_t update_queue_high_water{};
  std::uint64_t unmatched_venue_events{};
  std::uint64_t unmatched_fills{};
  std::uint64_t catalog_token_parse_failures{};
  std::uint64_t catalog_field_too_long{};
  std::uint64_t catalog_capacity_failures{};
  std::uint64_t bbo_replay_dropped{};
  std::uint64_t bbo_cross_source_dropped{};
  std::uint64_t bbo_stale_time_dropped{};
  std::uint64_t bbo_stale_sequence_dropped{};
  std::uint64_t bbo_generation_dropped{};
  std::uint64_t bbo_policy_excluded{};
  std::uint64_t bbo_policy_starved{};
  std::uint64_t book_top_mismatch{};
  std::uint64_t bbo_callbacks_emitted{};
  std::uint64_t bbo_callbacks_suppressed{};
  std::uint64_t bbo_origin_unknown{};
  std::uint64_t query_unmapped_open_orders{};
  std::uint64_t query_unmapped_positions{};
  std::uint64_t query_quarantine_dropped{};
};

class StrategyContext {
 public:
  [[nodiscard]] Result<OrderToken> place_order(
      const OrderRequest& request) noexcept;
  [[nodiscard]] Result<OrderToken> cancel(OrderToken token) noexcept;
  [[nodiscard]] Result<QueryToken> query_open_orders(
      AccountId account_id) noexcept;
  [[nodiscard]] Result<QueryToken> query_open_orders(
      AccountId account_id, InstrumentId instrument_id) noexcept;
  [[nodiscard]] Result<QueryToken> query_positions(
      AccountId account_id) noexcept;
  [[nodiscard]] Result<QueryToken> query_positions(
      AccountId account_id, InstrumentId instrument_id) noexcept;

  [[nodiscard]] Result<TimerHandle> schedule_timer(
      std::uint64_t first_deadline_ns,
      std::uint64_t interval_ns = 0) noexcept;
  [[nodiscard]] Error cancel_timer(TimerHandle handle) noexcept;

  [[nodiscard]] std::span<const OrderView> open_orders() const noexcept;
  [[nodiscard]] std::span<const PositionView> positions() const noexcept;
  [[nodiscard]] Result<OrderView> find_order(OrderToken token) const noexcept;
  [[nodiscard]] Result<PositionView> find_position(
      AccountId account_id, InstrumentId instrument_id,
      PositionSide side = PositionSide::Net) const noexcept;
  [[nodiscard]] Result<InstrumentInfo> find_instrument(
      InstrumentId instrument_id) const noexcept;
  [[nodiscard]] Result<InstrumentCatalogInfo> find_instrument(
      const InstrumentSelector& selector) const noexcept;
  [[nodiscard]] Result<InstrumentCatalogInfo> find_instrument(
      Venue venue, ProductType product,
      std::string_view canonical_symbol) const noexcept;
  [[nodiscard]] Result<InstrumentCatalogInfo> find_instrument_catalog(
      InstrumentId instrument_id) const noexcept;
  [[nodiscard]] bool execution_ready(
      InstrumentId instrument_id) const noexcept;
  [[nodiscard]] Result<OmsStatusUpdate> oms_status(
      std::uint8_t adapter_kind) const noexcept;

  [[nodiscard]] const StrategyParams& params() const noexcept;
  [[nodiscard]] RuntimeMetrics metrics() const noexcept;
  [[nodiscard]] std::uint64_t now_ns() const noexcept;
  void request_stop() noexcept;

 private:
  struct Ops {
    Result<OrderToken> (*place)(void*, const OrderRequest&) noexcept{};
    Result<OrderToken> (*cancel)(void*, OrderToken) noexcept{};
    Result<QueryToken> (*query_open_orders)(void*, AccountId) noexcept{};
    Result<QueryToken> (*query_open_orders_for)(
        void*, AccountId, InstrumentId) noexcept{};
    Result<QueryToken> (*query_positions)(void*, AccountId) noexcept{};
    Result<QueryToken> (*query_positions_for)(
        void*, AccountId, InstrumentId) noexcept{};
    Result<TimerHandle> (*schedule_timer)(void*, std::uint64_t,
                                         std::uint64_t) noexcept{};
    Error (*cancel_timer)(void*, TimerHandle) noexcept{};
    std::span<const OrderView> (*open_orders)(const void*) noexcept{};
    std::span<const PositionView> (*positions)(const void*) noexcept{};
    Result<OrderView> (*find_order)(const void*, OrderToken) noexcept{};
    Result<PositionView> (*find_position)(const void*, AccountId, InstrumentId,
                                         PositionSide) noexcept{};
    Result<InstrumentInfo> (*find_instrument)(const void*,
                                              InstrumentId) noexcept{};
    Result<InstrumentCatalogInfo> (*find_instrument_selector)(
        const void*, const InstrumentSelector&) noexcept{};
    Result<InstrumentCatalogInfo> (*find_instrument_catalog)(
        const void*, InstrumentId) noexcept{};
    bool (*execution_ready)(const void*, InstrumentId) noexcept{};
    Result<OmsStatusUpdate> (*oms_status)(const void*, std::uint8_t) noexcept{};
    const StrategyParams& (*params)(const void*) noexcept{};
    RuntimeMetrics (*metrics)(const void*) noexcept{};
    std::uint64_t (*now_ns)(const void*) noexcept{};
    void (*request_stop)(void*) noexcept{};
  };

  StrategyContext(void* state, const Ops* ops) noexcept
      : state_(state), ops_(ops) {}
  void* state_{};
  const Ops* ops_{};
  friend class RuntimeCore;
};

}  // namespace strategyframe
