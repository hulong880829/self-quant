#pragma once

#include <array>
#include <cstdint>
#include <memory>

#include "polymm/fairprice_client.h"
#include "polymm/halt_policy.h"
#include "polymm/order_lifecycle.h"
#include "polymm/pnl_ledger.h"
#include "polymm/rolling_pair_binding.h"
#include "polymm/signal.h"
#include "polymm/startup_reconcile.h"
#include "polymm/types.h"
#include "strategyframe/strategyframe.h"

namespace polymm {

enum class StartupFailure : std::uint8_t {
  InvalidParameters,
  InvalidInstrumentCatalog,
  BelowVenueMinimumSize,
  FairPriceClientStartFailed,
  TimerScheduleFailed,
  StartupReconcileRejected,
  OmsStatusFailed
};

enum class SettlementReason : std::uint8_t {
  FlattenExpiredNonFlat,
  CloseRetryExhausted,
  ExpiredDustPosition,
  ExpiredCloseFailed,
  ExpiredPositionUnreconciled,
  NextWindowNotReady
};

class PolyMm {
 public:
  explicit PolyMm(int* exit_code = nullptr) noexcept;
  ~PolyMm();
  PolyMm(const PolyMm&) = delete;
  PolyMm& operator=(const PolyMm&) = delete;
  PolyMm(PolyMm&&) noexcept = default;
  PolyMm& operator=(PolyMm&&) noexcept = default;

  void init(strategyframe::StrategyContext& context);
  void on_bbo_update(const strategyframe::BboUpdate& update);
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate& update);
  void on_oms_status(const strategyframe::OmsStatusUpdate& update);
  void on_instrument_catalog(
      const strategyframe::InstrumentCatalogInfo& catalog);
  void on_positions_snapshot(
      const strategyframe::PositionsSnapshot& snapshot);
  void on_query_complete(const strategyframe::QueryComplete& complete);
  void on_timer(const strategyframe::TimerEvent&);

 private:
  enum class Phase : std::uint8_t {
    WaitingReconcile,
    WaitingBinding,
    WaitingFreshMarket,
    Trading,
    StopOpening,
    CapacityBlocked,
    Halted,
  };
  struct Leg {
    Outcome outcome{Outcome::Up};
    strategyframe::InstrumentId instrument_id{};
    std::uint8_t price_scale{};
    std::uint8_t quantity_scale{};
    std::int64_t tick_units{};
    double tick_size{};
    strategyframe::FixedPoint bid{};
    strategyframe::FixedPoint ask{};
    double bid_value{};
    double ask_value{};
    std::uint32_t book_generation{};
    std::uint64_t last_bbo_ns{};
    LegOrder order{};
  };
  struct RetiredSlot {
    bool used{};
    std::array<Leg, 2> legs_{};
  };

  bool load_parameters();
  void drain_sidecar();
  void evaluate_binding();
  void drive();
  void drive_leg(Leg& leg);
  void drive_flattening();
  void drive_retired_cancels();
  void evaluate_signal();
  void submit_open(Leg& leg);
  void submit_close(Leg& leg, bool force);
  void cancel_leg(Leg& leg);
  void cancel_all();
  void begin_stop_opening() noexcept;
  void pause_opening_for_fairprice() noexcept;
  void resume_opening_after_fairprice() noexcept;
  void begin_flattening() noexcept;
  void maybe_complete_halt() noexcept;
  void fatal_startup(StartupFailure reason) noexcept;
  void enter_halted(SettlementReason reason) noexcept;
  void maybe_log_oms_status(
      const strategyframe::OmsStatusUpdate& update) noexcept;
  void finish_reconcile();
  void maybe_confirm_first_window();
  void maybe_query_retired();
  void note_query_failure();
  void recycle_retired_slots();
  [[nodiscard]] bool deactivate_current_window();
  [[nodiscard]] bool activate_window(const ActiveWindow& window);
  [[nodiscard]] bool install_leg(
      Leg& dest, Outcome outcome,
      const strategyframe::InstrumentCatalogInfo& catalog) noexcept;
  [[nodiscard]] bool meets_lot(
      const strategyframe::InstrumentCatalogInfo& catalog) const noexcept;
  [[nodiscard]] bool instrument_is_active(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] bool allows_place_open() const noexcept;
  [[nodiscard]] bool allows_place_close(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] bool risk_allows_open(const Leg& leg) const noexcept;
  [[nodiscard]] bool books_ready(std::uint64_t now_ns) const noexcept;
  [[nodiscard]] bool has_active_orders() const noexcept;
  [[nodiscard]] bool slot_has_orders(const RetiredSlot& slot) const noexcept;
  [[nodiscard]] bool position_safe() const noexcept;
  void poll_uncertain(Leg& leg, std::uint64_t now_ns);
  void apply_lifecycle(Leg& leg, const LifecycleResult& result);
  [[nodiscard]] strategyframe::OrderRequest make_order(
      const Leg& leg, strategyframe::Side side,
      strategyframe::TimeInForce tif,
      strategyframe::FixedPoint quantity,
      strategyframe::FixedPoint price, bool post_only) const;
  [[nodiscard]] strategyframe::FixedPoint quantity_fixed(
      const Leg& leg, double quantity) const noexcept;
  [[nodiscard]] strategyframe::FixedPoint price_fixed(
      const Leg& leg, double price) const noexcept;
  [[nodiscard]] Leg* leg(
      strategyframe::InstrumentId instrument_id) noexcept;
  [[nodiscard]] const Leg* leg(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] RetiredSlot* free_retired_slot() noexcept;

  strategyframe::StrategyContext* context_{};
  int* exit_code_{};
  Parameters parameters_{};
  SidecarConfig sidecar_config_{};
  std::unique_ptr<FairPriceClient> sidecar_;
  std::unique_ptr<SignalEngine> signal_;
  std::unique_ptr<PnlLedger> pnl_;
  std::unique_ptr<RollingPairBinding> binding_;
  std::array<Leg, 2> legs_{};
  std::array<RetiredSlot, kMaxRetiredWindows> retired_{};
  Phase phase_{Phase::WaitingReconcile};
  HaltPhase halt_phase_{HaltPhase::Running};
  StartupReconcile reconcile_{};
  bool fairprice_live_{};
  bool fairprice_paused_opening_{};
  bool reconcile_ready_{};
  bool first_window_inspected_{};
  bool query_inflight_{};
  strategyframe::QueryToken query_token_{};
  std::uint32_t query_backoff_index_{};
  std::uint64_t next_query_ns_{};
  std::uint32_t window_generation_{};
  std::uint64_t latest_fair_wall_ns_{};
  std::uint64_t last_position_check_ns_{};
  std::uint64_t next_error_log_ns_{};
  std::uint32_t error_suppressed_{};
  bool logged_connecting_{};
  bool logged_first_ready_{};
  bool logged_first_error_{};
  bool logged_trading_{};
};

static_assert(strategyframe::Strategy<PolyMm>);

}  // namespace polymm
