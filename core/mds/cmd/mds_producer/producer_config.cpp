#include "producer_config.h"

#include "mds/transport/shared_ring.h"
#include "utils/md/order_book.h"
#include "utils/md/wire.h"

#include <yaml-cpp/yaml.h>

#include <algorithm>
#include <cctype>
#include <cstdlib>
#include <map>
#include <regex>
#include <set>
#include <stdexcept>
#include <string_view>
#include <tuple>
#include <utility>

namespace mds::producer {
namespace {

using Product = utils::md::ProductType;
using Venue = utils::md::Venue;

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
  if (!node || !node.IsMap()) return fallback;
  const auto child = node[std::string(key)];
  return child ? child.as<T>() : std::move(fallback);
}

std::string uppercase(std::string value) {
  std::transform(value.begin(), value.end(), value.begin(),
                 [](unsigned char character) {
                   return static_cast<char>(std::toupper(character));
                 });
  return value;
}

std::string lowercase(std::string value) {
  std::transform(value.begin(), value.end(), value.begin(),
                 [](unsigned char character) {
                   return static_cast<char>(std::tolower(character));
                 });
  return value;
}

bool valid_symbol(std::string_view symbol) {
  return !symbol.empty() && symbol.size() <= 32 &&
         std::all_of(symbol.begin(), symbol.end(), [](unsigned char value) {
           return std::isalnum(value) != 0;
         });
}

bool power_of_two(std::size_t value) noexcept {
  return value != 0 && (value & (value - 1U)) == 0;
}

bool valid_snapshot_depth(Product product, std::size_t depth) {
  static constexpr std::size_t spot[] = {5,   10,  20,   50,
                                         100, 500, 1000, 5000};
  static constexpr std::size_t perpetual[] = {5,  10,  20,  50,
                                              100, 500, 1000};
  const std::size_t *allowed =
      product == Product::Spot ? spot : perpetual;
  const std::size_t count =
      product == Product::Spot ? std::size(spot) : std::size(perpetual);
  return std::find(allowed, allowed + count, depth) != allowed + count;
}

const char *required_environment(std::string_view name) {
  if (name.empty()) {
    return nullptr;
  }
  const std::string owned(name);
  const auto *value = std::getenv(owned.c_str());
  return value != nullptr && value[0] != '\0' ? value : nullptr;
}

struct VenueProductKey {
  Venue venue{Venue::Unknown};
  Product product{Product::Unknown};

  friend bool operator<(const VenueProductKey &left,
                        const VenueProductKey &right) noexcept {
    return std::tie(left.venue, left.product) <
           std::tie(right.venue, right.product);
  }
};

struct StreamKey {
  Venue venue{Venue::Unknown};
  Product product{Product::Unknown};
  std::string symbol;

  friend bool operator<(const StreamKey &left,
                        const StreamKey &right) noexcept {
    return std::tie(left.venue, left.product, left.symbol) <
           std::tie(right.venue, right.product, right.symbol);
  }
};

Venue parse_required_venue(const YAML::Node &node) {
  const auto parsed = exchange::parse_venue(node.as<std::string>());
  if (!parsed) {
    throw std::runtime_error("unsupported venue '" +
                             node.as<std::string>() + "'");
  }
  return *parsed;
}

Product parse_required_product(const YAML::Node &node) {
  const auto parsed = exchange::parse_product(node.as<std::string>());
  if (!parsed) {
    throw std::runtime_error("unsupported product '" +
                             node.as<std::string>() + "'");
  }
  return *parsed;
}

void validate_auth(const VenueEndpoint &endpoint,
                   const StreamSpec &stream) {
  if (!stream.requires_public_ws_login) {
    return;
  }
  if (!endpoint.auth.configured()) {
    throw std::runtime_error(
        "OKX fastest orderbook channel requires auth.api_key_env, "
        "auth.secret_env and auth.passphrase_env");
  }
  if (required_environment(endpoint.auth.api_key_env) == nullptr ||
      required_environment(endpoint.auth.secret_env) == nullptr ||
      required_environment(endpoint.auth.passphrase_env) == nullptr) {
    throw std::runtime_error(
        "OKX public WebSocket credential environment variable is missing or "
        "empty");
  }
}

}  // namespace

