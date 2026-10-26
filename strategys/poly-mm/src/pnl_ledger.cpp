#include "polymm/pnl_ledger.h"

#include <algorithm>
#include <cmath>
#include <cstdio>

#include "polymm/risk.h"

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
                               std::uint32_t window_generation,
                               bool taker) noexcept {
  for (OrderRef& value : orders_) {
    if (value.used && value.token == token) return true;
  }
  for (OrderRef& value : orders_) {
    if (!value.used) {
      value = {};
      value.used = true;
      value.token = token;
      value.instrument_id = instrument_id;
      value.side = side;
      value.purpose = purpose;
      value.window_generation = window_generation;
      value.taker = taker;
      return true;
    }
  }
  return false;
}

void PnlLedger::set_fee_rate(double fee_rate) noexcept {
  fee_rate_ = fee_rate;
}

bool PnlLedger::row_flat(const Row& value) noexcept {
  return std::abs(value.quantity) < 1e-9 &&
         std::abs(value.entry_fee_basis) < 1e-9;
}

void PnlLedger::archive_flat_row(Row& value) noexcept {
  archived_gross_ += value.gross_realized;
  archived_net_ += value.realized;
  archived_realized_fees_ += value.realized_fees;
  archived_total_fees_ += value.total_estimated_fees;
  value = {};
}

void PnlLedger::archive_settled_row(Row& value, double payout) noexcept {
  const double settlement_gross =
      payout - (value.cost - value.entry_fee_basis);
  const double settlement_net = payout - value.cost;
  archived_gross_ += value.gross_realized + settlement_gross;
  archived_net_ += value.realized + settlement_net;
  archived_realized_fees_ += value.realized_fees + value.entry_fee_basis;
  archived_total_fees_ += value.total_estimated_fees;
  value = {};
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
  const bool taker = reference->taker;
  const char* purpose =
      reference->purpose == OrderPurpose::Open
          ? "open"
          : reference->purpose == OrderPurpose::ForceFlatten ? "flatten"
                                                             : "close";
  if (reference->side == strategyframe::Side::Buy) {
    const double fee =
        taker ? polymarket_taker_fee(price, quantity_value, fee_rate_) : 0.0;
    value->quantity += quantity_value;
    value->cost += quantity_value * price + fee;
    value->entry_fee_basis += fee;
    value->total_estimated_fees += fee;
    std::fprintf(stderr,
                 "poly-mm fill purpose=%s liquidity=%s quantity=%.8f "
                 "price=%.8f estimated_fee=%.8f net_cost=%.8f "
                 "fee_source=estimated\n",
                 purpose, taker ? "taker" : "maker", quantity_value, price,
                 fee, quantity_value * price + fee);
    return true;
  }
  if (!(value->quantity > 0.0) ||
      quantity_value > value->quantity + 1e-12) {
    ++orphan_fills_;
    return false;
  }
  double closed = quantity_value;
  double allocated_cost = value->cost * (closed / value->quantity);
  double allocated_entry = value->entry_fee_basis * (closed / value->quantity);
  if (closed >= value->quantity - 1e-12) {
    closed = value->quantity;
    allocated_cost = value->cost;
    allocated_entry = value->entry_fee_basis;
  }
  const double gross_cost = allocated_cost - allocated_entry;
  const double proceeds = price * closed;
  const double exit_fee =
      taker ? polymarket_taker_fee(price, closed, fee_rate_) : 0.0;
  const double gross_delta = proceeds - gross_cost;
  const double net_delta = proceeds - exit_fee - allocated_cost;
  value->gross_realized += gross_delta;
  value->realized += net_delta;
  value->realized_fees += allocated_entry + exit_fee;
  value->total_estimated_fees += exit_fee;
  value->entry_fee_basis -= allocated_entry;
  value->cost -= allocated_cost;
  value->quantity -= closed;
  if (value->quantity < 1e-12) {
    value->realized_fees += value->entry_fee_basis;
    value->realized -= value->entry_fee_basis;
    value->quantity = 0.0;
    value->cost = 0.0;
    value->entry_fee_basis = 0.0;
  }
  std::fprintf(stderr,
               "poly-mm fill purpose=%s liquidity=%s quantity=%.8f "
               "price=%.8f estimated_fee=%.8f gross_realized=%.8f "
               "net_realized=%.8f fee_source=estimated\n",
               purpose, taker ? "taker" : "maker", closed, price, exit_fee,
               gross_delta, net_delta);
  return true;
}

