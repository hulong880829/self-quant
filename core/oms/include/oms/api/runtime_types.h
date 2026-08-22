#pragma once

#include <cstddef>
#include <cstdint>
#include <type_traits>

#include "oms/api/error.h"
#include "oms/api/order_types.h"

namespace oms::api {

inline constexpr std::size_t kMaxRuntimeLanes = 64;
inline constexpr std::size_t kTradeAdapterCapacityCount = 4;

inline constexpr std::uint32_t kDefaultOrderCapacity = 4096;
inline constexpr std::uint32_t kDefaultFillDedupCapacity = 8192;
inline constexpr std::uint32_t kDefaultPendingEventCapacity = 1024;
inline constexpr std::uint32_t kDefaultAdapterSendCapacity = 256;
inline constexpr std::uint32_t kDefaultAdapterReceiveCapacity = 1024;
inline constexpr std::uint32_t kMaximumRuntimeSlotCapacity = 1U << 24U;
inline constexpr std::uint32_t kMaximumAdapterReceiveCapacity = 1U << 26U;
inline constexpr std::uint32_t kRuntimeConfigAbiRevision = 2;

enum class ExecutionMode : std::uint8_t {
  Inline = 0,
  DedicatedIo = 1,
};

enum class AdapterCapacitySlot : std::uint8_t {
  BinanceSpot = 0,
  BinanceUsdm = 1,
  Polymarket = 2,
  Fake = 3,
};

struct LaneConfig {
  std::uint32_t lane_id{};
  std::uint32_t command_capacity{};
  std::uint32_t update_capacity{};
  std::int32_t strategy_cpu_id{-1};
};

// Indexed by AdapterCapacitySlot.
struct AdapterCapacityConfig {
  std::uint32_t send_capacity{};
  std::uint32_t receive_capacity{};
};

// All lane storage is described up front. Runtime construction allocates each
// lane's two rings once; these capacities never grow afterward.
struct RuntimeConfig {
  ExecutionMode mode{ExecutionMode::Inline};
  std::uint8_t reserved0[3]{};
  std::uint32_t lane_count{};
  std::int32_t io_cpu_id{-1};
  std::uint32_t deadline_capacity{};
  LaneConfig lanes[kMaxRuntimeLanes]{};
  // Appended in ABI revision 2. Zero preserves source compatibility with
  // revision-1 aggregate/config initialization and means "bounded default";
  // NormalizeRuntimeConfig materializes every value before construction.
  std::uint32_t order_capacity{};
  std::uint32_t fill_dedup_capacity{};
  std::uint32_t pending_event_capacity{};
  std::uint32_t abi_revision{};
  AdapterCapacityConfig adapter_capacities[kTradeAdapterCapacityCount]{};
};

[[nodiscard]] constexpr RuntimeConfig NormalizeRuntimeConfig(
    RuntimeConfig config) noexcept {
  if (config.order_capacity == 0)
    config.order_capacity = kDefaultOrderCapacity;
  if (config.fill_dedup_capacity == 0)
    config.fill_dedup_capacity = kDefaultFillDedupCapacity;
  if (config.pending_event_capacity == 0)
    config.pending_event_capacity = kDefaultPendingEventCapacity;
  for (auto& capacity : config.adapter_capacities) {
    if (capacity.send_capacity == 0)
      capacity.send_capacity = kDefaultAdapterSendCapacity;
    if (capacity.receive_capacity == 0)
      capacity.receive_capacity = kDefaultAdapterReceiveCapacity;
  }
  config.abi_revision = kRuntimeConfigAbiRevision;
  return config;
}

[[nodiscard]] constexpr bool ValidateRuntimeCapacities(
    const RuntimeConfig& config) noexcept {
  const auto valid_slots = [](std::uint32_t value) constexpr {
    return value >= 2 && value <= kMaximumRuntimeSlotCapacity &&
           (value & (value - 1U)) == 0;
  };
  if (config.abi_revision != kRuntimeConfigAbiRevision ||
      !valid_slots(config.order_capacity) ||
      !valid_slots(config.fill_dedup_capacity) ||
      !valid_slots(config.pending_event_capacity))
    return false;
  for (const auto& capacity : config.adapter_capacities) {
    if (!valid_slots(capacity.send_capacity) ||
        capacity.receive_capacity == 0 ||
        capacity.receive_capacity > kMaximumAdapterReceiveCapacity)
      return false;
  }
  return true;
}

enum class RuntimeCommandResultKind : std::uint8_t {
  Place = 1,
  Cancel = 2,
  RebindInstrument = 3,
};

struct RuntimeCommandResult {
  RuntimeCommandResultKind kind{RuntimeCommandResultKind::Place};
  Error error{Error::Ok};
  std::uint8_t reserved[6]{};
  union {
    CancelCommandCorrelation correlation;
    RebindPolymarketInstrumentResult rebind;
  };

