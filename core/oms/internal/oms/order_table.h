#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

#include "oms/api/error.h"
#include "oms/api/order_types.h"

namespace oms {

#if defined(__SIZEOF_INT128__)
__extension__ using WideNotional = __int128;
#else
using WideNotional = std::int64_t;
#endif

struct OrderRecord {
  api::NewOrderRequest request{};
  api::ExecutionRoutingSnapshot routing{};
  api::VenueOrderId venue_order_id{};
  api::OrderStatus status{api::OrderStatus::PendingSubmit};
  api::InflightAction inflight{api::InflightAction::None};
  std::array<std::uint8_t, 6> reserved{};
  std::int64_t cumulative_quantity{};
  std::int64_t remaining_quantity{};
  std::int64_t average_price{};
  WideNotional cumulative_notional{};
  std::uint64_t generation{};
  bool occupied{};
};

class OrderTable {
 public:
  explicit OrderTable(std::size_t capacity);
  ~OrderTable();
  OrderTable(const OrderTable&) = delete;
  OrderTable& operator=(const OrderTable&) = delete;

  [[nodiscard]] std::size_t capacity() const noexcept { return records_.size(); }
  [[nodiscard]] std::size_t size() const noexcept { return size_; }

  api::Error Insert(const api::NewOrderRequest& request,
                    api::OrderHandle& handle) noexcept;
  api::Error Insert(const api::PreparedOrderRequest& request,
                    api::OrderHandle& handle) noexcept;
  [[nodiscard]] OrderRecord* Lookup(api::OrderHandle handle) noexcept;
  [[nodiscard]] const OrderRecord* Lookup(api::OrderHandle handle) const noexcept;
  [[nodiscard]] OrderRecord* Find(api::RequestToken token) noexcept;
  [[nodiscard]] OrderRecord* Find(api::ClientOrderId client_id) noexcept;
  [[nodiscard]] OrderRecord* Find(api::VenueOrderId venue_id) noexcept;
  [[nodiscard]] api::OrderHandle HandleOf(
      const OrderRecord& record) const noexcept;
  [[nodiscard]] bool HasActiveOrInflight(
      api::InstrumentId instrument_id) const noexcept;

  api::Error BindVenueId(api::OrderHandle handle,
                         api::VenueOrderId venue_id) noexcept;
  api::Error Erase(api::OrderHandle handle) noexcept;

 private:
  struct Impl;

  std::vector<OrderRecord> records_;
  std::vector<std::uint32_t> free_slots_;
  std::size_t free_count_{};
  std::size_t size_{};
  Impl* impl_{};
};

}  // namespace oms
