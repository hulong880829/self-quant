#pragma once

#include <cstdint>

namespace tradefimm {

enum class Venue : std::uint8_t { Hyperliquid = 1, Ibkr = 2 };

enum class IbkrShortPolicy : std::uint8_t {
  InventoryOnly = 1,
  Allow = 2,
};

enum class Phase : std::uint8_t {
  WaitingCatalog = 1,
  WaitingOms,
  Reconciling,
  WaitingFreshBbo,
  Quoting,
  HedgePending,
  EmergencyUnwind,
  Suspended,
  Reconnecting,
  Halted,
};

enum class HedgeAction : std::uint8_t {
  None = 0,
  Accept,
  Hedge,
  Unwind,
  Halt,
};

struct LegSpec {
  Venue venue{Venue::Hyperliquid};
  std::int64_t multiplier{1};
  std::uint8_t multiplier_scale{};
  std::int64_t min_native{1};
  std::int64_t lot{1};
  std::int64_t max_position{};
};

struct FeeSpec {
  std::int64_t hl_maker_bps{};
  std::int64_t hl_taker_bps{};
  std::int64_t ib_buy_bps{};
  std::int64_t ib_sell_bps{};
  std::int64_t ib_passive_bps{};
  std::int64_t ib_taker_bps{};
};

struct PairSpec {
  LegSpec maker{};
  LegSpec hedge{};
  bool same_base{true};
  bool same_quote{true};
  IbkrShortPolicy ibkr_short_policy{IbkrShortPolicy::InventoryOnly};
  std::int64_t delta_tolerance{};
  std::int64_t quote_size_base{1};
  std::int64_t tick{1};
  std::int64_t slippage_per_base{};
  std::int64_t basis_offset{};
  std::int64_t basis_buffer{};
  std::int64_t target_profit_per_base{};
  std::int64_t max_net_delta{};
  std::int64_t max_notional{};
  std::int64_t max_unhedged_notional{};
  int max_hedge_slippage_ticks{};
  int max_hedge_retries{1};
  FeeSpec fees{};
};

struct Books {
  std::int64_t maker_bid{};
  std::int64_t maker_ask{};
  std::int64_t hedge_bid{};
  std::int64_t hedge_ask{};
  std::uint64_t maker_recv_ns{};
  std::uint64_t hedge_recv_ns{};
  bool maker_mds{true};
  bool hedge_mds{true};
  bool maker_execution_ready{true};
  bool hedge_execution_ready{true};
};

struct Quote {
  bool bid{};
  bool ask{};
  std::int64_t bid_price{};
  std::int64_t ask_price{};
  std::int64_t cost_per_base{};
};

struct HedgePlan {
  HedgeAction action{HedgeAction::None};
  std::int64_t native_quantity{};
  std::int64_t residual{};
};

class Engine {
 public:
  [[nodiscard]] bool start(const PairSpec& spec) noexcept;
  void note_catalog() noexcept;
  void note_oms(bool orders_mapped, bool positions_mapped) noexcept;
  void note_reconciled(bool mapped) noexcept;
  void note_baseline() noexcept;
  void note_disconnected() noexcept;
  void note_reconnecting() noexcept;
  void note_suspended() noexcept;
  void note_halted() noexcept;
  void set_positions(std::int64_t maker_native, std::int64_t hedge_native) noexcept;
  // Inventory and reservations are expressed in normalized base quantity.
  // For STK/CMDTY inventory comes from the IB position; for CASH it comes
  // from the available base-currency balance.
  void set_ib_inventory(std::int64_t available_base,
                        std::int64_t reserved_sell_base = 0) noexcept;
  void set_ib_sell_reservation(std::int64_t reserved_sell_base) noexcept;
  void set_books(const Books& books, std::uint64_t now_ns,
                 std::uint64_t max_age_ns) noexcept;
  void on_maker_fill(std::int64_t signed_native, std::int64_t price) noexcept;
  void on_hedge_fill(std::int64_t signed_native, std::int64_t price) noexcept;
  void note_hedge_terminal() noexcept;
  void note_unwind_terminal(bool filled) noexcept;

  [[nodiscard]] Phase phase() const noexcept { return phase_; }
  [[nodiscard]] bool baseline_ready() const noexcept { return baseline_; }
  [[nodiscard]] bool fresh() const noexcept { return fresh_; }
  [[nodiscard]] bool makers_must_cancel() const noexcept;
  [[nodiscard]] Quote quote() const noexcept;
  [[nodiscard]] HedgePlan plan() const noexcept;
  [[nodiscard]] std::int64_t net_delta() const noexcept { return net_delta_; }
  [[nodiscard]] std::int64_t maker_delta() const noexcept { return maker_delta_; }
  [[nodiscard]] std::int64_t hedge_delta() const noexcept { return hedge_delta_; }
  [[nodiscard]] std::int64_t session_pnl() const noexcept;
  [[nodiscard]] std::int64_t min_tradable_delta() const noexcept;
  [[nodiscard]] static std::int64_t hedge_native_for(
      std::int64_t net_delta, std::int64_t multiplier, std::int64_t lot,
      std::int64_t min_native, std::uint8_t multiplier_scale = 0) noexcept;

 private:
  [[nodiscard]] bool books_fresh(std::uint64_t now_ns,
                                 std::uint64_t max_age_ns) const noexcept;
  void recompute() noexcept;
  [[nodiscard]] std::int64_t fee_for(bool maker_leg, bool buy,
                                     std::int64_t notional) const noexcept;
  [[nodiscard]] bool ib_sell_allowed(
      std::int64_t additional_sell_base) const noexcept;

  PairSpec spec_{};
  Phase phase_{Phase::WaitingCatalog};
  bool started_{};
  bool fresh_{};
  bool baseline_{};
  std::int64_t maker_native_{};
  std::int64_t hedge_native_{};
  std::int64_t maker_delta_{};
  std::int64_t hedge_delta_{};
  std::int64_t net_delta_{};
  std::int64_t initial_maker_delta_{};
  std::int64_t initial_hedge_delta_{};
  std::int64_t initial_maker_mid_{};
  std::int64_t initial_hedge_mid_{};
  std::int64_t cash_{};
  std::int64_t fees_{};
  std::int64_t ib_available_base_{};
  std::int64_t ib_reserved_sell_base_{};
  Books books_{};
  std::uint64_t now_ns_{};
  std::uint64_t max_age_ns_{1};
  int hedge_failures_{};
};

}  // namespace tradefimm
