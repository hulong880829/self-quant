#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <string_view>
#include <vector>

#include "utils/md/order_book.h"

namespace mds::book {

struct DepthSnapshot {
  std::uint64_t last_update_id{};
  std::vector<utils::md::Level> bids{};
  std::vector<utils::md::Level> asks{};
};

struct DepthSnapshotView {
  std::uint64_t last_update_id{};
  std::span<const utils::md::Level> bids{};
  std::span<const utils::md::Level> asks{};
};

struct DepthUpdateView {
  // Exchange adapters normalize their native sequence fields into this
  // inclusive range. For prev/current protocols first_update_id is
  // prev_sequence + 1 and final_update_id is current_sequence. Single-id
  // protocols set both fields to the same value.
  std::uint64_t first_update_id{};
  std::uint64_t final_update_id{};
  std::span<const utils::md::Level> bids{};
  std::span<const utils::md::Level> asks{};
};

enum class BridgeAction : std::uint8_t {
  Applied,
  Resync,
  InvalidSnapshot,
};

enum class BridgeResyncReason : std::uint8_t {
  None,
  NotLoaded,
  InvalidRange,
  EmptySide,
  InvalidLevel,
  OutsideImproving,
  LadderApplyFailed,
  BoundaryExhausted,
  CrossedBook,
  Count,
};

[[nodiscard]] std::string_view
to_string(BridgeResyncReason reason) noexcept;

struct SnapshotLoadResult {
  BridgeAction action{BridgeAction::InvalidSnapshot};
  std::size_t loaded_bids{};
  std::size_t loaded_asks{};
  std::size_t outside_bids{};
  std::size_t outside_asks{};
  std::int64_t bid_base_ticks{};
  std::int64_t ask_base_ticks{};
};

class BookBridge {
 public:
  BookBridge(utils::md::OrderBook &book, std::int64_t tick_size,
             bool refine_snapshot_tick = false,
             bool allow_empty_sides = false) noexcept
      : book_(book), configured_tick_size_(tick_size),
        tick_size_(tick_size), prepared_tick_size_(tick_size),
        refine_snapshot_tick_(refine_snapshot_tick),
        allow_empty_sides_(allow_empty_sides) {}

  [[nodiscard]] SnapshotLoadResult
  LoadSnapshot(const DepthSnapshot &snapshot,
               std::uint32_t generation) noexcept;
  [[nodiscard]] SnapshotLoadResult
  LoadSnapshot(const DepthSnapshotView &snapshot,
               std::uint32_t generation) noexcept;
  [[nodiscard]] BridgeAction Apply(const DepthUpdateView &update) noexcept;
  [[nodiscard]] std::int64_t SnapshotTickSize(
      std::span<const utils::md::Level> bids,
      std::span<const utils::md::Level> asks) const noexcept;
  void PrepareSnapshotTick(
      std::span<const utils::md::Level> bids,
      std::span<const utils::md::Level> asks) noexcept;
  void SetLive() noexcept { book_.SetLive(); }
  [[nodiscard]] bool Maintains(utils::md::Side side,
                               std::int64_t price) const noexcept {
    return InWindow(side, price);
  }

  [[nodiscard]] std::uint64_t last_update_id() const noexcept {
    return last_update_id_;
  }
  [[nodiscard]] std::uint64_t outside_updates_ignored() const noexcept {
    return outside_updates_ignored_;
  }
  [[nodiscard]] BridgeResyncReason last_resync_reason() const noexcept {
    return last_resync_reason_;
  }
  [[nodiscard]] std::int64_t bid_base_ticks() const noexcept {
    return bid_base_ticks_;
  }
  [[nodiscard]] std::int64_t ask_base_ticks() const noexcept {
    return ask_base_ticks_;
  }

 private:
  [[nodiscard]] bool InWindow(utils::md::Side side,
                              std::int64_t price) const noexcept;

  utils::md::OrderBook &book_;
  std::int64_t configured_tick_size_{};
  std::int64_t tick_size_{};
  std::int64_t prepared_tick_size_{};
  std::int64_t bid_base_ticks_{};
  std::int64_t ask_base_ticks_{};
  std::uint64_t last_update_id_{};
  std::uint64_t outside_updates_ignored_{};
  BridgeResyncReason last_resync_reason_{BridgeResyncReason::None};
  bool refine_snapshot_tick_{};
  bool allow_empty_sides_{};
  bool loaded_{};
};

}  // namespace mds::book
