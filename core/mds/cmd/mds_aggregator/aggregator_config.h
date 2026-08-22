#pragma once

#include "mds/agg/aggregation_engine.h"
#include "mds/api/mds_api.h"
#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"

#include <array>
#include <cstddef>
#include <string>

namespace mds::aggregator {

struct MemberSpec {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  std::string source_quote_asset;
  std::uint64_t bbo_ttl_us{};
  std::uint64_t orderbook_ttl_us{};
  publish::RingLayout input_layout{publish::RingLayout::PerSymbol};
  std::size_t shard_count{1};
};

struct BookSpec {
  std::string symbol;
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  std::string base_asset;
  std::string quote_asset;
  std::array<MemberSpec, agg::kMaxMembers> members{};
  std::size_t member_count{};
  bool enable_bbo{true};
  bool enable_orderbook{true};
  bool cross_skew_observe_only{true};
  std::uint32_t cross_skew_threshold_us{50'000};
  bool fx_enabled{};
  std::uint64_t fx_ttl_us{100'000};
  std::uint32_t fx_max_depeg_bps{200};
};

struct AggregatorConfig {
  std::string prefix{"/selfquant.mds"};
  transport::RingOptions bbo_ring{};
  transport::RingOptions orderbook_ring{};
  std::array<BookSpec, 32> books{};
  std::size_t book_count{};
  bool unlink_on_shutdown{};
};

[[nodiscard]] api::Result<AggregatorConfig>
load_config(const std::string &path) noexcept;

[[nodiscard]] std::string input_segment_name(
    const AggregatorConfig &config, const BookSpec &book,
    const MemberSpec &member, std::string_view stream,
    std::size_t shard = 0);
[[nodiscard]] std::string output_segment_name(
    const AggregatorConfig &config, const BookSpec &book,
    std::string_view stream);
[[nodiscard]] std::string fx_segment_name(const AggregatorConfig &config);

}  // namespace mds::aggregator
