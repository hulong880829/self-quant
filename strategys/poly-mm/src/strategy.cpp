#include "polymm/strategy.h"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <cstdlib>
#include <limits>
#include <string>
#include <string_view>

#include "polymm/market_window.h"
#include "polymm/risk.h"
#include "utils/md/market_identity.h"

namespace polymm {
namespace {

constexpr std::uint8_t kAdapterConnecting = 1;
constexpr std::uint8_t kAdapterReady = 3;
constexpr std::uint64_t kOmsErrorLogIntervalNs = 30'000'000'000ULL;
constexpr std::uint64_t kQueryBackoffNs[] = {
    1'000'000'000ULL, 2'000'000'000ULL, 5'000'000'000ULL,
    10'000'000'000ULL, 30'000'000'000ULL};

std::uint64_t WallNowNs() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

double Fixed(strategyframe::FixedPoint value) noexcept {
  double divisor = 1.0;
  for (std::uint8_t index = 0; index < value.scale; ++index) divisor *= 10.0;
  return static_cast<double>(value.value) / divisor;
}

bool CopySymbol(std::string_view text,
                strategyframe::InstrumentSymbol& output) noexcept {
  if (text.empty() || text.size() > sizeof(output.value)) return false;
  std::memset(output.value, 0, sizeof(output.value));
  std::memcpy(output.value, text.data(), text.size());
  output.length = static_cast<std::uint16_t>(text.size());
  return true;
}

const char* StartupFailureName(StartupFailure reason) noexcept {
  switch (reason) {
    case StartupFailure::InvalidParameters:
      return "invalid_parameters";
    case StartupFailure::InvalidInstrumentCatalog:
      return "invalid_instrument_catalog";
    case StartupFailure::BelowVenueMinimumSize:
      return "below_venue_minimum_size";
    case StartupFailure::FairPriceClientStartFailed:
      return "fair_price_client_start_failed";
    case StartupFailure::TimerScheduleFailed:
      return "timer_schedule_failed";
    case StartupFailure::StartupReconcileRejected:
      return "startup_reconcile_rejected";
    case StartupFailure::OmsStatusFailed:
      return "oms_status_failed";
  }
  return "unknown";
}

const char* SettlementReasonName(SettlementReason reason) noexcept {
  switch (reason) {
    case SettlementReason::FlattenExpiredNonFlat:
      return "flatten_expired_non_flat";
    case SettlementReason::CloseRetryExhausted:
      return "close_retry_exhausted";
    case SettlementReason::ExpiredDustPosition:
      return "expired_dust_position";
    case SettlementReason::ExpiredCloseFailed:
      return "expired_close_failed";
    case SettlementReason::ExpiredPositionUnreconciled:
      return "expired_position_unreconciled";
    case SettlementReason::NextWindowNotReady:
      return "next_window_not_ready";
  }
  return "unknown";
}

}  // namespace

PolyMm::PolyMm(int* exit_code) noexcept : exit_code_(exit_code) {}

PolyMm::~PolyMm() {
  log_signal_summary();
  if (sidecar_) sidecar_->stop();
}

bool PolyMm::load_parameters() {
  const auto& params = context_->params();
  const auto account = params.require_int("account_id");
  if (!account || account.value <= 0) return false;
  parameters_.account_id = static_cast<std::uint32_t>(account.value);

  const auto venue_text = params.require_string("market.venue");
  const auto product_text = params.require_string("market.product");
  const auto up_text = params.require_string("market.up_symbol");
  const auto down_text = params.require_string("market.down_symbol");
  const auto window_ms = params.require_int("market.window_ms");
  if (!venue_text || !product_text || !up_text || !down_text || !window_ms ||
      window_ms.value <= 0) {
    return false;
  }
  const auto venue = utils::md::parse_canonical_venue(venue_text.value);
  const auto product = utils::md::parse_canonical_product(product_text.value);
  if (!venue || !product) return false;
  parameters_.venue = *venue;
  parameters_.product = *product;
  if (!CopySymbol(up_text.value, parameters_.up_symbol) ||
      !CopySymbol(down_text.value, parameters_.down_symbol)) {
    return false;
  }
  parameters_.window_ns =
      static_cast<std::uint64_t>(window_ms.value) * 1'000'000ULL;

  parameters_.vol_lookback_ms = static_cast<std::uint32_t>(
      params.optional_int("vol_lookback_ms", 30'000));
  parameters_.signal_horizon_ms = static_cast<std::uint32_t>(
      params.optional_int("signal_horizon_ms", 250));
  parameters_.vol_threshold =
      params.optional_double("vol_threshold", 0.20);
  parameters_.min_market_price =
      params.optional_double("min_market_price", 0.15);
  parameters_.max_market_price =
      params.optional_double("max_market_price", 0.85);
  parameters_.min_edge_ticks =
      params.optional_double("min_edge_ticks", 1.0);
  parameters_.order_shares =
      params.optional_double("order_shares", 1.0);
  parameters_.max_single_side_shares =
      params.optional_double("max_single_side_shares", 10.0);
  parameters_.imbalance_tolerance_shares =
      params.optional_double("imbalance_tolerance_shares", 0.1);
  parameters_.profit_spread_ticks =
      params.optional_int("profit_spread_ticks", 1);
  parameters_.close_improve_ticks =
      params.optional_int("close_improve_ticks", 0);
  parameters_.max_order_resting_ms = static_cast<std::uint32_t>(
      params.optional_int("max_order_resting_ms", 1'000));
  parameters_.requote_deviation_ticks =
      params.optional_int("requote_deviation_ticks", 1);
  parameters_.no_trade_before_expiry_ms = static_cast<std::uint32_t>(
      params.optional_int("no_trade_before_expiry_ms", 30'000));
  parameters_.expiry_order_grace_ms = static_cast<std::uint32_t>(
      params.optional_int("expiry_order_grace_ms", 3'000));
  parameters_.stop_loss_ticks =
      params.optional_double("stop_loss_ticks", 5.0);
  parameters_.fairprice_timer_us = static_cast<std::uint32_t>(
      params.optional_int("fairprice_timer_us", 200));
  parameters_.minimum_order_size =
      params.optional_double("minimum_order_size", 5.0);
  parameters_.startup_reconcile_timeout_ms = static_cast<std::uint32_t>(
      params.optional_int("startup_reconcile_timeout_ms", 10'000));
  parameters_.max_bbo_age_ms = static_cast<std::uint32_t>(
      params.optional_int("max_bbo_age_ms", 2'000));
  parameters_.max_bbo_skew_ms = static_cast<std::uint32_t>(
      params.optional_int("max_bbo_skew_ms", 500));
  if (params.contains("taker_fee_bps")) {
    std::fprintf(stderr,
                 "poly-mm config taker_fee_bps is removed; use "
                 "polymarket_taker_fee_rate\n");
    return false;
  }
  parameters_.polymarket_taker_fee_rate =
      params.optional_double("polymarket_taker_fee_rate", 0.07);
  parameters_.slippage_ticks =
      params.optional_double("slippage_ticks", 0.0);
  sidecar_config_.fairprice_host =
      params.optional_string("fairprice.host", "127.0.0.1");
  if (!params.contains("fairprice.service")) return false;
  if (const auto port = params.require_int("fairprice.service")) {
    sidecar_config_.fairprice_service = std::to_string(port.value);
  } else if (const auto text = params.require_string("fairprice.service")) {
    sidecar_config_.fairprice_service = std::string(text.value);
  } else {
    return false;
  }
  sidecar_config_.connect_timeout_ms = static_cast<std::uint32_t>(
      params.optional_int("fairprice.connect_timeout_ms", 1'000));
  sidecar_config_.fairprice_target =
      params.optional_string("fairprice.target", "/v1/stream");
  sidecar_config_.fairprice_origin =
      params.optional_string("fairprice.origin", "");
  sidecar_config_.fairprice_secure =
      params.optional_bool("fairprice.secure", true);
  sidecar_config_.fairprice_profile =
      params.optional_string("fairprice.profile", "");
  sidecar_config_.fairprice_symbol =
      params.optional_string("fairprice.symbol", "BTCUSDT");
  if (const char* token = std::getenv("FAIRPRICE_BROWSER_TOKEN"))
    sidecar_config_.fairprice_token = token;
  else if (const char* token = std::getenv("AGGDATA_BROWSER_TOKEN"))
    sidecar_config_.fairprice_token = token;

  const bool valid =
      parameters_.vol_lookback_ms > parameters_.signal_horizon_ms &&
         parameters_.signal_horizon_ms > 0 &&
         parameters_.vol_threshold >= 0.0 &&
         parameters_.min_market_price > 0.0 &&
         parameters_.max_market_price < 1.0 &&
         parameters_.min_market_price < parameters_.max_market_price &&
         parameters_.order_shares > 0.0 &&
         parameters_.max_single_side_shares > 0.0 &&
         parameters_.fairprice_timer_us > 0 &&
         parameters_.minimum_order_size > 0.0 &&
         parameters_.order_shares + 1e-9 >= parameters_.minimum_order_size &&
         parameters_.startup_reconcile_timeout_ms > 0 &&
         parameters_.expiry_order_grace_ms > 0 &&
         parameters_.max_bbo_age_ms > 0 &&
         parameters_.max_bbo_skew_ms > 0 &&
         parameters_.polymarket_taker_fee_rate >= 0.0 &&
         parameters_.polymarket_taker_fee_rate <= 1.0 &&
         parameters_.slippage_ticks >= 0.0 &&
         parameters_.window_ns > 0 &&
         !sidecar_config_.fairprice_service.empty();
  if (valid) {
    std::fprintf(stderr,
                 "poly-mm fee_model=polymarket_crypto fee_rate=%.4f "
                 "maker_fee=0\n",
                 parameters_.polymarket_taker_fee_rate);
  }
  return valid;
}

void PolyMm::fatal_startup(StartupFailure reason) noexcept {
  const bool entered =
      halt_phase_ != HaltPhase::Halted || phase_ != Phase::Halted;
  halt_phase_ = HaltPhase::Halted;
  phase_ = Phase::Halted;
  fairprice_live_ = false;
  if (exit_code_ != nullptr) *exit_code_ = 1;
  if (entered) {
    std::fprintf(stderr, "poly-mm fatal startup reason=%s\n",
                 StartupFailureName(reason));
  }
  if (context_) {
    cancel_all();
    context_->request_stop();
  }
}

void PolyMm::begin_stop_opening() noexcept {
  fairprice_paused_opening_ = false;
  halt_phase_ = polymm::begin_stop_opening(halt_phase_);
}

void PolyMm::pause_opening_for_fairprice() noexcept {
  if (halt_phase_ != HaltPhase::Running) return;
  fairprice_paused_opening_ = true;
  halt_phase_ = polymm::begin_stop_opening(halt_phase_);
}

void PolyMm::resume_opening_after_fairprice() noexcept {
  const HaltPhase resumed = polymm::resume_after_fairprice(
      halt_phase_, fairprice_paused_opening_);
  if (resumed == HaltPhase::Running) fairprice_paused_opening_ = false;
  halt_phase_ = resumed;
}

void PolyMm::refresh_fairprice_live() noexcept {
  const bool live = fairprice_gates_fresh(
      WallNowNs(), latest_fair_received_ns_, latest_accepted_wall_ns_);
  fairprice_live_ = live;
  if (live) resume_opening_after_fairprice();
  else pause_opening_for_fairprice();
}

void PolyMm::begin_flattening() noexcept {
  fairprice_paused_opening_ = false;
  halt_phase_ = polymm::begin_flattening(halt_phase_);
  cancel_all();
}

void PolyMm::enter_halted(SettlementReason reason) noexcept {
  const bool entered = phase_ != Phase::Halted;
  halt_phase_ = HaltPhase::Halted;
  phase_ = Phase::Halted;
  fairprice_live_ = false;
  if (exit_code_ != nullptr) *exit_code_ = 1;
  if (entered) {
    const std::uint64_t expiry =
        binding_ && binding_->active().valid ? binding_->active().expiry_ns
                                             : 0;
    std::fprintf(stderr,
                 "poly-mm halted reason=%s expiry_ns=%llu now_ns=%llu\n",
                 SettlementReasonName(reason),
                 static_cast<unsigned long long>(expiry),
                 static_cast<unsigned long long>(
                     context_ != nullptr ? context_->now_ns() : 0));
  }
  if (context_) {
    cancel_all();
    context_->request_stop();
  }
}

bool PolyMm::has_active_orders() const noexcept {
  for (const Leg& value : legs_)
    if (is_active_order(value.order.state)) return true;
  for (const RetiredSlot& slot : retired_) {
    if (!slot.used) continue;
    if (slot_has_orders(slot)) return true;
  }
  if (context_ && !context_->open_orders().empty()) return true;
  return false;
}

bool PolyMm::slot_has_orders(const RetiredSlot& slot) const noexcept {
  for (const Leg& value : slot.legs_)
    if (is_active_order(value.order.state)) return true;
  return false;
}

bool PolyMm::position_safe() const noexcept {
  if (pnl_ == nullptr) return true;
  for (const Leg& value : legs_) {
    const double quantity = pnl_->quantity(value.instrument_id);
    if (quantity > 0.0 &&
        !below_minimum(quantity, parameters_.minimum_order_size) &&
        value.order.state != OrderState::Dust) {
      return false;
    }
  }
  return !has_active_orders();
}

void PolyMm::maybe_complete_halt() noexcept {
  if (confirm_halt(halt_phase_, has_active_orders(), position_safe()) !=
      HaltPhase::Halted) {
    return;
  }
  halt_phase_ = HaltPhase::Halted;
  phase_ = Phase::Halted;
  if (exit_code_ != nullptr && pnl_ && !pnl_->flat()) *exit_code_ = 1;
  if (context_) context_->request_stop();
}

void PolyMm::init(strategyframe::StrategyContext& context) {
  context_ = &context;
  if (!load_parameters()) {
    fatal_startup(StartupFailure::InvalidParameters);
    return;
  }
  binding_ = std::make_unique<RollingPairBinding>(MarketBindingConfig{
      parameters_.venue, parameters_.product, parameters_.up_symbol,
      parameters_.down_symbol, parameters_.window_ns});
  signal_ = std::make_unique<SignalEngine>(
      parameters_, signal_sample_capacity(parameters_));
  pnl_ = std::make_unique<PnlLedger>(parameters_.account_id);
  pnl_->set_fee_rate(parameters_.polymarket_taker_fee_rate);
  sidecar_ = std::make_unique<FairPriceClient>(sidecar_config_);
  if (!sidecar_->start()) {
    fatal_startup(StartupFailure::FairPriceClientStartFailed);
    return;
  }
  const std::uint64_t interval =
      static_cast<std::uint64_t>(parameters_.fairprice_timer_us) * 1'000ULL;
  if (!context.schedule_timer(context.now_ns() + interval, interval)) {
    fatal_startup(StartupFailure::TimerScheduleFailed);
    return;
  }
  reconcile_ = {};
  reconcile_.started_ns = context.now_ns();
  reconcile_.timeout_ns =
      static_cast<std::uint64_t>(parameters_.startup_reconcile_timeout_ms) *
      1'000'000ULL;
  phase_ = Phase::WaitingReconcile;
  std::fprintf(stderr,
               "poly-mm startup stage=initialized account=%u window_ns=%llu\n",
               static_cast<unsigned>(parameters_.account_id),
               static_cast<unsigned long long>(parameters_.window_ns));
}

void PolyMm::finish_reconcile() {
  const auto verdict = reconcile_verdict(reconcile_, context_->now_ns());
  if (verdict == ReconcileVerdict::Pending) return;
  if (verdict == ReconcileVerdict::Reject) {
    fatal_startup(reconcile_.oms_reconcile_failed
                      ? StartupFailure::OmsStatusFailed
                      : StartupFailure::StartupReconcileRejected);
    return;
  }
  reconcile_ready_ = true;
  std::fprintf(stderr,
               "poly-mm startup stage=reconcile_ready source=%s\n",
               reconcile_.oms_reconcile_seen ? "oms_complete" : "timeout");
  if (binding_ && binding_->active().valid) {
    maybe_confirm_first_window();
  } else {
    phase_ = Phase::WaitingBinding;
  }
}

void PolyMm::maybe_confirm_first_window() {
  if (!reconcile_ready_ || !binding_ || !binding_->active().valid) return;
  if (!first_window_inspected_) {
    inspect_window_inventory(reconcile_, context_->open_orders(),
                             context_->positions(), parameters_.account_id,
                             parameters_.up_instrument_id,
                             parameters_.down_instrument_id);
    first_window_inspected_ = true;
    if (reconcile_.saw_open_order || reconcile_.saw_position) {
      halt_phase_ = HaltPhase::Halted;
      phase_ = Phase::Halted;
      if (exit_code_ != nullptr) *exit_code_ = 1;
      std::fprintf(stderr,
                   "poly-mm halted reason=current_window_inventory "
                   "up_id=%llu down_id=%llu\n",
                   static_cast<unsigned long long>(
                       parameters_.up_instrument_id),
                   static_cast<unsigned long long>(
                       parameters_.down_instrument_id));
      if (context_) context_->request_stop();
      return;
    }
  }
  if (phase_ == Phase::WaitingReconcile || phase_ == Phase::WaitingBinding ||
      phase_ == Phase::CapacityBlocked) {
    phase_ = Phase::WaitingFreshMarket;
  }
}

PolyMm::Leg* PolyMm::leg(
    strategyframe::InstrumentId instrument_id) noexcept {
  for (Leg& value : legs_)
    if (value.instrument_id == instrument_id) return &value;
  for (RetiredSlot& slot : retired_) {
    if (!slot.used) continue;
    for (Leg& value : slot.legs_)
      if (value.instrument_id == instrument_id) return &value;
  }
  return nullptr;
}

const PolyMm::Leg* PolyMm::leg(
    strategyframe::InstrumentId instrument_id) const noexcept {
  for (const Leg& value : legs_)
    if (value.instrument_id == instrument_id) return &value;
  for (const RetiredSlot& slot : retired_) {
    if (!slot.used) continue;
    for (const Leg& value : slot.legs_)
      if (value.instrument_id == instrument_id) return &value;
  }
  return nullptr;
}

PolyMm::RetiredSlot* PolyMm::free_retired_slot() noexcept {
  for (RetiredSlot& slot : retired_)
    if (!slot.used) return &slot;
  return nullptr;
}

bool PolyMm::instrument_is_active(
    strategyframe::InstrumentId instrument_id) const noexcept {
  return instrument_id != 0 &&
         (instrument_id == legs_[0].instrument_id ||
          instrument_id == legs_[1].instrument_id);
}

bool PolyMm::meets_lot(
    const strategyframe::InstrumentCatalogInfo& catalog) const noexcept {
  double factor = 1.0;
  for (std::uint8_t index = 0; index < catalog.quantity_scale; ++index)
    factor *= 10.0;
  return parameters_.order_shares * factor + 1e-9 >=
         static_cast<double>(catalog.lot_size);
}

bool PolyMm::install_leg(
    Leg& dest, Outcome outcome,
    const strategyframe::InstrumentCatalogInfo& catalog) noexcept {
  dest = {};
  dest.outcome = outcome;
  dest.instrument_id = catalog.instrument_id;
  dest.price_scale = catalog.price_scale;
  dest.quantity_scale = catalog.quantity_scale;
  dest.tick_units = catalog.tick_size;
  dest.tick_size = Fixed({catalog.tick_size, catalog.price_scale, {}});
  dest.order.state = OrderState::Idle;
  return dest.instrument_id != 0 && dest.tick_size > 0.0;
}

void PolyMm::park_retired_slot(RetiredSlot& slot) noexcept {
  const auto legs = legs_;
  const auto expiry =
      binding_ && binding_->active().valid ? binding_->active().expiry_ns : 0;
  const auto condition =
      binding_ && binding_->active().valid ? binding_->active().up.condition_id
                                          : strategyframe::ConditionId{};
  slot = {};
  slot.used = true;
  slot.expiry_ns = expiry;
  slot.condition_id = condition;
  slot.legs_ = legs;
}

bool PolyMm::deactivate_current_window() {
  if (!binding_ || !binding_->active().valid) return true;
  cancel_all();
  const bool had_window =
      legs_[0].instrument_id != 0 || legs_[1].instrument_id != 0;
  RetiredSlot* slot = nullptr;
  if (had_window) {
    slot = free_retired_slot();
    if (slot == nullptr) return false;
  }
  if (pnl_ != nullptr && !pnl_->can_archive_active()) return false;
  if (pnl_ != nullptr && !pnl_->archive_active()) return false;
  if (slot != nullptr) park_retired_slot(*slot);
  binding_->commit_deactivation();
  horizon_.discard_generation(window_generation_);
  log_signal_summary();
  legs_ = {};
  parameters_.up_instrument_id = 0;
  parameters_.down_instrument_id = 0;
  return true;
}

bool PolyMm::activate_window(const ActiveWindow& window) {
  if (!same_market_condition(window.up.condition_id, window.down.condition_id)) {
    std::fprintf(stderr,
                 "poly-mm activation rejected condition mismatch up_id=%llu "
                 "down_id=%llu\n",
                 static_cast<unsigned long long>(window.up.instrument_id),
                 static_cast<unsigned long long>(window.down.instrument_id));
    return false;
  }
  if (!meets_lot(window.up) || !meets_lot(window.down)) {
    fatal_startup(StartupFailure::BelowVenueMinimumSize);
    return false;
  }
  const bool had_window =
      legs_[0].instrument_id != 0 || legs_[1].instrument_id != 0;
  const bool needs_slot =
      had_window &&
      (is_active_order(legs_[0].order.state) ||
       is_active_order(legs_[1].order.state) ||
       (pnl_ != nullptr && !pnl_->active_flat()));
  RetiredSlot* slot = nullptr;
  if (needs_slot) {
    slot = free_retired_slot();
    if (slot == nullptr) return false;
    if (pnl_ != nullptr && !pnl_->can_archive_active()) return false;
  }
  if (pnl_ == nullptr ||
      !pnl_->commit_active_window(window.up.instrument_id,
                                  window.down.instrument_id)) {
    return false;
  }
  if (slot != nullptr) park_retired_slot(*slot);
  binding_->commit_activation(window);
  if (!install_leg(legs_[0], Outcome::Up, window.up) ||
      !install_leg(legs_[1], Outcome::Down, window.down)) {
    return false;
  }
  parameters_.up_instrument_id = window.up.instrument_id;
  parameters_.down_instrument_id = window.down.instrument_id;
  window_generation_ =
      static_cast<std::uint32_t>(binding_->active().generation);
  if (signal_) signal_->reset();
  fairprice_live_ = false;
  first_window_inspected_ = false;
  std::fprintf(stderr,
               "poly-mm binding activated generation=%u up_id=%llu "
               "down_id=%llu expiry_ns=%llu\n",
               static_cast<unsigned>(window_generation_),
               static_cast<unsigned long long>(parameters_.up_instrument_id),
               static_cast<unsigned long long>(
                   parameters_.down_instrument_id),
               static_cast<unsigned long long>(window.expiry_ns));
  maybe_confirm_first_window();
  return true;
}

void PolyMm::evaluate_binding() {
  if (!context_ || !binding_ || halt_phase_ == HaltPhase::Halted ||
      phase_ == Phase::Halted) {
    return;
  }
  const std::uint64_t now_wall = WallNowNs();
  if (binding_->propose_expiry(now_wall)) {
    if (!deactivate_current_window()) {
      if (phase_ != Phase::CapacityBlocked) {
        phase_ = Phase::CapacityBlocked;
        std::fputs("poly-mm capacity blocked on expiry archive\n", stderr);
      }
      return;
    }
    if (phase_ == Phase::Trading || phase_ == Phase::StopOpening ||
        phase_ == Phase::WaitingFreshMarket ||
        phase_ == Phase::CapacityBlocked) {
      phase_ = Phase::WaitingBinding;
    }
  }
  if (binding_->active().valid) return;
  if (!binding_->dirty()) return;
  const auto proposed = binding_->propose_live_window(
      now_wall, [this](strategyframe::InstrumentId instrument_id) {
        return context_->execution_ready(instrument_id);
      });
  if (!proposed.valid) return;
  if (!same_market_condition(proposed.up.condition_id,
                             proposed.down.condition_id)) {
    if (rejected_condition_expiry_ns_ != proposed.expiry_ns) {
      rejected_condition_expiry_ns_ = proposed.expiry_ns;
      std::fprintf(stderr,
                   "poly-mm activation rejected condition mismatch "
                   "up_id=%llu down_id=%llu\n",
                   static_cast<unsigned long long>(proposed.up.instrument_id),
                   static_cast<unsigned long long>(
                       proposed.down.instrument_id));
    }
    return;
  }
  if (!activate_window(proposed)) {
    if (phase_ != Phase::Halted) {
      phase_ = Phase::CapacityBlocked;
      std::fputs("poly-mm capacity blocked on activation archive\n", stderr);
    }
  }
}

void PolyMm::on_bbo_update(const strategyframe::BboUpdate& update) {
  if (halt_phase_ == HaltPhase::Halted && phase_ == Phase::Halted) return;
  drain_sidecar();
  Leg* value = nullptr;
  if (instrument_is_active(update.header.instrument_id))
    value = leg(update.header.instrument_id);
  if (value == nullptr) {
    if (pnl_) pnl_->update_bbo(update);
    return;
  }
  value->bid = update.bid.price;
  value->ask = update.ask.price;
  value->bid_value = Fixed(update.bid.price);
  value->ask_value = Fixed(update.ask.price);
  value->book_generation = update.header.book_generation;
  value->last_bbo_ns = context_->now_ns();
  pnl_->update_bbo(update);
  drive();
}

void PolyMm::drain_sidecar() {
  if (!sidecar_) return;
  SidecarEvent event;
  std::size_t budget = 128;
  while (budget-- != 0 && sidecar_->try_pop(event)) {
    switch (event.kind) {
      case SidecarEvent::Kind::FairPrice: {
        const FairUpdate update = signal_->update(event.fair_price);
        if (update == FairUpdate::Invalid) {
          signal_->reset();
          fairprice_live_ = false;
          latest_fair_received_ns_ = 0;
          latest_accepted_wall_ns_ = 0;
          pause_opening_for_fairprice();
          break;
        }
        if (update == FairUpdate::OutOfOrder) break;
        latest_fair_received_ns_ = event.fair_price.received_ns;
        if (update == FairUpdate::Accepted)
          latest_accepted_wall_ns_ = event.fair_price.wall_ns;
        refresh_fairprice_live();
        break;
      }
      case SidecarEvent::Kind::FairPriceDisconnected:
      case SidecarEvent::Kind::FairPriceUnavailable:
        fairprice_live_ = false;
        latest_fair_received_ns_ = 0;
        latest_accepted_wall_ns_ = 0;
        if (signal_) signal_->reset();
        pause_opening_for_fairprice();
        break;
    }
  }
}

void PolyMm::on_instrument_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog) {
  if (!binding_ || phase_ == Phase::Halted) return;
  const CatalogAdmit admit = binding_->on_catalog(catalog, WallNowNs());
  if (admit == CatalogAdmit::Rejected) {
    std::fprintf(stderr,
                 "poly-mm catalog rejected instrument_id=%llu\n",
                 static_cast<unsigned long long>(catalog.instrument_id));
  }
}

bool PolyMm::books_ready(std::uint64_t now_ns) const noexcept {
  return book_tradable(legs_[0].bid_value, legs_[0].ask_value) &&
         book_tradable(legs_[1].bid_value, legs_[1].ask_value) &&
         bbo_fresh(now_ns, legs_[0].last_bbo_ns, parameters_.max_bbo_age_ms) &&
         bbo_fresh(now_ns, legs_[1].last_bbo_ns, parameters_.max_bbo_age_ms) &&
         bbo_skew_ok(legs_[0].last_bbo_ns, legs_[1].last_bbo_ns,
                     parameters_.max_bbo_skew_ms);
}

bool PolyMm::allows_place_open() const noexcept {
  if (phase_ != Phase::Trading || halt_phase_ != HaltPhase::Running)
    return false;
  if (!binding_ || !binding_->active().valid) return false;
  const auto left =
      binding_->active().expiry_ns > WallNowNs()
          ? binding_->active().expiry_ns - WallNowNs()
          : 0;
  return !near_expiry(left, parameters_.no_trade_before_expiry_ms);
}

bool PolyMm::allows_place_close(
    strategyframe::InstrumentId instrument_id) const noexcept {
  if (!instrument_is_active(instrument_id)) return false;
  if (halt_phase_ == HaltPhase::Halted || phase_ == Phase::Halted)
    return false;
  if (halt_phase_ == HaltPhase::Flattening) return true;
  return phase_ == Phase::Trading || phase_ == Phase::StopOpening;
}

void PolyMm::drive() {
  if (!context_ || halt_phase_ == HaltPhase::Halted) return;
  if (phase_ == Phase::WaitingReconcile) {
    finish_reconcile();
    return;
  }
  evaluate_binding();
  drive_retired_cancels();
  drive_retired_release();
  if (phase_ == Phase::CapacityBlocked) return;
  refresh_fairprice_live();
  if (halt_phase_ == HaltPhase::Flattening) {
    drive_flattening();
    maybe_complete_halt();
    return;
  }
  if (phase_ == Phase::WaitingFreshMarket) {
    if (legs_[0].book_generation != 0 &&
        legs_[1].book_generation != 0 &&
        context_->execution_ready(legs_[0].instrument_id) &&
        context_->execution_ready(legs_[1].instrument_id) &&
        books_ready(context_->now_ns())) {
      phase_ = Phase::Trading;
      if (!logged_trading_) {
        logged_trading_ = true;
        std::fprintf(stderr, "poly-mm startup stage=trading\n");
      }
    } else {
      return;
    }
  }
  if (phase_ == Phase::Trading && binding_ && binding_->active().valid) {
    const auto left =
        binding_->active().expiry_ns > WallNowNs()
            ? binding_->active().expiry_ns - WallNowNs()
            : 0;
    if (near_expiry(left, parameters_.no_trade_before_expiry_ms)) {
      phase_ = Phase::StopOpening;
      cancel_open_orders();
    }
  }
  if (phase_ != Phase::Trading && phase_ != Phase::StopOpening) return;
  for (Leg& value : legs_) drive_leg(value);
  if (phase_ == Phase::Trading && halt_phase_ == HaltPhase::Running)
    evaluate_signal();
}

void PolyMm::drive_retired_cancels() {
  if (context_ == nullptr) return;
  const std::uint64_t now = context_->now_ns();
  const std::uint64_t timeout =
      static_cast<std::uint64_t>(parameters_.startup_reconcile_timeout_ms) *
      1'000'000ULL;
  for (RetiredSlot& slot : retired_) {
    if (!slot.used) continue;
    for (Leg& value : slot.legs_) {
      if (value.order.state == OrderState::PendingOpen ||
          value.order.state == OrderState::PendingClose ||
          value.order.state == OrderState::PendingForce) {
        cancel_leg(value);
      } else if (value.order.state == OrderState::PendingCancel &&
                 now > value.order.started_ns + timeout) {
        value.order.state = OrderState::NeedsReconcile;
        value.order.uncertain_since_ns = now;
      }
      if (value.order.state == OrderState::NeedsReconcile)
        poll_uncertain(value, now, false);
    }
  }
}

void PolyMm::drive_flattening() {
  bool active = false;
  for (Leg& value : legs_) {
    if (is_active_order(value.order.state)) {
      active = true;
      cancel_leg(value);
      continue;
    }
    const double quantity = pnl_->quantity(value.instrument_id);
    if (below_minimum(quantity, parameters_.minimum_order_size)) {
      value.order.state = OrderState::Dust;
      continue;
    }
    if (quantity > 0.0 && allows_close_submit(value.order.state) &&
        allows_place_close(value.instrument_id)) {
      submit_close(value, true);
    }
    if (is_active_order(value.order.state)) active = true;
  }
  if (!active && binding_ && binding_->active().valid &&
      WallNowNs() >= binding_->active().expiry_ns && pnl_ &&
      !pnl_->active_flat()) {
    enter_halted(SettlementReason::FlattenExpiredNonFlat);
  }
}

void PolyMm::drive_leg(Leg& value) {
  if (value.order.state == OrderState::NeedsReconcile) {
    poll_uncertain(value, context_->now_ns());
    return;
  }
  if (!allows_close(halt_phase_) ||
      !allows_close_submit(value.order.state) ||
      !allows_place_close(value.instrument_id)) {
    return;
  }
  const PnlView position = pnl_->view(value.instrument_id);
  if (below_minimum(position.quantity, parameters_.minimum_order_size)) {
    value.order.state = OrderState::Dust;
    return;
  }
  if (position.quantity <= 0.0) return;
  const bool stop = stop_loss_triggered(
      position, value.bid_value, value.tick_size, parameters_.stop_loss_ticks,
      parameters_.polymarket_taker_fee_rate, parameters_.slippage_ticks);
  submit_close(value, stop || halt_phase_ != HaltPhase::Running);
}

void PolyMm::evaluate_signal() {
  if (!allows_place_open() || !fairprice_live_ || !signal_->ready() ||
      !books_ready(context_->now_ns())) {
    return;
  }
  for (const Leg& value : legs_)
    if (is_active_order(value.order.state)) return;
  const auto left =
      binding_->active().expiry_ns > WallNowNs()
          ? binding_->active().expiry_ns - WallNowNs()
          : 0;
  const auto evaluation = signal_->evaluate_executable(
      legs_[0].bid_value, legs_[0].ask_value, legs_[1].bid_value,
      legs_[1].ask_value, legs_[0].tick_size, left);
  Leg* selected = nullptr;
  if (evaluation.direction == SignalEvaluation::Direction::Up)
    selected = &legs_[0];
  else if (evaluation.direction == SignalEvaluation::Direction::Down)
    selected = &legs_[1];
  if (selected != nullptr && allows_new_open(selected->order.state) &&
      risk_allows_open(*selected)) {
    submit_open(*selected, evaluation);
  }
}

bool PolyMm::risk_allows_open(const Leg& value) const noexcept {
  if (value.order.state == OrderState::Dust) return false;
  const double up = pnl_->quantity(parameters_.up_instrument_id);
  const double down = pnl_->quantity(parameters_.down_instrument_id);
  const double current =
      value.outcome == Outcome::Up ? up : down;
  const double other =
      value.outcome == Outcome::Up ? down : up;
  if (below_minimum(current, parameters_.minimum_order_size)) return false;
  return allows_open(
      current, other, parameters_.order_shares,
      parameters_.max_single_side_shares,
      parameters_.imbalance_tolerance_shares);
}

strategyframe::FixedPoint PolyMm::quantity_fixed(
    const Leg& value, double quantity) const noexcept {
  double factor = 1.0;
  for (std::uint8_t index = 0; index < value.quantity_scale; ++index)
    factor *= 10.0;
  return {static_cast<std::int64_t>(std::llround(quantity * factor)),
          value.quantity_scale, {}};
}

strategyframe::FixedPoint PolyMm::price_fixed(
    const Leg& value, double price) const noexcept {
  double factor = 1.0;
  for (std::uint8_t index = 0; index < value.price_scale; ++index)
    factor *= 10.0;
  std::int64_t units =
      static_cast<std::int64_t>(std::llround(price * factor));
  if (value.tick_units > 0)
    units = units / value.tick_units * value.tick_units;
  return {units, value.price_scale, {}};
}

strategyframe::OrderRequest PolyMm::make_order(
    const Leg& value, strategyframe::Side side,
    strategyframe::TimeInForce tif,
    strategyframe::FixedPoint quantity,
    strategyframe::FixedPoint price, bool post_only) const {
  strategyframe::OrderRequest request;
  request.account_id = parameters_.account_id;
  request.instrument_id = value.instrument_id;
  request.side = side;
  request.type = strategyframe::OrderType::Limit;
  request.time_in_force = tif;
  request.flags =
      post_only ? strategyframe::OrderFlag::PostOnly : 0;
  request.quantity = quantity;
  request.price = price;
  return request;
}

void PolyMm::submit_open(Leg& value, const SignalEvaluation& evaluation) {
  if (!allows_place_open() || !allows_open(halt_phase_) ||
      !allows_new_open(value.order.state)) {
    return;
  }
  if (!order_terms_ok(value.ask_value, parameters_.order_shares,
                      value.tick_size,
                      parameters_.polymarket_taker_fee_rate)) {
    begin_stop_opening();
    return;
  }
  std::fprintf(stderr,
               "poly-mm signal direction=%s predicted_edge_ticks=%.4f "
               "required_ticks=%.4f spread_ticks=%.4f entry_fee_ticks=%.4f "
               "slippage_ticks=%.4f entry_ask=%.4f\n",
               evaluation.direction == SignalEvaluation::Direction::Up
                   ? "up"
                   : "down",
               evaluation.edge_ticks, evaluation.required_ticks,
               evaluation.spread_ticks, evaluation.entry_fee_ticks,
               evaluation.slippage_ticks, value.ask_value);
  (void)horizon_.record(
      context_->now_ns(), window_generation_, value.instrument_id,
      evaluation.direction,
      (value.bid_value + value.ask_value) * 0.5, value.bid_value,
      value.ask_value, evaluation.edge_ticks, evaluation.required_ticks,
      evaluation.entry_fee_ticks * value.tick_size, value.tick_size,
      parameters_.polymarket_taker_fee_rate, parameters_.order_shares);
  auto request = make_order(
      value, strategyframe::Side::Buy, strategyframe::TimeInForce::FOK,
      quantity_fixed(value, parameters_.order_shares), value.ask, false);
  const auto submitted = context_->place_order(request);
  if (!submitted) {
    value.order.last_error = static_cast<std::int32_t>(submitted.error);
    begin_stop_opening();
    return;
  }
  value.order.state = OrderState::PendingOpen;
  value.order.purpose = OrderPurpose::Open;
  value.order.token = submitted.value;
  value.order.price = request.price;
  value.order.started_ns = context_->now_ns();
  value.order.saw_fill = false;
  if (!pnl_->remember_order(
          submitted.value, value.instrument_id, request.side,
          OrderPurpose::Open, window_generation_, true)) {
    (void)context_->cancel(submitted.value);
    begin_flattening();
  }
}

void PolyMm::submit_close(Leg& value, bool force) {
  if (!allows_close(halt_phase_) ||
      !allows_place_close(value.instrument_id) ||
      value.order.state == OrderState::NeedsReconcile) {
    return;
  }
  const PnlView position = pnl_->view(value.instrument_id);
  if (below_minimum(position.quantity, parameters_.minimum_order_size)) {
    value.order.state = OrderState::Dust;
    return;
  }
  if (position.quantity <= 0.0 || value.bid_value <= 0.0 ||
      !allows_close_submit(value.order.state)) {
    return;
  }
  if (!(value.tick_size > 0.0) ||
      !(parameters_.polymarket_taker_fee_rate >= 0.0 &&
        parameters_.polymarket_taker_fee_rate <= 1.0)) {
    begin_flattening();
    return;
  }
  strategyframe::TimeInForce tif = strategyframe::TimeInForce::GTC;
  strategyframe::FixedPoint price{};
  bool post_only = true;
  bool taker = false;
  const double target = close_target_price(
      position.average_cost, value.tick_size, parameters_.profit_spread_ticks);
  if (force) {
    if (!order_terms_ok(value.bid_value, position.quantity, value.tick_size,
                        parameters_.polymarket_taker_fee_rate)) {
      begin_flattening();
      return;
    }
    tif = strategyframe::TimeInForce::IOC;
    price = value.bid;
    post_only = false;
    taker = true;
  } else if (select_close_liquidity(value.bid_value, position.quantity, target,
                                    parameters_.polymarket_taker_fee_rate) ==
             CloseLiquidity::Ioc) {
    tif = strategyframe::TimeInForce::IOC;
    price = value.bid;
    post_only = false;
    taker = true;
  } else {
    const double improved =
        value.ask_value -
        static_cast<double>(parameters_.close_improve_ticks) * value.tick_size;
    const double desired = std::max(target, improved);
    if (!order_terms_ok(desired, position.quantity, value.tick_size,
                        parameters_.polymarket_taker_fee_rate)) {
      begin_flattening();
      return;
    }
    price = price_fixed(value, desired);
    if (Fixed(price) <= value.bid_value) return;
  }
  auto request = make_order(
      value, strategyframe::Side::Sell, tif,
      quantity_fixed(value, position.quantity), price, post_only);
  const auto submitted = context_->place_order(request);
  if (!submitted) {
    value.order.last_error = static_cast<std::int32_t>(submitted.error);
    if (halt_phase_ == HaltPhase::Flattening) {
      if (++value.order.cancel_attempts >= 5)
        enter_halted(SettlementReason::CloseRetryExhausted);
    } else {
      begin_flattening();
    }
    return;
  }
  value.order.state =
      force ? OrderState::PendingForce : OrderState::PendingClose;
  value.order.purpose =
      force ? OrderPurpose::ForceFlatten : OrderPurpose::Close;
  value.order.token = submitted.value;
  value.order.price = request.price;
  value.order.started_ns = context_->now_ns();
  value.order.saw_fill = false;
  if (!pnl_->remember_order(
          submitted.value, value.instrument_id, request.side,
          value.order.purpose, window_generation_, taker)) {
    (void)context_->cancel(submitted.value);
    begin_flattening();
  }
}

void PolyMm::cancel_leg(Leg& value) {
  if (!is_active_order(value.order.state) ||
      value.order.state == OrderState::PendingCancel ||
      value.order.state == OrderState::NeedsReconcile) {
    return;
  }
  if (context_->cancel(value.order.token)) {
    value.order.state = OrderState::PendingCancel;
    value.order.started_ns = context_->now_ns();
    ++value.order.cancel_attempts;
    return;
  }
  if (++value.order.cancel_attempts >= 5) begin_stop_opening();
}

void PolyMm::cancel_open_orders() {
  for (Leg& value : legs_) {
    if (cancel_on_stop_opening(value.order.purpose, value.order.state))
      cancel_leg(value);
  }
}

void PolyMm::cancel_all() noexcept {
  for (Leg& value : legs_) cancel_leg(value);
}

void PolyMm::apply_lifecycle(Leg& value, const LifecycleResult& result) {
  if (result.release_ledger)
    pnl_->clear_terminal_order(value.order.token);
  commit_lifecycle(value.order, result);
  if (result.kind == LifecycleKind::NeedsReconcile &&
      value.order.uncertain_since_ns == 0 && context_ != nullptr) {
    value.order.uncertain_since_ns = context_->now_ns();
  }
  if (result.stop_opening) begin_stop_opening();
}

void PolyMm::poll_uncertain(Leg& value, std::uint64_t now_ns,
                             bool stop_on_timeout) {
  if (value.order.uncertain_since_ns == 0)
    value.order.uncertain_since_ns = now_ns;
  const auto found = context_->find_order(value.order.token);
  if (found && is_terminal_status(found.value.status)) {
    strategyframe::ExecutionUpdate update;
    update.kind = strategyframe::ExecutionUpdate::Kind::Order;
    update.status = found.value.status;
    update.token = found.value.token;
    update.instrument_id = found.value.instrument_id;
    update.remaining_quantity = found.value.remaining_quantity;
    const auto decision = apply_order_event(
        value.order, update, pnl_->quantity(value.instrument_id),
        parameters_.minimum_order_size);
    apply_lifecycle(value, decision);
    return;
  }
  const std::uint64_t timeout =
      static_cast<std::uint64_t>(parameters_.startup_reconcile_timeout_ms) *
      1'000'000ULL;
  if (stop_on_timeout &&
      now_ns > value.order.uncertain_since_ns + timeout) {
    begin_stop_opening();
  }
  if (query_inflight_) return;
  if (now_ns < next_query_ns_) return;
  const auto submitted = context_->query_open_orders(
      parameters_.account_id, value.instrument_id);
  if (!submitted) {
    note_query_failure();
    return;
  }
  query_inflight_ = true;
  query_token_ = submitted.value;
  query_target_token_ = value.order.token;
  query_instrument_ = value.instrument_id;
  query_snapshot_seen_ = false;
  query_saw_open_ = false;
}

void PolyMm::on_order_update(
    const strategyframe::ExecutionUpdate& update) {
  if (update.kind == strategyframe::ExecutionUpdate::Kind::CommandResult &&
      update.update_type == kCommandRegisterInstrument && binding_) {
    binding_->set_dirty();
  }
  if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill && pnl_) {
    if (!pnl_->has_instrument(update.instrument_id)) {
      note_late_fill(update);
      return;
    }
    const std::uint64_t duplicates_before = pnl_->duplicate_fills();
    if (!pnl_->apply_fill(update)) {
      if (pnl_->duplicate_fills() != duplicates_before) return;
      if (halt_phase_ != HaltPhase::Halted &&
          instrument_is_active(update.instrument_id)) {
        begin_flattening();
      }
      return;
    }
  }
  if (update.kind == strategyframe::ExecutionUpdate::Kind::CommandResult &&
      update.update_type == kCommandRetireInstrument) {
    const bool completed = update.error == 0 || update.error == 4;
    if (completed) {
      for (RetiredSlot& slot : retired_) {
        if (!slot.used) continue;
        for (std::size_t index = 0; index < slot.legs_.size(); ++index) {
          if (slot.legs_[index].instrument_id == update.instrument_id) {
            slot.retirement_completed_mask |=
                static_cast<std::uint8_t>(1U << index);
          }
        }
      }
    }
  }
  Leg* value = leg(update.instrument_id);
  if (value == nullptr &&
      update.kind == strategyframe::ExecutionUpdate::Kind::CommandResult) {
    for (Leg& candidate : legs_) {
      if (same_token(candidate.order.token, update.token)) {
        value = &candidate;
        break;
      }
    }
    if (value == nullptr) {
      for (RetiredSlot& slot : retired_) {
        if (!slot.used) continue;
        for (Leg& candidate : slot.legs_) {
          if (same_token(candidate.order.token, update.token)) {
            value = &candidate;
            break;
          }
        }
        if (value != nullptr) break;
      }
    }
  }
  if (value == nullptr && update.instrument_id != 0) {
    for (RetiredSlot& slot : retired_) {
      if (!slot.used) continue;
      for (Leg& candidate : slot.legs_) {
        if (candidate.instrument_id == update.instrument_id &&
            candidate.order.token.sequence != 0) {
          value = &candidate;
          break;
        }
      }
      if (value != nullptr) break;
    }
  }
  if (value != nullptr) {
    const auto decision = apply_order_event(
        value->order, update, pnl_->quantity(value->instrument_id),
        parameters_.minimum_order_size);
    apply_lifecycle(*value, decision);
  }
  if (halt_phase_ != HaltPhase::Halted) drive();
}

void PolyMm::maybe_log_oms_status(
    const strategyframe::OmsStatusUpdate& update) noexcept {
  if (context_ == nullptr) return;
  const auto emit = [&](std::uint32_t suppressed) {
    const char* kind =
        update.kind ==
                strategyframe::OmsStatusUpdate::Kind::ReconcileComplete
            ? "reconcile_complete"
            : "venue_status";
    const char* phase = "waiting_reconcile";
    switch (phase_) {
      case Phase::WaitingReconcile:
        phase = "waiting_reconcile";
        break;
      case Phase::WaitingBinding:
        phase = "waiting_binding";
        break;
      case Phase::WaitingFreshMarket:
        phase = "waiting_fresh_market";
        break;
      case Phase::Trading:
        phase = "trading";
        break;
      case Phase::StopOpening:
        phase = "stop_opening";
        break;
      case Phase::CapacityBlocked:
        phase = "capacity_blocked";
        break;
      case Phase::Halted:
        phase = "halted";
        break;
    }
    std::fprintf(stderr,
                 "poly-mm OMS status kind=%s adapter=%u status=%u error=%d "
                 "generation=%llu phase=%s suppressed=%u\n",
                 kind, static_cast<unsigned>(update.adapter_kind),
                 static_cast<unsigned>(update.adapter_status),
                 static_cast<int>(update.error),
                 static_cast<unsigned long long>(update.generation), phase,
                 suppressed);
  };

  if (update.kind ==
      strategyframe::OmsStatusUpdate::Kind::ReconcileComplete) {
    emit(0);
    return;
  }

  if (update.error == 0 && update.adapter_status == kAdapterConnecting) {
    if (!logged_connecting_) {
      logged_connecting_ = true;
      emit(0);
    }
    return;
  }

  if (update.error == 0 && update.adapter_status == kAdapterReady) {
    if (!logged_first_ready_) {
      logged_first_ready_ = true;
      emit(0);
    }
    return;
  }

  if (update.error == 0) return;

  const std::uint64_t now = context_->now_ns();
  const bool waiting_fatal = phase_ == Phase::WaitingReconcile;
  if (waiting_fatal || !logged_first_error_ || now >= next_error_log_ns_) {
    emit(error_suppressed_);
    error_suppressed_ = 0;
    logged_first_error_ = true;
    if (!waiting_fatal) next_error_log_ns_ = now + kOmsErrorLogIntervalNs;
    return;
  }
  if (error_suppressed_ != std::numeric_limits<std::uint32_t>::max())
    ++error_suppressed_;
}

void PolyMm::on_oms_status(
    const strategyframe::OmsStatusUpdate& update) {
  note_oms_reconcile(reconcile_, update);
  maybe_log_oms_status(update);
  if (phase_ == Phase::WaitingReconcile) {
    const StartupOmsClass classified = classify_startup_oms_status(update);
    if (classified == StartupOmsClass::Transient) return;
    if (classified == StartupOmsClass::Fatal) {
      reconcile_.oms_reconcile_failed = true;
      finish_reconcile();
      return;
    }
    finish_reconcile();
    return;
  }
  if (update.error != 0) begin_flattening();
}

void PolyMm::note_query_failure() {
  query_inflight_ = false;
  query_token_ = {};
  query_instrument_ = 0;
  query_snapshot_seen_ = false;
  query_saw_open_ = false;
  const std::uint32_t index =
      std::min(query_backoff_index_,
               static_cast<std::uint32_t>(
                   sizeof(kQueryBackoffNs) / sizeof(kQueryBackoffNs[0]) - 1U));
  next_query_ns_ = context_->now_ns() + kQueryBackoffNs[index];
  if (query_backoff_index_ <
      sizeof(kQueryBackoffNs) / sizeof(kQueryBackoffNs[0]) - 1U) {
    ++query_backoff_index_;
  }
}

void PolyMm::note_late_fill(
    const strategyframe::ExecutionUpdate& update) {
  if (pnl_ == nullptr ||
      !pnl_->remember_late_fill(update.account_id, update.instrument_id,
                                update.trade_id)) {
    return;
  }
  std::fprintf(
      stderr,
      "poly-mm late fill trade_id=%.*s instrument_id=%llu quantity=%.8f "
      "price=%.8f\n",
      static_cast<int>(update.trade_id.length), update.trade_id.value,
      static_cast<unsigned long long>(update.instrument_id),
      Fixed(update.fill_quantity), Fixed(update.fill_price));
  if (phase_ == Phase::Trading || phase_ == Phase::WaitingFreshMarket ||
      phase_ == Phase::WaitingBinding) {
    phase_ = Phase::StopOpening;
  }
  cancel_open_orders();
}

void PolyMm::on_open_orders_snapshot(
    const strategyframe::OpenOrdersSnapshot& snapshot) {
  if (!query_inflight_ || snapshot.token != query_token_) return;
  query_snapshot_seen_ = true;
  query_saw_open_ = false;
  for (const auto& order : snapshot.orders) {
    if (order.tracked_token == query_target_token_) query_saw_open_ = true;
  }
}

void PolyMm::on_positions_snapshot(
    const strategyframe::PositionsSnapshot&) {}

void PolyMm::on_query_complete(
    const strategyframe::QueryComplete& complete) {
  if (!query_inflight_ || complete.token != query_token_) return;
  const auto classified = classify_open_order_query(
      complete.kind == strategyframe::QueryComplete::Kind::OpenOrders &&
          complete.error == 0,
      query_snapshot_seen_, query_saw_open_);
  const auto instrument = query_instrument_;
  query_inflight_ = false;
  query_token_ = {};
  query_instrument_ = 0;
  query_snapshot_seen_ = false;
  query_saw_open_ = false;
  if (classified == OpenOrderQueryResult::Failed) {
    note_query_failure();
    return;
  }
  query_backoff_index_ = 0;
  next_query_ns_ = context_->now_ns() + kQueryBackoffNs[0];
  if (classified == OpenOrderQueryResult::StillOpen) {
    const auto cancel_match = [this](Leg& value) {
      if (value.order.token == query_target_token_ &&
          value.order.token.sequence != 0) {
        cancel_leg(value);
      }
    };
    if (Leg* value = leg(instrument)) cancel_match(*value);
    for (RetiredSlot& slot : retired_) {
      if (!slot.used) continue;
      for (Leg& value : slot.legs_) {
        if (value.instrument_id == instrument) cancel_match(value);
      }
    }
  }
}

bool PolyMm::retired_orders_terminal(
    const RetiredSlot& slot) const noexcept {
  for (const Leg& value : slot.legs_) {
    if (is_active_order(value.order.state) ||
        value.order.state == OrderState::NeedsReconcile) {
      return false;
    }
  }
  return true;
}

bool PolyMm::retired_flat(const RetiredSlot& slot) const noexcept {
  if (pnl_ == nullptr) return false;
  const auto up = pnl_->stored(slot.legs_[0].instrument_id);
  const auto down = pnl_->stored(slot.legs_[1].instrument_id);
  return std::abs(up.quantity) < 1e-12 && std::abs(down.quantity) < 1e-12;
}

void PolyMm::log_expiry_accounting(
    const RetiredSlot& slot, const StoredPnl& up, const StoredPnl& down,
    const char* status) const noexcept {
  std::fprintf(
      stderr,
      "poly-mm expiry accounting\n"
      "condition_id=%.*s\n"
      "expiry_ns=%llu\n"
      "up_instrument_id=%llu\n"
      "up_quantity=%.8f\n"
      "up_cost=%.8f\n"
      "up_realized=%.8f\n"
      "down_instrument_id=%llu\n"
      "down_quantity=%.8f\n"
      "down_cost=%.8f\n"
      "down_realized=%.8f\n"
      "status=%s\n",
      static_cast<int>(slot.condition_id.length), slot.condition_id.value,
      static_cast<unsigned long long>(slot.expiry_ns),
      static_cast<unsigned long long>(slot.legs_[0].instrument_id),
      up.quantity, up.cost, up.realized,
      static_cast<unsigned long long>(slot.legs_[1].instrument_id),
      down.quantity, down.cost, down.realized, status);
}

void PolyMm::backoff_resolution(RetiredSlot& slot, std::uint64_t now,
                                bool conflict) noexcept {
  slot.next_resolution_ns = now + resolution_backoff_ns(slot.resolution_retry_index);
  if (slot.resolution_retry_index < 3) ++slot.resolution_retry_index;
  if (!slot.resolution_failure_logged) {
    slot.resolution_failure_logged = true;
    std::fprintf(stderr,
                 "poly-mm resolution request failed condition_id=%.*s\n",
                 static_cast<int>(slot.condition_id.length),
                 slot.condition_id.value);
  }
  if (conflict) {
    std::fprintf(stderr,
                 "poly-mm resolution conflict condition_id=%.*s\n",
                 static_cast<int>(slot.condition_id.length),
                 slot.condition_id.value);
  }
}

void PolyMm::drain_resolution() noexcept {
  if (sidecar_ == nullptr) return;
  for (int index = 0; index < 4; ++index) {
    ResolutionResult result;
    if (!sidecar_->try_pop_resolution(result)) return;
    apply_resolution(result);
  }
}

void PolyMm::apply_resolution(const ResolutionResult& result) noexcept {
  const auto action = resolution_disposition(
      active_resolution_request_id_, active_resolution_condition_,
      result.request_id, result.condition_id);
  if (action == ResolutionDisposition::DropLate) return;
  resolution_inflight_ = false;
  RetiredSlot* slot = nullptr;
  for (RetiredSlot& candidate : retired_) {
    if (!candidate.used ||
        candidate.resolution_request_id != active_resolution_request_id_ ||
        !(candidate.condition_id == active_resolution_condition_)) {
      continue;
    }
    slot = &candidate;
    break;
  }
  active_resolution_request_id_ = 0;
  active_resolution_condition_ = {};
  if (context_ == nullptr) return;
  const std::uint64_t now = context_->now_ns();
  if (slot == nullptr) return;
  if (action == ResolutionDisposition::FailActive || result.transport_error ||
      result.parsed.status != GammaResolutionStatus::Resolved) {
    backoff_resolution(
        *slot, now,
        action == ResolutionDisposition::Apply &&
            result.parsed.status == GammaResolutionStatus::InvalidResponse);
    return;
  }
  const auto up_id = slot->legs_[0].instrument_id;
  const auto down_id = slot->legs_[1].instrument_id;
  const auto up = pnl_->stored(up_id);
  const auto down = pnl_->stored(down_id);
  const double up_payout =
      result.parsed.winner == Outcome::Up ? up.quantity : 0.0;
  const double down_payout =
      result.parsed.winner == Outcome::Down ? down.quantity : 0.0;
  const double window_pnl = up.realized + down.realized + up_payout - up.cost +
                            down_payout - down.cost;
  std::fprintf(stderr,
               "poly-mm resolution resolved condition_id=%.*s winner=%s "
               "up_payout=%.8f down_payout=%.8f pnl=%.8f\n",
               static_cast<int>(slot->condition_id.length),
               slot->condition_id.value,
               result.parsed.winner == Outcome::Up ? "up" : "down", up_payout,
               down_payout, window_pnl);
  if (!pnl_->settle_expired_window(up_id, down_id, result.parsed.winner)) {
    backoff_resolution(*slot, now, false);
    return;
  }
  for (Leg& value : slot->legs_) {
    if (value.order.token.sequence != 0)
      pnl_->clear_terminal_order(value.order.token);
  }
  *slot = {};
}

void PolyMm::drive_retired_release() {
  if (!context_ || pnl_ == nullptr) return;
  const std::uint64_t wall = WallNowNs();
  const std::uint64_t now = context_->now_ns();
  const std::uint64_t grace =
      static_cast<std::uint64_t>(parameters_.expiry_order_grace_ms) *
      1'000'000ULL;
  constexpr std::uint64_t kSummaryNs = 30'000'000'000ULL;
  for (RetiredSlot& slot : retired_) {
    if (!slot.used) continue;
    if (slot.expiry_ns == 0 || wall < slot.expiry_ns + grace) continue;
    for (std::size_t index = 0; index < slot.legs_.size(); ++index) {
      const auto bit = static_cast<std::uint8_t>(1U << index);
      if ((slot.retirement_requested_mask & bit) != 0) continue;
      const auto instrument = slot.legs_[index].instrument_id;
      if (instrument == 0) {
        slot.retirement_requested_mask |= bit;
        slot.retirement_completed_mask |= bit;
        continue;
      }
      if (context_->request_instrument_retirement(
              instrument, strategyframe::RetirementReason::LifecycleEnded)) {
        slot.retirement_requested_mask |= bit;
      }
    }
    if (!retired_orders_terminal(slot) ||
        slot.retirement_completed_mask != 0x3) {
      continue;
    }
    const auto up_id = slot.legs_[0].instrument_id;
    const auto down_id = slot.legs_[1].instrument_id;
    if (retired_flat(slot)) {
      if (!slot.accounting_logged) {
        log_expiry_accounting(slot, pnl_->stored(up_id), pnl_->stored(down_id),
                              "flat");
        slot.accounting_logged = true;
      }
      if (!pnl_->release_expired_window(up_id, down_id)) continue;
      for (Leg& value : slot.legs_) {
        if (value.order.token.sequence != 0)
          pnl_->clear_terminal_order(value.order.token);
      }
      slot = {};
      continue;
    }
    if (slot.resolution_summary_ns == 0 ||
        now >= slot.resolution_summary_ns + kSummaryNs) {
      if (slot.resolution_pending_logged) {
        const auto up = pnl_->stored(up_id);
        const auto down = pnl_->stored(down_id);
        std::fprintf(stderr,
                     "poly-mm resolution still pending condition_id=%.*s "
                     "up_quantity=%.8f down_quantity=%.8f\n",
                     static_cast<int>(slot.condition_id.length),
                     slot.condition_id.value, up.quantity, down.quantity);
      }
      slot.resolution_summary_ns = now;
    }
    if (resolution_inflight_ || now < slot.next_resolution_ns ||
        sidecar_ == nullptr) {
      continue;
    }
    ResolutionRequest request;
    request.request_id = next_resolution_request_id_;
    request.condition_id = slot.condition_id;
    if (!sidecar_->try_push_resolution(request)) {
      slot.next_resolution_ns =
          now + resolution_backoff_ns(slot.resolution_retry_index);
      continue;
    }
    ++next_resolution_request_id_;
    slot.resolution_request_id = request.request_id;
    active_resolution_request_id_ = request.request_id;
    active_resolution_condition_ = slot.condition_id;
    resolution_inflight_ = true;
    if (!slot.resolution_pending_logged) {
      slot.resolution_pending_logged = true;
      std::fprintf(stderr,
                   "poly-mm resolution pending condition_id=%.*s\n",
                   static_cast<int>(slot.condition_id.length),
                   slot.condition_id.value);
    }
  }
}

void PolyMm::sample_signal_horizons(std::uint64_t now_ns) noexcept {
  if (next_signal_stats_ns_ != 0 && now_ns < next_signal_stats_ns_) return;
  next_signal_stats_ns_ = now_ns + 20'000'000ULL;
  for (const Leg& value : legs_) {
    if (value.instrument_id == 0) continue;
    horizon_.sample(now_ns, value.instrument_id, window_generation_,
                    value.bid_value, value.ask_value, value.last_bbo_ns);
  }
  if (horizon_.take_summary_request()) log_signal_summary();
}

void PolyMm::log_signal_summary() const noexcept {
  const HorizonSummary summary = horizon_.summary();
  if (summary.completed == 0 && summary.dropped == 0) return;
  std::fprintf(stderr,
               "poly-mm signal horizons completed=%llu dropped=%llu\n",
               static_cast<unsigned long long>(summary.completed),
               static_cast<unsigned long long>(summary.dropped));
  for (std::size_t index = 0; index < summary.horizons.size(); ++index) {
    const HorizonAggregate& horizon = summary.horizons[index];
    const double valid = static_cast<double>(horizon.valid);
    const double mid = valid > 0.0 ? horizon.mid_move_sum / valid : 0.0;
    const double mark = valid > 0.0 ? horizon.mark_to_bid_sum / valid : 0.0;
    const double taker =
        valid > 0.0 ? horizon.taker_exit_net_sum / valid : 0.0;
    const double mark_ratio =
        valid > 0.0 ? static_cast<double>(horizon.mark_positive) / valid : 0.0;
    const double taker_ratio =
        valid > 0.0 ? static_cast<double>(horizon.taker_positive) / valid
                    : 0.0;
    std::fprintf(stderr,
                 "poly-mm signal horizon_ms=%llu valid_samples=%llu "
                 "missing_samples=%llu dropped_samples=%llu "
                 "average_mid_move_ticks=%.4f average_mark_to_bid_ticks=%.4f "
                 "average_taker_exit_net_ticks=%.4f "
                 "mark_to_bid_positive_ratio=%.4f "
                 "taker_exit_positive_ratio=%.4f\n",
                 static_cast<unsigned long long>(kSignalHorizonNs[index] /
                                                 1'000'000ULL),
                 static_cast<unsigned long long>(horizon.valid),
                 static_cast<unsigned long long>(horizon.missing),
                 static_cast<unsigned long long>(summary.dropped), mid, mark,
                 taker, mark_ratio, taker_ratio);
  }
}

void PolyMm::on_timer(const strategyframe::TimerEvent&) {
  if (context_ != nullptr) sample_signal_horizons(context_->now_ns());
  if (halt_phase_ == HaltPhase::Halted) return;
  drain_sidecar();
  drain_resolution();
  const std::uint64_t now = context_->now_ns();
  if (phase_ == Phase::WaitingReconcile) {
    finish_reconcile();
    return;
  }
  evaluate_binding();
  if (now - last_position_check_ns_ >= 100'000'000ULL &&
      phase_ == Phase::Trading && binding_ && binding_->active().valid) {
    last_position_check_ns_ = now;
    for (const Leg& value : legs_) {
      if (binding_->active().expiry_ns <= WallNowNs()) continue;
      const auto position = context_->find_position(
          parameters_.account_id, value.instrument_id,
          strategyframe::PositionSide::Net);
      const double framework =
          position ? Fixed(position.value.quantity) : 0.0;
      if ((!position &&
           position.error != strategyframe::Error::NotFound) ||
          std::abs(framework - pnl_->quantity(value.instrument_id)) >
              1e-9) {
        begin_flattening();
        return;
      }
    }
  }
  for (Leg& value : legs_) {
    if (value.order.state == OrderState::NeedsReconcile) {
      poll_uncertain(value, now);
      continue;
    }
    if ((value.order.state == OrderState::PendingClose ||
         value.order.state == OrderState::PendingOpen ||
         value.order.state == OrderState::PendingForce) &&
        now > value.order.started_ns +
                  static_cast<std::uint64_t>(
                      parameters_.max_order_resting_ms) *
                      1'000'000ULL) {
      cancel_leg(value);
    } else if (value.order.state == OrderState::PendingClose) {
      const PnlView position = pnl_->view(value.instrument_id);
      const double target = close_target_price(
          position.average_cost, value.tick_size,
          parameters_.profit_spread_ticks);
      const double improved =
          value.ask_value -
          static_cast<double>(parameters_.close_improve_ticks) *
              value.tick_size;
      const auto desired = price_fixed(value, std::max(target, improved));
      if (std::llabs(desired.value - value.order.price.value) >
          parameters_.requote_deviation_ticks * value.tick_units) {
        cancel_leg(value);
      }
    }
  }
  drive_retired_cancels();
  drive();
}

}  // namespace polymm
