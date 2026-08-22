#pragma once

#include <cstddef>
#include <memory>
#include <span>

#include "strategyframe/error.h"
#include "strategyframe/types.h"

namespace strategyframe {

namespace detail {
struct ManagerAccess;
}

class OrderManager {
 public:
  explicit OrderManager(std::size_t capacity);
  ~OrderManager();
  OrderManager(OrderManager&&) noexcept;
  OrderManager& operator=(OrderManager&&) noexcept;
  OrderManager(const OrderManager&) = delete;
  OrderManager& operator=(const OrderManager&) = delete;

  [[nodiscard]] std::span<const OrderView> open_orders() const noexcept;
  [[nodiscard]] Result<OrderView> find(OrderToken token) const noexcept;
  [[nodiscard]] std::size_t size() const noexcept;
  [[nodiscard]] std::size_t capacity() const noexcept;

 private:
  [[nodiscard]] Error insert_pending(const OrderRequest& request,
                                     OrderToken token) noexcept;
  [[nodiscard]] Error reconcile_open(const OrderView& order) noexcept;
  [[nodiscard]] Error apply(const ExecutionUpdate& update) noexcept;
  void clear() noexcept;
  struct Impl;
  std::unique_ptr<Impl> impl_;
  friend class RuntimeCore;
  friend struct detail::ManagerAccess;
};

class PositionManager {
 public:
  explicit PositionManager(std::size_t capacity,
                           std::size_t fill_dedup_capacity = 8192);
  ~PositionManager();
  PositionManager(PositionManager&&) noexcept;
  PositionManager& operator=(PositionManager&&) noexcept;
  PositionManager(const PositionManager&) = delete;
  PositionManager& operator=(const PositionManager&) = delete;

  [[nodiscard]] std::span<const PositionView> positions() const noexcept;
  [[nodiscard]] Result<PositionView> find(
      AccountId account_id, InstrumentId instrument_id,
      PositionSide side = PositionSide::Net) const noexcept;
  [[nodiscard]] std::size_t size() const noexcept;
  [[nodiscard]] std::size_t capacity() const noexcept;

 private:
  [[nodiscard]] Error apply_fill(
      AccountId account_id, InstrumentId instrument_id, Side side,
      PositionSide position_side, const FixedPoint& quantity,
      const TradeId& trade_id, std::uint64_t generation) noexcept;
  [[nodiscard]] Error replace_snapshot(
      std::span<const PositionView> positions) noexcept;
  void retire_instrument(InstrumentId instrument_id) noexcept;
  struct Impl;
  std::unique_ptr<Impl> impl_;
  friend class RuntimeCore;
  friend struct detail::ManagerAccess;
};

class AccountManager {
 public:
  AccountManager(OrderManager& orders, PositionManager& positions) noexcept;

  [[nodiscard]] const OrderManager& orders() const noexcept { return orders_; }
  [[nodiscard]] const PositionManager& positions() const noexcept {
    return positions_;
  }

 private:
  OrderManager& orders_;
  PositionManager& positions_;
};

}  // namespace strategyframe
