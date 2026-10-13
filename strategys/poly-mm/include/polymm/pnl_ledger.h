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

class PnlLedger {
 public:
  explicit PnlLedger(strategyframe::AccountId account_id) noexcept;
  PnlLedger(strategyframe::AccountId account_id,
            strategyframe::InstrumentId up_instrument,
            strategyframe::InstrumentId down_instrument) noexcept;

  bool remember_order(strategyframe::OrderToken token,
                      strategyframe::InstrumentId instrument_id,
                      strategyframe::Side side, OrderPurpose purpose,
                      std::uint32_t window_generation) noexcept;
  bool apply_fill(const strategyframe::ExecutionUpdate& update) noexcept;
  void update_bbo(const strategyframe::BboUpdate& update) noexcept;
  [[nodiscard]] PnlView view(
      strategyframe::InstrumentId instrument_id) const noexcept;
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
    double realized{};
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

  strategyframe::AccountId account_id_{};
  std::array<Row, 2> active_{};
  std::array<Row, kPnlRetiredRows> retired_{};
  std::array<OrderRef, 64> orders_{};
  std::array<OrderRef, 128> tombstones_{};
  std::size_t tombstone_cursor_{};
  std::array<SeenTrade, 8192> trades_{};
  std::size_t trade_cursor_{};
  std::uint64_t duplicate_fills_{};
  std::uint64_t orphan_fills_{};
  double archived_realized_{};
};

}  // namespace polymm
