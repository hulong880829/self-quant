#include "mds/consume/aggregate_dispatch.h"
#include "mds/consume/aggregate_segment.h"
#include "mds/consume/order_book_reconstructor.h"
#include "mds/consume/segment_reader.h"
#include "mds/publish/wire_publisher.h"
#include "utils/md/wire_codec.h"

#include <array>
#include <cassert>
#include <chrono>
#include <string>
#include <unistd.h>

namespace {

utils::md::wire::RecordHeader header(utils::md::MessageType type,
                                     std::uint16_t length) {
  auto value = utils::md::wire::MakeHeader(type, length);
  value.instrument_id = 42;
  value.book_generation = 7;
  value.state = static_cast<std::uint8_t>(utils::md::BookState::Live);
  return value;
}

}  // namespace

int main() {
  using namespace utils::md;
  using namespace utils::md::wire;

  const std::array<std::string_view, 4> venues{
      "okx", "Binance", "gate", "bybit"};
  const auto profile = mds::consume::make_aggregate_profile(
      ProductType::Perpetual, "USDT", venues);
  assert(profile == "agg_perp_usdt_binance-bybit-gate-okx");
  assert(mds::consume::make_aggregate_segment_name(
             "/selfquant.test", ProductType::Perpetual, "USDT", venues,
             "BTCUSDT", "AggBbo") ==
         "/selfquant.test.agg_perp_usdt_binance-bybit-gate-okx.btcusdt."
         "aggbbo.2");
  assert(mds::consume::aggregate_topic_from_segment(
             "/selfquant.test.agg_spot_usdt_binance-okx.btcusdt.aggbbo.2") ==
         mds::consume::AggregateTopic::AggBbo);
  assert(mds::consume::aggregate_topic_from_segment(
             "/selfquant.test.agg_spot_usdt_binance-okx.btcusdt."
             "aggorderbook.2") ==
         mds::consume::AggregateTopic::AggOrderBook);

  mds::consume::OrderBookReconstructor book;
  SnapshotBeginRecord begin{};
  begin.header = header(MessageType::SnapshotBegin, sizeof(begin));
  begin.item_count = 2;
  begin.chunk_count_or_checksum = 2;
  assert(book.apply_snapshot_begin(begin) ==
         mds::consume::ReconstructionError::None);

  SnapshotChunkRecord bid{};
  bid.header = header(MessageType::SnapshotChunk, sizeof(bid));
  bid.side = static_cast<std::uint8_t>(Side::Bid);
  bid.level_count = 1;
  bid.levels[0] = {100, 3};
  assert(book.apply(bid) == mds::consume::ReconstructionError::None);
  auto ask = bid;
  ask.side = static_cast<std::uint8_t>(Side::Ask);
  ask.levels[0] = {101, 4};
  assert(book.apply(ask) == mds::consume::ReconstructionError::None);

  SnapshotEndRecord end{};
  end.header = header(MessageType::SnapshotEnd, sizeof(end));
  end.item_count = 2;
  assert(book.apply_snapshot_end(end) ==
         mds::consume::ReconstructionError::None);
  assert(book.live() && book.bids().begin()->first == 100 &&
         book.asks().begin()->first == 101);

  DeltaRecord delta{};
  delta.header = header(MessageType::BookDelta, sizeof(delta));
  delta.side = static_cast<std::uint8_t>(Side::Bid);
  delta.price = 100;
  delta.quantity = 5;
  assert(book.apply(delta) == mds::consume::ReconstructionError::None);
  assert(book.bids().begin()->second == 5);

  const auto unique = std::to_string(::getpid()) + "." +
                      std::to_string(std::chrono::steady_clock::now()
                                         .time_since_epoch()
                                         .count());
  mds::transport::RingOptions ring_options;
  ring_options.name = "/selfquant.consumer." + unique;
  ring_options.ring_bytes = 16U << 10U;
  ring_options.max_record_bytes = 512;
  ring_options.max_readers = 2;
  ring_options.unlink_on_close = true;
  auto ring = mds::transport::SharedRing::open(ring_options);
  assert(ring);
  mds::publish::WirePublisher publisher(std::move(ring.value));
  auto reader = mds::consume::SegmentReader::open(ring_options.name);
  assert(reader);

  EventHeader event{};
  event.instrument_id = 42;
  event.book_generation = 7;
  event.bus_seq = 1;
  event.source_seq = 1;
  event.state = BookState::Live;
  assert(publisher.publish_bbo({event, {100, 3}, {101, 4}}));
  auto consumed = reader.value.try_read();
  assert(consumed && consumed.value.has_value());
  BboRecord decoded{};
  assert(DecodeBbo(consumed.value->payload(), decoded) == CodecError::Ok);
  assert(decoded.bid_price == 100 && decoded.ask_price == 101);
  return 0;
}
