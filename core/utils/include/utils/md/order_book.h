#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <optional>

#include "utils/md/types.h"

namespace utils::md {

inline constexpr std::size_t kMaxLadderLevels = 16384;
inline constexpr std::size_t kLadderBitmapWords = kMaxLadderLevels / 64;

enum class LadderResult : std::uint8_t { Ok, InvalidPrice, OutOfWindow, NeedsRestart };

class Ladder {
 public:
  explicit Ladder(Side side,
                  std::size_t capacity = kMaxLadderLevels) noexcept;
  void Reset(std::int64_t window_base_ticks, std::int64_t tick_size,
             std::uint32_t generation) noexcept;
  LadderResult Apply(std::int64_t price, std::int64_t quantity) noexcept;
  LadderResult ChangeTickSize(std::int64_t tick_size) noexcept;
  [[nodiscard]] std::optional<Level> Best() const noexcept;
  [[nodiscard]] std::optional<Level> At(std::size_t index) const noexcept;
  [[nodiscard]] BookState state() const noexcept { return state_; }
  [[nodiscard]] std::size_t capacity() const noexcept { return capacity_; }
  [[nodiscard]] std::int64_t window_base_ticks() const noexcept {
    return window_base_ticks_;
  }
  [[nodiscard]] std::int64_t tick_size() const noexcept { return tick_size_; }
  [[nodiscard]] std::int32_t best_index() const noexcept { return best_index_; }
  [[nodiscard]] std::uint32_t generation() const noexcept { return generation_; }
  void SetLive() noexcept;

 private:
  void RecomputeBest() noexcept;
  [[nodiscard]] bool Occupied(std::size_t index) const noexcept;

  Side side_;
  std::size_t capacity_;
  std::int64_t window_base_ticks_{};
  std::int64_t tick_size_{};
  std::uint32_t generation_{};
  BookState state_{BookState::Empty};
  std::int32_t best_index_{-1};
  std::array<std::int64_t, kMaxLadderLevels> quantities_{};
  std::array<std::uint64_t, kLadderBitmapWords> occupied_{};
};

class OrderBook {
 public:
  explicit OrderBook(
      std::size_t capacity_per_side = kMaxLadderLevels) noexcept;
  void Reset(std::int64_t bid_base_ticks, std::int64_t ask_base_ticks,
             std::int64_t tick_size, std::uint32_t generation) noexcept;
  LadderResult Apply(Side side, std::int64_t price, std::int64_t quantity) noexcept;
  LadderResult ChangeTickSize(std::int64_t tick_size) noexcept;
  void SetLive() noexcept;
  [[nodiscard]] std::optional<BboEvent> Bbo(std::uint32_t instrument_id) const noexcept;
  [[nodiscard]] const Ladder& bids() const noexcept { return bids_; }
  [[nodiscard]] const Ladder& asks() const noexcept { return asks_; }
  [[nodiscard]] BookState state() const noexcept;

 private:
  Ladder bids_;
  Ladder asks_;
};

}  // namespace utils::md
