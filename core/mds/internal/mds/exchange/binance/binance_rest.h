#pragma once

#include "mds/exchange/binance/binance_adapter.h"

#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::exchange::binance {

struct PriceFilter {
  std::int64_t min_price{};
  std::int64_t max_price{};
  std::int64_t tick_size{};
};

struct LotSizeFilter {
  std::int64_t min_quantity{};
  std::int64_t max_quantity{};
  std::int64_t step_size{};
};

struct InstrumentMetadata {
  Profile profile{Profile::Spot};
  std::string venue_symbol;
  std::string pair;
  std::string base_asset;
  std::string quote_asset;
  std::string settle_asset;
  std::string status;
  std::string contract_type;
  std::string underlying_type;
  std::int8_t price_scale{};
  std::int8_t quantity_scale{};
  PriceFilter price_filter;
  LotSizeFilter lot_size;
  std::int64_t contract_size{1};
  std::int8_t contract_size_scale{};
  std::uint64_t onboard_time_ms{};
  std::uint64_t delivery_time_ms{};
};

struct DepthSnapshot {
  std::uint64_t last_update_id{};
  std::int8_t price_exponent{};
  std::int8_t quantity_exponent{};
  std::vector<PriceLevel> bids;
  std::vector<PriceLevel> asks;
};

class RestParser {
public:
  explicit RestParser(std::size_t capacity = 4U << 20U);
  ~RestParser();
  RestParser(const RestParser &) = delete;
  RestParser &operator=(const RestParser &) = delete;

  bool parse_exchange_info(Profile profile, std::string_view json,
                           std::string_view symbol, InstrumentMetadata &out,
                           std::string &error);
  bool parse_exchange_info(
      Profile profile, std::string_view json,
      std::span<const std::string_view> symbols,
      std::vector<InstrumentMetadata> &out, std::string &error);
  bool parse_depth(Profile profile, std::string_view json,
                   const InstrumentMetadata &metadata, DepthSnapshot &out,
                   std::string &error);

private:
#ifdef MDS_HAS_SIMDJSON
  struct Impl;
  Impl *impl_{};
#else
  std::size_t capacity_{};
#endif
};

} // namespace mds::exchange::binance
