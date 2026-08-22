#pragma once

#include "mds/api/mds_api.h"
#include "mds/exchange/capabilities.h"
#include "mds/exchange/venue_adapter.h"
#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"

#include <chrono>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <vector>

namespace mds::producer {

struct ProductDiscovery {
  std::vector<std::string> quote_assets;
  std::string symbol_regex;
  std::uint64_t minimum_turnover{};
  std::optional<std::size_t> max_symbols;
};

struct UniverseChange {
  std::vector<exchange::InstrumentMetadata> added;
  std::vector<exchange::InstrumentMetadata> retained;
  std::vector<exchange::InstrumentMetadata> retired;
};

struct PublicWsAuthEnv {
  std::string api_key_env;
  std::string secret_env;
  std::string passphrase_env;

  [[nodiscard]] bool configured() const noexcept {
    return !api_key_env.empty() && !secret_env.empty() &&
           !passphrase_env.empty();
  }
};

struct VenueEndpoint {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  std::string websocket_endpoint;
  std::string rest_endpoint;
  std::string discovery_endpoint;
  PublicWsAuthEnv auth;
  std::size_t max_symbols_per_connection{50};
  std::size_t max_symbols_per_ws{};
  std::uint32_t snapshot_pacing_ms{100};
  std::uint32_t snapshot_failure_backoff_ms{250};
  std::uint32_t snapshot_failure_backoff_max_ms{30'000};
  std::uint32_t snapshot_max_consecutive_failures{10};
  std::uint32_t snapshot_rate_limit_backoff_ms{60'000};
  std::uint32_t snapshot_ban_backoff_ms{300'000};
  std::uint32_t max_continuous_recovery_ms{300'000};
};

struct StreamSpec {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  std::string symbol;
  bool polymarket_rolling{};
  std::string polymarket_asset;
  std::string polymarket_period;
  std::string polymarket_outcome;
  std::uint32_t polymarket_prediscovery_seconds{30};
  std::uint32_t polymarket_rollover_grace_seconds{2};
  std::uint32_t polymarket_resolver_timeout_ms{3000};
  bool subscribe_ticker{};
  bool subscribe_orderbook{};
  std::string ticker_channel;
  std::string orderbook_channel;
  exchange::BookBootstrap orderbook_bootstrap{
      exchange::BookBootstrap::WsSnapshotThenDelta};
  std::size_t orderbook_depth{};
  std::size_t max_levels_per_message{};
  std::uint32_t effective_interval_ms{};
  bool orderbook_channel_override{};
  bool requires_public_ws_login{};
  std::size_t ladder_ticks_per_side{8192};
  std::size_t max_ladder_ticks_per_side{16384};
  std::uint32_t ladder_price_band_bps{10};
  std::size_t snapshot_depth{};
  std::string shm_prefix{"/selfquant.mds"};
  transport::RingOptions ring{};
  std::chrono::nanoseconds reader_lease_timeout{5'000'000'000};
  publish::RingLayout ring_layout{publish::RingLayout::PerSymbol};
  std::size_t shard_count{1};
  transport::RingOptions multiplex_ring{};
  std::optional<ProductDiscovery> discovery;
};

struct ConnectionSpec {
  VenueEndpoint endpoint;
  std::vector<StreamSpec> streams;
};

struct ProducerConfig {
  std::vector<ConnectionSpec> connections;
  bool unlink_on_shutdown{};
  std::size_t ring_count{};
  std::size_t instrument_count{};
  std::size_t max_instruments{4096};
  std::size_t max_rings{4096};
  std::uint64_t max_total_ring_bytes{16ULL << 30U};
};

[[nodiscard]] api::Result<ProducerConfig>
load_config(const std::string &path) noexcept;

[[nodiscard]] api::Result<UniverseChange> reconcile_universe(
    const ProductDiscovery &discovery,
    std::span<const exchange::InstrumentMetadata> metadata,
    std::span<const exchange::InstrumentMetadata> active) noexcept;

}  // namespace mds::producer
