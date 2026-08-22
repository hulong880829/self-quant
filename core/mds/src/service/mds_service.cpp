#include "mds/api/mds_api.h"
#include "mds/exchange/binance/binance_adapter.h"
#include "mds/exchange/capabilities.h"
#include "net/tls_websocket.h"
#include "mds/service/inprocess_aggregation.h"
#include "mds/service/venue_connection.h"
#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <iterator>
#include <memory>
#include <mutex>
#include <optional>
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

struct PendingSubscription {
  std::string venue;
  ProductType product{ProductType::Unknown};
  std::string symbol;
  bool ticker{};
  bool orderbook{};
  std::uint32_t depth{};
  std::uint32_t ladder_ticks{};
  std::uint32_t ladder_price_band_bps{10};
  std::uint32_t update_interval_ms{};
  std::string orderbook_channel;
};

struct PendingAggregate {
  AggregateSubscription subscription;
  mds::service::AggregateKind kind{mds::service::AggregateKind::Bbo};
};

struct ServiceState {
  std::mutex mutex;
  bool initialized{};
  bool started{};
  bool shutting_down{};
  MdsConfig config{};
  SubscriptionHandle next_handle{1};
  std::unordered_map<SubscriptionHandle, SubscriptionState> subscriptions{};
  std::unordered_map<SubscriptionHandle, PendingSubscription> pending{};
  std::unordered_map<SubscriptionHandle, PendingAggregate>
      pending_aggregates{};
  std::unordered_map<SubscriptionHandle, mds::service::VenueConnection *>
      subscription_sessions{};
  std::unique_ptr<mds::service::VenueConnectionManager> connection_manager{};
  std::vector<std::unique_ptr<mds::service::InProcessAggregation>>
      aggregations{};
  std::jthread worker{};
  net::SharedSslContext tls_context{};
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

const VenueProfile *profile_for(const MdsConfig &config,
                                std::string_view venue,
                                ProductType product) noexcept {
  for (const auto &profile : config.venues) {
    if (profile.venue == venue && profile.product == product) {
      return &profile;
    }
  }
  return nullptr;
}

std::string environment(std::string_view name) {
  if (name.empty()) {
    return {};
  }
  const std::string key(name);
  const char *value = std::getenv(key.c_str());
  return value == nullptr ? std::string{} : std::string(value);
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
  std::unordered_set<std::uint32_t> profiles;
  for (const auto &profile : config.venues) {
    const auto venue = exchange::parse_venue(profile.venue);
    if (!venue || exchange::capabilities(*venue, profile.product) == nullptr) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message = "unsupported venue/product profile"};
    }
    if (profile.max_streams_per_connection == 0 ||
        profile.messages_per_second == 0 ||
        profile.snapshot_pacing_ms == 0) {
      return {.error = ErrorCode::InvalidConfig,
              .message = "venue connection limits must be non-zero"};
    }
    const auto key =
        (static_cast<std::uint32_t>(*venue) << 8U) |
        static_cast<std::uint32_t>(profile.product);
    if (!profiles.insert(key).second) {
      return {.error = ErrorCode::InvalidConfig,
              .message = "duplicate venue/product profile"};
    }
    if (*venue != utils::md::Venue::Binance &&
        profile.protocol != WireProtocol::Json) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message = "SBE is only supported by Binance profiles"};
    }
    if (*venue == utils::md::Venue::Binance &&
        profile.protocol == WireProtocol::Sbe &&
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

Result<SubscriptionHandle>
subscribe(const TickerSubscription &subscription,
          const OrderBookSubscription *orderbook) {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return {.error = ErrorCode::NotInitialized,
            .message = "mds is not initialized"};
  }
  if (state.started) {
    return {.error = ErrorCode::AlreadyStarted,
            .message = "subscriptions cannot be added after start"};
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
  PendingSubscription pending;
  pending.venue = subscription.venue;
  pending.product = subscription.product;
  pending.symbol = subscription.symbol;
  pending.ticker = orderbook == nullptr;
  pending.orderbook = orderbook != nullptr;
  if (orderbook != nullptr) {
    pending.depth = orderbook->depth;
    pending.ladder_ticks = orderbook->ladder_levels_per_side;
    pending.ladder_price_band_bps = orderbook->ladder_price_band_bps;
    pending.update_interval_ms = orderbook->update_interval_ms;
    pending.orderbook_channel = orderbook->orderbook_channel;
  }
  state.subscriptions.emplace(handle, SubscriptionState::Pending);
  state.pending.emplace(handle, std::move(pending));
  return {.value = handle};
}

Result<SubscriptionHandle>
register_aggregate(const AggregateSubscription &subscription,
                   mds::service::AggregateKind kind) {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return {.error = ErrorCode::NotInitialized,
            .message = "mds is not initialized"};
  }
  if (state.started) {
    return {.error = ErrorCode::AlreadyStarted,
            .message = "aggregates cannot be added after start"};
  }
  if (subscription.symbol.empty() ||
      subscription.cross_skew_threshold_us == 0 ||
      subscription.fx_ttl_us == 0 ||
      subscription.fx_max_depeg_bps == 0 ||
      subscription.venues.size() < 2 ||
      subscription.venues.size() > mds::agg::kMaxMembers) {
    return {.error = ErrorCode::InvalidConfig,
            .message =
                "aggregate requires a symbol and 2-8 venues"};
  }
  std::unordered_set<utils::md::Venue> venues;
  for (const auto &name : subscription.venues) {
    const auto venue = exchange::parse_venue(name);
    if (!venue ||
        exchange::capabilities(*venue, subscription.product) == nullptr) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message = "aggregate venue/product is unsupported"};
    }
    const auto *capabilities =
        exchange::capabilities(*venue, subscription.product);
    const auto cadence_ms =
        kind == mds::service::AggregateKind::Bbo
            ? (capabilities->ticker.interval_ms != 0
                   ? capabilities->ticker.interval_ms
                   : capabilities->fastest_top10.interval_ms)
            : capabilities->fastest_top10.interval_ms;
    if (subscription.ttl_us == 0 && cadence_ms == 0) {
      return {.error = ErrorCode::InvalidConfig,
              .message =
                  "aggregate TTL cannot be derived from venue capability"};
    }
    if (!venues.insert(*venue).second) {
      return {.error = ErrorCode::InvalidConfig,
              .message = "aggregate venues contain a duplicate"};
    }
    const bool configured = std::any_of(
        state.config.venues.begin(), state.config.venues.end(),
        [&](const VenueProfile &profile) {
          const auto configured_venue =
              exchange::parse_venue(profile.venue);
          return configured_venue && *configured_venue == *venue &&
                 profile.product == subscription.product;
        });
    if (!configured) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message = "aggregate venue profile was not configured"};
    }
  }
  if (state.subscriptions.size() >= state.config.max_subscriptions) {
    return {.error = ErrorCode::QuotaExceeded,
            .message = "subscription limit reached"};
  }
  const auto handle = state.next_handle++;
  state.subscriptions.emplace(handle, SubscriptionState::Pending);
  state.pending_aggregates.emplace(
      handle, PendingAggregate{subscription, kind});
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
  auto tls_context = net::make_client_ssl_context(tls_error);
  if (!tls_context) {
    return {.error = ErrorCode::InternalError,
            .message = std::move(tls_error)};
  }
  state.config = config;
  state.next_handle = 1;
  state.started = false;
  state.tls_context = std::move(tls_context);
  state.connection_manager =
      std::make_unique<mds::service::VenueConnectionManager>(
          state.tls_context);
  state.initialized = true;
  return {};
}

