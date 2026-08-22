#include "mds/consume/order_book_reconstructor.h"

#include <utility>

namespace mds::consume {

namespace {

bool is_bid(std::uint8_t side) noexcept {
  return side == static_cast<std::uint8_t>(utils::md::Side::Bid);
}

bool is_ask(std::uint8_t side) noexcept {
  return side == static_cast<std::uint8_t>(utils::md::Side::Ask);
}

}  // namespace

ReconstructionError OrderBookReconstructor::apply_snapshot_begin(
    const utils::md::wire::SnapshotBeginRecord &record) {
  if (record.header.book_generation < generation_) {
    return ReconstructionError::StaleGeneration;
  }
  staging_bids_.clear();
  staging_asks_.clear();
  bids_.clear();
  asks_.clear();
  instrument_id_ = record.header.instrument_id;
  generation_ = record.header.book_generation;
  expected_levels_ = record.item_count;
  expected_chunks_ = record.chunk_count_or_checksum;
  received_levels_ = 0;
  received_chunks_ = 0;
  next_bid_chunk_ = 0;
  next_ask_chunk_ = 0;
  snapshot_active_ = true;
  state_ = utils::md::BookState::Building;
  return ReconstructionError::None;
}

ReconstructionError OrderBookReconstructor::apply(
    const utils::md::wire::SnapshotChunkRecord &record) {
  if (!is_bid(record.side) && !is_ask(record.side)) {
    return invalid_snapshot();
  }
  auto &next_chunk = is_bid(record.side) ? next_bid_chunk_ : next_ask_chunk_;
  if (!snapshot_active_ || record.header.instrument_id != instrument_id_ ||
      record.header.book_generation != generation_ ||
      record.chunk_index != next_chunk ||
      record.level_count > utils::md::kSnapshotLevelsPerChunk ||
      received_chunks_ >= expected_chunks_ ||
      received_levels_ + record.level_count > expected_levels_) {
    return invalid_snapshot();
  }
  for (std::size_t index = 0; index < record.level_count; ++index) {
    const auto &level = record.levels[index];
    if (is_bid(record.side)) {
      staging_bids_[level.price] = level.quantity;
    } else {
      staging_asks_[level.price] = level.quantity;
    }
  }
  ++next_chunk;
  ++received_chunks_;
  received_levels_ += record.level_count;
  return ReconstructionError::None;
}

ReconstructionError OrderBookReconstructor::apply_snapshot_end(
    const utils::md::wire::SnapshotEndRecord &record) {
  if (!snapshot_active_ || record.header.instrument_id != instrument_id_ ||
      record.header.book_generation != generation_ ||
      received_chunks_ != expected_chunks_ ||
      received_levels_ != expected_levels_ ||
      record.item_count != received_levels_) {
    return invalid_snapshot();
  }
  snapshot_active_ = false;
  state_ = static_cast<utils::md::BookState>(record.header.state);
  if (state_ == utils::md::BookState::Live) {
    bids_ = std::move(staging_bids_);
    asks_ = std::move(staging_asks_);
  } else {
    bids_.clear();
    asks_.clear();
    staging_bids_.clear();
    staging_asks_.clear();
  }
  return ReconstructionError::None;
}

ReconstructionError OrderBookReconstructor::apply(
    const utils::md::wire::DeltaRecord &record) {
  if (!is_bid(record.side) && !is_ask(record.side)) {
    return ReconstructionError::InvalidSide;
  }
  if (record.header.book_generation < generation_) {
    return ReconstructionError::StaleGeneration;
  }
  if (record.header.book_generation != generation_ ||
      state_ != utils::md::BookState::Live ||
      record.header.instrument_id != instrument_id_ ||
      record.header.state !=
          static_cast<std::uint8_t>(utils::md::BookState::Live)) {
    instrument_id_ = record.header.instrument_id;
    generation_ = record.header.book_generation;
    reset();
    return ReconstructionError::NeedsSnapshot;
  }
  if (is_bid(record.side)) {
    if (record.quantity == 0) {
      bids_.erase(record.price);
    } else {
      bids_[record.price] = record.quantity;
    }
  } else if (record.quantity == 0) {
    asks_.erase(record.price);
  } else {
    asks_[record.price] = record.quantity;
  }
  return ReconstructionError::None;
}

void OrderBookReconstructor::reset(utils::md::BookState state) noexcept {
  expected_levels_ = 0;
  expected_chunks_ = 0;
  received_levels_ = 0;
  received_chunks_ = 0;
  next_bid_chunk_ = 0;
  next_ask_chunk_ = 0;
  snapshot_active_ = false;
  state_ = state;
  bids_.clear();
  asks_.clear();
  staging_bids_.clear();
  staging_asks_.clear();
}

ReconstructionError OrderBookReconstructor::invalid_snapshot() noexcept {
  reset(utils::md::BookState::Invalid);
  return ReconstructionError::InvalidSnapshot;
}

}  // namespace mds::consume
