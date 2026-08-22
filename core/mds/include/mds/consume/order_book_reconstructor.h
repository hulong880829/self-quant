#pragma once

#include "utils/md/wire.h"

#include <cstdint>
#include <functional>
#include <map>

namespace mds::consume {

enum class ReconstructionError : std::uint8_t {
  None,
  StaleGeneration,
  NeedsSnapshot,
  InvalidSnapshot,
  InvalidSide,
};

class OrderBookReconstructor {
 public:
  using Bids = std::map<std::int64_t, std::int64_t, std::greater<>>;
  using Asks = std::map<std::int64_t, std::int64_t>;

  ReconstructionError
  apply_snapshot_begin(const utils::md::wire::SnapshotBeginRecord &record);
  ReconstructionError
  apply(const utils::md::wire::SnapshotChunkRecord &record);
  ReconstructionError
  apply_snapshot_end(const utils::md::wire::SnapshotEndRecord &record);
  ReconstructionError apply(const utils::md::wire::DeltaRecord &record);

  void reset(utils::md::BookState state =
                 utils::md::BookState::NeedsRestart) noexcept;
  [[nodiscard]] utils::md::InstrumentId instrument_id() const noexcept {
    return instrument_id_;
  }
  [[nodiscard]] std::uint32_t generation() const noexcept {
    return generation_;
  }
  [[nodiscard]] utils::md::BookState state() const noexcept { return state_; }
  [[nodiscard]] bool live() const noexcept {
    return state_ == utils::md::BookState::Live;
  }
  [[nodiscard]] const Bids &bids() const noexcept { return bids_; }
  [[nodiscard]] const Asks &asks() const noexcept { return asks_; }

 private:
  ReconstructionError invalid_snapshot() noexcept;

  utils::md::InstrumentId instrument_id_{};
  std::uint32_t generation_{};
  std::uint32_t expected_levels_{};
  std::uint32_t expected_chunks_{};
  std::uint32_t received_levels_{};
  std::uint32_t received_chunks_{};
  std::uint32_t next_bid_chunk_{};
  std::uint32_t next_ask_chunk_{};
  utils::md::BookState state_{utils::md::BookState::Empty};
  bool snapshot_active_{};
  Bids bids_{};
  Asks asks_{};
  Bids staging_bids_{};
  Asks staging_asks_{};
};

}  // namespace mds::consume