Result<SubscriptionHandle>
subticker(const TickerSubscription &subscription) {
  return subscribe(subscription, nullptr);
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
  return subscribe(subscription, &subscription);
}

Result<SubscriptionHandle>
register_agg_bbo(const AggregateSubscription &subscription) {
  return register_aggregate(subscription,
                            mds::service::AggregateKind::Bbo);
}

Result<SubscriptionHandle>
register_agg_orderbook(const AggregateSubscription &subscription) {
  return register_aggregate(subscription,
                            mds::service::AggregateKind::OrderBook);
}

Result<void> start() {
  struct ConnectionBuild {
    mds::service::VenueConnectionOptions options;
    std::vector<SubscriptionHandle> handles;
  };

  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return {.error = ErrorCode::NotInitialized};
  }
  if (state.started) {
    return {.error = ErrorCode::AlreadyStarted,
            .message = "mds has already started"};
  }
  if (state.pending.empty() && state.pending_aggregates.empty()) {
    return {.error = ErrorCode::InvalidConfig,
            .message = "no subscriptions were registered"};
  }

  std::vector<ConnectionBuild> builds;
  for (const auto &[handle, pending] : state.pending) {
    const auto venue = exchange::parse_venue(pending.venue);
    const auto *profile =
        profile_for(state.config, pending.venue, pending.product);
    if (!venue || profile == nullptr) {
      return {.error = ErrorCode::UnsupportedVenueProduct,
              .message = "subscription profile is unavailable"};
    }
    auto group = std::find_if(
        builds.begin(), builds.end(), [&](const ConnectionBuild &entry) {
          return entry.options.venue == *venue &&
                 entry.options.product == pending.product;
        });
    if (group == builds.end()) {
      ConnectionBuild created;
      created.options.venue = *venue;
      created.options.product = pending.product;
      created.options.websocket_endpoint = profile->websocket_endpoint;
      if (*venue == utils::md::Venue::Binance) {
        const auto scheme = created.options.websocket_endpoint.find("://");
        const auto path = created.options.websocket_endpoint.find(
            '/', scheme == std::string::npos ? 0 : scheme + 3);
        if (path == std::string::npos) {
          created.options.websocket_endpoint.append("/ws");
        }
      }
      created.options.rest_endpoint = profile->rest_endpoint;
      created.options.snapshot_pacing_ms = profile->snapshot_pacing_ms;
      created.options.credentials.api_key =
          environment(profile->api_key_env);
      created.options.credentials.secret =
          environment(profile->secret_env);
      created.options.credentials.passphrase =
          environment(profile->passphrase_env);
      builds.push_back(std::move(created));
      group = std::prev(builds.end());
    }

    auto stream = std::find_if(
        group->options.streams.begin(), group->options.streams.end(),
        [&](const mds::service::SymbolStreamOptions &entry) {
          return entry.symbol == pending.symbol;
        });
    if (stream == group->options.streams.end()) {
      mds::service::SymbolStreamOptions created;
      created.symbol = pending.symbol;
      created.shm_prefix = profile->shm_prefix;
      created.ring.backend = state.config.shm.backend;
      created.ring.mode = state.config.shm.mode;
      created.ring.hugetlbfs_mount =
          state.config.shm.hugetlbfs_mount;
      created.ring.ring_bytes = state.config.shm.ring_bytes;
      created.ring.max_record_bytes =
          state.config.shm.max_record_bytes;
      created.ring.max_readers = state.config.shm.max_readers;
      created.ring.allow_hugepage_fallback =
          state.config.shm.allow_hugepage_fallback;
      created.ring.unlink_on_close =
          state.config.shm.unlink_on_shutdown;
      created.reader_lease_timeout = std::chrono::nanoseconds(
          state.config.shm.reader_lease_timeout_ns);
      group->options.streams.push_back(std::move(created));
      stream = std::prev(group->options.streams.end());
    }

    const auto *caps = exchange::capabilities(*venue, pending.product);
    if (caps == nullptr) {
      return {.error = ErrorCode::UnsupportedVenueProduct};
    }
    if (pending.ticker) {
      stream->ticker = true;
      stream->ticker_channel = std::string(caps->ticker.channel);
    }
    if (pending.orderbook) {
      exchange::ResolvedOrderBookChannel resolved;
      std::string error;
      const std::optional<std::uint32_t> interval =
          pending.update_interval_ms == 0
              ? std::nullopt
              : std::optional<std::uint32_t>(
                    pending.update_interval_ms);
      if (!exchange::resolve_orderbook_channel(
              *venue, pending.product, pending.orderbook_channel,
              interval, resolved, error)) {
        return {.error = ErrorCode::InvalidConfig,
                .message = std::move(error)};
      }
      stream->orderbook = true;
      stream->orderbook_channel =
          std::string(resolved.capability.channel);
      stream->orderbook_bootstrap = resolved.capability.bootstrap;
      stream->update_interval_ms = resolved.capability.interval_ms;
      stream->snapshot_depth = pending.depth;
      stream->max_levels_per_message =
          std::max({std::size_t{100},
                    static_cast<std::size_t>(pending.depth),
                    resolved.capability.max_levels_per_message});
      stream->ladder_ticks_per_side = pending.ladder_ticks;
      stream->ladder_price_band_bps =
          pending.ladder_price_band_bps;
    }
    group->handles.push_back(handle);
  }

  for (auto &build : builds) {
    const auto *profile = profile_for(
        state.config, exchange::venue_name(build.options.venue),
        build.options.product);
    if (profile != nullptr &&
        build.options.streams.size() >
            profile->max_streams_per_connection) {
      return {.error = ErrorCode::QuotaExceeded,
              .message = "venue connection stream limit exceeded"};
    }
    auto created =
        state.connection_manager->create(std::move(build.options), true);
    if (!created) {
      for (const auto handle : build.handles) {
        state.subscriptions[handle] = SubscriptionState::Failed;
      }
      state.started = true;
      return {.error = created.error,
              .message = std::move(created.message)};
    }
    for (const auto handle : build.handles) {
      state.subscription_sessions.emplace(handle, created.value);
    }
  }

  for (auto &[handle, pending] : state.pending_aggregates) {
    auto runtime =
        std::make_unique<mds::service::InProcessAggregation>(
            handle, pending.kind, std::move(pending.subscription),
            state.config);
    auto opened = runtime->start();
    if (!opened) {
      state.subscriptions[handle] = SubscriptionState::Failed;
      state.started = true;
      state.pending.clear();
      state.pending_aggregates.clear();
      return opened;
    }
    state.aggregations.push_back(std::move(runtime));
  }
  state.started = true;
  state.pending.clear();
  state.pending_aggregates.clear();
  auto *manager = state.connection_manager.get();
  auto *service_state = &state;
  state.worker = std::jthread([manager, service_state](std::stop_token stop) {
    while (!stop.stop_requested()) {
      bool has_aggregations{};
      {
        std::lock_guard lock(service_state->mutex);
        has_aggregations = !service_state->aggregations.empty();
      }
      (void)manager->run_once(has_aggregations ? 0 : 25);
      bool handled{};
      if (has_aggregations) {
        const auto now = utils::runtime::Timestamp::NowMono();
        std::lock_guard lock(service_state->mutex);
        for (auto &runtime : service_state->aggregations) {
          handled = runtime->poll(now) || handled;
          service_state->subscriptions[runtime->handle()] =
              runtime->state();
        }
      }
      if (has_aggregations && !handled) {
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
      }
    }
  });
  return {};
}

