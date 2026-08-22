#include <atomic>
#include <charconv>
#include <cstdlib>
#include <new>
#include <stdexcept>

#include "strategyframe/manager_access.h"

namespace {

std::atomic<std::uint64_t> allocations{};

void Require(bool condition) {
  if (!condition) throw std::runtime_error("hotpath requirement failed");
}

strategyframe::TradeId Trade(std::uint64_t value) {
  strategyframe::TradeId result{};
  const auto converted =
      std::to_chars(result.value, result.value + sizeof(result.value), value);
  if (converted.ec != std::errc{}) return {};
  result.length = static_cast<std::uint16_t>(converted.ptr - result.value);
  return result;
}

}  // namespace

void* operator new(std::size_t size) {
  allocations.fetch_add(1, std::memory_order_relaxed);
  if (void* value = std::malloc(size)) return value;
  throw std::bad_alloc();
}

void operator delete(void* value) noexcept { std::free(value); }
void operator delete(void* value, std::size_t) noexcept { std::free(value); }

int main() {
  strategyframe::OrderManager orders(1024);
  strategyframe::PositionManager positions(16, 2048);
  strategyframe::OrderRequest request;
  request.account_id = 1;
  request.instrument_id = 9;
  request.side = strategyframe::Side::Buy;
  request.type = strategyframe::OrderType::Limit;
  request.quantity = {1, 0, {}};
  request.price = {100, 0, {}};
  request.client_order_id = "sealed";

  const std::uint64_t baseline =
      allocations.load(std::memory_order_relaxed);
  for (std::uint64_t sequence = 1; sequence <= 1000; ++sequence) {
    const strategyframe::OrderToken token{1, 1, sequence};
    Require(strategyframe::detail::ManagerAccess::InsertPending(
                orders, request, token) == strategyframe::Error::Ok);
    strategyframe::ExecutionUpdate fill;
    fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
    fill.status = strategyframe::OrderStatus::Filled;
    fill.account_id = request.account_id;
    fill.instrument_id = request.instrument_id;
    fill.token = token;
    fill.trade_id = Trade(sequence);
    fill.fill_quantity = request.quantity;
    fill.cumulative_quantity = request.quantity;
    fill.remaining_quantity = {};
    Require(strategyframe::detail::ManagerAccess::ApplyFill(
                positions, request.account_id, request.instrument_id,
                request.side, strategyframe::PositionSide::Net,
                fill.fill_quantity, fill.trade_id, 1) ==
            strategyframe::Error::Ok);
    Require(strategyframe::detail::ManagerAccess::Apply(orders, fill) ==
            strategyframe::Error::Ok);
    Require(orders.open_orders().empty());
  }
  Require(allocations.load(std::memory_order_relaxed) == baseline);
  Require(positions.find(1, 9).value.quantity.value == 1000);
  return 0;
}
