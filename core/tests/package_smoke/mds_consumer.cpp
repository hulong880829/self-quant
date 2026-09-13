#include "mds/api/mds_api.h"
#include "mds/consume/aggregate_segment.h"
#include "mds/consume/order_book_reconstructor.h"
#include "mds/consume/segment_reader.h"
#include "mds/consume/sequence_tracker.h"
#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"

#include <array>
#include <cstdint>
#include <string>
#include <type_traits>
#include <utility>

#if __has_include("mds/network/epoll_loop.h")
#error "internal mds/network/epoll_loop.h leaked into the installed SDK"
#endif

int main() {
  static_assert(mds::transport::kRingSchemaMajor == 5);
  static_assert(sizeof(utils::md::wire::AggBboRecord) == 416);
  static_assert(static_cast<std::uint16_t>(
                    mds::api::ErrorCode::AlreadyStarted) == 15);
  static_assert(std::is_same_v<
                decltype(mds::api::register_agg_bbo(
                    std::declval<const mds::api::AggregateSubscription &>())),
                mds::api::Result<mds::api::SubscriptionHandle>>);
  static_assert(std::is_same_v<
                decltype(mds::api::try_read_agg_orderbook(
                    std::declval<mds::api::SubscriptionHandle>(),
                    std::declval<mds::api::AggOrderBookRecord &>())),
                mds::api::ErrorCode>);
  mds::consume::SequenceTracker sequences;
  mds::consume::OrderBookReconstructor book;
  mds::consume::SegmentReader reader;
  (void)sequences;
  (void)book;
  (void)reader;
  auto *start_api = &mds::api::start;
  auto *register_bbo_api = &mds::api::register_agg_bbo;
  auto *register_book_api = &mds::api::register_agg_orderbook;
  auto *read_bbo_api = &mds::api::try_read_agg_bbo;
  (void)start_api;
  (void)register_bbo_api;
  (void)register_book_api;
  (void)read_bbo_api;
  const std::array<std::string_view, 2> venues{"okx", "binance"};
  const auto ticker_segment =
      std::string("/selfquant.mds.spot.btcusdt.ticker.") +
      std::to_string(utils::md::wire::kSchemaMajor);
  return mds::publish::make_publisher_segment_name(
                 "spot", "BTCUSDT", "ticker") == ticker_segment &&
         mds::consume::make_aggregate_profile(
             utils::md::ProductType::Perpetual, "USDT", venues) ==
             "agg_perp_usdt_binance-okx"
             ? 0
             : 1;
}
