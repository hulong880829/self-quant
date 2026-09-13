#pragma once

#include <array>
#include <cstddef>
#include <cstdint>

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
  void clear_terminal_order(strategyframe::OrderToken token) noexcept;
  [[nodiscard]] bool activate_window(
      strategyframe::InstrumentId up_instrument,
      strategyframe::InstrumentId down_instrument) noexcept;
  [[nodiscard]] std::uint64_t duplicate_fills() const noexcept;
  [[nodiscard]] std::uint64_t orphan_fills() const noexcept;
  [[nodiscard]] std::size_t used_order_slots() const noexcept;

 private:
  struct Row {
    strategyframe::InstrumentId instrument_id{};
    double quantity{};
    double cost{};
    double realized{};
    double bid{};
    double ask{};
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

  strategyframe::AccountId account_id_{};
  std::array<Row, 2> rows_{};
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
