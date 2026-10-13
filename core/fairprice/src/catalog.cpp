#include "fairprice/catalog.h"
#include "fairprice/market_data_repository.h"

#include <openssl/sha.h>

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cctype>
#include <sstream>
#include <thread>
#include <unordered_map>

namespace fairprice {
namespace {

bool valid_component(std::string_view value) {
  if (value.empty()) {
    return false;
  }
  for (const char c : value) {
    if (!std::isalnum(static_cast<unsigned char>(c)) && c != '_' && c != '-') {
      return false;
    }
  }
  return true;
}

bool is_bbo_kind(std::string_view kind) {
  return kind == "aggbbo" || kind == "agg_bbo";
}

bool is_book_kind(std::string_view kind) {
  return kind == "aggorderbook" || kind == "agg_orderbook";
}

void hash_snapshot(CatalogSnapshot &snapshot) {
  std::ostringstream raw;
  raw << snapshot.sample_interval_ms << '\n'
      << snapshot.retention_hours << '\n'
      << snapshot.depth << '\n';
  for (const auto &market : snapshot.markets) {
    raw << market.profile << '\n'
        << market.symbol << '\n'
        << market.bbo_segment << '\n'
        << market.book_segment << '\n';
  }
  const auto body = raw.str();
  SHA256(reinterpret_cast<const unsigned char *>(body.data()), body.size(),
         snapshot.hash.data());
}

}  // namespace

std::string canonical_symbol(std::string_view value) {
  std::string out;
  for (char c : value) {
    c = static_cast<char>(std::toupper(static_cast<unsigned char>(c)));
    if ((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
      out.push_back(c);
    }
  }
  return out;
}

std::string market_key(std::string_view profile, std::string_view symbol) {
  return std::string(profile) + '\0' + std::string(symbol);
}

bool parse_segment(std::string_view segment, std::string &profile,
                   std::string &symbol, Kind &kind, std::string &error) {
  kind = Kind::Unknown;
  for (const auto stream : {std::string_view{"aggbbo"},
                            std::string_view{"aggorderbook"}}) {
    const auto suffix = std::string(".") + std::string(stream) + ".2";
    if (segment.size() <= suffix.size() ||
        segment.substr(segment.size() - suffix.size()) != suffix) {
      continue;
    }
    auto prefix = segment.substr(0, segment.size() - suffix.size());
    const auto symbol_dot = prefix.rfind('.');
    if (segment.front() != '/' || symbol_dot == std::string_view::npos ||
        symbol_dot <= 1 || symbol_dot == prefix.size() - 1) {
      break;
    }
    const auto profile_dot = prefix.substr(0, symbol_dot).rfind('.');
    if (profile_dot == std::string_view::npos || profile_dot < 1 ||
        profile_dot == symbol_dot - 1) {
      break;
    }
    profile = std::string(
        prefix.substr(profile_dot + 1, symbol_dot - profile_dot - 1));
    symbol = canonical_symbol(prefix.substr(symbol_dot + 1));
    kind = stream == "aggbbo" ? Kind::Bbo : Kind::Book;
    if (valid_component(profile) && valid_component(symbol)) {
      return true;
    }
    break;
  }
  error = "invalid aggregate segment";
  return false;
}

mds::api::Result<Catalog> Catalog::open(MarketDataRepository *repository,
                                        CatalogSettings settings) {
  if (repository == nullptr || settings.sample_interval_ms == 0 ||
      settings.depth == 0) {
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "ClickHouse catalog settings are invalid"};
  }
  Catalog catalog;
  catalog.repository_ = repository;
  catalog.settings_ = settings;
  auto refreshed = catalog.refresh();
  if (!refreshed) {
    return {.error = refreshed.error, .message = std::move(refreshed.message)};
  }
  if (!catalog.loaded_) {
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "ClickHouse catalog is unavailable"};
  }
  return {.value = std::move(catalog)};
}

