#include "mds/api/mds_api.h"
#include "mds/exchange/binance/binance_adapter.h"
#include "mds/network/epoll_loop.h"
#include "mds/service/binance_session.h"
#include "mds/network/tls_websocket.h"

#include <chrono>
#include <memory>
#include <mutex>
#include <stop_token>
#include <thread>
#include <unordered_map>
#include <unordered_set>
#include <unistd.h>

#if defined(__x86_64__) || defined(__i386__)
#include <cpuid.h>
#endif

namespace mds::api {
namespace {

struct ServiceState {
  std::mutex mutex;
  bool initialized{};
  bool shutting_down{};
  MdsConfig config{};
  SubscriptionHandle next_handle{1};
  std::unordered_map<SubscriptionHandle, SubscriptionState> subscriptions{};
  std::unordered_map<SubscriptionHandle, mds::service::BinanceSession *>
      subscription_sessions{};
  std::unique_ptr<mds::service::SessionManager> session_manager{};
  std::jthread worker{};
  network::SharedSslContext tls_context{};
};

ServiceState &service() {
  static ServiceState state;
  return state;
}

bool has_profile(const MdsConfig &config, std::string_view venue,
                 ProductType product) {
  for (const auto &profile : config.venues) {
    if (profile.venue == venue && profile.product == product) {
      return true;
    }
  }
  return false;
}

bool stable_tsc_available() noexcept {
#if defined(__x86_64__) || defined(__i386__)
  if (__get_cpuid_max(0x80000000U, nullptr) < 0x80000007U) {
    return false;
  }
  unsigned eax{}, ebx{}, ecx{}, edx{};
  return __get_cpuid(0x80000007U, &eax, &ebx, &ecx, &edx) != 0 &&
         (edx & (1U << 8U)) != 0;
#else
  return false;
#endif
}

Result<void> validate_config(const MdsConfig &config) {
  if (config.max_subscriptions == 0 || config.shm.ring_bytes == 0 ||
      config.shm.max_record_bytes == 0 || config.shm.max_readers == 0 ||
      config.shm.reader_lease_timeout_ns == 0 ||
      config.shm.max_record_bytes > config.shm.ring_bytes / 8U ||
      (config.shm.ring_bytes & (config.shm.ring_bytes - 1U)) != 0 ||
      config.default_ladder_levels_per_side == 0 ||
      config.default_ladder_levels_per_side >
          config.max_ladder_levels_per_side ||
      config.max_ladder_levels_per_side > utils::md::kMaxLadderLevels) {
    return {.error = ErrorCode::InvalidConfig,
            .message = "invalid limits or shared ring geometry"};
  }
  constexpr std::size_t kLevelsPerChunk = 24;
  constexpr std::size_t kSnapshotChunkBytes = 496;
  const std::size_t chunks_per_side =
      (config.max_ladder_levels_per_side + kLevelsPerChunk - 1U) /
      kLevelsPerChunk;
  const std::size_t snapshot_burst_bytes =
      2U * chunks_per_side * kSnapshotChunkBytes;
  if (snapshot_burst_bytes > config.shm.ring_bytes / 4U) {
    return {.error = ErrorCode::InvalidConfig,
            .message =
                "full order-book snapshot burst exceeds 25% of ring_bytes"};
  }
  if (config.runtime.numa_node < -1 ||
      (config.runtime.mode == RuntimeMode::Manual &&
       config.runtime.cpu_ids.empty())) {
    return {.error = ErrorCode::InvalidConfig,
            .message = "invalid runtime placement configuration"};
  }
  const long online_cpus = ::sysconf(_SC_NPROCESSORS_ONLN);
  std::unordered_set<unsigned> cpu_ids;
  for (const unsigned cpu : config.runtime.cpu_ids) {
    if (online_cpus <= 0 || cpu >= static_cast<unsigned>(online_cpus) ||
        !cpu_ids.insert(cpu).second) {
      return {.error = ErrorCode::InvalidConfig,
              .message = "runtime CPU id is unavailable or duplicated"};
    }
  }
  if (config.runtime.require_stable_tsc && !stable_tsc_available()) {
    return {.error = ErrorCode::TscUnstable,
            .message = "invariant TSC is unavailable"};
  }
  std::unordered_set<unsigned> profiles;
  for (const auto &profile : config.venues) {
    if (profile.venue != "binance" ||
        (profile.product != ProductType::Spot &&
         profile.product != ProductType::Perpetual)) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message = "unsupported venue/product profile"};
    }
    if (profile.max_streams_per_connection == 0 ||
        profile.messages_per_second == 0) {
      return {.error = ErrorCode::InvalidConfig,
              .message = "venue connection limits must be non-zero"};
    }
    const unsigned key = static_cast<unsigned>(profile.product);
    if (!profiles.insert(key).second) {
      return {.error = ErrorCode::InvalidConfig,
              .message = "duplicate venue/product profile"};
    }
    if (profile.protocol == WireProtocol::Sbe &&
        exchange::binance::capability(
            profile.product == ProductType::Spot
                ? exchange::binance::Profile::Spot
                : exchange::binance::Profile::UsdM)
                .sbe != exchange::binance::Availability::Available) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message =
                  "Binance SBE is unavailable without official generated "
                  "stream_1_0 codecs"};
    }
  }
  return {};
}