Result<void> unsubscribe(SubscriptionHandle handle) {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return {.error = ErrorCode::NotInitialized};
  }
  if (state.started) {
    return {.error = ErrorCode::AlreadyStarted,
            .message = "running subscriptions cannot be removed"};
  }
  const auto found = state.subscriptions.find(handle);
  if (found == state.subscriptions.end()) {
    return {.error = ErrorCode::InvalidHandle};
  }
  found->second = SubscriptionState::Stopped;
  state.pending.erase(handle);
  state.pending_aggregates.erase(handle);
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
  case mds::service::MarketDataState::Live:
    return SubscriptionState::Live;
  case mds::service::MarketDataState::Failed:
    return SubscriptionState::Failed;
  case mds::service::MarketDataState::Stopped:
  default:
    return SubscriptionState::Pending;
  }
}

ErrorCode try_read_agg_bbo(SubscriptionHandle handle,
                           AggBboRecord &record) noexcept {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return ErrorCode::NotInitialized;
  }
  for (const auto &runtime : state.aggregations) {
    if (runtime->handle() == handle) {
      return runtime->read(record);
    }
  }
  const auto pending = state.pending_aggregates.find(handle);
  if (pending != state.pending_aggregates.end()) {
    return pending->second.kind == mds::service::AggregateKind::Bbo
               ? ErrorCode::AggregateNotReady
               : ErrorCode::SubscriptionTypeMismatch;
  }
  return state.subscriptions.contains(handle)
             ? ErrorCode::SubscriptionTypeMismatch
             : ErrorCode::InvalidHandle;
}

