#pragma once

#include "utils/md/types.h"
#include "utils/md/order_book.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace mds::api {

enum class ErrorCode : std::uint16_t {
  Ok = 0,
  AlreadyInitialized,
  NotInitialized,
  InvalidConfig,
  UnsupportedVenueProduct,
  InstrumentNotFound,
  ShmCreateFailed,
  HugepageUnavailable,
  TscUnstable,
  CpuBindFailed,
  QuotaExceeded,
  SubscriptionRejected,
  RecordOverwritten,
  InvalidHandle,
  InternalError,
};

template <typename T> struct Result {
  T value{};
  ErrorCode error{ErrorCode::Ok};
  std::string message{};
  [[nodiscard]] explicit operator bool() const noexcept {
    return error == ErrorCode::Ok;
  }
};

template <> struct Result<void> {
  ErrorCode error{ErrorCode::Ok};
  std::string message{};
  [[nodiscard]] explicit operator bool() const noexcept {
    return error == ErrorCode::Ok;
  }
};

using ProductType = utils::md::ProductType;
enum class WireProtocol : std::uint8_t { Json = 1, Sbe = 2 };
enum class RuntimeMode : std::uint8_t { Automatic, Manual };
enum class Strictness : std::uint8_t { Relaxed, Strict };
enum class ShmBackend : std::uint8_t { PosixShm, Hugetlbfs };
enum class RingMode : std::uint32_t { OverwriteOldest = 0, Lossless = 1 };
enum class SlowConsumerPolicy : std::uint8_t {
  Latest,
  Disconnect,
  Lossless
};
enum class SubscriptionState : std::uint8_t {
  Unknown,
  Pending,
  Live,
  Failed,
  Stopped
};

using SubscriptionHandle = std::uint64_t;

struct RuntimeConfig {
  RuntimeMode mode{RuntimeMode::Automatic};
  Strictness strictness{Strictness::Relaxed};
  std::vector<unsigned> cpu_ids{};
  int numa_node{-1};
  bool require_stable_tsc{false};
};

struct ShmConfig {
  ShmBackend backend{ShmBackend::PosixShm};
  RingMode mode{RingMode::OverwriteOldest};
  std::string hugetlbfs_mount{"/dev/hugepages"};
  std::size_t ring_bytes{8U << 20U};
  std::size_t max_record_bytes{64U << 10U};
  std::size_t max_readers{32};
  std::uint64_t reader_lease_timeout_ns{5'000'000'000ULL};
  bool allow_hugepage_fallback{false};
  bool unlink_on_shutdown{false};
};

struct VenueProfile {
  std::string venue{"binance"};
  ProductType product{ProductType::Spot};
  std::string websocket_endpoint{};
  std::string rest_endpoint{};
  WireProtocol protocol{WireProtocol::Json};
  std::uint32_t max_streams_per_connection{200};
  std::uint32_t messages_per_second{5};
  bool redundant_ab{false};
  bool allow_json_fallback{false};
};

struct MdsConfig {
  RuntimeConfig runtime{};
  ShmConfig shm{};
  std::vector<VenueProfile> venues{};
  std::size_t max_subscriptions{4096};
  std::uint32_t default_ladder_levels_per_side{8192};
  std::uint32_t max_ladder_levels_per_side{16384};
};

struct TickerSubscription {
  std::string venue{"binance"};
  ProductType product{ProductType::Spot};
  std::string symbol{};
};

struct OrderBookSubscription : TickerSubscription {
  std::uint32_t depth{1000};
  std::uint32_t ladder_levels_per_side{8192};
  SlowConsumerPolicy slow_consumer{SlowConsumerPolicy::Latest};
  std::uint32_t update_interval_ms{100};
};

Result<void> init(const MdsConfig &config);
Result<SubscriptionHandle> subticker(const TickerSubscription &subscription);
Result<SubscriptionHandle>
suborderbook(const OrderBookSubscription &subscription);
Result<void> unsubscribe(SubscriptionHandle handle);
SubscriptionState query_state(SubscriptionHandle handle) noexcept;
void shutdown() noexcept;

} // namespace mds::api
