#pragma once

#include <cstddef>
#include <cstdint>
#include <span>

#include "oms/exchange/adapter_types.h"

namespace oms::exchange {

struct AsyncIoDescriptor {
  int fd{-1};
  std::uint32_t events{};
  std::uint64_t generation{};
};

// Public, transport-agnostic extension point for asynchronous socket owners.
// Implementations are externally owned and all calls are serialized by the
// OmsApi owner. Times use the CLOCK_MONOTONIC nanosecond domain.
class AsyncIoDriver {
 public:
  virtual ~AsyncIoDriver() = default;

  // Upper bound used to preallocate owner-side descriptor storage.
  [[nodiscard]] virtual std::size_t descriptor_capacity() const noexcept = 0;
  [[nodiscard]] virtual std::size_t snapshot_descriptors(
      std::span<AsyncIoDescriptor> output) const noexcept = 0;
  virtual void service_io(int fd, std::uint64_t generation,
                          std::uint32_t events,
                          std::uint64_t now_ns) noexcept = 0;
  virtual void check_timeouts(std::uint64_t now_ns) noexcept = 0;
  [[nodiscard]] virtual std::uint64_t next_deadline_ns() const noexcept = 0;
};

// Public, transport-agnostic extension point. Implementations are externally
// owned and all calls are serialized by the OmsApi owner.
class TradeAdapter {
 public:
  virtual ~TradeAdapter() = default;

  [[nodiscard]] virtual AdapterIdentity identity() const noexcept = 0;
  [[nodiscard]] virtual AdapterStatus status() const noexcept = 0;
  [[nodiscard]] virtual AdapterCapabilities capabilities() const noexcept = 0;

  [[nodiscard]] virtual AdapterResult reserve_command(
      AdapterCommandKind kind, AdapterReservation& reservation) noexcept = 0;
  virtual void cancel_reservation(
      AdapterReservation reservation) noexcept = 0;
  [[nodiscard]] virtual AdapterResult commit_place(
      AdapterReservation reservation,
      const AdapterPlaceCommand& command) noexcept = 0;
  [[nodiscard]] virtual AdapterResult commit_cancel(
      AdapterReservation reservation,
      const AdapterCancelCommand& command) noexcept = 0;
  [[nodiscard]] virtual AdapterResult validate_rebind(
      const api::RebindPolymarketInstrumentRequest&) const noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult apply_rebind(
      const api::RebindPolymarketInstrumentRequest&) noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult query_open_orders(
      const AdapterQueryRequest&, const AdapterEventSink&) noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult query_positions(
      const AdapterQueryRequest&, const AdapterEventSink&) noexcept {
    return AdapterResult::Unsupported;
  }

  [[nodiscard]] virtual AdapterServiceResult service_io(
      std::uint64_t now_ns, std::uint32_t event_budget,
      const AdapterEventSink& sink) noexcept = 0;
  [[nodiscard]] virtual AdapterResult on_deadline(
      const AdapterDeadline& deadline, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept = 0;
  [[nodiscard]] virtual AdapterResult begin_reconcile(
      std::uint64_t generation, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept = 0;
  [[nodiscard]] virtual AdapterResult shutdown(
      std::uint64_t now_ns, const AdapterEventSink& sink) noexcept = 0;
};

}  // namespace oms::exchange
