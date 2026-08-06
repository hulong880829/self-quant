#include "utils/md/order_book.h"

#include <algorithm>
#include <bit>

namespace utils::md {

Ladder::Ladder(Side side, std::size_t capacity) noexcept
    : side_(side), capacity_(std::clamp<std::size_t>(capacity, 1, kMaxLadderLevels)) {}

void Ladder::Reset(std::int64_t window_base_ticks, std::int64_t tick_size,
                   std::uint32_t generation) noexcept {
  window_base_ticks_ = window_base_ticks;
  tick_size_ = tick_size;
  generation_ = generation;
  state_ = tick_size > 0 ? BookState::Building : BookState::Invalid;
  best_index_ = -1;
  quantities_.fill(0);
  occupied_.fill(0);
}

bool Ladder::Occupied(std::size_t index) const noexcept {
  return (occupied_[index / 64] & (std::uint64_t{1} << (index % 64))) != 0;
}

LadderResult Ladder::Apply(std::int64_t price, std::int64_t quantity) noexcept {
  if (state_ == BookState::NeedsRestart) return LadderResult::NeedsRestart;
  if (state_ == BookState::Invalid || tick_size_ <= 0 || price < 0 || quantity < 0 ||
      price % tick_size_ != 0) {
    state_ = BookState::Invalid;
    return LadderResult::InvalidPrice;
  }
  const std::int64_t ticks = price / tick_size_;
  const std::int64_t relative = ticks - window_base_ticks_;
  if (relative < 0 || relative >= static_cast<std::int64_t>(capacity_)) {
    state_ = BookState::Invalid;
    return LadderResult::OutOfWindow;
  }
  const auto index = static_cast<std::size_t>(relative);
  const auto bit = std::uint64_t{1} << (index % 64);
  if (quantity == 0) {
    quantities_[index] = 0;
    occupied_[index / 64] &= ~bit;
    if (best_index_ == static_cast<std::int32_t>(index)) RecomputeBest();
  } else {
    quantities_[index] = quantity;
    occupied_[index / 64] |= bit;
    if (best_index_ < 0 ||
        (side_ == Side::Bid && index > static_cast<std::size_t>(best_index_)) ||
        (side_ == Side::Ask && index < static_cast<std::size_t>(best_index_))) {
      best_index_ = static_cast<std::int32_t>(index);
    }
  }
  return LadderResult::Ok;
}

void Ladder::RecomputeBest() noexcept {
  best_index_ = -1;
  if (side_ == Side::Ask) {
    for (std::size_t word = 0; word < (capacity_ + 63) / 64; ++word) {
      if (occupied_[word] != 0) {
        best_index_ = static_cast<std::int32_t>(
            word * 64 +
            static_cast<std::size_t>(std::countr_zero(occupied_[word])));
        return;
      }
    }
  } else {
    for (std::size_t word = (capacity_ + 63) / 64; word-- > 0;) {
      std::uint64_t bits = occupied_[word];
      if (word == capacity_ / 64 && capacity_ % 64 != 0)
        bits &= (std::uint64_t{1} << (capacity_ % 64)) - 1;
      if (bits != 0) {
        best_index_ = static_cast<std::int32_t>(
            word * 64 + 63 -
            static_cast<std::size_t>(std::countl_zero(bits)));
        return;
      }
    }
  }
}

LadderResult Ladder::ChangeTickSize(std::int64_t tick_size) noexcept {
  if (tick_size == tick_size_) return LadderResult::Ok;
  state_ = BookState::NeedsRestart;
  return LadderResult::NeedsRestart;
}

std::optional<Level> Ladder::Best() const noexcept {
  if (best_index_ < 0) return std::nullopt;
  const auto index = static_cast<std::size_t>(best_index_);
  return Level{(window_base_ticks_ + static_cast<std::int64_t>(index)) * tick_size_,
               quantities_[index]};
}

std::optional<Level> Ladder::At(std::size_t index) const noexcept {
  if (index >= capacity_ || !Occupied(index)) return std::nullopt;
  return Level{(window_base_ticks_ + static_cast<std::int64_t>(index)) * tick_size_,
               quantities_[index]};
}

void Ladder::SetLive() noexcept {
  if (state_ == BookState::Building) state_ = BookState::Live;
}

OrderBook::OrderBook(std::size_t capacity_per_side) noexcept
    : bids_(Side::Bid, capacity_per_side), asks_(Side::Ask, capacity_per_side) {}

void OrderBook::Reset(std::int64_t bid_base_ticks, std::int64_t ask_base_ticks,
                      std::int64_t tick_size, std::uint32_t generation) noexcept {
  bids_.Reset(bid_base_ticks, tick_size, generation);
  asks_.Reset(ask_base_ticks, tick_size, generation);
}

LadderResult OrderBook::Apply(Side side, std::int64_t price, std::int64_t quantity) noexcept {
  return (side == Side::Bid ? bids_ : asks_).Apply(price, quantity);
}

LadderResult OrderBook::ChangeTickSize(std::int64_t tick_size) noexcept {
  const auto bid = bids_.ChangeTickSize(tick_size);
  const auto ask = asks_.ChangeTickSize(tick_size);
  return bid == LadderResult::NeedsRestart || ask == LadderResult::NeedsRestart
             ? LadderResult::NeedsRestart : LadderResult::Ok;
}

void OrderBook::SetLive() noexcept { bids_.SetLive(); asks_.SetLive(); }

BookState OrderBook::state() const noexcept {
  if (bids_.state() == BookState::NeedsRestart || asks_.state() == BookState::NeedsRestart)
    return BookState::NeedsRestart;
  if (bids_.state() == BookState::Invalid || asks_.state() == BookState::Invalid)
    return BookState::Invalid;
  if (bids_.state() == BookState::Live && asks_.state() == BookState::Live) return BookState::Live;
  return BookState::Building;
}

std::optional<BboEvent> OrderBook::Bbo(std::uint32_t instrument_id) const noexcept {
  if (state() != BookState::Live) return std::nullopt;
  const auto bid = bids_.Best();
  const auto ask = asks_.Best();
  if (!bid || !ask) return std::nullopt;
  BboEvent event{};
  event.header.instrument_id = instrument_id;
  event.header.book_generation = bids_.generation();
  event.header.state = BookState::Live;
  event.bid = *bid;
  event.ask = *ask;
  return event;
}

}  // namespace utils::md
