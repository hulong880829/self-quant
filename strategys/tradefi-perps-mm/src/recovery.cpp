#include "tradefimm/recovery.h"

namespace tradefimm {

bool Recovery::remember_trade(std::uint64_t trade_id) noexcept {
  for (int index = 0; index < trade_count_; ++index) {
    if (trades_[static_cast<std::size_t>(index)] == trade_id) return false;
  }
  if (trade_count_ >= kTradeCapacity) return false;
  trades_[static_cast<std::size_t>(trade_count_++)] = trade_id;
  return true;
}

bool Recovery::reserve(SlotId slot, std::uint64_t generation, bool buy,
                       std::uint64_t now_ns) noexcept {
  Slot& item = at(slot);
  if (item.state != SlotState::Empty && item.state != SlotState::Terminal)
    return false;
  if (halted_ || (slot != SlotId::Hedge && slot != SlotId::Unwind &&
                  (makers_stopped_ || !quoting_ || circuit_open_)))
    return false;
  item = Slot{};
  item.state = SlotState::Reserved;
  item.generation = generation;
  item.buy = buy;
  item.submitted_ns = now_ns;
  return true;
}

bool Recovery::bind_submission(SlotId slot, std::uint64_t generation,
                               std::uint64_t token) noexcept {
  Slot& item = at(slot);
  if (item.state != SlotState::Reserved || item.generation != generation ||
      token == 0)
    return false;
  item.token = token;
  item.state = SlotState::Submitted;
  return true;
}

void Recovery::rollback_reservation(SlotId slot,
                                    std::uint64_t generation) noexcept {
  Slot& item = at(slot);
  if (item.state == SlotState::Reserved && item.generation == generation)
    item = Slot{};
}

bool Recovery::submit(SlotId slot, std::uint64_t generation,
                      std::uint64_t token, bool buy,
                      std::uint64_t now_ns) noexcept {
  if (!reserve(slot, generation, buy, now_ns)) return false;
  if (bind_submission(slot, generation, token)) return true;
  rollback_reservation(slot, generation);
  return false;
}

const Slot* Recovery::slot_for_token(std::uint64_t token) const noexcept {
  for (const Slot& item : slots_) {
    if (item.token == token && item.state != SlotState::Empty) return &item;
  }
  return nullptr;
}

std::uint64_t Recovery::reconnect_delay_ns(bool maker_leg,
                                           std::uint64_t entropy) const noexcept {
  const int count = maker_leg ? maker_reconnects_ : hedge_reconnects_;
  std::uint64_t delay = 100'000'000ULL;
  for (int index = 1; index < count && delay < 5'000'000'000ULL; ++index) {
    delay *= 2;
  }
  if (delay > 5'000'000'000ULL) delay = 5'000'000'000ULL;
  const std::uint64_t jitter = (entropy % 50'000'000ULL);
  return delay + (maker_leg ? jitter : jitter / 2);
}

void Recovery::ack(SlotId slot, std::uint64_t generation) noexcept {
  Slot& item = at(slot);
  if (item.state == SlotState::Empty || generation != item.generation) return;
  if (item.state == SlotState::Submitted) item.state = SlotState::Acked;
}

bool Recovery::fill(SlotId slot, std::uint64_t trade_id, std::int64_t quantity,
                    std::uint64_t generation) noexcept {
  if (!remember_trade(trade_id)) return false;
  Slot& item = at(slot);
  if (item.state == SlotState::Empty || generation != item.generation)
    return false;
  item.filled += quantity;
  item.seen_fill = true;
  if (item.state != SlotState::Terminal)
    item.state = SlotState::PartiallyFilled;
  return true;
}

void Recovery::terminal(SlotId slot, std::uint64_t generation) noexcept {
  Slot& item = at(slot);
  if (item.state != SlotState::Empty && item.generation == generation)
    item.state = SlotState::Terminal;
}

void Recovery::cancel_ack(SlotId slot, std::uint64_t generation) noexcept {
  Slot& item = at(slot);
  if (generation != item.generation) return;
  if (item.state == SlotState::CancelPending || item.state == SlotState::Acked ||
      item.state == SlotState::Submitted ||
      item.state == SlotState::PartiallyFilled) {
    item.state = SlotState::Terminal;
  }
}

void Recovery::timeout(SlotId slot) noexcept {
  Slot& item = at(slot);
  if (item.state == SlotState::Submitted || item.state == SlotState::Acked ||
      item.state == SlotState::PartiallyFilled)
    item.state = SlotState::CancelPending;
}

void Recovery::mark_uncertain(SlotId slot) noexcept {
  Slot& item = at(slot);
  if (item.state == SlotState::Empty || item.state == SlotState::Terminal) return;
  item.state = SlotState::Uncertain;
  quoting_ = false;
}

void Recovery::query_open(SlotId slot) noexcept {
  Slot& item = at(slot);
  if (item.state != SlotState::Uncertain) return;
  item.state = SlotState::CancelPending;
}

void Recovery::query_filled(SlotId slot) noexcept {
  Slot& item = at(slot);
  item.seen_fill = true;
  item.state = SlotState::Terminal;
  if (slot == SlotId::MakerBid || slot == SlotId::MakerAsk) hedge_recovery_ = true;
}

bool Recovery::query_absent(SlotId slot) noexcept {
  Slot& item = at(slot);
  if (item.seen_fill) return false;
  item.state = SlotState::Empty;
  item.token = 0;
  return true;
}

void Recovery::maker_reject() noexcept {
  ++maker_rejects_;
  if (maker_rejects_ >= 3) {
    quoting_ = false;
    makers_stopped_ = true;
  }
}

void Recovery::begin_hedge() noexcept {
  makers_stopped_ = true;
  quoting_ = false;
  hedge_recovery_ = true;
}

void Recovery::complete_hedge() noexcept {
  if (halted_) return;
  hedge_recovery_ = false;
  if (maker_rejects_ < 3 && !circuit_open_) {
    makers_stopped_ = false;
    quoting_ = true;
  }
}

void Recovery::hedge_reject() noexcept {
  makers_stopped_ = true;
  quoting_ = false;
  hedge_recovery_ = true;
}

void Recovery::unwind_reject() noexcept {
  halt();
}

void Recovery::halt() noexcept {
  halted_ = true;
  quoting_ = false;
  makers_stopped_ = true;
}

void Recovery::maker_venue_down() noexcept {
  quoting_ = false;
  for (SlotId slot :
       {SlotId::MakerBid, SlotId::MakerAsk, SlotId::Unwind}) {
    if (at(slot).state == SlotState::Reserved ||
        at(slot).state == SlotState::Submitted ||
        at(slot).state == SlotState::Acked ||
        at(slot).state == SlotState::PartiallyFilled ||
        at(slot).state == SlotState::CancelPending)
      at(slot).state = SlotState::Uncertain;
  }
}

void Recovery::hedge_venue_down() noexcept {
  quoting_ = false;
  makers_stopped_ = true;
  for (SlotId slot : {SlotId::MakerBid, SlotId::MakerAsk}) {
    if (at(slot).state == SlotState::Submitted ||
        at(slot).state == SlotState::Acked ||
        at(slot).state == SlotState::PartiallyFilled)
      at(slot).state = SlotState::CancelPending;
  }
  Slot& hedge = at(SlotId::Hedge);
  if (hedge.state == SlotState::Reserved ||
      hedge.state == SlotState::Submitted ||
      hedge.state == SlotState::Acked ||
      hedge.state == SlotState::PartiallyFilled ||
      hedge.state == SlotState::CancelPending)
    hedge.state = SlotState::Uncertain;
}

void Recovery::note_reconnect(bool maker_leg) noexcept {
  int& count = maker_leg ? maker_reconnects_ : hedge_reconnects_;
  ++count;
  if (maker_leg) {
    ++maker_generation_;
    maker_stable_ = false;
  } else {
    ++hedge_generation_;
    hedge_stable_ = false;
  }
  quoting_ = false;
  if (count >= kMaxReconnects) circuit_open_ = true;
}

void Recovery::note_stable(bool maker_leg) noexcept {
  if (maker_leg) {
    maker_stable_ = true;
    maker_reconnects_ = 0;
  } else {
    hedge_stable_ = true;
    hedge_reconnects_ = 0;
  }
}

bool Recovery::can_resume_quotes() const noexcept {
  if (halted_ || circuit_open_ || makers_stopped_ || hedge_recovery_) return false;
  if (!maker_stable_ || !hedge_stable_) return false;
  if (!barrier_.complete()) return false;
  for (const Slot& item : slots_) {
    if (item.state == SlotState::Uncertain ||
        item.state == SlotState::Reserved ||
        item.state == SlotState::Submitted ||
        item.state == SlotState::CancelPending)
      return false;
  }
  return true;
}

}  // namespace tradefimm
