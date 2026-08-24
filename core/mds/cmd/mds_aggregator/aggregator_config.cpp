#include "aggregator_config.h"

#include "mds/consume/aggregate_segment.h"
#include "mds/exchange/capabilities.h"
#include "mds/publish/wire_publisher.h"
#include "utils/md/wire.h"

#include <yaml-cpp/yaml.h>

#include <algorithm>
#include <cctype>
#include <limits>
#include <stdexcept>
#include <string_view>
#include <vector>

namespace mds::aggregator {
namespace {

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

std::string uppercase(std::string value) {
  std::transform(value.begin(), value.end(), value.begin(),
                 [](unsigned char character) {
                   return static_cast<char>(std::toupper(character));
                 });
  return value;
}

bool power_of_two(std::size_t value) noexcept {
  return value != 0 && (value & (value - 1U)) == 0;
}

bool valid_token(std::string_view value, std::size_t maximum) noexcept {
  return !value.empty() && value.size() <= maximum &&
         std::all_of(value.begin(), value.end(), [](unsigned char character) {
           return std::isalnum(character) != 0;
         });
}

std::uint64_t ttl_override_us(const YAML::Node &node, std::string_view key,
                              bool output_enabled,
                              std::uint64_t derived_ttl_us) {
  const auto value = node[std::string(key)];
  if (!value) {
    return derived_ttl_us;
  }
  if (!output_enabled) {
    throw std::runtime_error(std::string(key) +
                             " requires its aggregate output");
  }
  const auto milliseconds = value.as<std::uint64_t>();
  if (milliseconds == 0 ||
      milliseconds > std::numeric_limits<std::uint64_t>::max() / 1'000U) {
    throw std::runtime_error(std::string(key) + " is out of range");
  }
  return milliseconds * 1'000U;
}

transport::RingOptions parse_ring(const YAML::Node &node,
                                  std::size_t minimum_record,
                                  bool unlink_on_shutdown) {
  reject_unknown(node,
                 {"ring_bytes", "max_record_bytes", "max_readers",
                  "unlink_on_shutdown"},
                 "shared_memory");
  transport::RingOptions ring;
  ring.mode = api::RingMode::OverwriteOldest;
  ring.ring_bytes = value_or<std::size_t>(node, "ring_bytes", 4U << 20U);
  ring.max_record_bytes =
      value_or<std::size_t>(node, "max_record_bytes", 32U << 10U);
  ring.max_readers = value_or<std::size_t>(node, "max_readers", 32);
  ring.unlink_on_close = unlink_on_shutdown;
  if (!power_of_two(ring.ring_bytes) ||
      ring.max_record_bytes <
          sizeof(transport::RecordHeader) + minimum_record ||
      ring.max_record_bytes > ring.ring_bytes / 8U ||
      ring.max_readers == 0 || ring.max_readers > transport::kMaxReaders) {
    throw std::runtime_error("shared_memory bounds are invalid");
  }
  return ring;
}

}  // namespace

api::Result<AggregatorConfig> load_config(const std::string &path) noexcept {
  try {
    const auto root = YAML::LoadFile(path);
    reject_unknown(root, {"shared_memory", "books"}, "root");
    AggregatorConfig config;
    const auto shared = root["shared_memory"];
    config.prefix =
        value_or<std::string>(shared, "prefix", "/selfquant.mds");
    if (config.prefix.size() < 2 || config.prefix.size() > 200 ||
        config.prefix.front() != '/' ||
        config.prefix.find('/', 1) != std::string::npos) {
      throw std::runtime_error(
          "shared_memory.prefix must be 2..200 bytes with one leading '/'");
    }
    const auto ring_node = shared["output"];
    config.unlink_on_shutdown =
        value_or<bool>(ring_node, "unlink_on_shutdown", false);
    config.bbo_ring =
        parse_ring(ring_node, sizeof(utils::md::wire::AggBboRecord),
                   config.unlink_on_shutdown);
    config.orderbook_ring =
        parse_ring(ring_node, sizeof(utils::md::wire::AggOrderBookRecord),
                   config.unlink_on_shutdown);
    reject_unknown(shared, {"prefix", "output"}, "shared_memory");

    const auto books = root["books"];
    if (!books || !books.IsSequence() || books.size() == 0 ||
        books.size() > config.books.size()) {
      throw std::runtime_error("books must contain 1..32 entries");
    }
    for (const auto &node : books) {
      reject_unknown(node,
                     {"symbol", "product", "base_asset", "quote_asset",
                      "outputs", "members", "cross_skew",
                      "quote_conversion"},
                     "books[]");
      auto &book = config.books[config.book_count++];
      book.symbol = uppercase(node["symbol"].as<std::string>());
      book.base_asset = uppercase(node["base_asset"].as<std::string>());
      book.quote_asset = uppercase(node["quote_asset"].as<std::string>());
      const auto product =
          exchange::parse_product(node["product"].as<std::string>());
      if (!product || !valid_token(book.symbol, 32) ||
          !valid_token(book.base_asset, 15) ||
          !valid_token(book.quote_asset, 15) ||
          book.symbol != book.base_asset + book.quote_asset) {
        throw std::runtime_error("book identity is invalid");
      }
      book.product = *product;

      if (const auto outputs = node["outputs"]) {
        reject_unknown(outputs, {"aggbbo", "aggorderbook"},
                       "books[].outputs");
        book.enable_bbo = value_or<bool>(outputs, "aggbbo", true);
        book.enable_orderbook =
            value_or<bool>(outputs, "aggorderbook", true);
      }
      if (!book.enable_bbo && !book.enable_orderbook) {
        throw std::runtime_error("at least one aggregate output is required");
      }

      if (const auto skew = node["cross_skew"]) {
        reject_unknown(skew, {"observe_only", "threshold_us"},
                       "books[].cross_skew");
        book.cross_skew_observe_only =
            value_or<bool>(skew, "observe_only", true);
        book.cross_skew_threshold_us =
            value_or<std::uint32_t>(skew, "threshold_us", 50'000);
        if (book.cross_skew_threshold_us == 0) {
          throw std::runtime_error("cross_skew.threshold_us must be positive");
        }
      }

      if (const auto conversion = node["quote_conversion"]) {
        reject_unknown(conversion,
                       {"enabled", "from_quote", "to_quote", "source",
                        "max_age_ms", "max_depeg_bps"},
                       "books[].quote_conversion");
        book.fx_enabled = value_or<bool>(conversion, "enabled", false);
        if (book.fx_enabled) {
          if (uppercase(conversion["from_quote"].as<std::string>()) != "USDC" ||
              uppercase(conversion["to_quote"].as<std::string>()) != "USDT" ||
              book.quote_asset != "USDT" ||
              uppercase(conversion["source"].as<std::string>()) !=
                  "BINANCE_SPOT_USDCUSDT") {
            throw std::runtime_error(
                "only Binance Spot USDCUSDT conversion is supported");
          }
          book.fx_ttl_us =
              value_or<std::uint64_t>(conversion, "max_age_ms", 100);
          if (book.fx_ttl_us >
              std::numeric_limits<std::uint64_t>::max() / 1'000U) {
            throw std::runtime_error("quote conversion max_age_ms is too large");
          }
          book.fx_ttl_us *= 1'000U;
          book.fx_max_depeg_bps =
              value_or<std::uint32_t>(conversion, "max_depeg_bps", 200);
          if (book.fx_ttl_us == 0 || book.fx_max_depeg_bps == 0) {
            throw std::runtime_error("quote conversion bounds are invalid");
          }
        }
      }

      const auto members = node["members"];
      if (!members || !members.IsSequence() || members.size() == 0 ||
          members.size() > agg::kMaxMembers) {
        throw std::runtime_error("members must contain 1..8 entries");
      }
      std::uint32_t venue_mask = 0;
      for (const auto &member_node : members) {
        reject_unknown(member_node,
                       {"venue", "source_quote_asset", "bbo_ttl_ms",
                        "orderbook_ttl_ms", "input_layout",
                        "shard_count"},
                       "books[].members[]");
        const auto venue =
            exchange::parse_venue(member_node["venue"].as<std::string>());
        if (!venue || *venue == utils::md::Venue::Unknown) {
          throw std::runtime_error("member venue is unsupported");
        }
        const auto venue_bit =
            std::uint32_t{1} << static_cast<std::uint16_t>(*venue);
        if ((venue_mask & venue_bit) != 0) {
          throw std::runtime_error("member venues must be unique");
        }
        venue_mask |= venue_bit;
        auto &member = book.members[book.member_count++];
        member.venue = *venue;
        const auto input_layout = value_or<std::string>(
            member_node, "input_layout", "per_symbol");
        if (input_layout == "per_symbol") {
          member.input_layout = publish::RingLayout::PerSymbol;
        } else if (input_layout == "multiplex") {
          member.input_layout = publish::RingLayout::Multiplex;
        } else {
          throw std::runtime_error(
              "member input_layout must be per_symbol or multiplex");
        }
        member.shard_count =
            value_or<std::size_t>(member_node, "shard_count", 1);
        if (member.shard_count == 0 || member.shard_count > 256 ||
            (member.input_layout == publish::RingLayout::PerSymbol &&
             member.shard_count != 1) ||
            member.venue == utils::md::Venue::Polymarket) {
          throw std::runtime_error("member input layout is invalid");
        }
        member.source_quote_asset = uppercase(value_or<std::string>(
            member_node, "source_quote_asset", book.quote_asset));
        if (member.source_quote_asset != book.quote_asset &&
            !(book.fx_enabled &&
              member.venue == utils::md::Venue::Hyperliquid &&
              member.source_quote_asset == "USDC" &&
              book.quote_asset == "USDT")) {
          throw std::runtime_error("member quote does not match output bucket");
        }
        const auto *capabilities =
            exchange::capabilities(member.venue, book.product);
        if (capabilities == nullptr ||
            (book.enable_bbo && capabilities->ticker.channel.empty()) ||
            (book.enable_orderbook &&
             (capabilities->fastest_top10.channel.empty() ||
              capabilities->fastest_top10.interval_ms == 0))) {
          throw std::runtime_error("member has no aggregation capability");
        }
        if (book.enable_bbo) {
          const auto cadence_ms =
              capabilities->ticker.interval_ms != 0
                  ? capabilities->ticker.interval_ms
                  : capabilities->fastest_top10.interval_ms;
          member.bbo_ttl_us =
              static_cast<std::uint64_t>(cadence_ms) *
              3'000U;
        }
        if (book.enable_orderbook) {
          member.orderbook_ttl_us =
              static_cast<std::uint64_t>(
                  capabilities->fastest_top10.interval_ms) *
              3'000U;
        }
        member.bbo_ttl_us =
            ttl_override_us(member_node, "bbo_ttl_ms", book.enable_bbo,
                            member.bbo_ttl_us);
        member.orderbook_ttl_us = ttl_override_us(
            member_node, "orderbook_ttl_ms", book.enable_orderbook,
            member.orderbook_ttl_us);
      }

      for (std::size_t slot = 0; slot < book.member_count; ++slot) {
        const auto &member = book.members[slot];
        for (std::size_t shard = 0; shard < member.shard_count; ++shard) {
          if (input_segment_name(config, book, member, "ticker", shard)
                  .empty() ||
              input_segment_name(config, book, member, "orderbook", shard)
                  .empty()) {
            throw std::runtime_error(
                "input segment name exceeds fixed capacity");
          }
        }
      }
      if ((book.enable_bbo &&
           output_segment_name(config, book, "aggbbo").empty()) ||
          (book.enable_orderbook &&
           output_segment_name(config, book, "aggorderbook").empty()) ||
          (book.fx_enabled && fx_segment_name(config).empty())) {
        throw std::runtime_error("output segment name exceeds fixed capacity");
      }
      for (std::size_t previous = 0; previous + 1 < config.book_count;
           ++previous) {
        const auto &other = config.books[previous];
        const bool same_bbo =
            book.enable_bbo && other.enable_bbo &&
            output_segment_name(config, book, "aggbbo") ==
                output_segment_name(config, other, "aggbbo");
        const bool same_book =
            book.enable_orderbook && other.enable_orderbook &&
            output_segment_name(config, book, "aggorderbook") ==
                output_segment_name(config, other, "aggorderbook");
        if (same_bbo || same_book) {
          throw std::runtime_error(
              "books must produce unique aggregate output segments");
        }
      }
    }
    return {.value = std::move(config)};
  } catch (const std::exception &error) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = std::string("failed to load aggregator config: ") +
                       error.what()};
  }
}

