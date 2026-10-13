#include "ibkr_config.h"

#include <yaml-cpp/yaml.h>

#include <algorithm>
#include <cctype>
#include <cmath>
#include <set>
#include <stdexcept>
#include <string_view>

namespace mds::ibkr {
namespace {

void require_map(const YAML::Node &node, std::string_view context) {
  if (!node || !node.IsMap()) {
    throw std::runtime_error(std::string(context) + " must be a map");
  }
}

void require_sequence(const YAML::Node &node, std::string_view context) {
  if (!node || !node.IsSequence()) {
    throw std::runtime_error(std::string(context) + " must be a sequence");
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
T value_or(const YAML::Node &node, std::string_view key, T fallback) {
  if (!node || !node.IsMap()) {
    return fallback;
  }
  const auto child = node[std::string(key)];
  return child ? child.as<T>() : std::move(fallback);
}

std::string upper(std::string value) {
  for (char &character : value) {
    character = static_cast<char>(std::toupper(static_cast<unsigned char>(character)));
  }
  return value;
}

bool power_of_two(std::size_t value) noexcept {
  return value != 0 && (value & (value - 1)) == 0;
}

bool valid_symbol(std::string_view symbol) {
  if (symbol.empty() || symbol.size() >= 32 || symbol.find('|') != std::string_view::npos) {
    return false;
  }
  return std::all_of(symbol.begin(), symbol.end(), [](unsigned char character) {
    return std::isalnum(character) != 0 || character == '.' || character == '-';
  });
}

bool cash_pair(std::string_view canonical, std::string &base, std::string &quote) {
  if (canonical.size() != 6) {
    return false;
  }
  if (!std::all_of(canonical.begin(), canonical.end(), [](unsigned char character) {
        return std::isalpha(character) != 0;
      })) {
    return false;
  }
  base.assign(canonical.substr(0, 3));
  quote.assign(canonical.substr(3));
  return true;
}

std::string request_identity(const InstrumentSpec &spec) {
  return spec.canonical_symbol + "|" + spec.request_symbol + "|" + spec.sec_type +
         "|" + spec.exchange + "|" + spec.primary_exchange + "|" + spec.currency +
         "|" + std::to_string(spec.con_id);
}

bool valid_env_name(std::string_view name) {
  if (name.empty()) {
    return false;
  }
  const auto first = static_cast<unsigned char>(name.front());
  if (std::isalpha(first) == 0 && first != '_') {
    return false;
  }
  return std::all_of(name.begin(), name.end(), [](unsigned char character) {
    return std::isalnum(character) != 0 || character == '_';
  });
}

void commit_subscription(IbkrConfig &config, InstrumentSpec spec,
                         std::set<std::string> &identities) {
  const bool stock_primary_missing =
      spec.sec_type == "STK" && spec.con_id == 0 && spec.primary_exchange.empty();
  if (!valid_symbol(spec.canonical_symbol) || spec.con_id < 0 ||
      spec.quantity_scale > 18 || stock_primary_missing ||
      (spec.sec_type == "STK" &&
       (spec.exchange != "SMART" || spec.currency != "USD")) ||
      (spec.sec_type == "CMDTY" && spec.currency.empty()) ||
      (spec.sec_type == "CASH" && spec.exchange != "IDEALPRO") ||
      spec.request_symbol.empty()) {
    throw std::runtime_error("ibkr subscription is invalid for " +
                             spec.canonical_symbol);
  }
  if (!identities.insert(request_identity(spec)).second) {
    throw std::runtime_error("duplicate ibkr subscription identity " +
                             request_identity(spec));
  }
  config.runtime.subscriptions.push_back(std::move(spec));
}

}  // namespace

api::Result<IbkrConfig> load_config(const std::string &path) {
  try {
    const auto root = YAML::LoadFile(path);
    reject_unknown(root,
                   {"ibkr", "shared_memory", "subscriptions", "postgres",
                    "selection"},
                   "root");
    const auto ibkr = root["ibkr"];
    const auto shared = root["shared_memory"];
    const auto subscriptions = root["subscriptions"];
    reject_unknown(ibkr,
                   {"host", "port", "client_id", "connect_timeout_ms",
                    "reconnect_initial_ms", "reconnect_max_ms",
                    "heartbeat_interval_ms", "market_data_mode",
                    "max_subscriptions", "subscribe_pacing_per_second"},
                   "ibkr");
    reject_unknown(shared,
                   {"prefix", "backend", "mode", "ring_layout", "shard_count",
                    "multiplex_ring_bytes", "ring_bytes", "max_record_bytes",
                    "max_readers", "unlink_on_shutdown"},
                   "shared_memory");
    require_sequence(subscriptions, "subscriptions");
    if (root["postgres"]) {
      const auto postgres = root["postgres"];
      reject_unknown(postgres, {"url_env"}, "postgres");
      const auto postgres_env = value_or<std::string>(postgres, "url_env", "");
      if (!valid_env_name(postgres_env)) {
        throw std::runtime_error("ibkr postgres url_env is invalid");
      }
    }
    const auto selection = root["selection"];
    if (selection) {
      reject_unknown(selection,
                     {"min_average_volume_90d", "min_market_cap_usd"},
                     "selection");
    }

    IbkrConfig config;
    config.runtime.host = value_or<std::string>(ibkr, "host", config.runtime.host);
    config.runtime.port = value_or<int>(ibkr, "port", config.runtime.port);
    config.runtime.client_id = value_or<int>(ibkr, "client_id", config.runtime.client_id);
    config.runtime.connect_timeout_ms = value_or<int>(
        ibkr, "connect_timeout_ms", config.runtime.connect_timeout_ms);
    config.runtime.reconnect_initial_ms = value_or<int>(
        ibkr, "reconnect_initial_ms", config.runtime.reconnect_initial_ms);
    config.runtime.reconnect_max_ms = value_or<int>(
        ibkr, "reconnect_max_ms", config.runtime.reconnect_max_ms);
    config.runtime.heartbeat_interval_ms = value_or<int>(
        ibkr, "heartbeat_interval_ms", config.runtime.heartbeat_interval_ms);
    config.market_data_mode =
        upper(value_or<std::string>(ibkr, "market_data_mode", "LIVE_ONLY"));
    config.max_subscriptions = value_or<std::size_t>(
        ibkr, "max_subscriptions", kHardMaxSubscriptions);
    config.runtime.subscribe_pacing_per_second = value_or<int>(
        ibkr, "subscribe_pacing_per_second",
        config.runtime.subscribe_pacing_per_second);
    if (root["postgres"]) {
      config.postgres_url_env =
          value_or<std::string>(root["postgres"], "url_env", "");
    }
    if (selection && selection["min_average_volume_90d"]) {
      config.runtime.filter.min_average_volume_90d =
          selection["min_average_volume_90d"].as<double>();
    }
    if (selection && selection["min_market_cap_usd"]) {
      config.runtime.filter.min_market_cap_usd =
          selection["min_market_cap_usd"].as<double>();
    }
    config.runtime.shm_prefix =
        value_or<std::string>(shared, "prefix", config.runtime.shm_prefix);
    config.backend = upper(value_or<std::string>(shared, "backend", "POSIX_SHM"));
    config.mode = upper(value_or<std::string>(shared, "mode", "OVERWRITE_OLDEST"));
    config.ring_layout =
        value_or<std::string>(shared, "ring_layout", "multiplex");
    config.shard_count = value_or<std::size_t>(shared, "shard_count", 1);
    config.runtime.ring_bytes = value_or<std::size_t>(
        shared, "multiplex_ring_bytes",
        value_or<std::size_t>(shared, "ring_bytes", config.runtime.ring_bytes));
    config.runtime.max_record_bytes = value_or<std::size_t>(
        shared, "max_record_bytes", config.runtime.max_record_bytes);
    config.runtime.max_readers =
        value_or<std::size_t>(shared, "max_readers", config.runtime.max_readers);
    config.runtime.unlink_on_shutdown =
        value_or<bool>(shared, "unlink_on_shutdown", false);

    if (config.runtime.host.empty() || config.runtime.port <= 0 ||
        config.runtime.port > 65535 || config.runtime.client_id < 0 ||
        config.runtime.client_id > 31 || config.runtime.connect_timeout_ms <= 0 ||
        config.runtime.reconnect_initial_ms <= 0 ||
        config.runtime.reconnect_max_ms < config.runtime.reconnect_initial_ms ||
        config.runtime.heartbeat_interval_ms <= 0 ||
        config.market_data_mode != "LIVE_ONLY" ||
        config.max_subscriptions == 0 ||
        config.max_subscriptions > kHardMaxSubscriptions ||
        config.runtime.subscribe_pacing_per_second <= 0 ||
        config.runtime.subscribe_pacing_per_second > 1000) {
      throw std::runtime_error("ibkr connection settings are invalid");
    }
    config.runtime.filter.max_subscriptions = config.max_subscriptions;
    const auto &filter = config.runtime.filter;
    const auto invalid_threshold = [](const std::optional<double> &value) {
      return value.has_value() &&
             (!std::isfinite(*value) || *value < 0.0);
    };
    if (invalid_threshold(filter.min_average_volume_90d) ||
        invalid_threshold(filter.min_market_cap_usd)) {
      throw std::runtime_error("ibkr selection settings are invalid");
    }
    if (config.runtime.shm_prefix.size() < 2 ||
        config.runtime.shm_prefix.front() != '/' || config.backend != "POSIX_SHM" ||
        config.mode != "OVERWRITE_OLDEST" || config.ring_layout != "multiplex" ||
        config.shard_count != 1 || !power_of_two(config.runtime.ring_bytes) ||
        config.runtime.max_record_bytes == 0 ||
        config.runtime.max_record_bytes > config.runtime.ring_bytes ||
        config.runtime.max_readers == 0 || config.runtime.max_readers > 64) {
      throw std::runtime_error("ibkr shared_memory settings are invalid");
    }

    std::set<std::string> identities;
    for (const auto &node : subscriptions) {
      reject_unknown(node,
                     {"canonical_symbol", "con_id", "sec_type", "exchange",
                      "primary_exchange", "currency", "price_scale",
                      "quantity_scale"},
                     "subscriptions[]");
      InstrumentSpec spec;
      spec.canonical_symbol = upper(node["canonical_symbol"].as<std::string>());
      spec.sec_type = upper(value_or<std::string>(node, "sec_type", "STK"));
      if (node["con_id"]) {
        spec.con_id = node["con_id"].as<std::int64_t>();
      }
      spec.quantity_scale =
          static_cast<std::uint8_t>(value_or<int>(node, "quantity_scale", 4));
      if (node["price_scale"]) {
        const auto scale = node["price_scale"].as<int>();
        if (scale < 0 || scale > 18) {
          throw std::runtime_error("ibkr subscription is invalid for " +
                                   spec.canonical_symbol);
        }
        spec.price_scale = static_cast<std::uint8_t>(scale);
      } else if (spec.sec_type == "STK") {
        spec.price_scale = 4;
      } else {
        spec.price_scale = std::nullopt;
      }
      if (spec.sec_type == "STK") {
        spec.exchange = upper(value_or<std::string>(node, "exchange", "SMART"));
        spec.currency = upper(value_or<std::string>(node, "currency", "USD"));
        spec.primary_exchange =
            upper(value_or<std::string>(node, "primary_exchange", ""));
        spec.request_symbol = spec.canonical_symbol;
      } else if (spec.sec_type == "CMDTY") {
        spec.exchange = upper(value_or<std::string>(node, "exchange", "SMART"));
        spec.currency = upper(value_or<std::string>(node, "currency", "USD"));
        spec.primary_exchange =
            upper(value_or<std::string>(node, "primary_exchange", ""));
        spec.request_symbol = spec.canonical_symbol;
      } else if (spec.sec_type == "CASH") {
        std::string base;
        std::string quote;
        if (!cash_pair(spec.canonical_symbol, base, quote)) {
          throw std::runtime_error(
              "ibkr subscription is invalid for " + spec.canonical_symbol);
        }
        spec.request_symbol = base;
        spec.currency = upper(value_or<std::string>(node, "currency", quote));
        spec.exchange = upper(value_or<std::string>(node, "exchange", "IDEALPRO"));
        spec.primary_exchange =
            upper(value_or<std::string>(node, "primary_exchange", ""));
        if (spec.currency != quote) {
          throw std::runtime_error(
              "ibkr subscription is invalid for " + spec.canonical_symbol);
        }
      } else {
        throw std::runtime_error(
            "ibkr subscription is invalid for " + spec.canonical_symbol);
      }
      commit_subscription(config, std::move(spec), identities);
    }
    if (!config.postgres_url_env.empty() &&
        !config.runtime.subscriptions.empty()) {
      throw std::runtime_error(
          "ibkr postgres and subscriptions are mutually exclusive");
    }
    if (config.postgres_url_env.empty() &&
        config.runtime.subscriptions.empty()) {
      throw std::runtime_error(
          "ibkr requires postgres or at least one subscription");
    }
    return {.value = std::move(config)};
  } catch (const std::exception &error) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = std::string("failed to load ibkr producer config: ") +
                       error.what()};
  }
}

}  // namespace mds::ibkr
