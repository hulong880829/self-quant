#include "fairprice/config.h"

#include "fairprice/log.h"
#include "utils/md/wire.h"

#include <yaml-cpp/yaml.h>

#include <algorithm>
#include <stdexcept>
#include <string>
#include <vector>

namespace fairprice {
namespace {

void require_map(const YAML::Node &node, std::string_view context) {
  if (!node || !node.IsMap()) {
    throw std::runtime_error(std::string(context) + " must be a map");
  }
}

void reject_unknown(const YAML::Node &node,
                    std::initializer_list<std::string_view> allowed,
                    std::string_view context) {
  require_map(node, context);
  for (const auto &entry : node) {
    const auto key = entry.first.as<std::string>();
    if (std::find(allowed.begin(), allowed.end(), key) == allowed.end()) {
      throw std::runtime_error(std::string(context) + ": unknown key '" + key +
                               "'");
    }
  }
}

template <typename T>
T require_value(const YAML::Node &node, std::string_view key,
                std::string_view context) {
  const auto child = node[std::string(key)];
  if (!child) {
    throw std::runtime_error(std::string(context) + " missing '" +
                             std::string(key) + "'");
  }
  return child.as<T>();
}

bool power_of_two(std::size_t value) noexcept {
  return value != 0 && (value & (value - 1)) == 0;
}

std::vector<std::string> parse_origins(const YAML::Node &node) {
  if (!node || !node.IsSequence() || node.size() == 0) {
    throw std::runtime_error("server.allowed_origins must be a non-empty list");
  }
  std::vector<std::string> origins;
  origins.reserve(node.size());
  for (const auto &item : node) {
    origins.push_back(item.as<std::string>());
  }
  return origins;
}

}  // namespace

mds::api::Result<Config> load_config(const std::string &path) noexcept {
  try {
    const auto root = YAML::LoadFile(path);
    reject_unknown(root,
                   {"version", "runtime", "market_data", "shared_memory",
                    "server", "stream", "history", "fair_price"},
                   "root");
    Config config;
    config.version = require_value<int>(root, "version", "root");
    if (config.version != 2) {
      throw std::runtime_error("version must be 2");
    }

    const auto runtime = root["runtime"];
    reject_unknown(runtime, {"log_level", "development"}, "runtime");
    config.log_level =
        require_value<std::string>(runtime, "log_level", "runtime");
    config.development = require_value<bool>(runtime, "development", "runtime");
    LogLevel parsed_level{};
    if (!parse_log_level(config.log_level, parsed_level)) {
      throw std::runtime_error(
          "runtime.log_level must be debug, info, warn, or error");
    }

    const auto market_data = root["market_data"];
    reject_unknown(market_data,
                   {"sample_interval_ms", "depth", "snapshot_stale_after_ms",
                    "catalog_refresh_interval_ms", "retention_hours",
                    "clickhouse", "gateway_bootstrap"},
                   "market_data");
    config.market_data.sample_interval_ms = require_value<std::uint64_t>(
        market_data, "sample_interval_ms", "market_data");
    config.market_data.depth =
        require_value<std::uint32_t>(market_data, "depth", "market_data");
    config.market_data.snapshot_stale_after_ms = require_value<std::uint64_t>(
        market_data, "snapshot_stale_after_ms", "market_data");
    config.market_data.catalog_refresh_interval_ms =
        require_value<std::uint64_t>(market_data, "catalog_refresh_interval_ms",
                                     "market_data");
    if (market_data["retention_hours"]) {
      config.market_data.retention_hours = require_value<std::uint32_t>(
          market_data, "retention_hours", "market_data");
    }
    if (config.market_data.sample_interval_ms == 0 ||
        config.market_data.depth == 0 ||
        config.market_data.snapshot_stale_after_ms == 0 ||
        config.market_data.catalog_refresh_interval_ms == 0) {
      throw std::runtime_error(
          "market_data.sample_interval_ms, depth, snapshot_stale_after_ms, "
          "and catalog_refresh_interval_ms are required");
    }
    const auto clickhouse = market_data["clickhouse"];
    reject_unknown(clickhouse,
                   {"host", "service", "database", "user", "password"},
                   "market_data.clickhouse");
    auto &ch = config.market_data.clickhouse;
    ch.host = require_value<std::string>(clickhouse, "host",
                                         "market_data.clickhouse");
    ch.service = require_value<std::string>(clickhouse, "service",
                                            "market_data.clickhouse");
    ch.database = require_value<std::string>(clickhouse, "database",
                                             "market_data.clickhouse");
    ch.user = require_value<std::string>(clickhouse, "user",
                                         "market_data.clickhouse");
    ch.password = require_value<std::string>(clickhouse, "password",
                                             "market_data.clickhouse");
    if (ch.host.empty() || ch.service.empty() || ch.database.empty() ||
        ch.user.empty() || ch.password.empty()) {
      throw std::runtime_error("market_data.clickhouse bounds are invalid");
    }
    const auto gateway = market_data["gateway_bootstrap"];
    reject_unknown(gateway, {"url", "token", "timeout_ms"},
                   "market_data.gateway_bootstrap");
    auto &gw = config.market_data.gateway_bootstrap;
    gw.url = require_value<std::string>(
        gateway, "url", "market_data.gateway_bootstrap");
    gw.token = require_value<std::string>(
        gateway, "token", "market_data.gateway_bootstrap");
    gw.timeout_ms = require_value<std::uint64_t>(
        gateway, "timeout_ms", "market_data.gateway_bootstrap");
    if (gw.token.empty() || gw.timeout_ms == 0 ||
        !(gw.url.starts_with("ws://") || gw.url.starts_with("wss://"))) {
      throw std::runtime_error(
          "market_data.gateway_bootstrap bounds are invalid");
    }
    config.snapshot_stale_after_ms = config.market_data.snapshot_stale_after_ms;
    config.catalog_refresh_interval_ms =
        config.market_data.catalog_refresh_interval_ms;


    const auto shm = root["shared_memory"];
    reject_unknown(shm, {"backend", "output"}, "shared_memory");
    config.shm_backend =
        require_value<std::string>(shm, "backend", "shared_memory");
    if (config.shm_backend != "posix_shm") {
      throw std::runtime_error("shared_memory.backend must be posix_shm");
    }
    const auto output = shm["output"];
    reject_unknown(output,
                   {"prefix", "ring_bytes", "max_record_bytes", "max_readers",
                    "reader_lease_timeout_ms", "unlink_on_shutdown"},
                   "shared_memory.output");
    config.shm_prefix =
        require_value<std::string>(output, "prefix", "shared_memory.output");
    if (config.shm_prefix.size() < 2 || config.shm_prefix.front() != '/') {
      throw std::runtime_error("shared_memory.output.prefix is invalid");
    }
    config.output_ring.backend = mds::api::ShmBackend::PosixShm;
    config.output_ring.mode = mds::api::RingMode::OverwriteOldest;
    config.output_ring.create = true;
    config.output_ring.ring_bytes = require_value<std::size_t>(
        output, "ring_bytes", "shared_memory.output");
    config.output_ring.max_record_bytes = require_value<std::size_t>(
        output, "max_record_bytes", "shared_memory.output");
    config.output_ring.max_readers = require_value<std::size_t>(
        output, "max_readers", "shared_memory.output");
    config.output_ring.unlink_on_close = require_value<bool>(
        output, "unlink_on_shutdown", "shared_memory.output");
    config.reader_lease_timeout_ms = require_value<std::uint64_t>(
        output, "reader_lease_timeout_ms", "shared_memory.output");
    if (!power_of_two(config.output_ring.ring_bytes) ||
        config.output_ring.max_record_bytes <
            sizeof(utils::md::wire::FairPriceRecordV2) ||
        config.output_ring.max_record_bytes >
            config.output_ring.ring_bytes / 8U ||
        config.output_ring.max_readers == 0) {
      throw std::runtime_error("shared_memory.output bounds are invalid");
    }

    const auto server = root["server"];
    reject_unknown(server,
                   {"listen_address", "port", "allowed_origins",
                    "browser_token", "max_clients",
                    "max_subscriptions_per_client", "read_header_timeout_ms",
                    "idle_timeout_ms", "max_header_bytes", "websocket"},
                   "server");
    config.listen_address =
        require_value<std::string>(server, "listen_address", "server");
    config.port = require_value<std::uint16_t>(server, "port", "server");
    config.allowed_origins = parse_origins(server["allowed_origins"]);
    config.browser_token =
        require_value<std::string>(server, "browser_token", "server");
    config.max_clients = require_value<int>(server, "max_clients", "server");
    config.max_subscriptions_per_client =
        require_value<int>(server, "max_subscriptions_per_client", "server");
    config.read_header_timeout_ms = require_value<std::uint64_t>(
        server, "read_header_timeout_ms", "server");
    config.idle_timeout_ms =
        require_value<std::uint64_t>(server, "idle_timeout_ms", "server");
    config.max_header_bytes =
        require_value<std::size_t>(server, "max_header_bytes", "server");
    if (config.listen_address.empty() || config.port == 0 ||
        config.max_clients <= 0 || config.max_subscriptions_per_client <= 0 ||
        config.max_header_bytes == 0) {
      throw std::runtime_error("server bounds are invalid");
    }
    const auto websocket = server["websocket"];
    reject_unknown(websocket,
                   {"handshake_timeout_ms", "read_limit_bytes",
                    "write_buffer_bytes", "pong_timeout_ms"},
                   "server.websocket");
    config.websocket.handshake_timeout_ms = require_value<std::uint64_t>(
        websocket, "handshake_timeout_ms", "server.websocket");
    config.websocket.read_limit_bytes = require_value<std::size_t>(
        websocket, "read_limit_bytes", "server.websocket");
    config.websocket.write_buffer_bytes = require_value<std::size_t>(
        websocket, "write_buffer_bytes", "server.websocket");
    config.websocket.pong_timeout_ms = require_value<std::uint64_t>(
        websocket, "pong_timeout_ms", "server.websocket");

    const auto stream = root["stream"];
    reject_unknown(stream, {"interval_ms"}, "stream");
    config.stream_interval_ms =
        require_value<std::uint64_t>(stream, "interval_ms", "stream");
    if (config.stream_interval_ms == 0) {
      throw std::runtime_error("stream.interval_ms must be positive");
    }

    const auto history = root["history"];
    reject_unknown(history, {"workers", "resolution_ms"}, "history");
    config.history_workers = require_value<int>(history, "workers", "history");
    config.history_resolution_ms =
        require_value<std::uint64_t>(history, "resolution_ms", "history");
    if (config.history_workers <= 0 || config.history_resolution_ms == 0) {
      throw std::runtime_error("history bounds are invalid");
    }

    const auto fair = root["fair_price"];
    reject_unknown(fair,
                   {"enabled", "depth", "lambda_per_bp", "imbalance_alpha",
                    "ewma_tau_ms", "impact_notional", "venue_dominance_ratio",
                    "stream_interval_ms", "heartbeat_ms", "persistence"},
                   "fair_price");
    config.fair_price.enabled =
        require_value<bool>(fair, "enabled", "fair_price");
    config.fair_price.depth_k = require_value<int>(fair, "depth", "fair_price");
    config.fair_price.lambda_per_bp =
        require_value<double>(fair, "lambda_per_bp", "fair_price");
    config.fair_price.imbalance_alpha =
        require_value<double>(fair, "imbalance_alpha", "fair_price");
    const auto tau_ms =
        require_value<std::uint64_t>(fair, "ewma_tau_ms", "fair_price");
    config.fair_price.ewma_tau_ns = tau_ms * 1'000'000ULL;
    config.fair_price.impact_notional =
        require_value<double>(fair, "impact_notional", "fair_price");
    config.fair_price.venue_dominance_ratio =
        require_value<double>(fair, "venue_dominance_ratio", "fair_price");
    config.fair_stream_interval_ms =
        require_value<std::uint64_t>(fair, "stream_interval_ms", "fair_price");
    config.fair_heartbeat_ms =
        require_value<std::uint64_t>(fair, "heartbeat_ms", "fair_price");
    const auto persistence = fair["persistence"];
    reject_unknown(persistence,
                   {"enabled", "database_url", "sample_interval_ms",
                    "retention_hours", "cleanup_interval_ms", "delete_batch",
                    "delete_max_batches", "operation_timeout_ms"},
                   "fair_price.persistence");
    config.persistence.enabled =
        require_value<bool>(persistence, "enabled", "fair_price.persistence");
    config.persistence.database_url = require_value<std::string>(
        persistence, "database_url", "fair_price.persistence");
    config.persistence.sample_interval_ms = require_value<std::uint64_t>(
        persistence, "sample_interval_ms", "fair_price.persistence");
    config.persistence.retention_hours = require_value<std::uint32_t>(
        persistence, "retention_hours", "fair_price.persistence");
    config.persistence.cleanup_interval_ms = require_value<std::uint64_t>(
        persistence, "cleanup_interval_ms", "fair_price.persistence");
    config.persistence.delete_batch =
        require_value<int>(persistence, "delete_batch", "fair_price.persistence");
    config.persistence.delete_max_batches = require_value<int>(
        persistence, "delete_max_batches", "fair_price.persistence");
    config.persistence.operation_timeout_ms = require_value<std::uint64_t>(
        persistence, "operation_timeout_ms", "fair_price.persistence");
    if (config.fair_stream_interval_ms == 0 || config.fair_heartbeat_ms == 0) {
      throw std::runtime_error("fair_price stream intervals must be positive");
    }
    if (config.persistence.enabled &&
        (config.persistence.database_url.empty() ||
         config.persistence.sample_interval_ms == 0 ||
         config.persistence.retention_hours == 0 ||
         config.persistence.delete_batch <= 0 ||
         config.persistence.delete_max_batches <= 0)) {
      throw std::runtime_error("fair_price.persistence bounds are invalid");
    }
    return {.value = std::move(config)};
  } catch (const std::exception &ex) {
    return {.error = mds::api::ErrorCode::InvalidConfig, .message = ex.what()};
  }
}

mds::api::Result<void> validate_loaded_config(const Config &config) noexcept {
  LogLevel parsed_level{};
  if (!parse_log_level(config.log_level, parsed_level)) {
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "runtime.log_level must be debug, info, warn, or error"};
  }
  if (config.market_data.sample_interval_ms == 0 ||
      config.market_data.depth == 0) {
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "market_data.sample_interval_ms and depth are required"};
  }
  if (config.persistence.enabled && config.persistence.database_url.empty()) {
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "fair_price.persistence.database_url is empty"};
  }
  return {};
}

}  // namespace fairprice