template <typename Subscription>
Result<SubscriptionHandle> subscribe(const Subscription &subscription) {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return {.error = ErrorCode::NotInitialized,
            .message = "mds is not initialized"};
  }
  if (subscription.symbol.empty()) {
    return {.error = ErrorCode::InvalidConfig,
            .message = "subscription symbol is empty"};
  }
  if (!has_profile(state.config, subscription.venue,
                   subscription.product)) {
    return {.error = ErrorCode::UnsupportedVenueProduct,
            .message = "venue profile was not configured"};
  }
  if (state.subscriptions.size() >= state.config.max_subscriptions) {
    return {.error = ErrorCode::QuotaExceeded,
            .message = "subscription limit reached"};
  }
  const auto handle = state.next_handle++;
  const auto profile = subscription.product == ProductType::Spot
                           ? exchange::binance::Profile::Spot
                           : exchange::binance::Profile::UsdM;
  mds::service::BinanceSessionOptions options;
  options.profile = profile;
  options.symbol = subscription.symbol;
  options.publish = true;
  options.ladder_levels_per_side = state.config.max_ladder_levels_per_side;
  options.max_ladder_levels_per_side =
      state.config.max_ladder_levels_per_side;
  options.reader_lease_timeout =
      std::chrono::nanoseconds(state.config.shm.reader_lease_timeout_ns);
  options.ring.backend = state.config.shm.backend;
  options.ring.mode = state.config.shm.mode;
  options.ring.hugetlbfs_mount = state.config.shm.hugetlbfs_mount;
  options.ring.ring_bytes = state.config.shm.ring_bytes;
  options.ring.max_record_bytes = state.config.shm.max_record_bytes;
  options.ring.max_readers = state.config.shm.max_readers;
  options.ring.allow_hugepage_fallback =
      state.config.shm.allow_hugepage_fallback;
  options.ring.unlink_on_close = state.config.shm.unlink_on_shutdown;
  for (const auto &venue : state.config.venues) {
    if (venue.venue == subscription.venue &&
        venue.product == subscription.product) {
      options.websocket_endpoint = venue.websocket_endpoint;
      options.rest_endpoint = venue.rest_endpoint;
      break;
    }
  }
  auto session = state.session_manager->create(std::move(options), true);
  if (!session) {
    return {.error = session.error, .message = std::move(session.message)};
  }
  state.subscriptions.emplace(handle, SubscriptionState::Pending);
  state.subscription_sessions.emplace(handle, session.value);
  return {.value = handle};
}

} // namespace

Result<void> init(const MdsConfig &config) {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (state.initialized || state.shutting_down) {
    return {.error = ErrorCode::AlreadyInitialized,
            .message = "mds is already initialized"};
  }
  if (auto validated = validate_config(config); !validated) {
    return validated;
  }
  std::string tls_error;
  auto tls_context = network::make_client_ssl_context(tls_error);
  if (!tls_context) {
    return {.error = ErrorCode::InternalError,
            .message = std::move(tls_error)};
  }
  state.config = config;
  state.next_handle = 1;
  state.tls_context = std::move(tls_context);
  state.session_manager =
      std::make_unique<mds::service::SessionManager>(state.tls_context);
  state.initialized = true;
  auto *manager = state.session_manager.get();
  state.worker = std::jthread([manager](std::stop_token stop) {
    while (!stop.stop_requested()) {
      (void)manager->run_once(25);
    }
  });
  return {};
}

Result<SubscriptionHandle>
subticker(const TickerSubscription &subscription) {
  return subscribe(subscription);
}

Result<SubscriptionHandle>
suborderbook(const OrderBookSubscription &subscription) {
  std::uint32_t configured_max =
      static_cast<std::uint32_t>(utils::md::kMaxLadderLevels);
  {
    auto &state = service();
    std::lock_guard lock(state.mutex);
    if (state.initialized) {
      configured_max = state.config.max_ladder_levels_per_side;
    }
  }
  if (subscription.depth == 0 || subscription.ladder_levels_per_side == 0 ||
      subscription.ladder_levels_per_side > configured_max) {
    return {.error = ErrorCode::InvalidConfig,
            .message = "depth or ladder capacity is outside supported bounds"};
  }
  return subscribe(subscription);
}

Result<void> unsubscribe(SubscriptionHandle handle) {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return {.error = ErrorCode::NotInitialized};
  }
  const auto found = state.subscriptions.find(handle);
  if (found == state.subscriptions.end()) {
    return {.error = ErrorCode::InvalidHandle};
  }
  found->second = SubscriptionState::Stopped;
  state.subscription_sessions.erase(handle);
  return {};
}

SubscriptionState query_state(SubscriptionHandle handle) noexcept {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  const auto found = state.subscriptions.find(handle);
  if (found == state.subscriptions.end()) {
    return SubscriptionState::Unknown;
  }
  if (found->second == SubscriptionState::Stopped) {
    return SubscriptionState::Stopped;
  }
  const auto session = state.subscription_sessions.find(handle);
  if (session == state.subscription_sessions.end() || session->second == nullptr) {
    return found->second;
  }
  switch (session->second->state()) {
  case mds::service::BinanceSessionState::Live:
    return SubscriptionState::Live;
  case mds::service::BinanceSessionState::Failed:
    return SubscriptionState::Failed;
  case mds::service::BinanceSessionState::Stopped:
  default:
    return SubscriptionState::Pending;
  }
}

void shutdown() noexcept {
  auto &state = service();
  std::jthread worker;
  {
    std::lock_guard lock(state.mutex);
    if (!state.initialized || state.shutting_down) {
      return;
    }
    state.shutting_down = true;
    state.initialized = false;
    for (auto &[handle, subscription] : state.subscriptions) {
      (void)handle;
      subscription = SubscriptionState::Stopped;
    }
    worker = std::move(state.worker);
  }
  worker.request_stop();
  if (worker.joinable()) {
    worker.join();
  }
  {
    std::lock_guard lock(state.mutex);
    state.subscriptions.clear();
    state.subscription_sessions.clear();
    if (state.session_manager) {
      state.session_manager->stop();
    }
    state.session_manager.reset();
    state.tls_context.reset();
    state.shutting_down = false;
  }
}

} // namespace mds::api
