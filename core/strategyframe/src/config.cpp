#include "strategyframe/config.h"

#include <yaml-cpp/yaml.h>

#include <algorithm>
#include <charconv>
#include <limits>
#include <map>
#include <stdexcept>
#include <string>
#include <variant>

#include "utils/md/market_identity.h"

namespace strategyframe {

struct StrategyParams::Impl {
  using Value = std::variant<std::int64_t, double, bool, std::string>;
  std::map<std::string, Value, std::less<>> values;
};

namespace {

void RequireMap(const YAML::Node& node, std::string_view context) {
  if (!node || !node.IsMap()) {
    throw std::runtime_error(std::string(context) + " must be a map");
  }
}

void RequireSequence(const YAML::Node& node, std::string_view context) {
  if (!node || !node.IsSequence()) {
    throw std::runtime_error(std::string(context) + " must be a sequence");
  }
}

void RejectUnknown(const YAML::Node& node,
                   std::initializer_list<std::string_view> allowed,
                   std::string_view context) {
  RequireMap(node, context);
  for (const auto& entry : node) {
    const std::string key = entry.first.as<std::string>();
    if (std::find(allowed.begin(), allowed.end(), key) == allowed.end()) {
      throw std::runtime_error(std::string(context) + ": unknown key '" + key +
                               "'");
    }
  }
}

template <typename T>
T ValueOr(const YAML::Node& node, std::string_view key, T fallback) {
  const YAML::Node child = node[std::string(key)];
  return child ? child.as<T>() : std::move(fallback);
}

bool PowerOfTwo(std::uint32_t value) noexcept {
  return value >= 2 && (value & (value - 1U)) == 0;
}

ProductType ParseProduct(const std::string& value) {
  const auto parsed = utils::md::parse_canonical_product(value);
  if (!parsed) {
    throw std::runtime_error("unsupported product: " + value);
  }
  return *parsed;
}

Venue ParseVenue(const std::string& value) {
  const auto parsed = utils::md::parse_canonical_venue(value);
  if (!parsed) {
    throw std::runtime_error("unsupported venue: " + value);
  }
  return *parsed;
}

BboPolicy ParseBboPolicy(std::string_view value) {
  if (value == "ticker_only") return BboPolicy::TickerOnly;
  if (value == "orderbook_only") return BboPolicy::OrderBookOnly;
  if (value == "newest_exchange_time")
    return BboPolicy::NewestExchangeTime;
  if (value == "legacy_passthrough")
    return BboPolicy::LegacyPassthrough;
  throw std::runtime_error("unsupported mds.bbo_policy");
}

bool HasSegmentRole(const MdsConfig& config, std::string_view role) {
  return std::any_of(
      config.segments.begin(), config.segments.end(),
      [role](const SegmentConfig& segment) {
        return segment.name.find(role) != std::string::npos;
      });
}

EndpointConfig ParseEndpoint(const YAML::Node& node,
                             std::string_view context) {
  if (!node) return {};
  RejectUnknown(node, {"host", "port"}, context);
  EndpointConfig result;
  result.host = ValueOr<std::string>(node, "host", {});
  result.service = ValueOr<std::string>(node, "port", "443");
  if (!result.host.empty() &&
      (result.host.find('/') != std::string::npos ||
       result.host.find(':') != std::string::npos)) {
    throw std::runtime_error(std::string(context) +
                             ".host must not include scheme, path, or port");
  }
  return result;
}

void FlattenStrategy(const YAML::Node& node, std::string prefix,
                     StrategyParams::Impl& output) {
  if (!node) return;
  if (node.IsMap()) {
    for (const auto& entry : node) {
      const std::string key = entry.first.as<std::string>();
      FlattenStrategy(entry.second, prefix.empty() ? key : prefix + "." + key,
                      output);
    }
    return;
  }
  if (node.IsSequence()) {
    if (prefix.empty())
      throw std::runtime_error("strategy root sequence is unsupported");
    output.values.emplace(prefix + ".__size",
                          static_cast<std::int64_t>(node.size()));
    for (std::size_t index = 0; index < node.size(); ++index) {
      FlattenStrategy(node[index], prefix + "." + std::to_string(index),
                      output);
    }
    return;
  }
  if (!node.IsScalar() || prefix.empty()) {
    throw std::runtime_error(
        "strategy values must be scalar or nested maps");
  }
  const std::string text = node.Scalar();
  if (text == "true" || text == "false") {
    output.values.emplace(std::move(prefix), text == "true");
    return;
  }
  std::int64_t integer{};
  const auto integer_result =
      std::from_chars(text.data(), text.data() + text.size(), integer);
  if (integer_result.ec == std::errc{} &&
      integer_result.ptr == text.data() + text.size()) {
    output.values.emplace(std::move(prefix), integer);
    return;
  }
  try {
    std::size_t consumed{};
    const double real = std::stod(text, &consumed);
    if (consumed == text.size()) {
      output.values.emplace(std::move(prefix), real);
      return;
    }
  } catch (...) {
  }
  output.values.emplace(std::move(prefix), text);
}

VenueExecutionConfig::Kind ParseVenueKind(std::string_view value) {
  if (value == "binance_spot")
    return VenueExecutionConfig::Kind::BinanceSpot;
  if (value == "binance_usdm")
    return VenueExecutionConfig::Kind::BinanceUsdm;
  if (value == "polymarket")
    return VenueExecutionConfig::Kind::Polymarket;
  throw std::runtime_error("unsupported OMS venue: " + std::string(value));
}

void ValidateCapacities(const CapacityConfig& capacities) {
  if (!PowerOfTwo(capacities.command_queue) ||
      !PowerOfTwo(capacities.update_queue) ||
      !PowerOfTwo(capacities.market_data_queue) ||
      !PowerOfTwo(capacities.order_table) ||
      !PowerOfTwo(capacities.position_table) ||
      !PowerOfTwo(capacities.fill_dedup) ||
      !PowerOfTwo(capacities.timer_table) ||
      !PowerOfTwo(capacities.instrument_directory)) {
    throw std::runtime_error("all capacities must be powers of two >= 2");
  }
}

}  // namespace

StrategyParams::StrategyParams() noexcept
    : impl_(std::make_shared<Impl>()) {}
StrategyParams::~StrategyParams() = default;
StrategyParams::StrategyParams(const StrategyParams&) noexcept = default;
StrategyParams& StrategyParams::operator=(const StrategyParams&) noexcept =
    default;
StrategyParams::StrategyParams(StrategyParams&&) noexcept = default;
StrategyParams& StrategyParams::operator=(StrategyParams&&) noexcept = default;
StrategyParams::StrategyParams(std::shared_ptr<const Impl> impl,
                               std::string prefix) noexcept
    : impl_(std::move(impl)), prefix_(std::move(prefix)) {}

std::string StrategyParams::key(std::string_view path) const {
  if (prefix_.empty()) return std::string(path);
  if (path.empty()) return prefix_;
  return prefix_ + "." + std::string(path);
}

bool StrategyParams::contains(std::string_view path) const noexcept {
  if (!impl_) return false;
  try {
    return impl_->values.find(key(path)) != impl_->values.end();
  } catch (...) {
    return false;
  }
}

Result<std::int64_t> StrategyParams::require_int(
    std::string_view path) const noexcept {
  if (!impl_) return {{}, Error::NotFound};
  std::string resolved;
  try {
    resolved = key(path);
  } catch (...) {
    return {{}, Error::Internal};
  }
  const auto found = impl_->values.find(resolved);
  if (found == impl_->values.end()) return {{}, Error::NotFound};
  if (const auto* value = std::get_if<std::int64_t>(&found->second))
    return {*value, Error::Ok};
  return {{}, Error::InvalidConfig};
}

Result<double> StrategyParams::require_double(
    std::string_view path) const noexcept {
  if (!impl_) return {{}, Error::NotFound};
  std::string resolved;
  try {
    resolved = key(path);
  } catch (...) {
    return {{}, Error::Internal};
  }
  const auto found = impl_->values.find(resolved);
  if (found == impl_->values.end()) return {{}, Error::NotFound};
  if (const auto* value = std::get_if<double>(&found->second))
    return {*value, Error::Ok};
  if (const auto* value = std::get_if<std::int64_t>(&found->second))
    return {static_cast<double>(*value), Error::Ok};
  return {{}, Error::InvalidConfig};
}

Result<std::string_view> StrategyParams::require_string(
    std::string_view path) const noexcept {
  if (!impl_) return {{}, Error::NotFound};
  std::string resolved;
  try {
    resolved = key(path);
  } catch (...) {
    return {{}, Error::Internal};
  }
  const auto found = impl_->values.find(resolved);
  if (found == impl_->values.end()) return {{}, Error::NotFound};
  if (const auto* value = std::get_if<std::string>(&found->second))
    return {*value, Error::Ok};
  return {{}, Error::InvalidConfig};
}

Result<bool> StrategyParams::require_bool(
    std::string_view path) const noexcept {
  if (!impl_) return {{}, Error::NotFound};
  std::string resolved;
  try {
    resolved = key(path);
  } catch (...) {
    return {{}, Error::Internal};
  }
  const auto found = impl_->values.find(resolved);
  if (found == impl_->values.end()) return {{}, Error::NotFound};
  if (const auto* value = std::get_if<bool>(&found->second))
    return {*value, Error::Ok};
  return {{}, Error::InvalidConfig};
}

std::int64_t StrategyParams::optional_int(
    std::string_view path, std::int64_t fallback) const noexcept {
  const auto result = require_int(path);
  return result ? result.value : fallback;
}

double StrategyParams::optional_double(std::string_view path,
                                       double fallback) const noexcept {
  const auto result = require_double(path);
  return result ? result.value : fallback;
}

bool StrategyParams::optional_bool(std::string_view path,
                                   bool fallback) const noexcept {
  const auto result = require_bool(path);
  return result ? result.value : fallback;
}

std::string_view StrategyParams::optional_string(
    std::string_view path, std::string_view fallback) const noexcept {
  const auto result = require_string(path);
  return result ? result.value : fallback;
}

StrategyParams StrategyParams::child(std::string_view path) const {
  return StrategyParams(impl_, key(path));
}

Result<std::size_t> StrategyParams::sequence_size(
    std::string_view path) const noexcept {
  std::string size_path;
  try {
    size_path = std::string(path) + ".__size";
  } catch (...) {
    return {{}, Error::Internal};
  }
  const auto result = require_int(size_path);
  if (!result || result.value < 0)
    return {{}, result ? Error::InvalidConfig : result.error};
  return {static_cast<std::size_t>(result.value), Error::Ok};
}

Result<StrategyFrameConfig> load_config(const std::string& path) noexcept {
  try {
    const YAML::Node root = YAML::LoadFile(path);
    RejectUnknown(root, {"mds", "oms", "strategy"}, "root");
    StrategyFrameConfig result;

    const YAML::Node mds = root["mds"];
    RejectUnknown(mds,
                  {"source", "bbo_policy", "bbo_policy_starvation_ms",
                   "segments", "producer",
                   "required_instruments"},
                  "mds");
    const std::string source =
        ValueOr<std::string>(mds, "source", "external_shm");
    if (source == "external_shm") {
      result.mds.source = MdsSourceMode::ExternalSharedMemory;
    } else if (source == "self_hosted") {
      result.mds.source = MdsSourceMode::SelfHosted;
    } else if (source == "replay") {
      result.mds.source = MdsSourceMode::Replay;
    } else {
      throw std::runtime_error("unsupported mds.source");
    }
    result.mds.bbo_policy = ParseBboPolicy(
        ValueOr<std::string>(mds, "bbo_policy", "legacy_passthrough"));
    const std::uint64_t starvation_ms = ValueOr<std::uint64_t>(
        mds, "bbo_policy_starvation_ms", 2000);
    if (starvation_ms >
        std::numeric_limits<std::uint64_t>::max() / 1'000'000ULL)
      throw std::runtime_error("mds.bbo_policy_starvation_ms overflows");
    result.mds.bbo_policy_starvation_ns =
        starvation_ms * 1'000'000ULL;

    if (const YAML::Node segments = mds["segments"]) {
      RequireSequence(segments, "mds.segments");
      for (const auto& item : segments) {
        RejectUnknown(item,
                      {"name", "ring_bytes", "max_record_bytes",
                       "expected_reader_budget", "heartbeat_interval_ns"},
                      "mds.segments[]");
        SegmentConfig segment;
        segment.name = item["name"].as<std::string>();
        segment.ring_bytes =
            ValueOr<std::size_t>(item, "ring_bytes", segment.ring_bytes);
        segment.max_record_bytes = ValueOr<std::size_t>(
            item, "max_record_bytes", segment.max_record_bytes);
        segment.expected_reader_budget = ValueOr<std::uint32_t>(
            item, "expected_reader_budget", 1);
        segment.heartbeat_interval_ns = ValueOr<std::uint64_t>(
            item, "heartbeat_interval_ns", segment.heartbeat_interval_ns);
        if (segment.name.empty() || segment.expected_reader_budget == 0)
          throw std::runtime_error("invalid mds segment");
        result.mds.segments.push_back(std::move(segment));
      }
    }
    if (const YAML::Node producer = mds["producer"]) {
      RejectUnknown(producer, {"config_path"}, "mds.producer");
      result.mds.producer_config_path =
          ValueOr<std::string>(producer, "config_path", {});
    }
    if (const YAML::Node required = mds["required_instruments"]) {
      RequireSequence(required, "mds.required_instruments");
      for (const auto& item : required) {
        RejectUnknown(item, {"venue", "product", "symbol"},
                      "mds.required_instruments[]");
        InstrumentSelector selector;
        selector.venue = ParseVenue(item["venue"].as<std::string>());
        selector.product =
            ParseProduct(item["product"].as<std::string>());
        selector.canonical_symbol = item["symbol"].as<std::string>();
        if (selector.venue == Venue::Unknown ||
            selector.product == ProductType::Unknown ||
            selector.canonical_symbol.empty()) {
          throw std::runtime_error("invalid required instrument selector");
        }
        result.mds.required_instruments.push_back(std::move(selector));
      }
    }
    if (result.mds.source == MdsSourceMode::ExternalSharedMemory &&
        result.mds.segments.empty()) {
      throw std::runtime_error("external_shm requires segments");
    }
    if (result.mds.source == MdsSourceMode::SelfHosted &&
        result.mds.producer_config_path.empty()) {
      throw std::runtime_error(
          "self_hosted requires producer.config_path");
    }

    const YAML::Node oms = root["oms"];
    RejectUnknown(oms,
                  {"threading", "idle_policy", "strategy_cpu", "io_cpu",
                   "numa_node", "strictness", "memory", "socket",
                   "capacities", "instruments", "venues", "session_epoch",
                   "event_budget", "metric_sample_rate",
                   "retire_deferred_warning_count",
                   "startup_timeout_ns"},
                  "oms");
    const std::string threading =
        ValueOr<std::string>(oms, "threading", "single_thread");
    result.threading = threading == "single_thread"
                           ? ThreadingMode::SingleThread
                           : threading == "multi_io_thread"
                                 ? ThreadingMode::MultiIoThread
                                 : throw std::runtime_error(
                                       "unsupported oms.threading");
    const std::string idle =
        ValueOr<std::string>(oms, "idle_policy", "busy_spin");
    if (idle == "busy_spin")
      result.idle_policy = IdlePolicy::BusySpin;
    else if (idle == "adaptive")
      result.idle_policy = IdlePolicy::Adaptive;
    else if (idle == "low_cpu")
      result.idle_policy = IdlePolicy::LowCpu;
    else
      throw std::runtime_error("unsupported oms.idle_policy");
    result.strategy_cpu = ValueOr<std::int32_t>(oms, "strategy_cpu", -1);
    result.io_cpu = ValueOr<std::int32_t>(oms, "io_cpu", -1);
    result.numa_node = ValueOr<std::int32_t>(oms, "numa_node", -1);
    const std::string strictness =
        ValueOr<std::string>(oms, "strictness", "strict");
    if (strictness == "strict")
      result.strictness = ConfigStrictness::Strict;
    else if (strictness == "relaxed")
      result.strictness = ConfigStrictness::Relaxed;
    else
      throw std::runtime_error("unsupported oms.strictness");
    if (result.strictness == ConfigStrictness::Strict &&
        !mds["bbo_policy"] &&
        HasSegmentRole(result.mds, ".ticker.") &&
        HasSegmentRole(result.mds, ".orderbook.")) {
      throw std::runtime_error(
          "strict ticker+orderbook subscriptions require mds.bbo_policy");
    }
    result.session_epoch =
        ValueOr<std::uint32_t>(oms, "session_epoch", 1);
    result.event_budget =
        ValueOr<std::uint32_t>(oms, "event_budget", 64);
    result.metric_sample_rate =
        ValueOr<std::uint32_t>(
            oms, "metric_sample_rate", result.metric_sample_rate);
    result.retire_deferred_warning_count = ValueOr<std::uint32_t>(
        oms, "retire_deferred_warning_count",
        result.retire_deferred_warning_count);
    result.startup_timeout_ns = ValueOr<std::uint64_t>(
        oms, "startup_timeout_ns", result.startup_timeout_ns);
    if (result.session_epoch == 0 || result.event_budget == 0 ||
        result.metric_sample_rate == 0 ||
        result.retire_deferred_warning_count == 0 ||
        result.startup_timeout_ns == 0)
      throw std::runtime_error("invalid OMS scalar setting");

    if (const YAML::Node memory = oms["memory"]) {
      RejectUnknown(memory, {"lock_pages", "prefault", "strict"},
                    "oms.memory");
      result.memory.lock_pages = ValueOr<bool>(memory, "lock_pages", false);
      result.memory.prefault = ValueOr<bool>(memory, "prefault", true);
      result.memory.strict = ValueOr<bool>(memory, "strict", false);
    }
    if (const YAML::Node socket = oms["socket"]) {
      RejectUnknown(socket,
                    {"tcp_nodelay", "receive_buffer_bytes",
                     "send_buffer_bytes", "busy_poll_us"},
                    "oms.socket");
      result.socket.tcp_nodelay =
          ValueOr<bool>(socket, "tcp_nodelay", true);
      result.socket.receive_buffer_bytes =
          ValueOr<std::int32_t>(socket, "receive_buffer_bytes", 0);
      result.socket.send_buffer_bytes =
          ValueOr<std::int32_t>(socket, "send_buffer_bytes", 0);
      result.socket.busy_poll_us =
          ValueOr<std::int32_t>(socket, "busy_poll_us", 0);
    }
    if (const YAML::Node capacities = oms["capacities"]) {
      RejectUnknown(capacities,
                    {"command_queue", "update_queue", "market_data_queue",
                     "order_table", "position_table", "fill_dedup",
                     "timer_table", "instrument_directory"},
                    "oms.capacities");
      result.capacities.command_queue = ValueOr<std::uint32_t>(
          capacities, "command_queue", result.capacities.command_queue);
      result.capacities.update_queue = ValueOr<std::uint32_t>(
          capacities, "update_queue", result.capacities.update_queue);
      result.capacities.market_data_queue = ValueOr<std::uint32_t>(
          capacities, "market_data_queue",
          result.capacities.market_data_queue);
      result.capacities.order_table = ValueOr<std::uint32_t>(
          capacities, "order_table", result.capacities.order_table);
      result.capacities.position_table = ValueOr<std::uint32_t>(
          capacities, "position_table", result.capacities.position_table);
      result.capacities.fill_dedup = ValueOr<std::uint32_t>(
          capacities, "fill_dedup", result.capacities.fill_dedup);
      result.capacities.timer_table = ValueOr<std::uint32_t>(
          capacities, "timer_table", result.capacities.timer_table);
      result.capacities.instrument_directory = ValueOr<std::uint32_t>(
          capacities, "instrument_directory",
          result.capacities.instrument_directory);
    }
    ValidateCapacities(result.capacities);

    if (const YAML::Node instruments = oms["instruments"]) {
      RequireSequence(instruments, "oms.instruments");
      for (const auto& item : instruments) {
        RejectUnknown(
            item,
            {"instrument_id", "venue", "product", "symbol", "price_scale",
             "quantity_scale", "tick_size", "lot_size",
             "polymarket_condition_id", "polymarket_token_id",
             "polymarket_outcome", "polymarket_negative_risk",
             "polymarket_signature_type", "minimum_order_size"},
            "oms.instruments[]");
        InstrumentConfig instrument;
        instrument.instrument_id =
            item["instrument_id"].as<InstrumentId>();
        instrument.venue =
            ParseVenue(item["venue"].as<std::string>());
        instrument.product =
            ParseProduct(item["product"].as<std::string>());
        instrument.symbol = item["symbol"].as<std::string>();
        instrument.price_scale =
            item["price_scale"].as<std::uint8_t>();
        instrument.quantity_scale =
            item["quantity_scale"].as<std::uint8_t>();
        instrument.tick_size = item["tick_size"].as<std::int64_t>();
        instrument.lot_size = item["lot_size"].as<std::int64_t>();
        instrument.polymarket_condition_id = ValueOr<std::string>(
            item, "polymarket_condition_id", {});
        instrument.polymarket_token_id =
            ValueOr<std::string>(item, "polymarket_token_id", {});
        instrument.polymarket_outcome =
            ValueOr<std::uint8_t>(item, "polymarket_outcome", 0);
        instrument.polymarket_negative_risk =
            ValueOr<bool>(item, "polymarket_negative_risk", false);
        instrument.polymarket_signature_type =
            ValueOr<std::uint8_t>(item, "polymarket_signature_type", 0);
        instrument.minimum_order_size =
            ValueOr<std::int64_t>(item, "minimum_order_size", 0);
        if (instrument.instrument_id == 0 || instrument.symbol.empty() ||
            instrument.tick_size <= 0 || instrument.lot_size <= 0) {
          throw std::runtime_error("invalid OMS instrument");
        }
        result.instruments.push_back(std::move(instrument));
      }
    }

    const YAML::Node venues = oms["venues"];
    if (venues) {
      RequireMap(venues, "oms.venues");
      for (const auto& entry : venues) {
        const std::string venue_name = entry.first.as<std::string>();
        const YAML::Node venue = entry.second;
        RejectUnknown(
            venue,
            {"enabled", "account_id", "rest", "trading_websocket",
             "user_websocket",
             "api_key_env", "secret_env", "passphrase_env",
             "signer_address_env", "funder_address_env", "private_key_env"},
            "oms.venues." + venue_name);
        VenueExecutionConfig config;
        config.kind = ParseVenueKind(venue_name);
        config.account_id =
            ValueOr<AccountId>(venue, "account_id", 0);
        config.enabled = ValueOr<bool>(venue, "enabled", false);
        config.rest = ParseEndpoint(venue["rest"], venue_name + ".rest");
        config.trading_websocket = ParseEndpoint(
            venue["trading_websocket"], venue_name + ".trading_websocket");
        config.user_websocket = ParseEndpoint(
            venue["user_websocket"], venue_name + ".user_websocket");
        config.api_key_env =
            ValueOr<std::string>(venue, "api_key_env", {});
        config.secret_env =
            ValueOr<std::string>(venue, "secret_env", {});
        config.passphrase_env =
            ValueOr<std::string>(venue, "passphrase_env", {});
        config.signer_address_env =
            ValueOr<std::string>(venue, "signer_address_env", {});
        config.funder_address_env =
            ValueOr<std::string>(venue, "funder_address_env", {});
        config.private_key_env =
            ValueOr<std::string>(venue, "private_key_env", {});
        if (config.enabled &&
            (config.account_id == 0 || config.api_key_env.empty() ||
             config.secret_env.empty())) {
          throw std::runtime_error("enabled venue requires credential envs");
        }
        result.venues.push_back(std::move(config));
      }
    }

    auto parameters = std::make_shared<StrategyParams::Impl>();
    if (const YAML::Node strategy = root["strategy"]) {
      RequireMap(strategy, "strategy");
      FlattenStrategy(strategy, {}, *parameters);
    }
    result.strategy = StrategyParams(std::move(parameters));
    return {std::move(result), Error::Ok};
  } catch (...) {
    return {{}, Error::InvalidConfig};
  }
}

}  // namespace strategyframe
