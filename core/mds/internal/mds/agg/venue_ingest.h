#pragma once

#include "mds/agg/aggregation_engine.h"
#include "mds/book/book_bridge.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/order_book.h"
#include "utils/md/wire_codec.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <span>
#include <string>

namespace mds::agg {

enum class IngestResult : std::uint8_t {
  Ignored,
  Instrument,
  Bbo,
  Book,
  NeedResync,
  Invalid,
};

class VenueIngest {
public:
  struct Selector {
    utils::md::Venue venue{utils::md::Venue::Unknown};
    utils::md::ProductType product{utils::md::ProductType::Unknown};
    std::string canonical_symbol;
  };

  explicit VenueIngest(std::size_t ladder_capacity =
                           utils::md::kMaxLadderLevels);
  VenueIngest(Selector selector,
              std::size_t ladder_capacity =
                  utils::md::kMaxLadderLevels);

  [[nodiscard]] IngestResult
  consume(std::uint64_t ring_sequence, std::uint32_t outer_type,
          std::span<const std::byte> payload,
          std::uint64_t ingress_mono_ns) noexcept;
  void reset() noexcept;

  [[nodiscard]] const utils::md::Instrument *instrument() const noexcept {
    return have_instrument_ ? &instrument_ : nullptr;
  }
  [[nodiscard]] const utils::md::wire::BboRecord *bbo() const noexcept {
    return have_bbo_ ? &bbo_ : nullptr;
  }
  [[nodiscard]] std::uint64_t bbo_ingress_ns() const noexcept {
    return bbo_ingress_ns_;
  }
  [[nodiscard]] bool instrument_changed() const noexcept {
    return instrument_changed_;
  }
  [[nodiscard]] bool book_input(BookInput &input) noexcept;

private:
  [[nodiscard]] bool sequence_ok(
      std::uint64_t ring_sequence,
      const utils::md::wire::RecordHeader &header) noexcept;
  void accept_sequence(std::uint64_t ring_sequence,
                       const utils::md::wire::RecordHeader &header) noexcept;
  [[nodiscard]] bool rebuild_book(
      const utils::md::wire::SnapshotEndRecord &end) noexcept;

  std::size_t ladder_capacity_{};
  Selector selector_{};
  utils::md::InstrumentId selected_instrument_id_{};
  std::unique_ptr<utils::md::OrderBook> book_{};
  std::optional<book::BookBridge> bridge_{};
  utils::md::Instrument instrument_{};
  utils::md::wire::BboRecord bbo_{};
  std::array<utils::md::Level, utils::md::kMaxLadderLevels> snapshot_bids_{};
  std::array<utils::md::Level, utils::md::kMaxLadderLevels> snapshot_asks_{};
  std::array<utils::md::Level, kLevelsPerMember> top_bids_{};
  std::array<utils::md::Level, kLevelsPerMember> top_asks_{};
  std::size_t snapshot_bid_count_{};
  std::size_t snapshot_ask_count_{};
  std::size_t top_bid_count_{};
  std::size_t top_ask_count_{};
  std::uint64_t snapshot_bridge_sequence_{};
  std::uint64_t bbo_ingress_ns_{};
  std::uint64_t book_ingress_ns_{};
  std::uint64_t book_exchange_ts_ns_{};
  std::uint64_t last_ring_sequence_{};
  std::uint64_t last_bus_sequence_{};
  std::uint64_t last_source_sequence_{};
  std::uint32_t source_generation_{};
  std::uint32_t book_generation_{};
  bool have_sequence_{};
  bool have_source_sequence_{};
  bool have_instrument_{};
  bool instrument_changed_{};
  bool have_bbo_{};
  bool snapshot_active_{};
  bool book_dirty_{};
};

}  // namespace mds::agg
