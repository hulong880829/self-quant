#include "tradefimm/engine.h"

#include <limits>

namespace tradefimm {
namespace {

std::int64_t floor_to(std::int64_t value, std::int64_t tick) noexcept {
  if (tick <= 1) return value;
  if (value >= 0) return (value / tick) * tick;
  return -(((-value) + tick - 1) / tick) * tick;
}

std::int64_t ceil_to(std::int64_t value, std::int64_t tick) noexcept {
  if (tick <= 1) return value;
  if (value >= 0) return ((value + tick - 1) / tick) * tick;
  return -((-value) / tick) * tick;
}

std::int64_t trunc_toward_zero(std::int64_t numerator,
                               std::int64_t denominator) noexcept {
  if (denominator == 0) return 0;
  return numerator / denominator;
}

std::int64_t align_toward_zero(std::int64_t quantity, std::int64_t lot) noexcept {
  if (lot <= 1) return quantity;
  const std::int64_t sign = quantity < 0 ? -1 : 1;
  const std::int64_t magnitude = quantity < 0 ? -quantity : quantity;
  return sign * ((magnitude / lot) * lot);
}

std::int64_t ceil_div(std::int64_t numerator, std::int64_t denominator) noexcept {
  if (denominator <= 0) return numerator;
  if (numerator <= 0) return 0;
  return (numerator + denominator - 1) / denominator;
}

std::int64_t bps(std::int64_t notional, std::int64_t rate) noexcept {
  if (notional <= 0 || rate <= 0) return 0;
  return (notional * rate) / 10000;
}

bool leg_ok(const LegSpec& leg) noexcept {
  return leg.multiplier > 0 && leg.multiplier_scale <= 18 &&
         leg.min_native > 0 && leg.lot > 0 && leg.min_native % leg.lot == 0 &&
         leg.max_position > 0;
}

std::int64_t scaled_product(std::int64_t native, std::int64_t multiplier,
                            std::uint8_t scale) noexcept {
  if (scale == 0) return native * multiplier;
  std::int64_t factor = 1;
  for (std::uint8_t index = 0; index < scale; ++index) factor *= 10;
  return (native * multiplier) / factor;
}

}  // namespace

std::int64_t Engine::hedge_native_for(std::int64_t net_delta,
                                      std::int64_t multiplier, std::int64_t lot,
                                      std::int64_t min_native,
                                      std::uint8_t multiplier_scale) noexcept {
  if (multiplier <= 0 || lot <= 0 || min_native <= 0 || multiplier_scale > 18)
    return 0;
  std::int64_t raw = 0;
  if (multiplier_scale == 0) {
    raw = trunc_toward_zero(-net_delta, multiplier);
  } else {
    std::int64_t factor = 1;
    for (std::uint8_t index = 0; index < multiplier_scale; ++index) {
      if (factor > std::numeric_limits<std::int64_t>::max() / 10) return 0;
      factor *= 10;
    }
    const std::int64_t magnitude = net_delta < 0 ? -net_delta : net_delta;
    if (magnitude != 0 &&
        factor > std::numeric_limits<std::int64_t>::max() / magnitude)
      return 0;
    raw = trunc_toward_zero(-net_delta * factor, multiplier);
  }
  const std::int64_t aligned = align_toward_zero(raw, lot);
  const std::int64_t magnitude = aligned < 0 ? -aligned : aligned;
  if (magnitude < min_native) return 0;
  return aligned;
}

std::int64_t Engine::min_tradable_delta() const noexcept {
  const std::int64_t maker = scaled_product(
      spec_.maker.min_native, spec_.maker.multiplier, spec_.maker.multiplier_scale);
  const std::int64_t hedge = scaled_product(
      spec_.hedge.min_native, spec_.hedge.multiplier, spec_.hedge.multiplier_scale);
  return maker < hedge ? maker : hedge;
}

bool Engine::start(const PairSpec& spec) noexcept {
  if (!spec.same_base || !spec.same_quote) return false;
  if (!leg_ok(spec.maker) || !leg_ok(spec.hedge)) return false;
  if (spec.maker.venue == spec.hedge.venue) return false;
  if (spec.quote_size_base <= 0 || spec.tick <= 0 || spec.max_net_delta <= 0 ||
      spec.max_notional <= 0 || spec.max_unhedged_notional <= 0 ||
      spec.basis_buffer < 0 || spec.max_hedge_slippage_ticks < 0)
    return false;
  const std::int64_t maker_min = scaled_product(
      spec.maker.min_native, spec.maker.multiplier, spec.maker.multiplier_scale);
  const std::int64_t hedge_min = scaled_product(
      spec.hedge.min_native, spec.hedge.multiplier, spec.hedge.multiplier_scale);
  const std::int64_t smallest = maker_min < hedge_min ? maker_min : hedge_min;
  if (spec.delta_tolerance < 0 || spec.delta_tolerance >= smallest) return false;
  spec_ = spec;
  started_ = true;
  phase_ = Phase::WaitingCatalog;
  return true;
}

void Engine::note_catalog() noexcept {
  if (phase_ == Phase::WaitingCatalog) phase_ = Phase::WaitingOms;
}

void Engine::note_oms(bool orders_mapped, bool positions_mapped) noexcept {
  if (phase_ != Phase::WaitingOms) return;
  phase_ = orders_mapped && positions_mapped ? Phase::Reconciling : Phase::Halted;
}

void Engine::note_reconciled(bool mapped) noexcept {
  if (phase_ != Phase::Reconciling && phase_ != Phase::Reconnecting &&
      phase_ != Phase::Suspended)
    return;
  phase_ = mapped ? Phase::WaitingFreshBbo : Phase::Halted;
}

void Engine::note_baseline() noexcept {
  if (baseline_ || !fresh_) return;
  initial_maker_delta_ = maker_delta_;
  initial_hedge_delta_ = hedge_delta_;
  initial_maker_mid_ = (books_.maker_bid + books_.maker_ask) / 2;
  initial_hedge_mid_ = (books_.hedge_bid + books_.hedge_ask) / 2;
  baseline_ = initial_maker_mid_ > 0 && initial_hedge_mid_ > 0;
}

void Engine::note_disconnected() noexcept {
  if (!started_ || phase_ == Phase::Halted) return;
  phase_ = Phase::Reconnecting;
}

void Engine::note_reconnecting() noexcept { note_disconnected(); }

void Engine::note_suspended() noexcept {
  if (!started_ || phase_ == Phase::Halted) return;
  phase_ = Phase::Suspended;
}

void Engine::note_halted() noexcept {
  if (!started_) return;
  phase_ = Phase::Halted;
}

void Engine::recompute() noexcept {
  maker_delta_ = scaled_product(maker_native_, spec_.maker.multiplier,
                                spec_.maker.multiplier_scale);
  hedge_delta_ = scaled_product(hedge_native_, spec_.hedge.multiplier,
                                spec_.hedge.multiplier_scale);
  net_delta_ = maker_delta_ + hedge_delta_;
}

void Engine::set_positions(std::int64_t maker_native,
                           std::int64_t hedge_native) noexcept {
  maker_native_ = maker_native;
  hedge_native_ = hedge_native;
  recompute();
  const std::int64_t ib_delta =
      spec_.maker.venue == Venue::Ibkr ? maker_delta_ : hedge_delta_;
  ib_available_base_ = ib_delta;
}

void Engine::set_ib_inventory(std::int64_t available_base,
                              std::int64_t reserved_sell_base) noexcept {
  ib_available_base_ = available_base;
  ib_reserved_sell_base_ =
      reserved_sell_base > 0 ? reserved_sell_base : 0;
}

void Engine::set_ib_sell_reservation(
    std::int64_t reserved_sell_base) noexcept {
  ib_reserved_sell_base_ =
      reserved_sell_base > 0 ? reserved_sell_base : 0;
}

bool Engine::books_fresh(std::uint64_t now_ns,
                         std::uint64_t max_age_ns) const noexcept {
  if (!books_.maker_mds || !books_.hedge_mds || !books_.maker_execution_ready ||
      !books_.hedge_execution_ready)
    return false;
  if (books_.maker_bid <= 0 || books_.maker_ask <= books_.maker_bid ||
      books_.hedge_bid <= 0 || books_.hedge_ask <= books_.hedge_bid)
    return false;
  if (books_.maker_recv_ns == 0 || books_.hedge_recv_ns == 0) return false;
  if (now_ns < books_.maker_recv_ns || now_ns < books_.hedge_recv_ns) return false;
  return now_ns - books_.maker_recv_ns <= max_age_ns &&
         now_ns - books_.hedge_recv_ns <= max_age_ns;
}

void Engine::set_books(const Books& books, std::uint64_t now_ns,
                       std::uint64_t max_age_ns) noexcept {
  books_ = books;
  now_ns_ = now_ns;
  max_age_ns_ = max_age_ns == 0 ? 1 : max_age_ns;
  fresh_ = books_fresh(now_ns_, max_age_ns_);
  if (phase_ == Phase::WaitingFreshBbo && fresh_) {
    note_baseline();
    if (baseline_) {
      const HedgePlan next = plan();
      if (next.action == HedgeAction::Accept) phase_ = Phase::Quoting;
      else if (next.action == HedgeAction::Hedge) phase_ = Phase::HedgePending;
      else if (next.action == HedgeAction::Unwind) phase_ = Phase::EmergencyUnwind;
      else phase_ = Phase::Halted;
    }
  } else if (phase_ == Phase::Quoting && !fresh_) {
    phase_ = Phase::Suspended;
  }
}

void Engine::on_maker_fill(std::int64_t signed_native, std::int64_t price) noexcept {
  maker_native_ += signed_native;
  if (spec_.maker.venue == Venue::Ibkr) {
    ib_available_base_ +=
        scaled_product(signed_native, spec_.maker.multiplier,
                       spec_.maker.multiplier_scale);
  }
  recompute();
  if (baseline_ && price > 0) {
    const std::int64_t magnitude =
        signed_native < 0 ? -signed_native : signed_native;
    const std::int64_t notional =
        price * scaled_product(magnitude, spec_.maker.multiplier,
                               spec_.maker.multiplier_scale);
    cash_ -= price * scaled_product(signed_native, spec_.maker.multiplier,
                                    spec_.maker.multiplier_scale);
    fees_ += fee_for(true, signed_native > 0, notional);
  }
  if (phase_ == Phase::Halted) return;
  const HedgePlan next = plan();
  if (next.action == HedgeAction::Hedge) phase_ = Phase::HedgePending;
  else if (next.action == HedgeAction::Unwind) phase_ = Phase::EmergencyUnwind;
  else if (next.action == HedgeAction::Halt) phase_ = Phase::Halted;
  else if (next.action == HedgeAction::Accept && fresh_) phase_ = Phase::Quoting;
}

void Engine::on_hedge_fill(std::int64_t signed_native, std::int64_t price) noexcept {
  hedge_native_ += signed_native;
  if (spec_.hedge.venue == Venue::Ibkr) {
    ib_available_base_ +=
        scaled_product(signed_native, spec_.hedge.multiplier,
                       spec_.hedge.multiplier_scale);
  }
  recompute();
  if (baseline_ && price > 0) {
    const std::int64_t magnitude =
        signed_native < 0 ? -signed_native : signed_native;
    const std::int64_t notional =
        price * scaled_product(magnitude, spec_.hedge.multiplier,
                               spec_.hedge.multiplier_scale);
    cash_ -= price * scaled_product(signed_native, spec_.hedge.multiplier,
                                    spec_.hedge.multiplier_scale);
    fees_ += fee_for(false, signed_native > 0, notional);
  }
}

void Engine::note_hedge_terminal() noexcept {
  if (phase_ != Phase::HedgePending) return;
  const HedgePlan next = plan();
  if (next.action == HedgeAction::Accept) {
    hedge_failures_ = 0;
    phase_ = fresh_ ? Phase::Quoting : Phase::WaitingFreshBbo;
    return;
  }
  ++hedge_failures_;
  if (next.action == HedgeAction::Hedge &&
      hedge_failures_ <= spec_.max_hedge_retries) {
    phase_ = Phase::HedgePending;
    return;
  }
  if (next.action == HedgeAction::Unwind || next.action == HedgeAction::Hedge)
    phase_ = Phase::EmergencyUnwind;
  else
    phase_ = Phase::Halted;
}

void Engine::note_unwind_terminal(bool filled) noexcept {
  if (phase_ != Phase::EmergencyUnwind) return;
  if (filled) {
    const HedgePlan next = plan();
    if (next.action == HedgeAction::Accept) {
      phase_ = fresh_ ? Phase::Quoting : Phase::WaitingFreshBbo;
      return;
    }
  }
  phase_ = Phase::Halted;
}

bool Engine::makers_must_cancel() const noexcept {
  return phase_ != Phase::Quoting;
}

std::int64_t Engine::fee_for(bool maker_leg, bool buy,
                             std::int64_t notional) const noexcept {
  const Venue venue = maker_leg ? spec_.maker.venue : spec_.hedge.venue;
  if (venue == Venue::Hyperliquid) {
    const bool passive = maker_leg;
    return bps(notional, passive ? spec_.fees.hl_maker_bps
                                 : spec_.fees.hl_taker_bps);
  }
  if (maker_leg) {
    // IB passive limits are best-effort: the market can move before arrival.
    // Price and reserve them at the highest applicable rate.
    std::int64_t rate = spec_.fees.ib_taker_bps;
    if (spec_.fees.ib_passive_bps > rate) rate = spec_.fees.ib_passive_bps;
    const std::int64_t side_rate =
        buy ? spec_.fees.ib_buy_bps : spec_.fees.ib_sell_bps;
    if (side_rate > rate) rate = side_rate;
    return bps(notional, rate);
  }
  return bps(notional, buy ? spec_.fees.ib_buy_bps : spec_.fees.ib_sell_bps);
}

bool Engine::ib_sell_allowed(std::int64_t additional_sell_base) const noexcept {
  if (spec_.ibkr_short_policy == IbkrShortPolicy::Allow) return true;
  if (additional_sell_base <= 0) return true;
  if (ib_reserved_sell_base_ > ib_available_base_) return false;
  return additional_sell_base <=
         ib_available_base_ - ib_reserved_sell_base_;
}

Quote Engine::quote() const noexcept {
  Quote result;
  if (phase_ != Phase::Quoting || !fresh_ || !baseline_) return result;
  const std::int64_t size = spec_.quote_size_base;
  const std::int64_t hedge_native = hedge_native_for(
      -size, spec_.hedge.multiplier, spec_.hedge.lot, spec_.hedge.min_native,
      spec_.hedge.multiplier_scale);
  const std::int64_t hedge_covered =
      hedge_native == 0
          ? 0
          : scaled_product(hedge_native < 0 ? -hedge_native : hedge_native,
                           spec_.hedge.multiplier, spec_.hedge.multiplier_scale);
  if (hedge_covered != size) return result;
  const std::int64_t bid_notional = books_.hedge_bid * size;
  const std::int64_t ask_notional = books_.hedge_ask * size;
  if (spec_.max_notional > 0 &&
      (bid_notional > spec_.max_notional || ask_notional > spec_.max_notional))
    return result;
  if (bid_notional > spec_.max_unhedged_notional ||
      ask_notional > spec_.max_unhedged_notional)
    return result;
  const std::int64_t bid_fee =
      fee_for(true, true, bid_notional) + fee_for(false, false, bid_notional);
  const std::int64_t ask_fee =
      fee_for(true, false, ask_notional) + fee_for(false, true, ask_notional);
  const std::int64_t bid_cost = ceil_div(bid_fee, size);
  const std::int64_t ask_cost = ceil_div(ask_fee, size);
  const std::int64_t edge_bid = bid_cost + spec_.slippage_per_base +
                                spec_.basis_buffer + spec_.target_profit_per_base;
  const std::int64_t edge_ask = ask_cost + spec_.slippage_per_base +
                                spec_.basis_buffer + spec_.target_profit_per_base;
  std::int64_t bid = floor_to(books_.hedge_bid + spec_.basis_offset - edge_bid,
                              spec_.tick);
  std::int64_t ask =
      ceil_to(books_.hedge_ask + spec_.basis_offset + edge_ask, spec_.tick);
  if (spec_.maker.venue == Venue::Ibkr) {
    if (bid >= books_.maker_ask) bid = 0;
    if (ask <= books_.maker_bid) ask = 0;
  }
  const std::int64_t buy_pos = maker_delta_ + size;
  const std::int64_t sell_pos = maker_delta_ - size;
  const bool bid_sells_ib = spec_.maker.venue == Venue::Hyperliquid;
  const bool ask_sells_ib = spec_.maker.venue == Venue::Ibkr;
  const bool can_buy =
      buy_pos <= spec_.maker.max_position &&
      buy_pos <= spec_.max_net_delta &&
      (!bid_sells_ib || ib_sell_allowed(size));
  const bool can_sell =
      sell_pos >= -spec_.maker.max_position &&
      sell_pos >= -spec_.max_net_delta &&
      (!ask_sells_ib || ib_sell_allowed(size));
  if (can_buy && bid > 0) {
    result.bid = true;
    result.bid_price = bid;
  }
  if (can_sell && ask > 0) {
    result.ask = true;
    result.ask_price = ask;
  }
  result.cost_per_base = bid_cost > ask_cost ? bid_cost : ask_cost;
  return result;
}

HedgePlan Engine::plan() const noexcept {
  HedgePlan result;
  result.residual = net_delta_;
  const std::int64_t magnitude = net_delta_ < 0 ? -net_delta_ : net_delta_;
  if (magnitude <= spec_.delta_tolerance) {
    result.action = HedgeAction::Accept;
    result.residual = net_delta_;
    return result;
  }
  const std::int64_t hedge_native = hedge_native_for(
      net_delta_, spec_.hedge.multiplier, spec_.hedge.lot, spec_.hedge.min_native,
      spec_.hedge.multiplier_scale);
  if (phase_ != Phase::EmergencyUnwind && hedge_native != 0) {
    const std::int64_t hedge_change =
        scaled_product(hedge_native, spec_.hedge.multiplier,
                       spec_.hedge.multiplier_scale);
    const std::int64_t projected = hedge_delta_ + hedge_change;
    const std::int64_t projected_magnitude =
        projected < 0 ? -projected : projected;
    if (projected_magnitude > spec_.hedge.max_position) {
      result.action = HedgeAction::Halt;
      return result;
    }
    result.action = HedgeAction::Hedge;
    result.native_quantity = hedge_native;
    result.residual =
        net_delta_ + hedge_change;
    return result;
  }
  const std::int64_t unwind_native = hedge_native_for(
      net_delta_, spec_.maker.multiplier, spec_.maker.lot, spec_.maker.min_native,
      spec_.maker.multiplier_scale);
  if (unwind_native != 0) {
    result.action = HedgeAction::Unwind;
    result.native_quantity = unwind_native;
    result.residual =
        net_delta_ + scaled_product(unwind_native, spec_.maker.multiplier,
                                    spec_.maker.multiplier_scale);
    return result;
  }
  result.action = HedgeAction::Halt;
  result.residual = net_delta_;
  return result;
}

std::int64_t Engine::session_pnl() const noexcept {
  if (!baseline_) return 0;
  const std::int64_t maker_mid = fresh_ ? (books_.maker_bid + books_.maker_ask) / 2
                                       : initial_maker_mid_;
  const std::int64_t hedge_mid = fresh_ ? (books_.hedge_bid + books_.hedge_ask) / 2
                                       : initial_hedge_mid_;
  const std::int64_t mtm =
      (maker_mid - initial_maker_mid_) * initial_maker_delta_ +
      (hedge_mid - initial_hedge_mid_) * initial_hedge_delta_;
  return mtm + cash_ - fees_;
}

}  // namespace tradefimm
