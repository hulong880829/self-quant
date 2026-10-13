#pragma once

#include "fairprice/engine.h"
#include "fairprice/market_data_repository.h"

#include "mds/api/mds_api.h"
#include "mds/transport/shared_ring.h"

#include <cstdint>
#include <string>
#include <vector>

namespace fairprice {

struct WebSocketConfig {
  std::uint64_t handshake_timeout_ms{5000};
  std::size_t read_limit_bytes{4096};
  std::size_t write_buffer_bytes{65536};
  std::uint64_t pong_timeout_ms{45000};
};

struct PersistenceConfig {
  bool enabled{true};
  std::string database_url;
  std::uint64_t sample_interval_ms{1000};
  std::uint32_t retention_hours{24};
  std::uint64_t cleanup_interval_ms{300000};
  int delete_batch{10000};
  int delete_max_batches{20};
  std::uint64_t operation_timeout_ms{5000};
};

struct GatewayBootstrapConfig {
  std::string url;
  std::string token;
  std::uint64_t timeout_ms{5000};
};

struct MarketDataConfig {
  std::uint64_t sample_interval_ms{};
  std::uint32_t depth{};
  std::uint64_t snapshot_stale_after_ms{15000};
  std::uint64_t catalog_refresh_interval_ms{1000};
  std::uint32_t retention_hours{24};
  ClickHouseReadConfig clickhouse;
  GatewayBootstrapConfig gateway_bootstrap;
};

struct Config {
  int version{2};
  std::string log_level{"info"};
  bool development{true};

  MarketDataConfig market_data{};
  std::uint64_t snapshot_stale_after_ms{15000};
  std::uint64_t catalog_refresh_interval_ms{1000};

  std::string shm_backend{"posix_shm"};
  std::string shm_prefix{"/selfquant.mds"};
  mds::transport::RingOptions output_ring{};
  std::uint64_t reader_lease_timeout_ms{5000};

  std::string listen_address{"127.0.0.1"};
  std::uint16_t port{9093};
  std::vector<std::string> allowed_origins{};
  std::string browser_token;
  int max_clients{128};
  int max_subscriptions_per_client{16};
  std::uint64_t read_header_timeout_ms{5000};
  std::uint64_t idle_timeout_ms{60000};
  std::size_t max_header_bytes{16384};
  WebSocketConfig websocket{};

  std::uint64_t stream_interval_ms{50};
  int history_workers{1};
  std::uint64_t history_resolution_ms{60000};

  FairPriceConfig fair_price{};
  std::uint64_t fair_stream_interval_ms{5};
  std::uint64_t fair_heartbeat_ms{1000};
  PersistenceConfig persistence{};
};

mds::api::Result<Config> load_config(const std::string &path) noexcept;
mds::api::Result<void> validate_loaded_config(const Config &config) noexcept;

}  // namespace fairprice
