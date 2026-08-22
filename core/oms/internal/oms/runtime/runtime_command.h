#pragma once

#include <cstdint>
#include <type_traits>

#include "oms/api/order_types.h"
#include "oms/exchange/adapter_types.h"

namespace oms::runtime {

enum class RuntimeCommandKind : std::uint8_t {
  Place = 1,
  Cancel = 2,
  ReservedCancelAll = 3,
  ReservedReconcile = 4,
  RebindInstrument = 5,
  PlacePrepared = 6,
  QueryOpenOrders = 7,
  QueryPositions = 8,
};

// The inactive request remains zero-filled. Keeping fixed fields makes command
// slots trivially copyable and avoids variant lifetime work on the hot path.
struct RuntimeCommand {
  struct OrderPayload {
    api::NewOrderRequest place{};
    api::CancelOrderRequest cancel{};
  };

  RuntimeCommandKind kind{RuntimeCommandKind::Place};
  std::uint8_t reserved[3]{};
  std::uint32_t lane{};
  std::uint64_t enqueue_time_ns{};
  union {
    OrderPayload orders;
    api::RebindPolymarketInstrumentRequest rebind;
    api::PreparedOrderRequest prepared;
    exchange::AdapterQueryRequest query;
  };

  constexpr RuntimeCommand() noexcept : orders{} {}
};

static_assert(std::is_trivially_copyable_v<RuntimeCommandKind>);
static_assert(std::is_standard_layout_v<RuntimeCommandKind>);
static_assert(std::is_trivially_copyable_v<RuntimeCommand>);
static_assert(std::is_standard_layout_v<RuntimeCommand>);
static_assert(sizeof(RuntimeCommandKind) == 1);
static_assert(static_cast<std::uint8_t>(RuntimeCommandKind::Place) == 1);
static_assert(static_cast<std::uint8_t>(RuntimeCommandKind::Cancel) == 2);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::ReservedCancelAll) == 3);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::ReservedReconcile) == 4);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::RebindInstrument) == 5);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::PlacePrepared) == 6);

}  // namespace oms::runtime
