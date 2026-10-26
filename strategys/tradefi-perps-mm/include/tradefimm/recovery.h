#pragma once

#include <array>
#include <cstdint>

namespace tradefimm {

enum class SlotId : std::uint8_t { MakerBid = 0, MakerAsk, Hedge, Unwind };

enum class SlotState : std::uint8_t {
  Empty = 0,
  Reserved,
  Submitted,
  Acked,
  PartiallyFilled,
  CancelPending,
  Uncertain,
  Terminal,
};

struct Slot {
  SlotState state{SlotState::Empty};
  std::uint64_t generation{};
  std::uint64_t token{};
  std::int64_t filled{};
  bool seen_fill{};
  bool buy{true};
  std::uint64_t submitted_ns{};
};

struct ReconcileBarrier {
  bool maker_orders{};
  bool maker_positions{};
  bool hedge_orders{};
  bool hedge_positions{};
  bool fills_caught_up{};
  bool orders_determined{};
  bool delta_within_tolerance{};
  bool maker_oms_ready{};
  bool hedge_oms_ready{};
  bool maker_bbo_fresh{};
  bool hedge_bbo_fresh{};

  [[nodiscard]] bool complete() const noexcept {
    return maker_orders && maker_positions && hedge_orders && hedge_positions &&
           fills_caught_up && orders_determined && delta_within_tolerance &&
           maker_oms_ready && hedge_oms_ready && maker_bbo_fresh &&
           hedge_bbo_fresh;
  }
};

class Recovery {
 public:
  static constexpr int kTradeCapacity = 64;
  static constexpr int kMaxReconnects = 3;

  [[nodiscard]] bool reserve(SlotId slot, std::uint64_t generation,
                             bool buy = true,
                             std::uint64_t now_ns = 0) noexcept;
  [[nodiscard]] bool bind_submission(SlotId slot, std::uint64_t generation,
                                     std::uint64_t token) noexcept;
  void rollback_reservation(SlotId slot, std::uint64_t generation) noexcept;
  [[nodiscard]] bool submit(SlotId slot, std::uint64_t generation,
                            std::uint64_t token, bool buy = true,
                            std::uint64_t now_ns = 0) noexcept;
  void ack(SlotId slot, std::uint64_t generation) noexcept;
  [[nodiscard]] bool fill(SlotId slot, std::uint64_t trade_id,
                          std::int64_t quantity, std::uint64_t generation) noexcept;
  void terminal(SlotId slot, std::uint64_t generation) noexcept;
  void cancel_ack(SlotId slot, std::uint64_t generation) noexcept;
  void timeout(SlotId slot) noexcept;
  void mark_uncertain(SlotId slot) noexcept;
  void query_open(SlotId slot) noexcept;
  void query_filled(SlotId slot) noexcept;
  [[nodiscard]] bool query_absent(SlotId slot) noexcept;
  void maker_reject() noexcept;
  void begin_hedge() noexcept;
  void complete_hedge() noexcept;
  void hedge_reject() noexcept;
  void unwind_reject() noexcept;
  void halt() noexcept;
  void maker_venue_down() noexcept;
  void hedge_venue_down() noexcept;
  void note_reconnect(bool maker_leg) noexcept;
  void note_stable(bool maker_leg) noexcept;

  [[nodiscard]] const Slot& slot(SlotId id) const noexcept {
    return slots_[static_cast<std::size_t>(id)];
  }
  [[nodiscard]] bool quoting_allowed() const noexcept { return quoting_; }
  [[nodiscard]] bool halted() const noexcept { return halted_; }
  [[nodiscard]] bool makers_stopped() const noexcept { return makers_stopped_; }
  [[nodiscard]] int maker_rejects() const noexcept { return maker_rejects_; }
  [[nodiscard]] bool hedge_recovery() const noexcept { return hedge_recovery_; }
  [[nodiscard]] bool circuit_open() const noexcept { return circuit_open_; }
  [[nodiscard]] std::uint64_t maker_generation() const noexcept {
    return maker_generation_;
  }
  [[nodiscard]] std::uint64_t hedge_generation() const noexcept {
    return hedge_generation_;
  }
  [[nodiscard]] ReconcileBarrier& barrier() noexcept { return barrier_; }
  [[nodiscard]] const ReconcileBarrier& barrier() const noexcept {
    return barrier_;
  }
  [[nodiscard]] bool can_resume_quotes() const noexcept;
  [[nodiscard]] std::uint64_t reconnect_delay_ns(bool maker_leg,
                                                 std::uint64_t entropy) const noexcept;
  [[nodiscard]] const Slot* slot_for_token(std::uint64_t token) const noexcept;

 private:
  [[nodiscard]] Slot& at(SlotId id) noexcept {
    return slots_[static_cast<std::size_t>(id)];
  }
  [[nodiscard]] bool remember_trade(std::uint64_t trade_id) noexcept;

  std::array<Slot, 4> slots_{};
  std::array<std::uint64_t, kTradeCapacity> trades_{};
  int trade_count_{};
  int maker_rejects_{};
  int maker_reconnects_{};
  int hedge_reconnects_{};
  bool quoting_{true};
  bool makers_stopped_{};
  bool hedge_recovery_{};
  bool halted_{};
  bool circuit_open_{};
  bool maker_stable_{};
  bool hedge_stable_{};
  std::uint64_t maker_generation_{1};
  std::uint64_t hedge_generation_{1};
  ReconcileBarrier barrier_{};
};

}  // namespace tradefimm