void PnlLedger::update_bbo(
    const strategyframe::BboUpdate& update) noexcept {
  Row* value = row(update.header.instrument_id);
  if (value == nullptr) return;
  value->bid = Fixed(update.bid.price);
  value->ask = Fixed(update.ask.price);
}

StoredPnl PnlLedger::stored(
    strategyframe::InstrumentId instrument_id) const noexcept {
  const Row* value = row(instrument_id);
  if (value == nullptr) return {};
  return {true,
          value->quantity,
          value->cost,
          value->realized,
          value->gross_realized,
          value->entry_fee_basis,
          value->realized_fees,
          value->total_estimated_fees};
}

bool PnlLedger::release_expired_window(
    strategyframe::InstrumentId up,
    strategyframe::InstrumentId down) noexcept {
  if (up == 0 || down == 0 || up == down) return false;
  Row* up_row = row(up);
  Row* down_row = row(down);
  if ((up_row != nullptr && !row_flat(*up_row)) ||
      (down_row != nullptr && !row_flat(*down_row))) {
    return false;
  }
  if (up_row != nullptr) archive_flat_row(*up_row);
  if (down_row != nullptr) archive_flat_row(*down_row);
  return true;
}

bool PnlLedger::settle_expired_window(
    strategyframe::InstrumentId up, strategyframe::InstrumentId down,
    Outcome winner) noexcept {
  if (up == 0 || down == 0 || up == down) return false;
  for (const SettledWindow& settled : settled_windows_) {
    if (settled.up == up && settled.down == down) return true;
  }
  Row* up_row = row(up);
  Row* down_row = row(down);
  if (up_row == nullptr || down_row == nullptr) return false;
  const double up_payout = winner == Outcome::Up ? up_row->quantity : 0.0;
  const double down_payout =
      winner == Outcome::Down ? down_row->quantity : 0.0;
  last_window_pnl_ = up_row->realized + (up_payout - up_row->cost) +
                     down_row->realized + (down_payout - down_row->cost);
  archive_settled_row(*up_row, up_payout);
  archive_settled_row(*down_row, down_payout);
  settled_windows_[settled_cursor_ % settled_windows_.size()] = {up, down};
  ++settled_cursor_;
  std::fprintf(stderr,
               "poly-mm window settlement up=%llu down=%llu winner=%s "
               "pnl=%.8f\n",
               static_cast<unsigned long long>(up),
               static_cast<unsigned long long>(down),
               winner == Outcome::Up ? "up" : "down", last_window_pnl_);
  return true;
}

double PnlLedger::last_window_pnl() const noexcept {
  return last_window_pnl_;
}

bool PnlLedger::remember_late_fill(
    strategyframe::AccountId account,
    strategyframe::InstrumentId instrument,
    const strategyframe::TradeId& trade_id) noexcept {
  return remember_trade(account, instrument, trade_id);
}

double PnlLedger::archived_realized() const noexcept { return archived_net_; }

double PnlLedger::archived_gross() const noexcept { return archived_gross_; }

double PnlLedger::archived_net() const noexcept { return archived_net_; }

double PnlLedger::archived_realized_fees() const noexcept {
  return archived_realized_fees_;
}

double PnlLedger::archived_total_fees() const noexcept {
  return archived_total_fees_;
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
  const bool occupied =
      active_[0].instrument_id != 0 || active_[1].instrument_id != 0;
  if (!occupied) {
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
  if (active_[0].instrument_id == 0 && active_[1].instrument_id == 0)
    return true;
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
  value->total_estimated_fees -= value->entry_fee_basis;
  value->entry_fee_basis = 0.0;
  value->quantity = 0.0;
  value->cost = 0.0;
}

void PnlLedger::recycle_instrument(
    strategyframe::InstrumentId instrument_id) noexcept {
  for (Row& value : retired_) {
    if (value.instrument_id != instrument_id) continue;
    if (value.quantity != 0.0 || std::abs(value.entry_fee_basis) >= 1e-9 ||
        has_live_order(instrument_id)) {
      return;
    }
    archive_flat_row(value);
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
