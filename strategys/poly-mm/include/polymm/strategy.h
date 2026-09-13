#pragma once

#include <array>
#include <cstdint>
#include <memory>

#include "polymm/fairprice_client.h"
#include "polymm/halt_policy.h"
#include "polymm/order_lifecycle.h"
#include "polymm/pnl_ledger.h"
#include "polymm/signal.h"
#include "polymm/startup_reconcile.h"
#include "polymm/types.h"
#include "strategyframe/strategyframe.h"

namespace polymm {

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
  void on_timer(const strategyframe::TimerEvent&);

 private:
  enum class Phase : std::uint8_t {
    WaitingCatalog,
    WaitingReconcile,
    Trading,
    RolloverDraining,
    WaitingMarket,
    SettlementManual,
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

  bool load_parameters();
  void drain_sidecar();
  void handle_catalog(const strategyframe::InstrumentCatalogInfo& catalog);
  bool activate_next_window();
  void drive();
  void drive_rollover();
  void drive_leg(Leg& leg);
  void drive_flattening();
  void evaluate_signal();
  void submit_open(Leg& leg);
  void submit_close(Leg& leg, bool force);
  void cancel_leg(Leg& leg);
  void cancel_all();
  void begin_rollover();
  void begin_stop_opening() noexcept;
  void begin_flattening() noexcept;
  void maybe_complete_halt() noexcept;
  void fatal_startup() noexcept;
  void enter_settlement_manual() noexcept;
  void inspect_startup_account();
  void finish_reconcile();
  bool position_reconciled() const;
  bool risk_allows_open(const Leg& leg) const noexcept;
  bool books_ready(std::uint64_t now_ns) const noexcept;
  bool pending_window_ready(std::uint64_t now_ns) const noexcept;
  bool has_active_orders() const noexcept;
  bool position_safe() const noexcept;
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

  strategyframe::StrategyContext* context_{};
  int* exit_code_{};
  Parameters parameters_{};
  SidecarConfig sidecar_config_{};
  std::unique_ptr<FairPriceClient> sidecar_;
  std::unique_ptr<SignalEngine> signal_;
  std::unique_ptr<PnlLedger> pnl_;
  std::array<Leg, 2> legs_{};
  std::array<Leg, 2> pending_legs_{};
  std::array<strategyframe::InstrumentCatalogInfo, 2> catalogs_{};
  std::array<strategyframe::InstrumentCatalogInfo, 2> pending_catalogs_{};
  Phase phase_{Phase::WaitingCatalog};
  HaltPhase halt_phase_{HaltPhase::Running};
  StartupReconcile reconcile_{};
  bool fairprice_live_{};
  std::uint32_t window_generation_{};
  std::uint64_t latest_fair_wall_ns_{};
  std::uint64_t last_position_check_ns_{};
};

static_assert(strategyframe::Strategy<PolyMm>);

}  // namespace polymm
