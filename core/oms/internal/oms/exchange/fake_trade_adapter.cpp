#include "oms/exchange/fake_trade_adapter.h"

#include <limits>

namespace oms::exchange {

FakeTradeAdapter::FakeTradeAdapter(std::size_t send_capacity,
                                   std::size_t receive_capacity)
    : send_(send_capacity), receive_(receive_capacity) {
  if (send_.empty() || receive_.empty()) status_ = AdapterStatus::Failed;
}

AdapterIdentity FakeTradeAdapter::identity() const noexcept {
  return {AdapterKind::Fake, 0, utils::md::Venue::Unknown,
          utils::md::ProductType::Unknown, {}};
}

AdapterStatus FakeTradeAdapter::status() const noexcept { return status_; }

AdapterCapabilities FakeTradeAdapter::capabilities() const noexcept {
  AdapterCapabilities result{};
  result.bits = std::numeric_limits<std::uint64_t>::max();
  return result;
}

AdapterResult FakeTradeAdapter::reserve_command(
    AdapterCommandKind kind, AdapterReservation& reservation) noexcept {
  reservation = {};
  const bool cancel_disconnected =
      kind == AdapterCommandKind::Cancel &&
      status_ == AdapterStatus::Reconnecting;
  if (status_ != AdapterStatus::Ready &&
      status_ != AdapterStatus::Backpressured && !cancel_disconnected)
    return AdapterResult::NotReady;
  for (std::size_t offset = 0; offset < send_.size(); ++offset) {
    const std::size_t slot = (next_send_ + offset) % send_.size();
    if (send_[slot].state != SlotState::Free) continue;
    if (++next_generation_ == 0) ++next_generation_;
    send_[slot].generation = next_generation_;
    send_[slot].state = SlotState::Reserved;
    send_[slot].kind = kind;
    reservation = {AdapterKind::Fake, {}, static_cast<std::uint32_t>(slot),
                   next_generation_};
    next_send_ = (slot + 1) % send_.size();
    return AdapterResult::Ok;
  }
  status_ = AdapterStatus::Backpressured;
  return AdapterResult::WouldBlock;
}

bool FakeTradeAdapter::valid(AdapterReservation reservation,
                             AdapterCommandKind kind) const noexcept {
  if (reservation.adapter != AdapterKind::Fake ||
      reservation.slot >= send_.size() || reservation.generation == 0)
    return false;
  const Slot& slot = send_[reservation.slot];
  return slot.state == SlotState::Reserved &&
         slot.generation == reservation.generation && slot.kind == kind;
}

void FakeTradeAdapter::release(std::size_t slot) noexcept {
  const std::uint64_t generation = send_[slot].generation;
  send_[slot] = {};
  send_[slot].generation = generation;
  if (status_ == AdapterStatus::Backpressured)
    status_ = AdapterStatus::Ready;
}

void FakeTradeAdapter::cancel_reservation(
    AdapterReservation reservation) noexcept {
  if (reservation.slot >= send_.size()) return;
  if (send_[reservation.slot].state == SlotState::Reserved &&
      send_[reservation.slot].generation == reservation.generation)
    release(reservation.slot);
}

AdapterResult FakeTradeAdapter::commit(AdapterReservation reservation,
                                       const AdapterCommand& command) noexcept {
  if (!valid(reservation, command.kind))
    return AdapterResult::StaleReservation;
  Slot& slot = send_[reservation.slot];
  slot.command = command;
  slot.state = SlotState::Queued;
  return AdapterResult::Ok;
}

AdapterResult FakeTradeAdapter::commit_place(
    AdapterReservation reservation,
    const AdapterPlaceCommand& command) noexcept {
  AdapterCommand value{};
  value.kind = AdapterCommandKind::Place;
  value.place = command;
  return commit(reservation, value);
}

AdapterResult FakeTradeAdapter::commit_cancel(
    AdapterReservation reservation,
    const AdapterCancelCommand& command) noexcept {
  AdapterCommand value{};
  value.kind = AdapterCommandKind::Cancel;
  value.cancel = command;
  return commit(reservation, value);
}

AdapterServiceResult FakeTradeAdapter::service_io(
    std::uint64_t now_ns, std::uint32_t event_budget,
    const AdapterEventSink& sink) noexcept {
  AdapterServiceResult result{};
  if (status_ == AdapterStatus::Failed || status_ == AdapterStatus::Stopped) {
    result.result = AdapterResult::NotReady;
    return result;
  }
  // Fake sends complete at the transport boundary; venue outcomes are supplied
  // independently through inject(), exactly like asynchronous real transports.
  for (Slot& slot : send_) {
    if (slot.state == SlotState::Queued)
      release(static_cast<std::size_t>(&slot - send_.data()));
  }
  while (result.events_processed < event_budget && receive_size_ != 0) {
    Pending& pending = receive_[receive_begin_];
    if (pending.due_ns > now_ns) {
      result.next_deadline_ns = pending.due_ns;
      break;
    }
    if (pending.control == Control::Disconnect) {
      status_ = AdapterStatus::Reconnecting;
    } else if (pending.control == Control::Reconnect) {
      status_ = AdapterStatus::Ready;
    } else {
      if (status_ == AdapterStatus::Reconnecting) break;
      if (sink.on_event == nullptr) {
        result.result = AdapterResult::InvalidArgument;
        return result;
      }
      const AdapterResult offered = sink.on_event(sink.context, pending.event);
      if (offered != AdapterResult::Ok) {
        result.result = offered;
        return result;
      }
      ++result.events_processed;
    }
    receive_begin_ = (receive_begin_ + 1) % receive_.size();
    --receive_size_;
  }
  return result;
}

AdapterResult FakeTradeAdapter::on_deadline(
    const AdapterDeadline&, std::uint64_t, const AdapterEventSink&) noexcept {
  return AdapterResult::Unsupported;
}

AdapterResult FakeTradeAdapter::begin_reconcile(
    std::uint64_t generation, std::uint64_t, const AdapterEventSink& sink) noexcept {
  if (generation == 0 || sink.on_event == nullptr)
    return AdapterResult::InvalidArgument;
  AdapterEvent event{};
  event.kind = AdapterEventKind::ReconcileComplete;
  event.source = identity();
  event.reconcile.generation = generation;
  event.reconcile.result = AdapterResult::Ok;
  return sink.on_event(sink.context, event);
}

AdapterResult FakeTradeAdapter::shutdown(
    std::uint64_t now_ns, const AdapterEventSink& sink) noexcept {
  status_ = AdapterStatus::Stopped;
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  AdapterEvent event{};
  event.kind = AdapterEventKind::Status;
  event.source = identity();
  event.status = {identity(), status_, AdapterResult::Ok, {}, now_ns};
  return sink.on_event(sink.context, event);
}

AdapterResult FakeTradeAdapter::inject(Control control,
                                       const api::VenueEvent& venue,
                                       std::uint64_t due_ns) noexcept {
  if (receive_size_ == receive_.size()) return AdapterResult::WouldBlock;
  const std::size_t tail = (receive_begin_ + receive_size_) % receive_.size();
  receive_[tail] = {};
  receive_[tail].due_ns = due_ns;
  receive_[tail].control = control;
  receive_[tail].event.kind = AdapterEventKind::Venue;
  receive_[tail].event.source = identity();
  receive_[tail].event.venue = venue;
  ++receive_size_;
  return AdapterResult::Ok;
}

std::uint64_t FakeTradeAdapter::next_event_ns() const noexcept {
  return receive_size_ == 0 ? 0 : receive_[receive_begin_].due_ns;
}

std::uint64_t FakeTradeAdapter::last_event_ns() const noexcept {
  if (receive_size_ == 0) return 0;
  return receive_[(receive_begin_ + receive_size_ - 1) % receive_.size()]
      .due_ns;
}

}  // namespace oms::exchange