api::Result<ProducerConfig> load_config(const std::string &path) noexcept {
  try {
    const auto root = YAML::LoadFile(path);
    reject_unknown(root,
                   {"shared_memory", "order_book", "venues", "subscriptions",
                    "capacity"},
                   "root");

    const auto shared = root["shared_memory"];
    reject_unknown(shared,
                   {"prefix", "backend", "mode", "hugetlbfs_mount",
                    "ring_bytes", "max_record_bytes", "max_readers",
                    "reader_lease_timeout_ns", "allow_hugepage_fallback",
                    "unlink_on_shutdown", "ring_layout", "shard_count",
                    "multiplex_ring_bytes"},
                   "shared_memory");
    const auto prefix =
        value_or<std::string>(shared, "prefix", "/selfquant.mds");
    if (prefix.size() < 2 || prefix.front() != '/') {
      throw std::runtime_error(
          "shared_memory.prefix must start with '/' and not be '/'");
    }
    transport::RingOptions ring;
    const auto backend =
        uppercase(value_or<std::string>(shared, "backend", "POSIX_SHM"));
    if (backend == "POSIX_SHM") {
      ring.backend = api::ShmBackend::PosixShm;
    } else if (backend == "HUGETLBFS") {
      ring.backend = api::ShmBackend::Hugetlbfs;
    } else {
      throw std::runtime_error("shared_memory.backend is invalid");
    }
    const auto mode =
        uppercase(value_or<std::string>(shared, "mode", "OVERWRITE_OLDEST"));
    if (mode == "OVERWRITE_OLDEST") {
      ring.mode = api::RingMode::OverwriteOldest;
    } else if (mode == "LOSSLESS") {
      ring.mode = api::RingMode::Lossless;
    } else {
      throw std::runtime_error("shared_memory.mode is invalid");
    }
    ring.hugetlbfs_mount =
        value_or<std::string>(shared, "hugetlbfs_mount", "/dev/hugepages");
    ring.ring_bytes = value_or<std::size_t>(shared, "ring_bytes", 8U << 20U);
    ring.max_record_bytes =
        value_or<std::size_t>(shared, "max_record_bytes", 64U << 10U);
    ring.max_readers = value_or<std::size_t>(shared, "max_readers", 32);
    ring.allow_hugepage_fallback =
        value_or<bool>(shared, "allow_hugepage_fallback", false);
    ring.unlink_on_close =
        value_or<bool>(shared, "unlink_on_shutdown", false);
    const auto layout_text =
        lowercase(value_or<std::string>(shared, "ring_layout", "per_symbol"));
    publish::RingLayout ring_layout;
    if (layout_text == "per_symbol") {
      ring_layout = publish::RingLayout::PerSymbol;
    } else if (layout_text == "multiplex") {
      ring_layout = publish::RingLayout::Multiplex;
    } else if (layout_text == "both") {
      ring_layout = publish::RingLayout::Both;
    } else {
      throw std::runtime_error(
          "shared_memory.ring_layout must be per_symbol, multiplex or both");
    }
    const auto shard_count =
        value_or<std::size_t>(shared, "shard_count", 1);
    auto multiplex_ring = ring;
    multiplex_ring.ring_bytes = value_or<std::size_t>(
        shared, "multiplex_ring_bytes", ring.ring_bytes);
    const auto lease_timeout = value_or<std::uint64_t>(
        shared, "reader_lease_timeout_ns", 5'000'000'000ULL);
    constexpr auto minimum_record_bytes =
        sizeof(transport::RecordHeader) +
        sizeof(utils::md::wire::SnapshotChunkRecord);
    if (!power_of_two(ring.ring_bytes) ||
        ring.max_record_bytes < minimum_record_bytes ||
        ring.max_record_bytes > ring.ring_bytes || ring.max_readers == 0 ||
        ring.max_readers > transport::kMaxReaders || lease_timeout == 0) {
      throw std::runtime_error("shared_memory bounds are invalid");
    }
    if (shard_count == 0 || shard_count > 256 ||
        !power_of_two(multiplex_ring.ring_bytes) ||
        multiplex_ring.max_record_bytes > multiplex_ring.ring_bytes) {
      throw std::runtime_error("shared_memory multiplex bounds are invalid");
    }

    const auto capacity = root["capacity"];
    if (capacity) {
      reject_unknown(capacity,
                     {"max_instruments", "max_rings",
                      "max_total_ring_bytes"},
                     "capacity");
    }
    const auto max_instruments =
        value_or<std::size_t>(capacity, "max_instruments", 4096);
    const auto max_rings =
        value_or<std::size_t>(capacity, "max_rings", 4096);
    const auto max_total_ring_bytes = value_or<std::uint64_t>(
        capacity, "max_total_ring_bytes", 16ULL << 30U);
    if (max_instruments == 0 || max_instruments > 4096 ||
        max_rings == 0 || max_total_ring_bytes == 0) {
      throw std::runtime_error("capacity bounds are invalid");
    }

    const auto order_book = root["order_book"];
    if (order_book) {
      reject_unknown(order_book,
                     {"default_ladder_ticks_per_side",
                      "max_ladder_ticks_per_side",
                      "default_price_band_bps",
                      "default_ladder_levels_per_side",
                      "max_ladder_levels_per_side"},
                     "order_book");
    }
    const bool has_new_default =
        order_book && bool(order_book["default_ladder_ticks_per_side"]);
    const bool has_old_default =
        order_book && bool(order_book["default_ladder_levels_per_side"]);
    const bool has_new_max =
        order_book && bool(order_book["max_ladder_ticks_per_side"]);
    const bool has_old_max =
        order_book && bool(order_book["max_ladder_levels_per_side"]);
    if ((has_new_default && has_old_default) ||
        (has_new_max && has_old_max)) {
      throw std::runtime_error(
          "do not mix ladder_ticks_per_side with deprecated "
          "ladder_levels_per_side aliases");
    }
    const auto default_ladder = has_new_default
                                    ? order_book["default_ladder_ticks_per_side"]
                                          .as<std::size_t>()
                                    : value_or<std::size_t>(
                                          order_book,
                                          "default_ladder_levels_per_side",
                                          8192);
    const auto max_ladder =
        has_new_max
            ? order_book["max_ladder_ticks_per_side"].as<std::size_t>()
            : value_or<std::size_t>(order_book,
                                    "max_ladder_levels_per_side",
                                    utils::md::kMaxLadderLevels);
    if (default_ladder == 0 || default_ladder > max_ladder ||
        max_ladder > utils::md::kMaxLadderLevels) {
      throw std::runtime_error("order_book ladder bounds are invalid");
    }
    const auto default_price_band_bps =
        value_or<std::uint32_t>(order_book, "default_price_band_bps", 10);
    if (default_price_band_bps == 0 ||
        default_price_band_bps > 10'000) {
      throw std::runtime_error(
          "order_book.default_price_band_bps must be in [1, 10000]");
    }

    std::map<VenueProductKey, VenueEndpoint> endpoints;
    const auto venue_nodes = root["venues"];
    require_sequence(venue_nodes, "venues");
    for (const auto &node : venue_nodes) {
      reject_unknown(node,
                     {"venue", "product", "websocket_endpoint",
                      "rest_endpoint", "discovery_endpoint", "protocol", "redundant_ab",
                      "allow_json_fallback", "auth",
                      "max_symbols_per_connection", "max_symbols_per_ws",
                      "snapshot_pacing_ms",
                      "snapshot_failure_backoff_ms",
                      "snapshot_failure_backoff_max_ms",
                      "snapshot_max_consecutive_failures",
                      "snapshot_rate_limit_backoff_ms",
                      "snapshot_ban_backoff_ms",
                      "max_continuous_recovery_ms"},
                     "venues[]");
      const auto venue = parse_required_venue(node["venue"]);
      const auto product = parse_required_product(node["product"]);
      if (exchange::capabilities(venue, product) == nullptr) {
        throw std::runtime_error("unsupported venue/product combination");
      }
      const auto protocol =
          uppercase(value_or<std::string>(node, "protocol", "JSON"));
      if (protocol != "JSON" ||
          value_or<bool>(node, "redundant_ab", false)) {
        throw std::runtime_error(
            "only JSON without A/B redundancy is currently supported");
      }
      VenueEndpoint endpoint;
      endpoint.venue = venue;
      endpoint.product = product;
      endpoint.websocket_endpoint =
          node["websocket_endpoint"].as<std::string>();
      endpoint.rest_endpoint =
          value_or<std::string>(node, "rest_endpoint", "");
      endpoint.discovery_endpoint =
          value_or<std::string>(node, "discovery_endpoint", "");
      endpoint.max_symbols_per_connection =
          value_or<std::size_t>(node, "max_symbols_per_connection", 50);
      endpoint.max_symbols_per_ws =
          value_or<std::size_t>(node, "max_symbols_per_ws", 0);
      endpoint.snapshot_pacing_ms =
          value_or<std::uint32_t>(node, "snapshot_pacing_ms", 100);
      endpoint.snapshot_failure_backoff_ms = value_or<std::uint32_t>(
          node, "snapshot_failure_backoff_ms", 250);
      endpoint.snapshot_failure_backoff_max_ms = value_or<std::uint32_t>(
          node, "snapshot_failure_backoff_max_ms", 30'000);
      endpoint.snapshot_max_consecutive_failures = value_or<std::uint32_t>(
          node, "snapshot_max_consecutive_failures", 10);
      endpoint.snapshot_rate_limit_backoff_ms = value_or<std::uint32_t>(
          node, "snapshot_rate_limit_backoff_ms", 60'000);
      endpoint.snapshot_ban_backoff_ms = value_or<std::uint32_t>(
          node, "snapshot_ban_backoff_ms", 300'000);
      endpoint.max_continuous_recovery_ms = value_or<std::uint32_t>(
          node, "max_continuous_recovery_ms", 300'000);
      const bool polymarket =
          venue == Venue::Polymarket &&
          product == Product::BinaryOption;
      if (endpoint.websocket_endpoint.empty() ||
          (!polymarket && endpoint.rest_endpoint.empty()) ||
          (polymarket && endpoint.discovery_endpoint.empty()) ||
          endpoint.max_symbols_per_connection == 0 ||
          endpoint.snapshot_pacing_ms == 0 ||
          endpoint.snapshot_failure_backoff_ms == 0 ||
          endpoint.snapshot_failure_backoff_max_ms <
              endpoint.snapshot_failure_backoff_ms ||
          endpoint.snapshot_max_consecutive_failures == 0 ||
          endpoint.snapshot_rate_limit_backoff_ms == 0 ||
          endpoint.snapshot_ban_backoff_ms <
              endpoint.snapshot_rate_limit_backoff_ms ||
          endpoint.max_continuous_recovery_ms == 0) {
        throw std::runtime_error("venue endpoint bounds are invalid");
      }
      if (const auto auth = node["auth"]) {
        if (venue != Venue::Okx) {
          throw std::runtime_error(
              "venues[].auth is supported only for OKX public WebSocket "
              "login");
        }
        reject_unknown(auth,
                       {"api_key_env", "secret_env", "passphrase_env"},
                       "venues[].auth");
        endpoint.auth.api_key_env =
            value_or<std::string>(auth, "api_key_env", "");
        endpoint.auth.secret_env =
            value_or<std::string>(auth, "secret_env", "");
        endpoint.auth.passphrase_env =
            value_or<std::string>(auth, "passphrase_env", "");
        const bool any = !endpoint.auth.api_key_env.empty() ||
                         !endpoint.auth.secret_env.empty() ||
                         !endpoint.auth.passphrase_env.empty();
        if (any && !endpoint.auth.configured()) {
          throw std::runtime_error(
              "venues[].auth must provide all three environment names");
        }
      }
      const VenueProductKey key{venue, product};
      const auto [found, inserted] = endpoints.emplace(key, endpoint);
      (void)found;
      if (!inserted) {
        throw std::runtime_error(
            "venues[] must be unique by (venue, product)");
      }
    }

    std::map<StreamKey, StreamSpec> streams;
    const auto subscription_nodes = root["subscriptions"];
    require_sequence(subscription_nodes, "subscriptions");
    for (const auto &node : subscription_nodes) {
      reject_unknown(node,
                     {"stream", "venue", "symbol", "symbols", "discovery",
                      "product", "depth",
                      "ladder_ticks_per_side", "ladder_levels_per_side",
                      "price_band_bps", "orderbook_channel",
                      "update_interval_ms", "polymarket"},
                     "subscriptions[]");
      const auto venue = node["venue"] ? parse_required_venue(node["venue"])
                                       : Venue::Binance;
      const auto product = parse_required_product(node["product"]);
      const bool polymarket =
          venue == Venue::Polymarket &&
          product == Product::BinaryOption;
      const bool has_symbol = bool(node["symbol"]);
      const bool has_symbols = bool(node["symbols"]);
      const bool has_discovery = bool(node["discovery"]);
      if (std::size_t(has_symbol) + std::size_t(has_symbols) +
              std::size_t(has_discovery) !=
          1) {
        throw std::runtime_error(
            "subscription requires exactly one of symbol, symbols or "
            "discovery");
      }
      std::vector<std::string> logical_symbols;
      std::optional<ProductDiscovery> discovery;
      if (has_symbol) {
        logical_symbols.push_back(node["symbol"].as<std::string>());
      } else if (has_symbols) {
        require_sequence(node["symbols"], "subscriptions[].symbols");
        for (const auto &symbol_node : node["symbols"]) {
          logical_symbols.push_back(symbol_node.as<std::string>());
        }
        if (logical_symbols.empty()) {
          throw std::runtime_error(
              "subscriptions[].symbols must not be empty");
        }
      } else {
        const auto source = node["discovery"];
        reject_unknown(source,
                       {"quote_assets", "symbol_regex",
                        "minimum_turnover", "max_symbols"},
                       "subscriptions[].discovery");
        ProductDiscovery parsed;
        if (const auto quotes = source["quote_assets"]) {
          require_sequence(quotes,
                           "subscriptions[].discovery.quote_assets");
          for (const auto &quote : quotes) {
            const auto value = uppercase(quote.as<std::string>());
            if (value.empty() || value.size() > 15) {
              throw std::runtime_error(
                  "discovery quote_assets contains an invalid asset");
            }
            parsed.quote_assets.push_back(value);
          }
          std::sort(parsed.quote_assets.begin(),
                    parsed.quote_assets.end());
          parsed.quote_assets.erase(
              std::unique(parsed.quote_assets.begin(),
                          parsed.quote_assets.end()),
              parsed.quote_assets.end());
        }
        parsed.symbol_regex =
            value_or<std::string>(source, "symbol_regex", "");
        if (!parsed.symbol_regex.empty()) {
          try {
            (void)std::regex(parsed.symbol_regex,
                             std::regex::ECMAScript);
          } catch (const std::regex_error &) {
            throw std::runtime_error(
                "subscriptions[].discovery.symbol_regex is invalid");
          }
        }
        parsed.minimum_turnover =
            value_or<std::uint64_t>(source, "minimum_turnover", 0);
        if (source["max_symbols"]) {
          const auto maximum = source["max_symbols"].as<std::size_t>();
          if (maximum == 0) {
            throw std::runtime_error(
                "subscriptions[].discovery.max_symbols must be positive");
          }
          parsed.max_symbols = maximum;
        }
        if (ring_layout != publish::RingLayout::Multiplex &&
            !parsed.max_symbols) {
          throw std::runtime_error(
              "product discovery requires an explicit max_symbols before "
              "multiplex-only publication");
        }
        logical_symbols.emplace_back();
        discovery = std::move(parsed);
      }
      std::string market_asset;
      std::string market_period;
      std::string requested_outcomes;
      std::uint32_t prediscovery_seconds{30};
      std::uint32_t rollover_grace_seconds{2};
      std::uint32_t resolver_timeout_ms{3000};
      if (polymarket) {
        const auto market = node["polymarket"];
        reject_unknown(
            market,
            {"asset", "period", "outcomes", "prediscovery_seconds",
             "rollover_grace_seconds", "resolver_timeout_ms"},
            "subscriptions[].polymarket");
        market_asset =
            uppercase(value_or<std::string>(market, "asset", ""));
        market_period =
            lowercase(value_or<std::string>(market, "period", ""));
        requested_outcomes =
            uppercase(value_or<std::string>(market, "outcomes", "BOTH"));
        prediscovery_seconds = value_or<std::uint32_t>(
            market, "prediscovery_seconds", 30);
        rollover_grace_seconds = value_or<std::uint32_t>(
            market, "rollover_grace_seconds", 2);
        resolver_timeout_ms = value_or<std::uint32_t>(
            market, "resolver_timeout_ms", 3000);
        if (market_asset.empty() || market_asset.size() > 12 ||
            market_period != "5m" ||
            (requested_outcomes != "BOTH" &&
             requested_outcomes != "UP" &&
             requested_outcomes != "DOWN") ||
            prediscovery_seconds == 0 || prediscovery_seconds >= 300 ||
            rollover_grace_seconds > 30 || resolver_timeout_ms == 0) {
          throw std::runtime_error(
              "invalid Polymarket rolling subscription");
        }
      } else if (node["polymarket"]) {
        throw std::runtime_error(
            "subscriptions[].polymarket requires polymarket/binary-option");
      }
      for (const auto &logical_symbol_value : logical_symbols) {
      const auto logical_symbol = uppercase(logical_symbol_value);
      std::vector<std::string> outcomes;
      if (!polymarket) {
        outcomes.emplace_back();
      } else if (requested_outcomes == "BOTH") {
        outcomes = {"UP", "DOWN"};
      } else {
        outcomes = {requested_outcomes};
      }
      for (const auto &outcome : outcomes) {
      auto symbol = logical_symbol + outcome;
      if (!discovery && !valid_symbol(symbol)) {
        throw std::runtime_error("invalid subscription symbol '" + symbol +
                                 "'");
      }
      const VenueProductKey endpoint_key{venue, product};
      const auto endpoint = endpoints.find(endpoint_key);
      if (endpoint == endpoints.end()) {
        throw std::runtime_error(
            "subscription has no matching venue/product endpoint");
      }
      const auto stream = lowercase(node["stream"].as<std::string>());
      if (stream != "ticker" && stream != "orderbook") {
        throw std::runtime_error(
            "subscription stream must be ticker or orderbook");
      }
      const StreamKey key{venue, product,
                          discovery ? std::string(1, '\x1f') + "DISCOVERY"
                                    : symbol};
      auto [entry, inserted] = streams.try_emplace(key);
      auto &spec = entry->second;
      if (inserted) {
        spec.venue = venue;
        spec.product = product;
        spec.symbol = symbol;
        spec.discovery = discovery;
        spec.polymarket_rolling = polymarket;
        spec.polymarket_asset = market_asset;
        spec.polymarket_period = market_period;
        spec.polymarket_outcome = outcome;
        spec.polymarket_prediscovery_seconds = prediscovery_seconds;
        spec.polymarket_rollover_grace_seconds = rollover_grace_seconds;
        spec.polymarket_resolver_timeout_ms = resolver_timeout_ms;
        spec.shm_prefix = prefix;
        spec.ring = ring;
        spec.ring_layout = ring_layout;
        spec.shard_count = shard_count;
        spec.multiplex_ring = multiplex_ring;
        spec.reader_lease_timeout =
            std::chrono::nanoseconds(lease_timeout);
        spec.ladder_ticks_per_side = default_ladder;
        spec.max_ladder_ticks_per_side = max_ladder;
        spec.ladder_price_band_bps = default_price_band_bps;
        const auto *venue_capabilities =
            exchange::capabilities(venue, product);
        spec.ticker_channel =
            std::string(venue_capabilities->ticker.channel);
      }
      if (stream == "ticker") {
        if (node["depth"] || node["ladder_ticks_per_side"] ||
            node["ladder_levels_per_side"] ||
            node["price_band_bps"] ||
            node["orderbook_channel"] || node["update_interval_ms"]) {
          throw std::runtime_error(
              "orderbook-only fields are invalid for ticker subscriptions");
        }
        spec.subscribe_ticker = true;
        continue;
      }

      if (node["ladder_ticks_per_side"] &&
          node["ladder_levels_per_side"]) {
        throw std::runtime_error(
            "do not mix ladder_ticks_per_side with its deprecated alias");
      }
      const auto ladder =
          node["ladder_ticks_per_side"]
              ? node["ladder_ticks_per_side"].as<std::size_t>()
              : value_or<std::size_t>(
                    node, "ladder_levels_per_side", default_ladder);
      if (ladder == 0 || ladder > max_ladder) {
        throw std::runtime_error("invalid orderbook ladder bounds for " +
                                 symbol);
      }
      const auto price_band_bps = value_or<std::uint32_t>(
          node, "price_band_bps", default_price_band_bps);
      if (price_band_bps == 0 || price_band_bps > 10'000) {
        throw std::runtime_error(
            "price_band_bps must be in [1, 10000]");
      }
      const auto requested_channel =
          value_or<std::string>(node, "orderbook_channel", "");
      std::optional<std::uint32_t> requested_interval;
      if (node["update_interval_ms"]) {
        requested_interval = node["update_interval_ms"].as<std::uint32_t>();
      }
      exchange::ResolvedOrderBookChannel resolved;
      std::string channel_error;
      if (!exchange::resolve_orderbook_channel(
              venue, product, requested_channel, requested_interval, resolved,
              channel_error)) {
        throw std::runtime_error(channel_error);
      }
      std::size_t snapshot_depth{};
      if (venue == Venue::Binance) {
        snapshot_depth = value_or<std::size_t>(
            node, "depth", product == Product::Spot ? 5000 : 1000);
        if (!valid_snapshot_depth(product, snapshot_depth)) {
          throw std::runtime_error(
              "invalid Binance snapshot depth for " + symbol);
        }
      } else if (node["depth"]) {
        throw std::runtime_error(
            "depth is only configurable for Binance; use "
            "orderbook_channel for this venue");
      }
      if (spec.subscribe_orderbook &&
          (spec.orderbook_channel != resolved.capability.channel ||
           spec.effective_interval_ms != resolved.capability.interval_ms ||
           spec.max_levels_per_message !=
               resolved.capability.max_levels_per_message ||
           spec.ladder_ticks_per_side != ladder ||
           spec.ladder_price_band_bps != price_band_bps ||
           spec.snapshot_depth != snapshot_depth)) {
        throw std::runtime_error(
            "conflicting duplicate orderbook subscription for " + symbol);
      }
      spec.subscribe_orderbook = true;
      spec.orderbook_channel =
          std::string(resolved.capability.channel);
      spec.orderbook_bootstrap = resolved.capability.bootstrap;
      spec.orderbook_depth = resolved.capability.depth_per_side;
      spec.max_levels_per_message =
          resolved.capability.max_levels_per_message;
      spec.effective_interval_ms = resolved.capability.interval_ms;
      spec.orderbook_channel_override = resolved.explicit_override;
      spec.requires_public_ws_login =
          resolved.capability.requires_public_ws_login;
      spec.ladder_ticks_per_side = ladder;
      spec.ladder_price_band_bps = price_band_bps;
      spec.snapshot_depth = snapshot_depth;
      validate_auth(endpoint->second, spec);
      }
      }
    }
    if (streams.empty()) {
      throw std::runtime_error("subscriptions must not be empty");
    }

    ProducerConfig config;
    config.unlink_on_shutdown = ring.unlink_on_close;
    config.max_instruments = max_instruments;
    config.max_rings = max_rings;
    config.max_total_ring_bytes = max_total_ring_bytes;
    config.instrument_count = 0;
    for (const auto &[key, stream] : streams) {
      const auto endpoint =
          endpoints.find({key.venue, key.product});
      config.instrument_count +=
          stream.discovery
              ? stream.discovery->max_symbols.value_or(
                    endpoint->second.max_symbols_per_connection)
              : 1;
    }
    for (const auto &[endpoint_key, endpoint] : endpoints) {
      ConnectionSpec connection;
      connection.endpoint = endpoint;
      for (const auto &[stream_key, stream] : streams) {
        if (stream_key.venue == endpoint_key.venue &&
            stream_key.product == endpoint_key.product) {
          connection.streams.push_back(stream);
        }
      }
      if (connection.streams.empty()) {
        continue;
      }
      std::size_t connection_instruments{};
      for (const auto &stream : connection.streams) {
        connection_instruments +=
            stream.discovery
                ? stream.discovery->max_symbols.value_or(
                      endpoint.max_symbols_per_connection)
                : 1;
      }
      if (connection_instruments > endpoint.max_symbols_per_connection) {
        throw std::runtime_error(
            "venue connection exceeds max_symbols_per_connection");
      }
      if (endpoint.venue == Venue::Polymarket &&
          ring_layout != publish::RingLayout::PerSymbol) {
        throw std::runtime_error(
            "Polymarket supports only ring_layout: per_symbol");
      }
      config.connections.push_back(std::move(connection));
    }

    std::size_t per_symbol_rings{};
    std::map<VenueProductKey, std::uint8_t> multiplex_streams;
    for (const auto &[key, stream] : streams) {
      const auto stream_instruments =
          stream.discovery
              ? stream.discovery->max_symbols.value_or(
                    endpoints.find({key.venue, key.product})
                        ->second.max_symbols_per_connection)
              : std::size_t{1};
      auto &mask = multiplex_streams[{key.venue, key.product}];
      if (stream.subscribe_ticker) {
        if (ring_layout != publish::RingLayout::Multiplex) {
          per_symbol_rings += stream_instruments;
        }
        mask |= 1;
      }
      if (stream.subscribe_orderbook) {
        if (ring_layout != publish::RingLayout::Multiplex) {
          per_symbol_rings += stream_instruments;
        }
        mask |= 2;
      }
    }
    std::size_t multiplex_rings{};
    if (ring_layout != publish::RingLayout::PerSymbol) {
      for (const auto &[key, mask] : multiplex_streams) {
        (void)key;
        multiplex_rings +=
            (std::size_t((mask & 1U) != 0) +
             std::size_t((mask & 2U) != 0)) *
            shard_count;
      }
    }
    config.ring_count = per_symbol_rings + multiplex_rings;
    const auto total_bytes =
        static_cast<std::uint64_t>(per_symbol_rings) * ring.ring_bytes +
        static_cast<std::uint64_t>(multiplex_rings) *
            multiplex_ring.ring_bytes;
    if (config.instrument_count > max_instruments ||
        config.ring_count > max_rings ||
        total_bytes > max_total_ring_bytes) {
      throw std::runtime_error(
          "capacity budget exceeded by instruments, rings or ring bytes");
    }

    return {.value = std::move(config)};
  } catch (const std::exception &error) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = std::string("failed to load producer config: ") +
                       error.what()};
  }
}

api::Result<UniverseChange> reconcile_universe(
    const ProductDiscovery &discovery,
    std::span<const exchange::InstrumentMetadata> metadata,
    std::span<const exchange::InstrumentMetadata> active) noexcept {
  try {
    std::optional<std::regex> expression;
    if (!discovery.symbol_regex.empty()) {
      expression.emplace(discovery.symbol_regex, std::regex::ECMAScript);
    }
    std::set<std::string> quotes(discovery.quote_assets.begin(),
                                 discovery.quote_assets.end());
    std::map<std::string, exchange::InstrumentMetadata> selected;
    for (const auto &entry : metadata) {
      if (entry.canonical_symbol.empty() || entry.venue_symbol.empty() ||
          entry.tick_size <= 0 || entry.lot_size <= 0) {
        continue;
      }
      if (!quotes.empty() && !quotes.contains(uppercase(entry.quote_asset))) {
        continue;
      }
      if (expression &&
          !std::regex_match(entry.canonical_symbol, *expression)) {
        continue;
      }
      if (discovery.minimum_turnover != 0 &&
          entry.turnover_24h != 0 &&
          entry.turnover_24h < discovery.minimum_turnover) {
        continue;
      }
      selected.try_emplace(entry.canonical_symbol, entry);
    }
    if (discovery.max_symbols &&
        selected.size() > *discovery.max_symbols) {
      return {.error = api::ErrorCode::InvalidConfig,
              .message =
                  "discovered universe exceeds configured max_symbols"};
    }
    std::map<std::string, exchange::InstrumentMetadata> previous;
    for (const auto &entry : active) {
      previous.try_emplace(entry.canonical_symbol, entry);
    }
    UniverseChange change;
    for (const auto &[symbol, entry] : selected) {
      if (previous.contains(symbol)) {
        change.retained.push_back(entry);
      } else {
        change.added.push_back(entry);
      }
    }
    for (const auto &[symbol, entry] : previous) {
      if (!selected.contains(symbol)) {
        change.retired.push_back(entry);
      }
    }
    return {.value = std::move(change)};
  } catch (const std::exception &error) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = std::string("failed to reconcile product universe: ") +
                       error.what()};
  }
}

}  // namespace mds::producer
