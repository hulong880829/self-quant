#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>

#include "polymm/rolling_pair_binding.h"
#include "polymm/types.h"
#include "strategyframe/types.h"

namespace polymm {

struct PnlView {
  double quantity{};
  double average_cost{};
  double realized{};
  double unrealized_mid{};
  double unrealized_exit{};
  double total_exit{};
};

struct StoredPnl {
  bool present{};
  double quantity{};
  double cost{};
  double realized{};
  double gross_realized{};
  double entry_fee_basis{};
  double realized_fees{};
  double total_estimated_fees{};
};

class PnlLedger {
 public:
  explicit PnlLedger(strategyframe::AccountId account_id) noexcept;
  PnlLedger(strategyframe::AccountId account_id,
            strategyframe::InstrumentId up_instrument,
            strategyframe::InstrumentId down_instrument) noexcept;

  bool remember_order(strategyframe::OrderToken token,
                      strategyframe::InstrumentId instrument_id,
                      strategyframe::Side side, OrderPurpose purpose,
                      std::uint32_t window_generation,
                      bool taker = false) noexcept;
  void set_fee_rate(double fee_rate) noexcept;
  bool apply_fill(const strategyframe::ExecutionUpdate& update) noexcept;
  void update_bbo(const strategyframe::BboUpdate& update) noexcept;
  [[nodiscard]] PnlView view(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] StoredPnl stored(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] bool release_expired_window(
      strategyframe::InstrumentId up,
      strategyframe::InstrumentId down) noexcept;
  [[nodiscard]] bool settle_expired_window(
      strategyframe::InstrumentId up,
      strategyframe::InstrumentId down, Outcome winner) noexcept;
  [[nodiscard]] double last_window_pnl() const noexcept;
  [[nodiscard]] bool remember_late_fill(
      strategyframe::AccountId account,
      strategyframe::InstrumentId instrument,
      const strategyframe::TradeId& trade_id) noexcept;
  [[nodiscard]] double archived_realized() const noexcept;
  [[nodiscard]] double archived_gross() const noexcept;
  [[nodiscard]] double archived_net() const noexcept;
  [[nodiscard]] double archived_realized_fees() const noexcept;
  [[nodiscard]] double archived_total_fees() const noexcept;
  [[nodiscard]] double quantity(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] bool flat() const noexcept;
  [[nodiscard]] bool active_flat() const noexcept;
  void clear_terminal_order(strategyframe::OrderToken token) noexcept;
  [[nodiscard]] bool commit_active_window(
      strategyframe::InstrumentId up_instrument,
      strategyframe::InstrumentId down_instrument) noexcept;
  [[nodiscard]] bool activate_window(
      strategyframe::InstrumentId up_instrument,
      strategyframe::InstrumentId down_instrument) noexcept;
  [[nodiscard]] bool can_archive_active() const noexcept;
  [[nodiscard]] bool archive_active() noexcept;
  [[nodiscard]] bool retired_nonzero() const noexcept;
  [[nodiscard]] bool has_instrument(
      strategyframe::InstrumentId instrument_id) const noexcept;
  void confirm_zero(strategyframe::InstrumentId instrument_id) noexcept;
  void recycle_instrument(
      strategyframe::InstrumentId instrument_id) noexcept;
  void confirm_snapshot_zeros(
      std::span<const strategyframe::QueriedPositionView> positions) noexcept;
  [[nodiscard]] std::uint64_t duplicate_fills() const noexcept;
  [[nodiscard]] std::uint64_t orphan_fills() const noexcept;
  [[nodiscard]] std::size_t used_order_slots() const noexcept;
  [[nodiscard]] bool has_live_order(
      strategyframe::InstrumentId instrument_id) const noexcept;

 private:
  struct Row {
    strategyframe::InstrumentId instrument_id{};
    double quantity{};
    double cost{};
    double entry_fee_basis{};
    double gross_realized{};
    double realized{};
    double realized_fees{};
    double total_estimated_fees{};
    double bid{};
    double ask{};
    bool retired{};
  };
  struct OrderRef {
    bool used{};
    strategyframe::OrderToken token{};
    strategyframe::InstrumentId instrument_id{};
    strategyframe::Side side{strategyframe::Side::Buy};
    OrderPurpose purpose{OrderPurpose::Open};
    std::uint32_t window_generation{};
    bool taker{};
  };
  struct SeenTrade {
    bool used{};
    strategyframe::AccountId account_id{};
    strategyframe::InstrumentId instrument_id{};
    strategyframe::TradeId trade_id{};
  };

  [[nodiscard]] Row* row(
      strategyframe::InstrumentId instrument_id) noexcept;
  [[nodiscard]] const Row* row(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] const OrderRef* order(
      strategyframe::OrderToken token) const noexcept;
  [[nodiscard]] bool remember_trade(
      strategyframe::AccountId account_id,
      strategyframe::InstrumentId instrument_id,
      const strategyframe::TradeId& trade_id) noexcept;
  [[nodiscard]] bool archive_active_rows() noexcept;
  [[nodiscard]] static bool row_flat(const Row& value) noexcept;
  void archive_flat_row(Row& value) noexcept;
  void archive_settled_row(Row& value, double payout) noexcept;

  struct SettledWindow {
    strategyframe::InstrumentId up{};
    strategyframe::InstrumentId down{};
  };

  strategyframe::AccountId account_id_{};
  std::array<SettledWindow, 32> settled_windows_{};
  std::size_t settled_cursor_{};
  double last_window_pnl_{};
  std::array<Row, 2> active_{};
  std::array<Row, kPnlRetiredRows> retired_{};
  std::array<OrderRef, 64> orders_{};
  std::array<OrderRef, 128> tombstones_{};
  std::size_t tombstone_cursor_{};
  std::array<SeenTrade, 8192> trades_{};
  std::size_t trade_cursor_{};
  std::uint64_t duplicate_fills_{};
  std::uint64_t orphan_fills_{};
  double fee_rate_{};
  double archived_gross_{};
  double archived_net_{};
  double archived_realized_fees_{};
  double archived_total_fees_{};
};

}  // namespace polymm
