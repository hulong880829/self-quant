#pragma once

#include <cstddef>
#include <cstdint>
#include <type_traits>
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
  api::ResolvedInstrument routing{};
  api::VenueOrderId venue_order_id{};
  api::OrderStatus status{api::OrderStatus::PendingSubmit};
  api::InflightAction inflight{api::InflightAction::None};
  bool in_terminal{};
  std::uint8_t reserved[4]{};
  std::uint32_t terminal_prev{0xffffffffu};
  std::uint32_t terminal_next{0xffffffffu};
  std::int64_t cumulative_quantity{};
  std::int64_t remaining_quantity{};
  std::int64_t average_price{};
  WideNotional cumulative_notional{};
  std::uint64_t generation{};
  bool occupied{};
};

static_assert(std::is_trivially_copyable_v<OrderRecord>);
static_assert(std::is_standard_layout_v<OrderRecord>);
static_assert(sizeof(OrderRecord) == 432);
static_assert(alignof(OrderRecord) == 16);

class OrderTable {
 public:
  explicit OrderTable(std::size_t capacity);
  ~OrderTable();
  OrderTable(const OrderTable&) = delete;
  OrderTable& operator=(const OrderTable&) = delete;

  [[nodiscard]] std::size_t capacity() const noexcept { return records_.size(); }
  [[nodiscard]] std::size_t size() const noexcept { return size_; }

  api::Error Insert(const api::SubmitOrderRequest& request,
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
  [[nodiscard]] api::OrderHandle FindNonTerminal(
      api::InstrumentId instrument_id) const noexcept;

  api::Error BindVenueId(api::OrderHandle handle,
                         api::VenueOrderId venue_id) noexcept;
  api::Error MarkTerminal(api::OrderHandle handle) noexcept;
  api::Error Erase(api::OrderHandle handle) noexcept;
  api::Error EraseTerminalByInstrument(api::InstrumentId instrument_id) noexcept;
  [[nodiscard]] bool WasRetiredToken(api::RequestToken token) const noexcept;

 private:
  struct Impl;
  struct TerminalLink {
    std::uint32_t slot{0xffffffffu};
    std::uint64_t generation{};
  };

  void UnlinkTerminal(std::uint32_t slot) noexcept;
  [[nodiscard]] api::Error ReclaimOldestTerminal() noexcept;
  [[nodiscard]] static bool IsTerminalStatus(api::OrderStatus status) noexcept;

  std::vector<OrderRecord> records_;
  std::vector<std::uint32_t> free_slots_;
  std::size_t free_count_{};
  std::size_t size_{};
  void RememberRetiredToken(api::RequestToken token) noexcept;

  TerminalLink terminal_head_{};
  TerminalLink terminal_tail_{};
  std::array<api::RequestToken, 128> retired_tokens_{};
  std::uint32_t retired_token_count_{};
  std::uint32_t retired_token_next_{};
  Impl* impl_{};
};

}  // namespace oms
