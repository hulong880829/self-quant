#include "consumer_config.h"

#include "mds/exchange/capabilities.h"
#include "mds/publish/wire_publisher.h"

#include <yaml-cpp/yaml.h>

#include <algorithm>
#include <cerrno>
#include <filesystem>
#include <limits>
#include <stdexcept>
#include <string_view>
#include <unordered_map>
#include <unordered_set>
#include <unistd.h>

namespace mds::consumer {
namespace {

constexpr std::uint64_t kMinimumGatewayPublishIntervalMs = 10;
constexpr std::uint64_t kMinimumServiceIntervalMs = 200;
constexpr std::size_t kMaximumDepth = 50;
constexpr std::uint32_t kMaximumRetentionHours = 24;
constexpr std::size_t kMaximumMultiplexShards = 256;
constexpr std::size_t kMaximumDrainRecords = 65'536;

template <typename T>
T value_or(const YAML::Node &node, std::string_view key, T fallback) {
  const auto child = node[std::string(key)];
  return child ? child.as<T>() : fallback;
}

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

template <std::size_t Size>
std::string_view fixed_view(const std::array<char, Size> &value) noexcept {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.data(), static_cast<std::size_t>(end - value.begin())};
}

Selector parse_selector(const YAML::Node &node) {
  if (!node) {
    return {};
  }
  reject_unknown(node, {"symbol", "product", "venue", "all"},
                 "segments[].selector");
  if (node.size() != 1) {
    throw std::runtime_error(
        "segments[].selector must contain exactly one of symbol, product, "
        "venue, or all");
  }
  Selector selector;
  if (node["symbol"]) {
    selector.kind = SelectorKind::Symbol;
    selector.symbol = node["symbol"].as<std::string>();
    for (char &character : selector.symbol) {
      if (character >= 'a' && character <= 'z') {
        character = static_cast<char>(character - ('a' - 'A'));
      }
    }
    if (selector.symbol.empty()) {
      throw std::runtime_error("segments[].selector.symbol is required");
    }
  } else if (node["product"]) {
    selector.kind = SelectorKind::Product;
    const auto product =
        exchange::parse_product(node["product"].as<std::string>());
    if (!product) {
      throw std::runtime_error("segments[].selector.product is unsupported");
    }
    selector.product = *product;
  } else if (node["venue"]) {
    selector.kind = SelectorKind::Venue;
    const auto venue = exchange::parse_venue(node["venue"].as<std::string>());
    if (!venue || *venue == utils::md::Venue::Unknown) {
      throw std::runtime_error("segments[].selector.venue is unsupported");
    }
    selector.venue = *venue;
  } else if (!node["all"].as<bool>()) {
    throw std::runtime_error("segments[].selector.all must be true");
  }
  return selector;
}

std::vector<std::string> parse_segment_names(const YAML::Node &node) {
  if (node.IsScalar()) {
    return {node.as<std::string>()};
  }
  reject_unknown(node, {"name", "multiplex", "selector"}, "segments[]");
  const bool has_name = bool(node["name"]);
  const bool has_multiplex = bool(node["multiplex"]);
  if (has_name == has_multiplex) {
    throw std::runtime_error(
        "segments[] must contain exactly one of name or multiplex");
  }
  if (has_name) {
    return {node["name"].as<std::string>()};
  }
  const auto multiplex = node["multiplex"];
  reject_unknown(multiplex,
                 {"prefix", "venue", "product", "stream", "shard_count"},
                 "segments[].multiplex");
  const auto prefix = multiplex["prefix"].as<std::string>();
  const auto venue_text = multiplex["venue"].as<std::string>();
  const auto product_text = multiplex["product"].as<std::string>();
  const auto stream = multiplex["stream"].as<std::string>();
  const auto venue = exchange::parse_venue(venue_text);
  const auto product = exchange::parse_product(product_text);
  const auto shard_count =
      value_or<std::size_t>(multiplex, "shard_count", 1);
  if (!venue || *venue == utils::md::Venue::Unknown || !product ||
      shard_count == 0 || shard_count > kMaximumMultiplexShards) {
    throw std::runtime_error("segments[].multiplex identity is invalid");
  }
  std::vector<std::string> names;
  names.reserve(shard_count);
  for (std::size_t shard = 0; shard < shard_count; ++shard) {
    auto name = publish::make_multiplex_segment_name(
        prefix, exchange::venue_name(*venue), exchange::product_name(*product),
        stream, shard);
    if (name.empty()) {
      throw std::runtime_error("segments[].multiplex segment name is invalid");
    }
    names.push_back(std::move(name));
  }
  return names;
}

std::uint32_t parse_retention(const YAML::Node &node) {
  if (!node) {
    return kMaximumRetentionHours;
  }
  const auto value = node.as<std::string>();
  if (value.size() < 2 || value.back() != 'h') {
    throw std::runtime_error("recording.retention must use hours, for example 24h");
  }
  std::size_t consumed{};
  const auto hours = std::stoul(value.substr(0, value.size() - 1), &consumed);
  if (consumed != value.size() - 1 || hours == 0 ||
      hours > kMaximumRetentionHours) {
    throw std::runtime_error("recording.retention must be within 1h..24h");
  }
  return static_cast<std::uint32_t>(hours);
}

enum class AggregateStream { Bbo, OrderBook, Invalid };

struct SegmentIdentity {
  AggregateStream stream{AggregateStream::Invalid};
  std::string identity;
};

SegmentIdentity aggregate_identity(std::string_view segment) {
  constexpr std::string_view bbo_suffix = ".aggbbo.2";
  constexpr std::string_view book_suffix = ".aggorderbook.2";
  SegmentIdentity result;
  std::size_t suffix_size{};
  if (segment.ends_with(bbo_suffix)) {
    result.stream = AggregateStream::Bbo;
    suffix_size = bbo_suffix.size();
  } else if (segment.ends_with(book_suffix)) {
    result.stream = AggregateStream::OrderBook;
    suffix_size = book_suffix.size();
  } else {
    return result;
  }
  if (segment.empty() || segment.front() != '/' ||
      segment.find(".agg_") == std::string_view::npos) {
    result.stream = AggregateStream::Invalid;
    return result;
  }
  result.identity.assign(segment.substr(0, segment.size() - suffix_size));
  return result;
}

void validate_aggregate_segments(const ConsumerConfig &config) {
  std::unordered_map<std::string, std::uint8_t> streams_by_identity;
  streams_by_identity.reserve(config.segments.size());
  for (const auto &segment : config.segments) {
    const auto parsed = aggregate_identity(segment.name);
    if (parsed.stream == AggregateStream::Invalid) {
      throw std::runtime_error(
          "gateway/recording segments must be aggbbo or aggorderbook streams");
    }
    const std::uint8_t stream_bit =
        parsed.stream == AggregateStream::Bbo ? 1U : 2U;
    auto &mask = streams_by_identity[parsed.identity];
    if ((mask & stream_bit) != 0) {
      throw std::runtime_error(
          "aggregate segment identity and stream must not be duplicated");
    }
    mask |= stream_bit;
  }
}

void validate_gateway(const GatewayConfig &gateway, bool requested) {
  if (gateway.listen_address.empty() || gateway.port == 0 ||
      gateway.max_clients == 0 ||
      gateway.max_subscriptions_per_client == 0 ||
      gateway.send_queue_slots != 1 ||
      gateway.publish_interval_ms < kMinimumGatewayPublishIntervalMs ||
      gateway.ping_interval_ms < kMinimumServiceIntervalMs ||
      gateway.pong_timeout_ms < kMinimumServiceIntervalMs ||
      gateway.slow_client_timeout_ms < kMinimumServiceIntervalMs ||
      gateway.depth == 0 || gateway.depth > kMaximumDepth) {
    throw std::runtime_error("gateway bounds are invalid");
  }
  if (requested && gateway.auth_token_env.empty()) {
    throw std::runtime_error(
        "plaintext WebSocket gateway requires auth_token_env");
  }
}

void parse_ingestion(const YAML::Node &node, ConsumerConfig &config) {
  if (!node) {
    return;
  }
  reject_unknown(node,
                 {"stale_after_ms", "hard_reset_after_ms",
                  "max_drain_records"},
                 "ingestion");
  auto &ingestion = config.ingestion;
  ingestion.stale_after_ms = value_or<std::uint64_t>(
      node, "stale_after_ms", ingestion.stale_after_ms);
  ingestion.hard_reset_after_ms = value_or<std::uint64_t>(
      node, "hard_reset_after_ms", ingestion.hard_reset_after_ms);
  ingestion.max_drain_records = value_or<std::size_t>(
      node, "max_drain_records", ingestion.max_drain_records);
  if (ingestion.stale_after_ms == 0 ||
      ingestion.hard_reset_after_ms <= ingestion.stale_after_ms ||
      ingestion.max_drain_records == 0 ||
      ingestion.max_drain_records > kMaximumDrainRecords) {
    throw std::runtime_error("ingestion bounds are invalid");
  }
}

std::filesystem::path existing_directory(std::filesystem::path path) {
  while (!path.empty() && !std::filesystem::exists(path)) {
    path = path.parent_path();
  }
  if (path.empty() || !std::filesystem::is_directory(path)) {
    throw std::runtime_error(
        "recording.output_directory has no existing parent");
  }
  return path;
}

void validate_recording(const RecordingConfig &recording) {
  if (recording.output_directory.empty()) {
    throw std::runtime_error("recording.output_directory is required");
  }
  if (recording.sample_interval_ms < kMinimumServiceIntervalMs ||
      recording.retention_hours == 0 ||
      recording.retention_hours > kMaximumRetentionHours ||
      recording.depth == 0 || recording.depth > kMaximumDepth ||
      recording.shard_hours != 1 || recording.zstd_level < -7 ||
      recording.zstd_level > 22 || recording.writer_queue_slots == 0 ||
      recording.topic_slots == 0) {
    throw std::runtime_error("recording bounds are invalid");
  }
  const auto parent = existing_directory(recording.output_directory);
  if (access(parent.c_str(), W_OK) != 0) {
    throw std::runtime_error("recording output directory is not writable");
  }
  const auto available = std::filesystem::space(parent).available;
  if (available < recording.min_free_disk_bytes) {
    throw std::runtime_error(
        "recording output has less than min_free_disk_bytes available");
  }
}

void parse_clickhouse_bbo(const YAML::Node &node, ConsumerConfig &config) {
  reject_unknown(
      node,
      {"host", "service", "database", "table", "user", "password_env",
       "shm_prefix", "sample_interval_ms", "stale_cutoff_ms",
       "flush_interval_ms", "request_timeout_ms", "batch_rows", "queue_rows",
       "unresolved_rows", "selectors"},
      "clickhouse_bbo");
  auto &output = config.clickhouse_bbo;
  auto &options = output.options;
  options.host = value_or<std::string>(node, "host", options.host);
  options.service = value_or<std::string>(node, "service", options.service);
  options.database = value_or<std::string>(node, "database", options.database);
  options.table = value_or<std::string>(node, "table", options.table);
  options.user = value_or<std::string>(node, "user", options.user);
  output.password_env =
      value_or<std::string>(node, "password_env", output.password_env);
  options.sample_interval_ms = value_or<std::uint64_t>(
      node, "sample_interval_ms", options.sample_interval_ms);
  options.stale_cutoff_ms = value_or<std::uint64_t>(
      node, "stale_cutoff_ms", options.stale_cutoff_ms);
  options.flush_interval_ms = value_or<std::uint64_t>(
      node, "flush_interval_ms", options.flush_interval_ms);
  options.request_timeout_ms = value_or<std::uint64_t>(
      node, "request_timeout_ms", options.request_timeout_ms);
  options.batch_rows =
      value_or<std::size_t>(node, "batch_rows", options.batch_rows);
  options.queue_rows =
      value_or<std::size_t>(node, "queue_rows", options.queue_rows);
  options.unresolved_rows =
      value_or<std::size_t>(node, "unresolved_rows", options.unresolved_rows);

  const auto prefix = value_or<std::string>(node, "shm_prefix", "");
  const auto selectors = node["selectors"];
  if (prefix.empty() || !selectors || !selectors.IsSequence() ||
      selectors.size() == 0) {
    throw std::runtime_error(
        "clickhouse_bbo requires shm_prefix and explicit selectors");
  }
  std::unordered_set<std::string> unique_segments;
  for (const auto &selector_node : selectors) {
    reject_unknown(selector_node, {"venue", "product", "shard"},
                   "clickhouse_bbo selector");
    const auto venue =
        exchange::parse_venue(value_or<std::string>(selector_node, "venue", ""));
    const auto product = exchange::parse_product(
        value_or<std::string>(selector_node, "product", ""));
    if (!venue || !product || *venue == utils::md::Venue::Polymarket ||
        *product == utils::md::ProductType::BinaryOption) {
      throw std::runtime_error(
          "clickhouse_bbo selectors must name an explicit crypto "
          "venue/product (all and polymarket are forbidden)");
    }
    const auto shard = value_or<std::size_t>(
        selector_node, "shard", std::numeric_limits<std::size_t>::max());
    if (shard == std::numeric_limits<std::size_t>::max() ||
        shard >= kMaximumMultiplexShards) {
      throw std::runtime_error(
          "clickhouse_bbo selector.shard is required and out of range");
    }
    auto segment = publish::make_multiplex_segment_name(
        prefix, exchange::venue_name(*venue), exchange::product_name(*product),
        "ticker", shard);
    if (segment.empty() || !unique_segments.insert(segment).second) {
      throw std::runtime_error(
          "clickhouse_bbo selectors must resolve to unique multiplex rings");
    }
    options.selectors.push_back({*venue, *product});
    output.segments.push_back(std::move(segment));
  }
  if (options.host.empty() || options.sample_interval_ms < 200 ||
      options.stale_cutoff_ms == 0 || options.flush_interval_ms == 0 ||
      options.request_timeout_ms == 0 || options.batch_rows == 0 ||
      options.queue_rows == 0 || options.unresolved_rows == 0) {
    throw std::runtime_error("clickhouse_bbo bounds are invalid");
  }
}

}  // namespace

