#include "mds/book/book_pipeline.h"

namespace mds::book {

BookPipeline::BookPipeline(utils::md::InstrumentId instrument_id,
                           std::size_t ladder_ticks_per_side,
                           std::int64_t tick_size,
                           publish::WirePublisher *publisher,
                           bool refine_snapshot_tick,
                           bool allow_empty_sides)
    : instrument_id_(instrument_id),
      book_(std::make_unique<utils::md::OrderBook>(
          ladder_ticks_per_side)),
      bridge_(std::make_unique<BookBridge>(
          *book_, tick_size, refine_snapshot_tick, allow_empty_sides)),
      publisher_(publisher),
      allow_empty_sides_(allow_empty_sides) {}

PipelineResult BookPipeline::LoadImage(
    const utils::md::EventHeader &header, std::uint64_t image_sequence,
    std::span<const utils::md::Level> bids,
    std::span<const utils::md::Level> asks,
    bool publish_image) noexcept {
  const auto loaded = bridge_->LoadSnapshot(
      DepthSnapshotView{image_sequence, bids, asks},
      header.book_generation);
  if (loaded.action != BridgeAction::Applied) {
    return PipelineResult::InvalidImage;
  }
  bridge_->SetLive();
  if (publisher_ != nullptr && publish_image) {
    auto snapshot_header = header;
    snapshot_header.instrument_id = instrument_id_;
    snapshot_header.source_seq = image_sequence;
    snapshot_header.state = utils::md::BookState::Live;
    if (!publisher_->publish_snapshot(snapshot_header, bids, asks)) {
      return PipelineResult::PublishFailed;
    }
    if (!PublishCanonical(snapshot_header)) {
      return PipelineResult::PublishFailed;
    }
  }
  return PipelineResult::Applied;
}

bool BookPipeline::publish_level(const utils::md::EventHeader &header,
                                 utils::md::Side side,
                                 const utils::md::Level &level) noexcept {
  if (publisher_ == nullptr) {
    return true;
  }
  utils::md::BookDelta event{};
  event.header = header;
  event.header.instrument_id = instrument_id_;
  event.header.state = utils::md::BookState::Live;
  event.side = side;
  event.level = level;
  return bool(publisher_->publish_delta(event));
}

PipelineResult BookPipeline::ApplyDelta(
    const utils::md::EventHeader &header, const DepthUpdateView &update,
    bool publish_delta) noexcept {
  const auto action = bridge_->Apply(update);
  if (action == BridgeAction::Resync) {
    return PipelineResult::Resync;
  }
  if (action == BridgeAction::InvalidSnapshot) {
    return PipelineResult::InvalidImage;
  }
  if (publisher_ != nullptr && publish_delta) {
    for (const auto &level : update.bids) {
      if (bridge_->Maintains(utils::md::Side::Bid, level.price) &&
          !publish_level(header, utils::md::Side::Bid, level)) {
        return PipelineResult::PublishFailed;
      }
    }
    for (const auto &level : update.asks) {
      if (bridge_->Maintains(utils::md::Side::Ask, level.price) &&
          !publish_level(header, utils::md::Side::Ask, level)) {
        return PipelineResult::PublishFailed;
      }
    }
    if (!PublishCanonical(header)) {
      return PipelineResult::PublishFailed;
    }
  }
  return PipelineResult::Applied;
}

bool BookPipeline::PublishCanonical(
    const utils::md::EventHeader &header) noexcept {
  const auto value = book_->Bbo(instrument_id_);
  if (!value) {
    return allow_empty_sides_;
  }
  if (publisher_ == nullptr) {
    return true;
  }
  auto event = *value;
  event.header = header;
  event.header.instrument_id = instrument_id_;
  event.header.state = utils::md::BookState::Live;
  return bool(publisher_->publish_bbo(event));
}

}  // namespace mds::book
