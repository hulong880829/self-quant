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

PnlLedger::PnlLedger(strategyframe::AccountId account_id) noexcept
    : account_id_(account_id) {}

PnlLedger::PnlLedger(strategyframe::AccountId account_id,
                     strategyframe::InstrumentId up_instrument,
                     strategyframe::InstrumentId down_instrument) noexcept
    : PnlLedger(account_id) {
  (void)commit_active_window(up_instrument, down_instrument);
}

PnlLedger::Row* PnlLedger::row(
    strategyframe::InstrumentId instrument_id) noexcept {
  if (instrument_id == 0) return nullptr;
  for (Row& value : active_)
    if (value.instrument_id == instrument_id) return &value;
  for (Row& value : retired_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

const PnlLedger::Row* PnlLedger::row(
    strategyframe::InstrumentId instrument_id) const noexcept {
  if (instrument_id == 0) return nullptr;
  for (const Row& value : active_)
    if (value.instrument_id == instrument_id) return &value;
  for (const Row& value : retired_)
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
  for (const Row& value : active_)
    if (value.quantity != 0.0) return false;
  for (const Row& value : retired_)
    if (value.instrument_id != 0 && value.quantity != 0.0) return false;
  return true;
}

bool PnlLedger::active_flat() const noexcept {
  return active_[0].quantity == 0.0 && active_[1].quantity == 0.0;
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

bool PnlLedger::has_live_order(
    strategyframe::InstrumentId instrument_id) const noexcept {
  for (const OrderRef& value : orders_) {
    if (value.used && value.instrument_id == instrument_id) return true;
  }
  return false;
}

bool PnlLedger::archive_active_rows() noexcept {
  const bool keep =
      active_[0].quantity != 0.0 || active_[1].quantity != 0.0 ||
      has_live_order(active_[0].instrument_id) ||
      has_live_order(active_[1].instrument_id);
  if (!keep) {
    for (Row& value : active_) archived_realized_ += value.realized;
    active_ = {};
    return true;
  }
  std::size_t free = 0;
  for (const Row& value : retired_)
    if (value.instrument_id == 0) ++free;
  if (free < 2) return false;
  for (Row& value : active_) {
    if (value.instrument_id == 0) continue;
    for (Row& dest : retired_) {
      if (dest.instrument_id != 0) continue;
      dest = value;
      dest.retired = true;
      break;
    }
  }
  active_ = {};
  return true;
}

bool PnlLedger::commit_active_window(
    strategyframe::InstrumentId up_instrument,
    strategyframe::InstrumentId down_instrument) noexcept {
  if (up_instrument == 0 || down_instrument == 0 ||
      up_instrument == down_instrument) {
    return false;
  }
  if (active_[0].instrument_id == up_instrument &&
      active_[1].instrument_id == down_instrument) {
    return true;
  }
  if (active_[0].instrument_id != 0 || active_[1].instrument_id != 0) {
    if (!archive_active_rows()) return false;
  }
  active_[0] = {};
  active_[1] = {};
  active_[0].instrument_id = up_instrument;
  active_[1].instrument_id = down_instrument;
  return true;
}

bool PnlLedger::activate_window(
    strategyframe::InstrumentId up_instrument,
    strategyframe::InstrumentId down_instrument) noexcept {
  return commit_active_window(up_instrument, down_instrument);
}

bool PnlLedger::can_archive_active() const noexcept {
  const bool keep =
      active_[0].quantity != 0.0 || active_[1].quantity != 0.0 ||
      has_live_order(active_[0].instrument_id) ||
      has_live_order(active_[1].instrument_id);
  if (!keep) return true;
  std::size_t free = 0;
  for (const Row& value : retired_)
    if (value.instrument_id == 0) ++free;
  return free >= 2;
}

bool PnlLedger::archive_active() noexcept { return archive_active_rows(); }

bool PnlLedger::retired_nonzero() const noexcept {
  for (const Row& value : retired_) {
    if (value.instrument_id != 0 && value.quantity != 0.0) return true;
  }
  return false;
}

bool PnlLedger::has_instrument(
    strategyframe::InstrumentId instrument_id) const noexcept {
  return row(instrument_id) != nullptr;
}

void PnlLedger::confirm_snapshot_zeros(
    std::span<const strategyframe::QueriedPositionView> positions) noexcept {
  std::array<strategyframe::InstrumentId, kPnlRetiredRows> ids{};
  std::size_t count = 0;
  for (const Row& value : retired_) {
    if (value.instrument_id == 0) continue;
    bool held = false;
    for (const auto& position : positions) {
      if (position.instrument_id != value.instrument_id) continue;
      if (position.quantity.value != 0) {
        held = true;
        break;
      }
    }
    if (!held && count < ids.size()) ids[count++] = value.instrument_id;
  }
  for (std::size_t index = 0; index < count; ++index) {
    confirm_zero(ids[index]);
    recycle_instrument(ids[index]);
  }
}

void PnlLedger::confirm_zero(
    strategyframe::InstrumentId instrument_id) noexcept {
  Row* value = row(instrument_id);
  if (value == nullptr) return;
  value->quantity = 0.0;
  value->cost = 0.0;
}

void PnlLedger::recycle_instrument(
    strategyframe::InstrumentId instrument_id) noexcept {
  for (Row& value : retired_) {
    if (value.instrument_id != instrument_id) continue;
    if (value.quantity != 0.0 || has_live_order(instrument_id)) return;
    archived_realized_ += value.realized;
    value = {};
    return;
  }
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