  constexpr RuntimeCommandResult() noexcept : correlation{} {}
};

enum class RuntimeUpdateKind : std::uint8_t {
  Order = 1,
  Fill = 2,
  CommandResult = 3,
  Control = 4,
  OpenOrderSnapshot = 5,
  PositionSnapshot = 6,
  QueryComplete = 7,
};

enum class RuntimeControlKind : std::uint8_t {
  VenueStatus = 1,
  ReconcileComplete = 2,
};

struct RuntimeControlUpdate {
  RuntimeControlKind kind{RuntimeControlKind::VenueStatus};
  Error error{Error::Ok};
  std::uint8_t adapter_kind{};
  std::uint8_t adapter_status{};
  std::uint8_t reserved[4]{};
  std::uint64_t generation{};
  std::uint64_t event_time_ns{};
};

// Fixed fields avoid a variant discriminator/lifetime and keep ring slots
// directly writable. Only the field selected by kind is meaningful.
struct RuntimeUpdate {
  RuntimeUpdateKind kind{RuntimeUpdateKind::Order};
  std::uint8_t reserved[7]{};
  std::uint64_t published_time_ns{};
  OrderUpdate order{};
  FillUpdate fill{};
  RuntimeCommandResult command_result{};
  RuntimeControlUpdate control{};
  OpenOrderSnapshotItem open_order{};
  PositionSnapshotItem position{};
  QueryComplete query_complete{};
};

using ExecutionUpdate = RuntimeUpdate;

struct QueueMetrics {
  std::uint64_t capacity{};
  std::uint64_t depth{};
  std::uint64_t high_water{};
  std::uint64_t full_count{};
};

struct LatencyMetrics {
  std::uint64_t sample_count{};
  std::uint64_t total_ns{};
  std::uint64_t maximum_ns{};
  std::uint64_t latest_ns{};
};

struct DeadlineMetrics {
  std::uint64_t capacity{};
  std::uint64_t depth{};
  std::uint64_t high_water{};
  std::uint64_t capacity_exceeded_count{};
};

struct LaneMetrics {
  std::uint32_t lane_id{};
  std::uint32_t reserved{};
  QueueMetrics command_queue{};
  QueueMetrics update_queue{};
  LatencyMetrics enqueue_to_owner{};
  LatencyMetrics owner_to_drain{};
  std::uint64_t starvation_count{};
};

struct RuntimeMetrics {
  ExecutionMode mode{ExecutionMode::Inline};
  std::uint8_t reserved[3]{};
  std::uint32_t lane_count{};
  DeadlineMetrics deadlines{};
  std::uint64_t unmatched_venue_events{};
  std::uint64_t unmatched_fills{};
  LaneMetrics lanes[kMaxRuntimeLanes]{};
};

static_assert(std::is_trivially_copyable_v<ExecutionMode>);
static_assert(std::is_trivially_copyable_v<AdapterCapacitySlot>);
static_assert(std::is_trivially_copyable_v<LaneConfig>);
static_assert(std::is_trivially_copyable_v<AdapterCapacityConfig>);
static_assert(std::is_trivially_copyable_v<RuntimeConfig>);
static_assert(std::is_trivially_copyable_v<RuntimeCommandResultKind>);
static_assert(std::is_trivially_copyable_v<RuntimeCommandResult>);
static_assert(std::is_trivially_copyable_v<RuntimeUpdateKind>);
static_assert(std::is_trivially_copyable_v<RuntimeControlKind>);
static_assert(std::is_trivially_copyable_v<RuntimeControlUpdate>);
static_assert(std::is_trivially_copyable_v<RuntimeUpdate>);
static_assert(std::is_trivially_copyable_v<QueueMetrics>);
static_assert(std::is_trivially_copyable_v<LatencyMetrics>);
static_assert(std::is_trivially_copyable_v<DeadlineMetrics>);
static_assert(std::is_trivially_copyable_v<LaneMetrics>);
static_assert(std::is_trivially_copyable_v<RuntimeMetrics>);

static_assert(std::is_standard_layout_v<ExecutionMode>);
static_assert(std::is_standard_layout_v<AdapterCapacitySlot>);
static_assert(std::is_standard_layout_v<LaneConfig>);
static_assert(std::is_standard_layout_v<AdapterCapacityConfig>);
static_assert(std::is_standard_layout_v<RuntimeConfig>);
static_assert(std::is_standard_layout_v<RuntimeCommandResultKind>);
static_assert(std::is_standard_layout_v<RuntimeCommandResult>);
static_assert(std::is_standard_layout_v<RuntimeUpdateKind>);
static_assert(std::is_standard_layout_v<RuntimeControlKind>);
static_assert(std::is_standard_layout_v<RuntimeControlUpdate>);
static_assert(std::is_standard_layout_v<RuntimeUpdate>);
static_assert(std::is_standard_layout_v<QueueMetrics>);
static_assert(std::is_standard_layout_v<LatencyMetrics>);
static_assert(std::is_standard_layout_v<DeadlineMetrics>);
static_assert(std::is_standard_layout_v<LaneMetrics>);
static_assert(std::is_standard_layout_v<RuntimeMetrics>);

static_assert(sizeof(ExecutionMode) == 1);
static_assert(sizeof(AdapterCapacitySlot) == 1);
static_assert(sizeof(LaneConfig) == 16);
static_assert(sizeof(AdapterCapacityConfig) == 8);
static_assert(sizeof(RuntimeConfig) == 1088);
static_assert(sizeof(RuntimeCommandResultKind) == 1);
static_assert(sizeof(RuntimeCommandResult) == 40);
static_assert(sizeof(RuntimeUpdateKind) == 1);
static_assert(sizeof(RuntimeControlKind) == 1);
static_assert(sizeof(RuntimeControlUpdate) == 24);
static_assert(sizeof(QueueMetrics) == 32);
static_assert(sizeof(LatencyMetrics) == 32);
static_assert(sizeof(DeadlineMetrics) == 32);
static_assert(sizeof(LaneMetrics) == 144);
static_assert(sizeof(RuntimeMetrics) == 9272);

static_assert(static_cast<std::uint8_t>(ExecutionMode::Inline) == 0);
static_assert(static_cast<std::uint8_t>(ExecutionMode::DedicatedIo) == 1);
static_assert(static_cast<std::uint8_t>(AdapterCapacitySlot::BinanceSpot) == 0);
static_assert(static_cast<std::uint8_t>(AdapterCapacitySlot::BinanceUsdm) == 1);
static_assert(static_cast<std::uint8_t>(AdapterCapacitySlot::Polymarket) == 2);
static_assert(static_cast<std::uint8_t>(AdapterCapacitySlot::Fake) == 3);
static_assert(static_cast<std::uint8_t>(RuntimeCommandResultKind::Place) == 1);
static_assert(static_cast<std::uint8_t>(RuntimeCommandResultKind::Cancel) == 2);
static_assert(
    static_cast<std::uint8_t>(RuntimeCommandResultKind::RebindInstrument) == 3);
static_assert(static_cast<std::uint8_t>(RuntimeUpdateKind::Order) == 1);
static_assert(static_cast<std::uint8_t>(RuntimeUpdateKind::Fill) == 2);
static_assert(static_cast<std::uint8_t>(RuntimeUpdateKind::CommandResult) == 3);
static_assert(static_cast<std::uint8_t>(RuntimeUpdateKind::Control) == 4);
static_assert(static_cast<std::uint8_t>(RuntimeControlKind::VenueStatus) == 1);
static_assert(static_cast<std::uint8_t>(
                  RuntimeControlKind::ReconcileComplete) == 2);

}  // namespace oms::api
