#pragma once

#include "fairprice/engine.h"
#include "utils/md/wire.h"

#include <cstddef>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

namespace fairprice {

struct BboView {
  bool ready{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  utils::md::wire::AggBboRecord record{};
  std::shared_ptr<const std::vector<std::byte>> browser;
};

struct BookSnapshot {
  bool ready{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  std::uint64_t exchange_ts_ns{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t member_count{};
  std::uint32_t member_mask{};
  std::uint32_t active_mask{};
  std::array<std::uint8_t, 8> venue_slot_ids{};
  std::array<char, 16> base_asset{};
  std::array<char, 16> quote_asset{};
  std::array<ResidualLevel, kMaxDepth> bids{};
  std::array<ResidualLevel, kMaxDepth> asks{};
  int bid_count{};
  int ask_count{};
  std::shared_ptr<const std::vector<std::byte>> browser20;
  std::shared_ptr<const std::vector<std::byte>> browser50;
};

std::string json_escape(std::string_view value);
void append_u64(std::string &out, std::uint64_t value);
void append_i64(std::string &out, std::int64_t value);
void append_f64(std::string &out, double value);
std::string encode_fair_price_json(std::string_view profile,
                                   std::string_view symbol,
                                   const FairPricePod &pod);
std::string encode_snapshot_json(std::string_view profile,
                                 std::string_view symbol, const BboView *bbo,
                                 const BookSnapshot *book, int depth);

struct MarketJson {
  std::string profile;
  std::string symbol;
  std::string base;
  std::string quote;
  std::vector<std::string> venues;
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  bool has_bbo{};
  bool has_order_book{};
  bool recording{};
  bool live{};
  std::uint64_t updated_ns{};
};

std::string encode_markets_json(const std::vector<MarketJson> &markets);
std::string encode_status_json(std::string_view status);
std::string encode_error_json(std::string_view error);
std::string encode_reset_json(std::string_view profile, std::string_view symbol,
                              std::string_view channel,
                              std::string_view reason);
std::string encode_ack_json(bool ok, std::string_view op,
                            std::string_view profile, std::string_view symbol,
                            std::string_view channel, std::string_view error);

std::string_view venue_name(std::uint8_t id);

}  // namespace fairprice
