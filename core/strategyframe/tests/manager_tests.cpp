#include <array>
#include <cstring>
#include <stdexcept>

#include "strategyframe/managers.h"
#include "strategyframe/manager_access.h"

namespace {

void Require(bool condition) {
  if (!condition) throw std::runtime_error("manager requirement failed");
}

}  // namespace

int main() {
  strategyframe::OrderManager orders(8);
  strategyframe::PositionManager positions(8);
  strategyframe::AccountManager accounts(orders, positions);
  Require(orders.capacity() == 8);
  Require(positions.capacity() == 8);
  Require(accounts.orders().open_orders().empty());
  Require(accounts.positions().positions().empty());
  Require(!orders.find({1, 1, 1}));
  Require(!positions.find(1, 1));

  strategyframe::OrderRequest request;
  request.account_id = 7;
  request.instrument_id = 42;
  request.side = strategyframe::Side::Buy;
  request.type = strategyframe::OrderType::Limit;
  request.quantity = {100, 2, {}};
  request.price = {2500, 2, {}};
  request.client_order_id = "manager-test";
  const strategyframe::OrderToken token{1, 9, 11};
  Require(strategyframe::detail::ManagerAccess::InsertPending(
              orders, request, token) == strategyframe::Error::Ok);
  Require(orders.size() == 1);
  Require(orders.find(token).value.status ==
          strategyframe::OrderStatus::PendingSubmit);

  strategyframe::ExecutionUpdate accepted;
  accepted.kind = strategyframe::ExecutionUpdate::Kind::Order;
  accepted.token = token;
  accepted.status = strategyframe::OrderStatus::Open;
  Require(strategyframe::detail::ManagerAccess::Apply(orders, accepted) ==
          strategyframe::Error::Ok);
  strategyframe::ExecutionUpdate invalid = accepted;
  invalid.status = strategyframe::OrderStatus::PendingSubmit;
  std::memcpy(invalid.client_order_id.value, "corrupt", 7);
  invalid.client_order_id.length = 7;
  Require(strategyframe::detail::ManagerAccess::Apply(orders, invalid) ==
          strategyframe::Error::InvalidState);
  Require(orders.find(token).value.client_order_id == "manager-test");

  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.token = token;
  fill.account_id = request.account_id;
  fill.instrument_id = request.instrument_id;
  fill.status = strategyframe::OrderStatus::PartiallyFilled;
  fill.fill_quantity = {25, 2, {}};
  fill.cumulative_quantity = {25, 2, {}};
  fill.remaining_quantity = {75, 2, {}};
  constexpr char trade_one[] = "trade-one";
  std::memcpy(fill.trade_id.value, trade_one, sizeof(trade_one) - 1U);
  fill.trade_id.length =
      static_cast<std::uint16_t>(sizeof(trade_one) - 1U);
  Require(strategyframe::detail::ManagerAccess::ApplyFill(
              positions, request.account_id, request.instrument_id,
              request.side, strategyframe::PositionSide::Net,
              fill.fill_quantity, fill.trade_id, 1) ==
          strategyframe::Error::Ok);
  Require(strategyframe::detail::ManagerAccess::ApplyFill(
              positions, request.account_id, request.instrument_id,
              request.side, strategyframe::PositionSide::Net,
              fill.fill_quantity, fill.trade_id, 1) ==
          strategyframe::Error::InvalidState);
  Require(strategyframe::detail::ManagerAccess::Apply(orders, fill) ==
          strategyframe::Error::Ok);
  Require(orders.find(token).value.status ==
          strategyframe::OrderStatus::PartiallyFilled);
  Require(positions.find(7, 42).value.quantity.value == 25);

  fill.status = strategyframe::OrderStatus::Filled;
  fill.fill_quantity = {75, 2, {}};
  fill.cumulative_quantity = {100, 2, {}};
  fill.remaining_quantity = {0, 2, {}};
  constexpr char trade_two[] = "trade-two";
  fill.trade_id = {};
  std::memcpy(fill.trade_id.value, trade_two, sizeof(trade_two) - 1U);
  fill.trade_id.length =
      static_cast<std::uint16_t>(sizeof(trade_two) - 1U);
  Require(strategyframe::detail::ManagerAccess::ApplyFill(
              positions, request.account_id, request.instrument_id,
              request.side, strategyframe::PositionSide::Net,
              fill.fill_quantity, fill.trade_id, 1) ==
          strategyframe::Error::Ok);
  Require(strategyframe::detail::ManagerAccess::Apply(orders, fill) ==
          strategyframe::Error::Ok);
  Require(orders.open_orders().empty());
  Require(positions.find(7, 42).value.quantity.value == 100);
  Require(strategyframe::detail::ManagerAccess::Apply(orders, fill) ==
          strategyframe::Error::NotFound);
  const auto closing_trade = [] {
    strategyframe::TradeId id;
    constexpr char value[] = "trade-close";
    std::memcpy(id.value, value, sizeof(value) - 1U);
    id.length = static_cast<std::uint16_t>(sizeof(value) - 1U);
    return id;
  }();
  Require(strategyframe::detail::ManagerAccess::ApplyFill(
              positions, 7, 42, strategyframe::Side::Sell,
              strategyframe::PositionSide::Net, {100, 2, {}},
              closing_trade, 2) == strategyframe::Error::Ok);
  Require(!positions.find(7, 42));
  Require(positions.positions().empty());

  const auto add_position = [&](strategyframe::InstrumentId instrument,
                                char trade_value) {
    strategyframe::TradeId id;
    id.value[0] = trade_value;
    id.length = 1;
    Require(strategyframe::detail::ManagerAccess::ApplyFill(
                positions, 7, instrument, strategyframe::Side::Buy,
                strategyframe::PositionSide::Net, {1, 0, {}}, id, 3) ==
            strategyframe::Error::Ok);
  };
  add_position(42, 'd');
  add_position(43, 'e');
  add_position(42, 'f');
  Require(positions.size() == 2);
  strategyframe::detail::ManagerAccess::RetireInstrument(positions, 42);
  Require(!positions.find(7, 42));
  Require(static_cast<bool>(positions.find(7, 43)));
  Require(positions.size() == 1);

  strategyframe::PositionManager rolling_dedup(4, 2);
  const auto trade = [](char value) {
    strategyframe::TradeId id;
    id.value[0] = value;
    id.length = 1;
    return id;
  };
  for (const char value : {'a', 'b', 'c'}) {
    Require(strategyframe::detail::ManagerAccess::ApplyFill(
                rolling_dedup, 1, 2, strategyframe::Side::Buy,
                strategyframe::PositionSide::Net, {1, 0, {}},
                trade(value), 1) == strategyframe::Error::Ok);
  }
  Require(strategyframe::detail::ManagerAccess::ApplyFill(
              rolling_dedup, 1, 2, strategyframe::Side::Buy,
              strategyframe::PositionSide::Net, {1, 0, {}}, trade('c'),
              1) == strategyframe::Error::InvalidState);
  Require(strategyframe::detail::ManagerAccess::ApplyFill(
              rolling_dedup, 1, 2, strategyframe::Side::Buy,
              strategyframe::PositionSide::Net, {1, 0, {}}, trade('a'),
              1) == strategyframe::Error::Ok);
  std::array<strategyframe::PositionView, 2> duplicate_snapshot{};
  duplicate_snapshot[0].account_id = 1;
  duplicate_snapshot[0].instrument_id = 2;
  duplicate_snapshot[0].quantity = {1, 0, {}};
  duplicate_snapshot[1] = duplicate_snapshot[0];
  Require(strategyframe::detail::ManagerAccess::ReplaceSnapshot(
              rolling_dedup, duplicate_snapshot) ==
          strategyframe::Error::InvalidState);
  Require(rolling_dedup.positions().empty());
  duplicate_snapshot[0].quantity = {};
  duplicate_snapshot[1] = {};
  Require(strategyframe::detail::ManagerAccess::ReplaceSnapshot(
              rolling_dedup, duplicate_snapshot) ==
          strategyframe::Error::Ok);
  Require(rolling_dedup.positions().empty());
  return 0;
}