std::string input_segment_name(const AggregatorConfig &config,
                               const BookSpec &book,
                               const MemberSpec &member,
                               std::string_view stream,
                               std::size_t shard) {
  if (member.input_layout == publish::RingLayout::Multiplex) {
    return publish::make_multiplex_segment_name(
        config.prefix, exchange::venue_name(member.venue),
        exchange::product_name(book.product), stream, shard);
  }
  std::string source_symbol = book.base_asset;
  source_symbol.append(member.source_quote_asset);
  return publish::make_publisher_segment_name(
      config.prefix, exchange::segment_profile(member.venue, book.product),
      source_symbol, stream);
}

std::string output_segment_name(const AggregatorConfig &config,
                                const BookSpec &book,
                                std::string_view stream) {
  std::array<std::string_view, agg::kMaxMembers> names{};
  for (std::size_t index = 0; index < book.member_count; ++index) {
    names[index] = exchange::venue_name(book.members[index].venue);
  }
  return consume::make_aggregate_segment_name(
      config.prefix, book.product, book.quote_asset,
      std::span<const std::string_view>(names.data(), book.member_count),
      book.symbol, stream);
}

std::string fx_segment_name(const AggregatorConfig &config) {
  return publish::make_publisher_segment_name(
      config.prefix,
      exchange::segment_profile(utils::md::Venue::Binance,
                                utils::md::ProductType::Spot),
      "USDCUSDT", "ticker");
}

}  // namespace mds::aggregator
