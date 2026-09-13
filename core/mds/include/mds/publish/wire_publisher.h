#pragma once

#include "mds/api/mds_api.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/order_book.h"
#include "utils/md/wire_codec.h"

#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::publish {

enum class RingLayout : std::uint8_t { PerSymbol, Multiplex, Both };

class WirePublisher {
public:
  using ReaderChangeHook = void (*)(void *, WirePublisher &) noexcept;

  WirePublisher() = default;
  explicit WirePublisher(transport::SharedRing &ring) noexcept;
  explicit WirePublisher(transport::SharedRing &&ring) noexcept;

  WirePublisher(const WirePublisher &) = delete;
  WirePublisher &operator=(const WirePublisher &) = delete;
  WirePublisher(WirePublisher &&other) noexcept;
  WirePublisher &operator=(WirePublisher &&other) noexcept;

  [[nodiscard]] std::uint64_t producer_epoch() const noexcept;
  [[nodiscard]] std::string_view segment_name() const noexcept;
  [[nodiscard]] std::uint64_t current_bus_seq() const noexcept {
    return current_bus_seq_;
  }
  [[nodiscard]] std::uint64_t bbo_origin_missing() const noexcept {
    return bbo_origin_missing_;
  }
  [[nodiscard]] std::uint32_t reader_registry_generation() const noexcept;
  void set_mirror(WirePublisher *mirror) noexcept { mirror_ = mirror; }
  std::size_t reclaim_stale_readers(std::uint64_t now_ns,
                                    std::uint64_t lease_timeout_ns) noexcept;
  void set_reader_change_hook(void *context, ReaderChangeHook hook) noexcept;
  // Returns true when the registry changed. The hook can republish the latest
  // Instrument and snapshot after a late reader attaches.
  bool poll_reader_change() noexcept;
  // Call once during setup before publishing AggOrderBook records. This keeps
  // the large encode buffer off ordinary ticker/orderbook publishers while
  // preserving zero allocations on the aggregate hot path.
  api::Result<void> prepare_aggregate_orderbook() noexcept;

  api::Result<std::uint64_t>
  publish_instrument(const utils::md::EventHeader &header,
                     const utils::md::Instrument &instrument) noexcept;
  api::Result<std::uint64_t>
  publish_instrument_catalog(
      const utils::md::EventHeader &header,
      const utils::md::InstrumentCatalog &catalog) noexcept;
  api::Result<std::uint64_t>
  publish_bbo(const utils::md::BboEvent &event) noexcept;
  api::Result<std::uint64_t>
  publish_ticker(const utils::md::TickerEvent &event) noexcept;
  api::Result<std::uint64_t>
  publish_delta(const utils::md::BookDelta &event) noexcept;
  api::Result<std::uint64_t>
  publish_agg_bbo(const utils::md::EventHeader &header,
                  const utils::md::wire::AggBboRecord &record,
                  std::uint16_t flags = 0) noexcept;
  api::Result<std::uint64_t>
  publish_agg_orderbook(
      const utils::md::EventHeader &header,
      const utils::md::wire::AggOrderBookRecord &record) noexcept;
  api::Result<std::uint64_t>
  publish_snapshot_begin(const utils::md::EventHeader &header,
                         std::uint32_t level_count,
                         std::uint32_t chunk_count) noexcept;
  api::Result<std::uint64_t>
  publish_snapshot_chunk(const utils::md::EventHeader &header,
                         std::uint32_t chunk_index,
                         utils::md::Side side,
                         std::span<const utils::md::Level> levels) noexcept;
  api::Result<std::uint64_t>
  publish_snapshot_end(const utils::md::EventHeader &header,
                       std::uint32_t received_levels,
                       std::uint32_t checksum) noexcept;

  // Publishes Begin, contiguous 24-level chunks, then End. If the ring applies
  // backpressure, QuotaExceeded is returned immediately; records already
  // committed remain visible and the caller must start a new generation.
  api::Result<void>
  publish_snapshot(const utils::md::EventHeader &header,
                   utils::md::Side side,
                   std::span<const utils::md::Level> levels,
                   std::uint32_t checksum = 0) noexcept;
  api::Result<void>
  publish_snapshot(const utils::md::EventHeader &header,
                   std::span<const utils::md::Level> bids,
                   std::span<const utils::md::Level> asks,
                   std::uint32_t checksum = 0) noexcept;
  api::Result<void>
  publish_snapshot(const utils::md::EventHeader &header,
                   const utils::md::OrderBook &book,
                   std::uint32_t checksum = 0);

private:
  api::Result<std::uint64_t>
  publish_encoded(utils::md::MessageType type,
                  const utils::md::wire::EncodeResult &encoded,
                  std::span<const std::byte> bytes) noexcept;
  bool assign_bus_seq(const utils::md::EventHeader &header,
                      utils::md::wire::HeaderFields &fields) noexcept;

  std::optional<transport::SharedRing> owned_ring_{};
  transport::SharedRing *ring_{};
  std::uint64_t current_bus_seq_{};
  std::uint64_t bbo_origin_missing_{};
  bool bus_seq_exhausted_{};
  std::uint32_t observed_registry_generation_{};
  void *reader_change_context_{};
  ReaderChangeHook reader_change_hook_{};
  std::vector<std::byte> aggregate_buffer_{};
  WirePublisher *mirror_{};
};

class TickerOrderBookPublishers {
public:
  TickerOrderBookPublishers() = default;
  TickerOrderBookPublishers(WirePublisher &&ticker,
                            WirePublisher &&order_book) noexcept;

  TickerOrderBookPublishers(const TickerOrderBookPublishers &) = delete;
  TickerOrderBookPublishers &
  operator=(const TickerOrderBookPublishers &) = delete;
  TickerOrderBookPublishers(TickerOrderBookPublishers &&) noexcept = default;
  TickerOrderBookPublishers &
  operator=(TickerOrderBookPublishers &&) noexcept = default;

  static api::Result<TickerOrderBookPublishers>
  open(std::string_view profile, std::string_view symbol,
       const transport::RingOptions &options);

  [[nodiscard]] WirePublisher &ticker() noexcept { return ticker_; }
  [[nodiscard]] WirePublisher &order_book() noexcept { return order_book_; }
  [[nodiscard]] const WirePublisher &ticker() const noexcept { return ticker_; }
  [[nodiscard]] const WirePublisher &order_book() const noexcept {
    return order_book_;
  }

private:
  WirePublisher ticker_{};
  WirePublisher order_book_{};
};

std::string make_publisher_segment_name(std::string_view profile,
                                        std::string_view symbol,
                                        std::string_view stream);
std::string make_publisher_segment_name(std::string_view prefix,
                                        std::string_view profile,
                                        std::string_view symbol,
                                        std::string_view stream);
std::string make_multiplex_segment_name(std::string_view prefix,
                                        std::string_view venue,
                                        std::string_view product,
                                        std::string_view stream,
                                        std::size_t shard);

} // namespace mds::publish