bool Selector::matches(const utils::md::Instrument &instrument) const
    noexcept {
  switch (kind) {
    case SelectorKind::Symbol:
      return fixed_view(instrument.canonical_symbol) == symbol;
    case SelectorKind::Product:
      return instrument.product_type == product;
    case SelectorKind::Venue:
      return instrument.venue == venue;
    case SelectorKind::All:
      return true;
  }
  return false;
}

bool Selector::matches(const utils::md::InstrumentCatalog &catalog) const
    noexcept {
  switch (kind) {
    case SelectorKind::Symbol:
      return fixed_view(catalog.canonical_symbol) == symbol;
    case SelectorKind::Product:
      return catalog.product_type == product;
    case SelectorKind::Venue:
      return catalog.venue == venue;
    case SelectorKind::All:
      return true;
  }
  return false;
}

bool recording_available() noexcept {
#if defined(MDS_HAS_ZSTD)
  return true;
#else
  return false;
#endif
}

api::Result<ConsumerConfig> load_config(const std::string &path,
                                        bool gateway_requested,
                                        bool recording_requested,
                                        bool clickhouse_bbo_requested) noexcept {
  try {
    const auto root = YAML::LoadFile(path);
    reject_unknown(root, {"segments", "ingestion", "gateway", "recording",
                          "clickhouse_bbo"}, "root");
    ConsumerConfig config;
    parse_ingestion(root["ingestion"], config);

    const auto segments = root["segments"];
    if ((!segments || !segments.IsSequence() || segments.size() == 0) &&
        !clickhouse_bbo_requested) {
      throw std::runtime_error("segments must contain at least one entry");
    }
    std::unordered_set<std::string> unique_segments;
    if (segments) config.segments.reserve(segments.size());
    for (const auto &node : segments) {
      const auto selector =
          node.IsMap() ? parse_selector(node["selector"]) : Selector{};
      for (auto segment : parse_segment_names(node)) {
        if (segment.empty() || segment.front() != '/' ||
            !unique_segments.insert(segment).second) {
          throw std::runtime_error(
              "segments must resolve to unique absolute shared-memory names");
        }
        config.segments.push_back(
            {.name = std::move(segment), .selector = selector});
      }
    }

    const auto gateway = root["gateway"];
    if (gateway) {
      reject_unknown(
          gateway,
          {"listen_address", "port", "auth_token_env", "max_clients",
           "max_subscriptions_per_client",
           "send_queue_slots", "publish_interval_ms", "ping_interval_ms",
           "pong_timeout_ms", "slow_client_timeout_ms", "depth",
           "reuse_port"},
          "gateway");
      config.gateway.listen_address = value_or<std::string>(
          gateway, "listen_address", config.gateway.listen_address);
      const auto port = value_or<std::uint32_t>(
          gateway, "port", config.gateway.port);
      if (port > std::numeric_limits<std::uint16_t>::max()) {
        throw std::runtime_error("gateway.port is out of range");
      }
      config.gateway.port = static_cast<std::uint16_t>(port);
      config.gateway.auth_token_env = value_or<std::string>(
          gateway, "auth_token_env", config.gateway.auth_token_env);
      config.gateway.max_clients = value_or<std::size_t>(
          gateway, "max_clients", config.gateway.max_clients);
      config.gateway.max_subscriptions_per_client = value_or<std::size_t>(
          gateway, "max_subscriptions_per_client",
          config.gateway.max_subscriptions_per_client);
      config.gateway.send_queue_slots = value_or<std::size_t>(
          gateway, "send_queue_slots", config.gateway.send_queue_slots);
      config.gateway.publish_interval_ms = value_or<std::uint64_t>(
          gateway, "publish_interval_ms",
          config.gateway.publish_interval_ms);
      config.gateway.ping_interval_ms = value_or<std::uint64_t>(
          gateway, "ping_interval_ms", config.gateway.ping_interval_ms);
      config.gateway.pong_timeout_ms = value_or<std::uint64_t>(
          gateway, "pong_timeout_ms", config.gateway.pong_timeout_ms);
      config.gateway.slow_client_timeout_ms = value_or<std::uint64_t>(
          gateway, "slow_client_timeout_ms",
          config.gateway.slow_client_timeout_ms);
      config.gateway.depth =
          value_or<std::size_t>(gateway, "depth", config.gateway.depth);
      config.gateway.reuse_port =
          value_or<bool>(gateway, "reuse_port", config.gateway.reuse_port);
    }

    const auto recording = root["recording"];
    if (recording) {
      reject_unknown(
          recording,
          {"output_directory", "sample_interval_ms", "retention", "depth",
           "shard_hours", "zstd_level", "writer_queue_slots", "topic_slots",
           "min_free_disk_bytes"},
          "recording");
      config.recording.output_directory = value_or<std::string>(
          recording, "output_directory", config.recording.output_directory);
      config.recording.sample_interval_ms = value_or<std::uint64_t>(
          recording, "sample_interval_ms",
          config.recording.sample_interval_ms);
      config.recording.retention_hours = parse_retention(recording["retention"]);
      config.recording.depth = value_or<std::size_t>(
          recording, "depth", config.recording.depth);
      config.recording.shard_hours = value_or<std::uint32_t>(
          recording, "shard_hours", config.recording.shard_hours);
      config.recording.zstd_level = value_or<int>(
          recording, "zstd_level", config.recording.zstd_level);
      config.recording.writer_queue_slots = value_or<std::size_t>(
          recording, "writer_queue_slots",
          config.recording.writer_queue_slots);
      config.recording.topic_slots = value_or<std::size_t>(
          recording, "topic_slots", config.recording.topic_slots);
      config.recording.min_free_disk_bytes = value_or<std::uint64_t>(
          recording, "min_free_disk_bytes",
          config.recording.min_free_disk_bytes);
    }

    const auto clickhouse_bbo = root["clickhouse_bbo"];
    if (clickhouse_bbo) {
      parse_clickhouse_bbo(clickhouse_bbo, config);
    } else if (clickhouse_bbo_requested) {
      throw std::runtime_error(
          "clickhouse_bbo configuration is required with --clickhouse-bbo");
    }

    if (gateway) {
      validate_gateway(config.gateway, gateway_requested);
    } else if (gateway_requested) {
      throw std::runtime_error(
          "gateway configuration is required with --gateway");
    }
    if (recording) {
      validate_recording(config.recording);
    }
    if (gateway_requested || recording_requested) {
      validate_aggregate_segments(config);
    }
    if (recording_requested) {
      if (!recording) {
        throw std::runtime_error(
            "recording configuration is required with --record");
      }
      if (!recording_available()) {
        return {
            .error = api::ErrorCode::InvalidConfig,
            .message =
                "recording is unavailable: install libzstd-dev and rebuild "
                "with -DMDS_ENABLE_RECORDING=ON"};
      }
    }
    return {.value = std::move(config)};
  } catch (const std::exception &error) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = std::string("failed to load consumer config: ") +
                       error.what()};
  }
}

}  // namespace mds::consumer
