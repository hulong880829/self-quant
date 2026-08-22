#include "polymm/strategy.h"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <string>
#include <string_view>

#include "polymm/catalog_rollover.h"
#include "polymm/market_window.h"
#include "polymm/risk.h"

namespace polymm {
namespace {

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

bool Terminal(strategyframe::OrderStatus status) noexcept {
  return status == strategyframe::OrderStatus::Filled ||
         status == strategyframe::OrderStatus::Canceled ||
         status == strategyframe::OrderStatus::Rejected ||
         status == strategyframe::OrderStatus::Expired;
}

bool SameToken(strategyframe::OrderToken lhs,
               strategyframe::OrderToken rhs) noexcept {
  return lhs.sequence != 0 && lhs == rhs;
}

}  // namespace

PolyMm::~PolyMm() {
  if (sidecar_) sidecar_->stop();
}

bool PolyMm::load_parameters() {
  const auto& params = context_->params();
  const auto account = params.require_int("account_id");
  if (!account || account.value <= 0) return false;
  parameters_.account_id = static_cast<std::uint32_t>(account.value);
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
  parameters_.stop_loss_ticks =
      params.optional_double("stop_loss_ticks", 5.0);
  parameters_.fairprice_timer_us = static_cast<std::uint32_t>(
      params.optional_int("fairprice_timer_us", 200));
  sidecar_config_.fairprice_host =
      params.optional_string("fairprice.host", "127.0.0.1");
  sidecar_config_.fairprice_service =
      params.optional_string("fairprice.service", "8080");
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
  if (const char* token = std::getenv("AGGDATA_BROWSER_TOKEN"))
    sidecar_config_.fairprice_token = token;

  return parameters_.vol_lookback_ms > parameters_.signal_horizon_ms &&
         parameters_.signal_horizon_ms > 0 &&
         parameters_.vol_threshold >= 0.0 &&
         parameters_.min_market_price > 0.0 &&
         parameters_.max_market_price < 1.0 &&
         parameters_.min_market_price < parameters_.max_market_price &&
         parameters_.order_shares > 0.0 &&
         parameters_.max_single_side_shares > 0.0 &&
         parameters_.fairprice_timer_us > 0;
}

void PolyMm::init(strategyframe::StrategyContext& context) {
  context_ = &context;
  if (!load_parameters()) {
    halt();
    return;
  }
  const auto up = context.find_instrument(
      {strategyframe::Venue::Polymarket,
       strategyframe::ProductType::BinaryOption, "btc5mup"});
  const auto down = context.find_instrument(
      {strategyframe::Venue::Polymarket,
       strategyframe::ProductType::BinaryOption, "btc5mdown"});
  if (!up || !down ||
      up.value.venue != strategyframe::Venue::Polymarket ||
      down.value.venue != strategyframe::Venue::Polymarket ||
      up.value.expiry_time_ns == 0 ||
      up.value.expiry_time_ns != down.value.expiry_time_ns ||
      up.value.lot_size <= 0 || down.value.lot_size <= 0) {
    halt();
    return;
  }
  const auto meets_minimum = [this](
                                 const auto& catalog) {
    double factor = 1.0;
    for (std::uint8_t index = 0; index < catalog.quantity_scale; ++index)
      factor *= 10.0;
    return parameters_.order_shares * factor + 1e-9 >=
           static_cast<double>(catalog.lot_size);
  };
  if (!meets_minimum(up.value) || !meets_minimum(down.value)) {
    halt();
    return;
  }
  parameters_.up_instrument_id = up.value.instrument_id;
  parameters_.down_instrument_id = down.value.instrument_id;
  catalogs_[0] = up.value;
  catalogs_[1] = down.value;
  legs_[0] = {Outcome::Up,
              up.value.instrument_id,
              up.value.price_scale,
              up.value.quantity_scale,
              up.value.tick_size,
              Fixed({up.value.tick_size, up.value.price_scale, {}})};
  legs_[1] = {Outcome::Down,
              down.value.instrument_id,
              down.value.price_scale,
              down.value.quantity_scale,
              down.value.tick_size,
              Fixed({down.value.tick_size, down.value.price_scale, {}})};
  phase_ = Phase::WaitingMarket;
  signal_ = std::make_unique<SignalEngine>(parameters_);
  pnl_ = std::make_unique<PnlLedger>(
      parameters_.account_id, parameters_.up_instrument_id,
      parameters_.down_instrument_id);
  sidecar_ = std::make_unique<FairPriceClient>(sidecar_config_);
  if (!sidecar_->start()) {
    halt();
    return;
  }
  const std::uint64_t interval =
      static_cast<std::uint64_t>(parameters_.fairprice_timer_us) * 1'000ULL;
  if (!context.schedule_timer(context.now_ns() + interval, interval)) halt();
}

PolyMm::Leg* PolyMm::leg(
    strategyframe::InstrumentId instrument_id) noexcept {
  for (Leg& value : legs_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

const PolyMm::Leg* PolyMm::leg(
    strategyframe::InstrumentId instrument_id) const noexcept {
  for (const Leg& value : legs_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

void PolyMm::on_bbo_update(const strategyframe::BboUpdate& update) {
  if (phase_ == Phase::Halted) return;
  drain_sidecar();
  Leg* value = leg(update.header.instrument_id);
  if (value == nullptr) return;
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
      case SidecarEvent::Kind::FairPrice:
        fairprice_live_ = signal_->update(event.fair_price);
        latest_fair_wall_ns_ = event.fair_price.wall_ns;
        break;
      case SidecarEvent::Kind::FairPriceDisconnected:
        fairprice_live_ = false;
        cancel_all();
        break;
    }
  }
}

void PolyMm::on_instrument_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog) {
  handle_catalog(catalog);
}

void PolyMm::handle_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog) {
  if (catalog.venue != strategyframe::Venue::Polymarket ||
      catalog.product != strategyframe::ProductType::BinaryOption) {
    return;
  }
  for (std::size_t index = 0; index < catalogs_.size(); ++index) {
    if (!is_next_physical_catalog(catalogs_[index], catalog)) continue;
    pending_catalogs_[index] = catalog;
    begin_rollover();
    return;
  }
}

void PolyMm::begin_rollover() {
  if (phase_ == Phase::Halted ||
      phase_ == Phase::RolloverDraining) return;
  phase_ = Phase::RolloverDraining;
  cancel_all();
}

void PolyMm::drive() {
  if (phase_ == Phase::Halted || !context_) return;
  if (context_->metrics().unmatched_fills != 0) {
    cancel_all();
    halt();
    return;
  }
  if (latest_fair_wall_ns_ != 0 &&
      WallNowNs() > latest_fair_wall_ns_ + 2'000'000'000ULL) {
    fairprice_live_ = false;
    cancel_all();
  }
  if (phase_ == Phase::RolloverDraining) {
    drive_rollover();
    return;
  }
  if (phase_ == Phase::WaitingMarket) {
    if (legs_[0].book_generation != 0 &&
        legs_[1].book_generation != 0 &&
        legs_[0].bid_value > 0.0 && legs_[0].ask_value > 0.0 &&
        legs_[1].bid_value > 0.0 && legs_[1].ask_value > 0.0) {
      phase_ = Phase::Trading;
    } else {
      return;
    }
  }
  if (phase_ != Phase::Trading) return;
  const auto left =
      catalogs_[0].expiry_time_ns > WallNowNs()
          ? catalogs_[0].expiry_time_ns - WallNowNs()
          : 0;
  if (near_expiry(left, parameters_.no_trade_before_expiry_ms)) {
    begin_rollover();
    drive_rollover();
    return;
  }
  for (Leg& value : legs_) drive_leg(value);
  evaluate_signal();
}

void PolyMm::drive_leg(Leg& value) {
  const PnlView position = pnl_->view(value.instrument_id);
  if (position.quantity <= 0.0 || value.order_state != OrderState::Idle)
    return;
  const bool stop = stop_loss_triggered(
      position, value.tick_size, parameters_.stop_loss_ticks);
  submit_close(value, stop);
}

void PolyMm::evaluate_signal() {
  if (!fairprice_live_ || !signal_->ready()) return;
  for (const Leg& value : legs_)
    if (value.order_state != OrderState::Idle) return;
  const double up_mid = (legs_[0].bid_value + legs_[0].ask_value) * 0.5;
  const double spread = legs_[0].ask_value - legs_[0].bid_value;
  const auto evaluation = signal_->evaluate(
      up_mid, spread, legs_[0].tick_size,
      catalogs_[0].expiry_time_ns > WallNowNs()
          ? catalogs_[0].expiry_time_ns - WallNowNs()
          : 0);
  Leg* selected = nullptr;
  if (evaluation.direction == SignalEvaluation::Direction::Up)
    selected = &legs_[0];
  else if (evaluation.direction == SignalEvaluation::Direction::Down)
    selected = &legs_[1];
  if (selected != nullptr && risk_allows_open(*selected))
    submit_open(*selected);
}

bool PolyMm::risk_allows_open(const Leg& value) const noexcept {
  const double up = pnl_->quantity(parameters_.up_instrument_id);
  const double down = pnl_->quantity(parameters_.down_instrument_id);
  const double current =
      value.outcome == Outcome::Up ? up : down;
  const double other =
      value.outcome == Outcome::Up ? down : up;
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

void PolyMm::submit_open(Leg& value) {
  if (value.ask_value <= 0.0) return;
  auto request = make_order(
      value, strategyframe::Side::Buy, strategyframe::TimeInForce::IOC,
      quantity_fixed(value, parameters_.order_shares), value.ask, false);
  const auto submitted = context_->place_order(request);
  if (!submitted) return;
  value.order_state = OrderState::PendingOpen;
  value.order_purpose = OrderPurpose::Open;
  value.order_token = submitted.value;
  value.order_price = request.price;
  value.order_started_ns = context_->now_ns();
  if (!pnl_->remember_order(
          submitted.value, value.instrument_id, request.side,
          OrderPurpose::Open, window_generation_)) {
    (void)context_->cancel(submitted.value);
    halt();
  }
}

void PolyMm::submit_close(Leg& value, bool force) {
  const PnlView position = pnl_->view(value.instrument_id);
  if (position.quantity <= 0.0 || value.bid_value <= 0.0) return;
  strategyframe::TimeInForce tif = strategyframe::TimeInForce::GTC;
  strategyframe::FixedPoint price{};
  bool post_only = true;
  if (force) {
    tif = strategyframe::TimeInForce::IOC;
    price = value.bid;
    post_only = false;
  } else {
    const double target =
        position.average_cost +
        static_cast<double>(parameters_.profit_spread_ticks) *
            value.tick_size;
    if (value.bid_value >= target) {
      tif = strategyframe::TimeInForce::IOC;
      price = value.bid;
      post_only = false;
    } else {
      const double improved =
          value.ask_value -
          static_cast<double>(parameters_.close_improve_ticks) *
              value.tick_size;
      const double desired = std::max(target, improved);
      price = price_fixed(value, desired);
      if (Fixed(price) <= value.bid_value) return;
    }
  }
  auto request = make_order(
      value, strategyframe::Side::Sell, tif,
      quantity_fixed(value, position.quantity), price, post_only);
  const auto submitted = context_->place_order(request);
  if (!submitted) return;
  value.order_state =
      force ? OrderState::PendingForce : OrderState::PendingClose;
  value.order_purpose =
      force ? OrderPurpose::ForceFlatten : OrderPurpose::Close;
  value.order_token = submitted.value;
  value.order_price = request.price;
  value.order_started_ns = context_->now_ns();
  if (!pnl_->remember_order(
          submitted.value, value.instrument_id, request.side,
          value.order_purpose, window_generation_)) {
    (void)context_->cancel(submitted.value);
    halt();
  }
}

void PolyMm::cancel_leg(Leg& value) {
  if (value.order_state == OrderState::Idle ||
      value.order_state == OrderState::PendingCancel) {
    return;
  }
  if (context_->cancel(value.order_token))
    value.order_state = OrderState::PendingCancel;
}

void PolyMm::cancel_all() {
  for (Leg& value : legs_) cancel_leg(value);
}

bool PolyMm::position_reconciled() const {
  if (!pnl_->flat()) return false;
  for (const Leg& value : legs_) {
    const auto position = context_->find_position(
        parameters_.account_id, value.instrument_id,
        strategyframe::PositionSide::Net);
    if (position && position.value.quantity.value != 0) return false;
    if (!position && position.error != strategyframe::Error::NotFound)
      return false;
  }
  return true;
}

void PolyMm::drive_rollover() {
  bool active_order = false;
  for (Leg& value : legs_) {
    if (value.order_state != OrderState::Idle) {
      active_order = true;
      cancel_leg(value);
      continue;
    }
    if (pnl_->quantity(value.instrument_id) > 0.0) {
      submit_close(value, true);
      if (value.order_state != OrderState::Idle) {
        active_order = true;
      } else if (catalogs_[0].expiry_time_ns != 0 &&
                 WallNowNs() >= catalogs_[0].expiry_time_ns) {
        halt();
        return;
      }
    }
  }
  if (active_order || !context_->open_orders().empty()) return;
  if (!position_reconciled()) {
    halt();
    return;
  }
  if (!catalog_rollover_ready(pending_catalogs_)) {
    if (catalogs_[0].expiry_time_ns != 0 &&
        WallNowNs() >= catalogs_[0].expiry_time_ns) {
      halt();
    }
    return;
  }
  (void)activate_next_window();
}

bool PolyMm::activate_next_window() {
  if (!catalog_rollover_ready(pending_catalogs_)) return false;
  if (!pnl_->activate_window(pending_catalogs_[0].instrument_id,
                             pending_catalogs_[1].instrument_id)) {
    halt();
    return false;
  }
  catalogs_ = pending_catalogs_;
  pending_catalogs_ = {};
  parameters_.up_instrument_id = catalogs_[0].instrument_id;
  parameters_.down_instrument_id = catalogs_[1].instrument_id;
  ++window_generation_;
  signal_->reset();
  fairprice_live_ = false;
  for (std::size_t index = 0; index < legs_.size(); ++index) {
    Leg& value = legs_[index];
    value.instrument_id = catalogs_[index].instrument_id;
    value.price_scale = catalogs_[index].price_scale;
    value.quantity_scale = catalogs_[index].quantity_scale;
    value.tick_units = catalogs_[index].tick_size;
    value.tick_size = Fixed(
        {catalogs_[index].tick_size, catalogs_[index].price_scale, {}});
    value.bid = {};
    value.ask = {};
    value.bid_value = 0.0;
    value.ask_value = 0.0;
    value.book_generation = 0;
  }
  phase_ = Phase::WaitingMarket;
  return true;
}

void PolyMm::on_order_update(
    const strategyframe::ExecutionUpdate& update) {
  if (phase_ == Phase::Halted) return;
  Leg* value = leg(update.instrument_id);
  if (value == nullptr) return;
  if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill) {
    const std::uint64_t duplicates_before = pnl_->duplicate_fills();
    if (!pnl_->apply_fill(update)) {
      if (pnl_->duplicate_fills() != duplicates_before) return;
      halt();
      return;
    }
  }
  if (update.kind == strategyframe::ExecutionUpdate::Kind::Order &&
      SameToken(update.token, value->order_token) &&
      Terminal(update.status)) {
    value->order_state = OrderState::Idle;
    value->order_token = {};
    value->order_started_ns = 0;
  }
  drive();
}

void PolyMm::on_oms_status(
    const strategyframe::OmsStatusUpdate& update) {
  if (update.error != 0) {
    cancel_all();
    halt();
  }
}

void PolyMm::on_timer(const strategyframe::TimerEvent&) {
  if (phase_ == Phase::Halted) return;
  drain_sidecar();
  const std::uint64_t now = context_->now_ns();
  if (now - last_position_check_ns_ >= 100'000'000ULL) {
    last_position_check_ns_ = now;
    for (const Leg& value : legs_) {
      if (catalogs_[value.outcome == Outcome::Up ? 0U : 1U]
                  .expiry_time_ns <= WallNowNs()) {
        continue;
      }
      const auto position = context_->find_position(
          parameters_.account_id, value.instrument_id,
          strategyframe::PositionSide::Net);
      const double framework =
          position ? Fixed(position.value.quantity) : 0.0;
      if ((!position &&
           position.error != strategyframe::Error::NotFound) ||
          std::abs(framework - pnl_->quantity(value.instrument_id)) >
              1e-9) {
        cancel_all();
        halt();
        return;
      }
    }
  }
  for (Leg& value : legs_) {
    if ((value.order_state == OrderState::PendingClose ||
         value.order_state == OrderState::PendingOpen) &&
        now > value.order_started_ns +
                  static_cast<std::uint64_t>(
                      parameters_.max_order_resting_ms) *
                      1'000'000ULL) {
      cancel_leg(value);
    } else if (value.order_state == OrderState::PendingClose) {
      const PnlView position = pnl_->view(value.instrument_id);
      const double target =
          position.average_cost +
          static_cast<double>(parameters_.profit_spread_ticks) *
              value.tick_size;
      const double improved =
          value.ask_value -
          static_cast<double>(parameters_.close_improve_ticks) *
              value.tick_size;
      const auto desired = price_fixed(value, std::max(target, improved));
      if (std::llabs(desired.value - value.order_price.value) >
          parameters_.requote_deviation_ticks * value.tick_units) {
        cancel_leg(value);
      }
    }
  }
  drive();
}

void PolyMm::halt() noexcept {
  phase_ = Phase::Halted;
  fairprice_live_ = false;
  if (context_) cancel_all();
}

}  // namespace polymm
