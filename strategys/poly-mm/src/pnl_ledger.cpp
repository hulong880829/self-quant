#include "polymm/pnl_ledger.h"

#include <algorithm>
#include <cmath>

namespace polymm {
namespace {

double Fixed(strategyframe::FixedPoint value) noexcept {
  double divisor = 1.0;
  for (std::uint8_t index = 0; index < value.scale; ++index) divisor *= 10.0;
  return static_cast<double>(value.value) / divisor;
}

bool SameTrade(const strategyframe::TradeId& lhs,
               const strategyframe::TradeId& rhs) noexcept {
  return lhs == rhs;
}

}  // namespace

PnlLedger::PnlLedger(strategyframe::AccountId account_id,
                     strategyframe::InstrumentId up_instrument,
                     strategyframe::InstrumentId down_instrument) noexcept
    : account_id_(account_id) {
  rows_[0].instrument_id = up_instrument;
  rows_[1].instrument_id = down_instrument;
}

PnlLedger::Row* PnlLedger::row(
    strategyframe::InstrumentId instrument_id) noexcept {
  for (Row& value : rows_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

const PnlLedger::Row* PnlLedger::row(
    strategyframe::InstrumentId instrument_id) const noexcept {
  for (const Row& value : rows_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

bool PnlLedger::remember_order(strategyframe::OrderToken token,
                               strategyframe::InstrumentId instrument_id,
                               strategyframe::Side side,
                               OrderPurpose purpose,
                               std::uint32_t window_generation) noexcept {
  for (OrderRef& value : orders_) {
    if (value.used && value.token == token) return true;
  }
  for (OrderRef& value : orders_) {
    if (!value.used) {
      value = {true, token, instrument_id, side, purpose, window_generation};
      return true;
    }
  }
  return false;
}

const PnlLedger::OrderRef* PnlLedger::order(
    strategyframe::OrderToken token) const noexcept {
  if (token.sequence == 0) return nullptr;
  for (const OrderRef& value : orders_)
    if (value.used && value.token == token) return &value;
  for (const OrderRef& value : tombstones_)
    if (value.used && value.token == token) return &value;
  return nullptr;
}

bool PnlLedger::remember_trade(
    strategyframe::AccountId account_id,
    strategyframe::InstrumentId instrument_id,
    const strategyframe::TradeId& trade_id) noexcept {
  if (trade_id.length == 0) return false;
  for (const SeenTrade& value : trades_) {
    if (value.used && value.account_id == account_id &&
        value.instrument_id == instrument_id &&
        SameTrade(value.trade_id, trade_id)) {
      return false;
    }
  }
  trades_[trade_cursor_] = {true, account_id, instrument_id, trade_id};
  trade_cursor_ = (trade_cursor_ + 1) % trades_.size();
  return true;
}

bool PnlLedger::apply_fill(
    const strategyframe::ExecutionUpdate& update) noexcept {
  if (update.kind != strategyframe::ExecutionUpdate::Kind::Fill ||
      update.account_id != account_id_ || update.instrument_id == 0) {
    if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill)
      ++orphan_fills_;
    return false;
  }
  const OrderRef* reference = order(update.token);
  Row* value = row(update.instrument_id);
  if (reference == nullptr || value == nullptr) {
    ++orphan_fills_;
    return false;
  }
  if (!remember_trade(update.account_id, update.instrument_id,
                      update.trade_id)) {
    ++duplicate_fills_;
    return false;
  }
  const double quantity_value = Fixed(update.fill_quantity);
  const double price = Fixed(update.fill_price);
  if (!(quantity_value > 0.0) || !(price > 0.0)) return false;
  if (reference->side == strategyframe::Side::Buy) {
    value->quantity += quantity_value;
    value->cost += quantity_value * price;
    return true;
  }
  if (!(value->quantity > 0.0) ||
      quantity_value > value->quantity + 1e-12) {
    ++orphan_fills_;
    return false;
  }
  const double closed = quantity_value;
  const double average = value->cost / value->quantity;
  value->realized += (price - average) * closed;
  value->quantity -= closed;
  value->cost -= average * closed;
  if (value->quantity < 1e-12) {
    value->quantity = 0.0;
    value->cost = 0.0;
  }
  return true;
}

void PnlLedger::update_bbo(
    const strategyframe::BboUpdate& update) noexcept {
  Row* value = row(update.header.instrument_id);
  if (value == nullptr) return;
  value->bid = Fixed(update.bid.price);
  value->ask = Fixed(update.ask.price);
}

PnlView PnlLedger::view(
    strategyframe::InstrumentId instrument_id) const noexcept {
  const Row* value = row(instrument_id);
  if (value == nullptr) return {};
  PnlView result;
  result.quantity = value->quantity;
  result.average_cost =
      value->quantity > 0.0 ? value->cost / value->quantity : 0.0;
  result.realized = value->realized;
  const double mid =
      value->bid > 0.0 && value->ask > 0.0
          ? (value->bid + value->ask) * 0.5
          : value->bid;
  result.unrealized_mid =
      (mid - result.average_cost) * result.quantity;
  result.unrealized_exit =
      (value->bid - result.average_cost) * result.quantity;
  result.total_exit = result.realized + result.unrealized_exit;
  return result;
}

double PnlLedger::quantity(
    strategyframe::InstrumentId instrument_id) const noexcept {
  const Row* value = row(instrument_id);
  return value == nullptr ? 0.0 : value->quantity;
}

bool PnlLedger::flat() const noexcept {
  return rows_[0].quantity == 0.0 && rows_[1].quantity == 0.0;
}

void PnlLedger::clear_terminal_order(
    strategyframe::OrderToken token) noexcept {
  for (OrderRef& value : orders_) {
    if (value.used && value.token == token) {
      tombstones_[tombstone_cursor_] = value;
      tombstone_cursor_ = (tombstone_cursor_ + 1) % tombstones_.size();
      value = {};
      return;
    }
  }
}

bool PnlLedger::activate_window(
    strategyframe::InstrumentId up_instrument,
    strategyframe::InstrumentId down_instrument) noexcept {
  if (!flat() || up_instrument == 0 || down_instrument == 0 ||
      up_instrument == down_instrument) {
    return false;
  }
  for (const Row& value : rows_) archived_realized_ += value.realized;
  rows_ = {};
  rows_[0].instrument_id = up_instrument;
  rows_[1].instrument_id = down_instrument;
  for (OrderRef& value : orders_) value = {};
  tombstones_ = {};
  tombstone_cursor_ = 0;
  return true;
}

std::size_t PnlLedger::used_order_slots() const noexcept {
  std::size_t count = 0;
  for (const OrderRef& value : orders_)
    if (value.used) ++count;
  return count;
}

std::uint64_t PnlLedger::duplicate_fills() const noexcept {
  return duplicate_fills_;
}

std::uint64_t PnlLedger::orphan_fills() const noexcept {
  return orphan_fills_;
}

}  // namespace polymm
