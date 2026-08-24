#pragma once

#include "mds/exchange/venue_adapter.h"
#include "mds/instrument/instrument_manager.h"
#include "mds/publish/wire_publisher.h"
#include "net/epoll_loop.h"
#include "net/tls_websocket.h"
#include "mds/service/market_data_session.h"
#include "mds/transport/shared_ring.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::service {

struct PublicWsCredentials {
  std::string api_key;
  std::string secret;
  std::string passphrase;

  [[nodiscard]] bool complete() const noexcept {
    return !api_key.empty() && !secret.empty() && !passphrase.empty();
  }
};

struct SymbolStreamOptions {
  std::string symbol;
  bool polymarket_rolling{};
  std::string polymarket_asset;
  std::string polymarket_period;
  std::string polymarket_outcome;
  std::uint32_t polymarket_prediscovery_seconds{30};
  std::uint32_t polymarket_rollover_grace_seconds{2};
  std::uint32_t polymarket_resolver_timeout_ms{3000};
  std::string ticker_channel;
  std::string orderbook_channel;
  exchange::BookBootstrap orderbook_bootstrap{
      exchange::BookBootstrap::WsSnapshotThenDelta};
  std::string shm_prefix{"/selfquant.mds"};
  std::size_t ladder_ticks_per_side{8192};
  std::uint32_t ladder_price_band_bps{10};
  std::size_t snapshot_depth{1000};
  std::size_t max_levels_per_message{1000};
  std::uint32_t update_interval_ms{};
  bool ticker{};
  bool orderbook{};
  transport::RingOptions ring{};
  publish::RingLayout ring_layout{publish::RingLayout::PerSymbol};
  std::size_t shard_count{1};
  transport::RingOptions multiplex_ring{};
  std::chrono::nanoseconds reader_lease_timeout{5'000'000'000};
};

struct VenueConnectionOptions {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  std::string websocket_endpoint;
  std::string rest_endpoint;
  std::string discovery_endpoint;
  PublicWsCredentials credentials;
  std::vector<SymbolStreamOptions> streams;
  std::shared_ptr<instrument::InstrumentManager> instrument_manager;
  std::size_t max_symbols_per_ws{};
  std::uint32_t snapshot_pacing_ms{100};
  std::uint32_t snapshot_failure_backoff_ms{250};
  std::uint32_t snapshot_failure_backoff_max_ms{30'000};
  std::uint32_t snapshot_max_consecutive_failures{10};
  std::uint32_t snapshot_rate_limit_backoff_ms{60'000};
  std::uint32_t snapshot_ban_backoff_ms{300'000};
  std::chrono::milliseconds connect_timeout{10'000};
  std::chrono::milliseconds request_timeout{10'000};
  std::chrono::milliseconds recovery_deadline{30'000};
  std::chrono::milliseconds idle_timeout{60'000};
  std::chrono::milliseconds reconnect_base{250};
  std::chrono::milliseconds reconnect_max{30'000};
  std::chrono::milliseconds max_continuous_recovery_duration{300'000};
};

[[nodiscard]] std::vector<std::vector<std::size_t>>
PartitionWebSocketSymbols(
    std::span<const SymbolStreamOptions> streams,
    std::size_t max_symbols_per_ws, utils::md::Venue venue);

class VenueConnection final : public MarketDataSession {
 public:
  VenueConnection(net::EpollLoop &loop, net::SharedSslContext tls,
                  VenueConnectionOptions options);
  ~VenueConnection() override;
  VenueConnection(const VenueConnection &) = delete;
  VenueConnection &operator=(const VenueConnection &) = delete;

  api::Result<void> start(Clock::time_point now = Clock::now()) override;
  void tick(Clock::time_point now = Clock::now()) noexcept override;
  void stop() noexcept override;
  [[nodiscard]] MarketDataState state() const noexcept override;
  [[nodiscard]] const MarketDataMetrics &metrics() const noexcept override;
  [[nodiscard]] std::string_view error_message() const noexcept override;

  [[nodiscard]] utils::md::Venue venue() const noexcept;
  [[nodiscard]] utils::md::ProductType product() const noexcept;
  [[nodiscard]] std::span<const SymbolStreamOptions> streams() const noexcept;
  [[nodiscard]] std::size_t websocket_shard_count() const noexcept;
  [[nodiscard]] std::optional<std::size_t>
  websocket_shard(std::string_view canonical_symbol) const noexcept;

 private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

class VenueConnectionManager {
 public:
  VenueConnectionManager();
  explicit VenueConnectionManager(net::SharedSslContext tls);

  api::Result<VenueConnection *>
  create(VenueConnectionOptions options, bool start_immediately = true);
  int run_once(int timeout_ms);
  void stop() noexcept;

 private:
  net::EpollLoop loop_;
  net::SharedSslContext tls_;
  std::shared_ptr<instrument::InstrumentManager> instrument_manager_{
      std::make_shared<instrument::InstrumentManager>()};
  std::vector<std::unique_ptr<VenueConnection>> connections_;
};

}  // namespace mds::service
