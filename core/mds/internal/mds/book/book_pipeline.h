#pragma once

#include "mds/book/book_bridge.h"
#include "mds/publish/wire_publisher.h"

#include <cstddef>
#include <cstdint>
#include <memory>
#include <span>

namespace mds::book {

enum class PipelineResult : std::uint8_t {
  Applied,
  Resync,
  InvalidImage,
  PublishFailed,
};

// Venue-neutral fixed-capacity order-book pipeline. Exchange adapters own
// protocol sequencing and call either LoadImage or ApplyDelta after they have
// normalized prices, quantities and sequence identifiers.
class BookPipeline {
 public:
  BookPipeline(utils::md::InstrumentId instrument_id,
               std::size_t ladder_ticks_per_side,
               std::int64_t tick_size,
               publish::WirePublisher *publisher,
               bool refine_snapshot_tick = false,
               bool allow_empty_sides = false);

  BookPipeline(const BookPipeline &) = delete;
  BookPipeline &operator=(const BookPipeline &) = delete;

  [[nodiscard]] PipelineResult
  LoadImage(const utils::md::EventHeader &header,
            std::uint64_t image_sequence,
            std::span<const utils::md::Level> bids,
            std::span<const utils::md::Level> asks,
            bool publish_image = true) noexcept;

  [[nodiscard]] PipelineResult
  ApplyDelta(const utils::md::EventHeader &header,
             const DepthUpdateView &update,
             bool publish_delta = true) noexcept;

  [[nodiscard]] bool
  PublishCanonical(const utils::md::EventHeader &header) noexcept;

  [[nodiscard]] const utils::md::OrderBook &book() const noexcept {
    return *book_;
  }
  [[nodiscard]] std::uint64_t last_update_id() const noexcept {
    return bridge_->last_update_id();
  }
  [[nodiscard]] BridgeResyncReason last_resync_reason() const noexcept {
    return bridge_->last_resync_reason();
  }
  [[nodiscard]] std::int64_t SnapshotTickSize(
      std::span<const utils::md::Level> bids,
      std::span<const utils::md::Level> asks) const noexcept {
    return bridge_->SnapshotTickSize(bids, asks);
  }
  void PrepareSnapshotTick(
      std::span<const utils::md::Level> bids,
      std::span<const utils::md::Level> asks) noexcept {
    bridge_->PrepareSnapshotTick(bids, asks);
  }

 private:
  [[nodiscard]] bool publish_level(
      const utils::md::EventHeader &header, utils::md::Side side,
      const utils::md::Level &level) noexcept;

  utils::md::InstrumentId instrument_id_{};
  std::unique_ptr<utils::md::OrderBook> book_;
  std::unique_ptr<BookBridge> bridge_;
  publish::WirePublisher *publisher_{};
  bool allow_empty_sides_{};
};

}  // namespace mds::book
