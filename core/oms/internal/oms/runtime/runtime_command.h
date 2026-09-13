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
  RegisterInstrument = 5,
  RetireInstrument = 6,
  QueryOpenOrders = 7,
  QueryPositions = 8,
};

// The inactive request remains zero-filled. Keeping fixed fields makes command
// slots trivially copyable and avoids variant lifetime work on the hot path.
struct RuntimeCommand {
  RuntimeCommandKind kind{RuntimeCommandKind::Place};
  std::uint8_t reserved[3]{};
  std::uint32_t lane{};
  std::uint64_t enqueue_time_ns{};
  union {
    api::SubmitOrderRequest place;
    api::CancelOrderRequest cancel;
    api::RegisterInstrumentRequest register_instrument;
    api::RetireInstrumentRequest retire_instrument;
    exchange::AdapterQueryRequest query;
  };

  constexpr RuntimeCommand() noexcept : place{} {}
};

static_assert(std::is_trivially_copyable_v<RuntimeCommandKind>);
static_assert(std::is_standard_layout_v<RuntimeCommandKind>);
static_assert(std::is_trivially_copyable_v<RuntimeCommand>);
static_assert(std::is_standard_layout_v<RuntimeCommand>);
static_assert(sizeof(RuntimeCommandKind) == 1);
static_assert(sizeof(RuntimeCommand) == 272);
static_assert(alignof(RuntimeCommand) == 8);
static_assert(static_cast<std::uint8_t>(RuntimeCommandKind::Place) == 1);
static_assert(static_cast<std::uint8_t>(RuntimeCommandKind::Cancel) == 2);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::ReservedCancelAll) == 3);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::ReservedReconcile) == 4);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::RegisterInstrument) == 5);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandKind::RetireInstrument) == 6);

}  // namespace oms::runtime