ErrorCode try_read_agg_orderbook(SubscriptionHandle handle,
                                 AggOrderBookRecord &record) noexcept {
  auto &state = service();
  std::lock_guard lock(state.mutex);
  if (!state.initialized) {
    return ErrorCode::NotInitialized;
  }
  for (const auto &runtime : state.aggregations) {
    if (runtime->handle() == handle) {
      return runtime->read(record);
    }
  }
  const auto pending = state.pending_aggregates.find(handle);
  if (pending != state.pending_aggregates.end()) {
    return pending->second.kind ==
                   mds::service::AggregateKind::OrderBook
               ? ErrorCode::AggregateNotReady
               : ErrorCode::SubscriptionTypeMismatch;
  }
  return state.subscriptions.contains(handle)
             ? ErrorCode::SubscriptionTypeMismatch
             : ErrorCode::InvalidHandle;
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
    state.pending.clear();
    state.pending_aggregates.clear();
    state.subscription_sessions.clear();
    for (auto &runtime : state.aggregations) {
      runtime->stop();
    }
    state.aggregations.clear();
    if (state.connection_manager) {
      state.connection_manager->stop();
    }
    state.connection_manager.reset();
    state.tls_context.reset();
    state.started = false;
    state.shutting_down = false;
  }
}

} // namespace mds::api
