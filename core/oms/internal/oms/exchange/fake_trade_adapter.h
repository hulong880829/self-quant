#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

#include "oms/exchange/trade_adapter.h"

namespace oms::exchange {

// Deterministic transport used by tests and by the source-compatible Create
// path. It implements the same reservation, status and event-retention
// contract as venue adapters.
class FakeTradeAdapter final : public TradeAdapter {
 public:
  enum class Control : std::uint8_t { Event, Disconnect, Reconnect };

  FakeTradeAdapter(std::size_t send_capacity, std::size_t receive_capacity);

  [[nodiscard]] AdapterIdentity identity() const noexcept override;
  [[nodiscard]] AdapterStatus status() const noexcept override;
  [[nodiscard]] AdapterCapabilities capabilities() const noexcept override;
  [[nodiscard]] AdapterResult reserve_command(
      AdapterCommandKind kind, AdapterReservation& reservation) noexcept override;
  void cancel_reservation(AdapterReservation reservation) noexcept override;
  [[nodiscard]] AdapterResult commit_place(
      AdapterReservation reservation,
      const AdapterPlaceCommand& command) noexcept override;
  [[nodiscard]] AdapterResult commit_cancel(
      AdapterReservation reservation,
      const AdapterCancelCommand& command) noexcept override;
  [[nodiscard]] AdapterServiceResult service_io(
      std::uint64_t now_ns, std::uint32_t event_budget,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult on_deadline(
      const AdapterDeadline& deadline, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult begin_reconcile(
      std::uint64_t generation, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult shutdown(
      std::uint64_t now_ns, const AdapterEventSink& sink) noexcept override;

  [[nodiscard]] AdapterResult inject(Control control, const api::VenueEvent& event,
                                     std::uint64_t due_ns) noexcept;
  [[nodiscard]] std::uint64_t next_event_ns() const noexcept;
  [[nodiscard]] std::uint64_t last_event_ns() const noexcept;

 private:
  enum class SlotState : std::uint8_t { Free, Reserved, Queued };
  struct Slot {
    AdapterCommand command{};
    std::uint64_t generation{};
    SlotState state{SlotState::Free};
    AdapterCommandKind kind{AdapterCommandKind::Place};
  };
  struct Pending {
    std::uint64_t due_ns{};
    Control control{Control::Event};
    AdapterEvent event{};
  };

  [[nodiscard]] bool valid(AdapterReservation reservation,
                           AdapterCommandKind kind) const noexcept;
  void release(std::size_t slot) noexcept;
  [[nodiscard]] AdapterResult commit(AdapterReservation reservation,
                                     const AdapterCommand& command) noexcept;

  std::vector<Slot> send_;
  std::vector<Pending> receive_;
  std::size_t receive_begin_{};
  std::size_t receive_size_{};
  std::size_t next_send_{};
  std::uint64_t next_generation_{1};
  AdapterStatus status_{AdapterStatus::Ready};
};

}  // namespace oms::exchange
