#pragma once

#include "utils/md/types.h"
#include "utils/md/order_book.h"
#include "utils/md/wire.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace mds::api {

enum class ErrorCode : std::uint16_t {
  Ok = 0,
  AlreadyInitialized = 1,
  NotInitialized = 2,
  InvalidConfig = 3,
  UnsupportedVenueProduct = 4,
  InstrumentNotFound = 5,
  ShmCreateFailed = 6,
  HugepageUnavailable = 7,
  TscUnstable = 8,
  CpuBindFailed = 9,
  QuotaExceeded = 10,
  SubscriptionRejected = 11,
  RecordOverwritten = 12,
  InvalidHandle = 13,
  InternalError = 14,
  AlreadyStarted = 15,
  AggregateNotReady = 16,
  SubscriptionTypeMismatch = 17,
  InstrumentMismatch = 18,
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
  std::string api_key_env{};
  std::string secret_env{};
  std::string passphrase_env{};
  std::string shm_prefix{"/selfquant.mds"};
  WireProtocol protocol{WireProtocol::Json};
  std::uint32_t max_streams_per_connection{200};
  std::uint32_t messages_per_second{5};
  std::uint32_t snapshot_pacing_ms{100};
  bool redundant_ab{false};
  bool allow_json_fallback{false};
};

struct MdsConfig {
  RuntimeConfig runtime{};
  ShmConfig shm{};
  std::vector<VenueProfile> venues{};
  std::size_t max_subscriptions{4096};
  // Deprecated field names retained for source compatibility. Values are
  // price-tick window widths, not populated order-book level counts.
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
  // Deprecated field name retained for ABI/source compatibility; unit=ticks.
  std::uint32_t ladder_levels_per_side{8192};
  SlowConsumerPolicy slow_consumer{SlowConsumerPolicy::Latest};
  // Zero selects the venue/channel default. Non-zero is only accepted when
  // the selected native channel supports a configurable interval.
  std::uint32_t update_interval_ms{};
  std::string orderbook_channel{};
  std::uint32_t ladder_price_band_bps{10};
};

struct AggregateSubscription {
  std::vector<std::string> venues{};
  ProductType product{ProductType::Spot};
  std::string symbol{};
  // Zero derives a per-member TTL from the selected venue capability.
  std::uint64_t ttl_us{};
  bool cross_skew_observe_only{true};
  std::uint32_t cross_skew_threshold_us{50'000};
  std::uint64_t fx_ttl_us{100'000};
  std::uint32_t fx_max_depeg_bps{200};
};

using AggBboRecord = utils::md::wire::AggBboRecord;
using AggOrderBookRecord = utils::md::wire::AggOrderBookRecord;

Result<void> init(const MdsConfig &config);
Result<SubscriptionHandle> subticker(const TickerSubscription &subscription);
Result<SubscriptionHandle>
suborderbook(const OrderBookSubscription &subscription);
Result<SubscriptionHandle>
register_agg_bbo(const AggregateSubscription &subscription);
Result<SubscriptionHandle>
register_agg_orderbook(const AggregateSubscription &subscription);
Result<void> start();
Result<void> unsubscribe(SubscriptionHandle handle);
SubscriptionState query_state(SubscriptionHandle handle) noexcept;
// Copies the newest in-process aggregate image. Successful calls do not
// allocate; AggregateNotReady means no complete image has been built yet.
ErrorCode try_read_agg_bbo(SubscriptionHandle handle,
                           AggBboRecord &record) noexcept;
ErrorCode try_read_agg_orderbook(SubscriptionHandle handle,
                                 AggOrderBookRecord &record) noexcept;
void shutdown() noexcept;

} // namespace mds::api