mds::api::Result<bool> Catalog::refresh() {
  if (repository_ == nullptr) {
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "ClickHouse catalog repository is missing"};
  }
  const auto queried = repository_->query_catalog();
  if (!queried) {
    const auto message = "ClickHouse catalog: " +
                         std::string(market_data_error_name(queried.error)) +
                         ": " + queried.message;
    if (loaded_) {
      std::fprintf(stderr, "fairprice: retain last-good catalog (%s)\n",
                   message.c_str());
      return {.value = false};
    }
    return {.error = mds::api::ErrorCode::InvalidConfig, .message = message};
  }

  struct Pair {
    CatalogMarket market;
    bool has_bbo{};
    bool has_book{};
  };
  std::unordered_map<std::string, Pair> by_key;
  for (const auto &row : queried.value) {
    if (!row.active) {
      continue;
    }
    std::string profile;
    std::string symbol;
    Kind kind = Kind::Unknown;
    std::string parse_error;
    if (!parse_segment(row.segment, profile, symbol, kind, parse_error)) {
      std::fprintf(stderr,
                   "fairprice: skip catalog row invalid segment=%s (%s)\n",
                   row.segment.c_str(), parse_error.c_str());
      continue;
    }
    if (!row.profile.empty()) {
      profile = row.profile;
    }
    if (!row.symbol.empty()) {
      symbol = canonical_symbol(row.symbol);
    }
    Kind declared = Kind::Unknown;
    if (is_bbo_kind(row.kind)) {
      declared = Kind::Bbo;
    } else if (is_book_kind(row.kind)) {
      declared = Kind::Book;
    }
    if (declared != Kind::Unknown && declared != kind) {
      std::fprintf(stderr,
                   "fairprice: skip catalog row kind mismatch profile=%s "
                   "symbol=%s kind=%s segment=%s\n",
                   profile.c_str(), symbol.c_str(), row.kind.c_str(),
                   row.segment.c_str());
      continue;
    }
    auto &pair = by_key[market_key(profile, symbol)];
    pair.market.profile = profile;
    pair.market.symbol = symbol;
    if (kind == Kind::Bbo) {
      pair.market.bbo_segment = row.segment;
      pair.has_bbo = true;
    } else {
      pair.market.book_segment = row.segment;
      pair.has_book = true;
    }
  }

  CatalogSnapshot parsed;
  parsed.state = "recording";
  parsed.sample_interval_ms = settings_.sample_interval_ms;
  parsed.retention_hours = settings_.retention_hours;
  parsed.depth = settings_.depth;
  for (auto &[_, pair] : by_key) {
    if (!pair.has_bbo || !pair.has_book) {
      const char *missing = !pair.has_bbo ? "aggbbo" : "aggorderbook";
      std::fprintf(stderr,
                   "fairprice: skip incomplete market profile=%s symbol=%s "
                   "missing=%s\n",
                   pair.market.profile.c_str(), pair.market.symbol.c_str(),
                   missing);
      continue;
    }
    pair.market.recording = true;
    parsed.markets.push_back(std::move(pair.market));
  }
  std::sort(parsed.markets.begin(), parsed.markets.end(),
            [](const CatalogMarket &left, const CatalogMarket &right) {
              if (left.symbol == right.symbol) {
                return left.profile < right.profile;
              }
              return left.symbol < right.symbol;
            });
  hash_snapshot(parsed);
  if (loaded_ && current_.hash == parsed.hash) {
    return {.value = false};
  }
  current_ = std::move(parsed);
  loaded_ = true;
  return {.value = true};
}

mds::api::Result<bool> Catalog::wait_for_change(std::uint64_t timeout_ms) {
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
  for (;;) {
    auto refreshed = refresh();
    if (!refreshed) {
      return refreshed;
    }
    if (refreshed.value) {
      return refreshed;
    }
    if (std::chrono::steady_clock::now() >= deadline) {
      return {.value = false};
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
  }
}

}  // namespace fairprice
