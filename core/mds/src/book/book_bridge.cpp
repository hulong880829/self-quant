#include "mds/book/book_bridge.h"

#include <algorithm>
#include <limits>

namespace mds::book {
namespace {

bool valid_level(const utils::md::Level &level,
                 std::int64_t tick_size) noexcept {
  return tick_size > 0 && level.price >= 0 && level.quantity >= 0 &&
         level.price % tick_size == 0;
}

}  // namespace

SnapshotLoadResult BookBridge::LoadSnapshot(
    const DepthSnapshot &snapshot, std::uint32_t generation) noexcept {
  SnapshotLoadResult result{};
  loaded_ = false;
  book_.Reset(0, 0, 0, generation);
  if (tick_size_ <= 0 || snapshot.bids.empty() || snapshot.asks.empty()) {
    return result;
  }

  std::int64_t best_bid = std::numeric_limits<std::int64_t>::min();
  std::int64_t best_ask = std::numeric_limits<std::int64_t>::max();
  for (const auto &level : snapshot.bids) {
    if (!valid_level(level, tick_size_)) {
      return result;
    }
    if (level.quantity > 0) {
      best_bid = std::max(best_bid, level.price);
    }
  }
  for (const auto &level : snapshot.asks) {
    if (!valid_level(level, tick_size_)) {
      return result;
    }
    if (level.quantity > 0) {
      best_ask = std::min(best_ask, level.price);
    }
  }
  if (best_bid == std::numeric_limits<std::int64_t>::min() ||
      best_ask == std::numeric_limits<std::int64_t>::max() ||
      best_bid >= best_ask) {
    return result;
  }

  const auto capacity =
      static_cast<std::int64_t>(book_.bids().capacity());
  // Keep one quarter of each fixed window available for BBO improvements.
  // Anchoring the snapshot BBO at an edge would force a resync on every
  // one-tick price improvement.
  const auto improvement_headroom = std::max<std::int64_t>(1, capacity / 4);
  bid_base_ticks_ =
      best_bid / tick_size_ - (capacity - improvement_headroom - 1);
  ask_base_ticks_ = best_ask / tick_size_ - improvement_headroom;
  result.bid_base_ticks = bid_base_ticks_;
  result.ask_base_ticks = ask_base_ticks_;
  book_.Reset(bid_base_ticks_, ask_base_ticks_, tick_size_, generation);

  const auto load = [&](utils::md::Side side,
                        const std::vector<utils::md::Level> &levels,
                        std::size_t &inside, std::size_t &outside) {
    for (const auto &level : levels) {
      if (level.quantity == 0) {
        continue;
      }
      if (!InWindow(side, level.price)) {
        ++outside;
        continue;
      }
      if (book_.Apply(side, level.price, level.quantity) !=
          utils::md::LadderResult::Ok) {
        return false;
      }
      ++inside;
    }
    return true;
  };
  if (!load(utils::md::Side::Bid, snapshot.bids, result.loaded_bids,
            result.outside_bids) ||
      !load(utils::md::Side::Ask, snapshot.asks, result.loaded_asks,
            result.outside_asks) ||
      result.loaded_bids == 0 || result.loaded_asks == 0) {
    book_.Reset(0, 0, 0, generation);
    result.action = BridgeAction::InvalidSnapshot;
    return result;
  }

  last_update_id_ = snapshot.last_update_id;
  loaded_ = true;
  result.action = BridgeAction::Applied;
  return result;
}

bool BookBridge::InWindow(utils::md::Side side,
                          std::int64_t price) const noexcept {
  if (price < 0 || tick_size_ <= 0 || price % tick_size_ != 0) {
    return false;
  }
  const auto ticks = price / tick_size_;
  const auto base =
      side == utils::md::Side::Bid ? bid_base_ticks_ : ask_base_ticks_;
  const auto capacity =
      static_cast<std::int64_t>(book_.bids().capacity());
  return ticks >= base && ticks - base < capacity;
}

BridgeAction BookBridge::Apply(const DepthUpdateView &update) noexcept {
  if (!loaded_ || update.first_update_id > update.final_update_id) {
    return BridgeAction::Resync;
  }
  if (update.final_update_id <= last_update_id_) {
    return BridgeAction::Applied;
  }
  const auto old_bid = book_.bids().Best();
  const auto old_ask = book_.asks().Best();
  if (!old_bid || !old_ask) {
    loaded_ = false;
    return BridgeAction::Resync;
  }

  const auto validate = [&](utils::md::Side side,
                            std::span<const utils::md::Level> levels) {
    const auto best =
        side == utils::md::Side::Bid ? old_bid->price : old_ask->price;
    for (const auto &level : levels) {
      if (!valid_level(level, tick_size_)) {
        return false;
      }
      if (InWindow(side, level.price)) {
        continue;
      }
      const bool strictly_worse =
          side == utils::md::Side::Bid ? level.price < best
                                       : level.price > best;
      if (level.quantity != 0 && !strictly_worse) {
        return false;
      }
    }
    return true;
  };
  if (!validate(utils::md::Side::Bid, update.bids) ||
      !validate(utils::md::Side::Ask, update.asks)) {
    loaded_ = false;
    return BridgeAction::Resync;
  }

  bool deleted_best_bid = false;
  bool deleted_best_ask = false;
  const auto apply = [&](utils::md::Side side,
                         std::span<const utils::md::Level> levels,
                         std::int64_t old_best, bool &deleted_best) {
    for (const auto &level : levels) {
      if (!InWindow(side, level.price)) {
        ++outside_updates_ignored_;
        continue;
      }
      deleted_best |= level.quantity == 0 && level.price == old_best;
      if (book_.Apply(side, level.price, level.quantity) !=
          utils::md::LadderResult::Ok) {
        return false;
      }
    }
    return true;
  };
  if (!apply(utils::md::Side::Bid, update.bids, old_bid->price,
             deleted_best_bid) ||
      !apply(utils::md::Side::Ask, update.asks, old_ask->price,
             deleted_best_ask)) {
    loaded_ = false;
    return BridgeAction::Resync;
  }

  const auto new_bid = book_.bids().Best();
  const auto new_ask = book_.asks().Best();
  const bool bid_exhausted =
      deleted_best_bid &&
      (!new_bid || book_.bids().best_index() == 0);
  const bool ask_exhausted =
      deleted_best_ask &&
      (!new_ask || book_.asks().best_index() ==
                       static_cast<std::int32_t>(book_.asks().capacity() - 1U));
  if (bid_exhausted || ask_exhausted || !new_bid || !new_ask ||
      new_bid->price >= new_ask->price) {
    loaded_ = false;
    return BridgeAction::Resync;
  }
  last_update_id_ = update.final_update_id;
  return BridgeAction::Applied;
}

}  // namespace mds::book
