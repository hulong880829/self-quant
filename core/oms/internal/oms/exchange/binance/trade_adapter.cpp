#include "oms/exchange/binance/trade_adapter.h"

#include <cstring>
#include <limits>

namespace oms::exchange::binance {
namespace {

template <typename Id>
std::string_view id_view(const Id& id) noexcept {
  return id.length <= id.value.size()
             ? std::string_view{id.value.data(), id.length}
             : std::string_view{};
}

AdapterEvent base_event(AdapterIdentity identity,
                        AdapterEventKind kind) noexcept {
  AdapterEvent event{};
  event.kind = kind;
  event.source = identity;
  return event;
}

}  // namespace

BinanceTradeAdapter::BinanceTradeAdapter(BinanceAdapterConfig config) noexcept
    : config_(config), builder_(config.product) {
  const bool valid =
      !config_.credentials.api_key.empty() &&
      !config_.credentials.secret_key.empty() &&
      config_.recv_window_ms != 0 && config_.recv_window_ms <= 60000 &&
      config_.request_timeout_ns != 0 && config_.transport != nullptr &&
      config_.price_scale <= 18 && config_.quantity_scale <= 18 &&
      config_.callbacks.resolve_symbol != nullptr &&
      config_.callbacks.resolve_cancel != nullptr;
  status_ = valid ? AdapterStatus::Authenticating : AdapterStatus::Failed;
  if (valid) {
    lifecycle_action_ = listen_key_.start();
    if (config_.transport->start_trading_stream() != AdapterResult::Ok) {
      status_ = AdapterStatus::Reconnecting;
      trading_start_required_ = true;
    }
  }
}

AdapterIdentity BinanceTradeAdapter::identity() const noexcept {
  AdapterIdentity result{};
  result.kind = config_.product == Product::Spot ? AdapterKind::BinanceSpot
                                                 : AdapterKind::BinanceUsdm;
  result.venue = utils::md::Venue::Binance;
  result.product_type = config_.product == Product::Spot
                            ? utils::md::ProductType::Spot
                            : utils::md::ProductType::Perpetual;
  return result;
}

AdapterCapabilities BinanceTradeAdapter::capabilities() const noexcept {
  return profile(config_.product).capabilities;
}

AdapterResult BinanceTradeAdapter::reserve_command(
    AdapterCommandKind kind, AdapterReservation& reservation) noexcept {
  reservation = {};
  if (kind == AdapterCommandKind::Place &&
      status_ == AdapterStatus::Backpressured)
    return AdapterResult::WouldBlock;
  if (status_ != AdapterStatus::Ready &&
      status_ != AdapterStatus::Backpressured)
    return AdapterResult::NotReady;
  for (std::size_t offset = 0; offset < send_.size(); ++offset) {
    const std::size_t index = (next_send_ + offset) % send_.size();
    SendSlot& slot = send_[index];
    if (slot.state != SlotState::Free) continue;
    if (++next_generation_ == 0) ++next_generation_;
    slot.generation = next_generation_;
    slot.state = SlotState::Reserved;
    slot.reserved_kind = kind;
    reservation.adapter = identity().kind;
    reservation.slot = static_cast<std::uint32_t>(index);
    reservation.generation = slot.generation;
    next_send_ = (index + 1) % send_.size();
    return AdapterResult::Ok;
  }
  status_ = AdapterStatus::Backpressured;
  return AdapterResult::WouldBlock;
}

bool BinanceTradeAdapter::valid_reservation(
    AdapterReservation reservation, AdapterCommandKind kind) const noexcept {
  if (reservation.adapter != identity().kind ||
      reservation.slot >= send_.size() || reservation.generation == 0)
    return false;
  const SendSlot& slot = send_[reservation.slot];
  return slot.state == SlotState::Reserved &&
         slot.generation == reservation.generation &&
         slot.reserved_kind == kind;
}

void BinanceTradeAdapter::cancel_reservation(
    AdapterReservation reservation) noexcept {
  if (reservation.adapter != identity().kind ||
      reservation.slot >= send_.size())
    return;
  SendSlot& slot = send_[reservation.slot];
  if (slot.state == SlotState::Reserved &&
      slot.generation == reservation.generation)
    release(reservation.slot);
}

AdapterResult BinanceTradeAdapter::commit_place(
    AdapterReservation reservation,
    const AdapterPlaceCommand& command) noexcept {
  if (!valid_reservation(reservation, AdapterCommandKind::Place))
    return AdapterResult::StaleReservation;
  SendSlot& slot = send_[reservation.slot];
  slot.command = {};
  slot.command.kind = AdapterCommandKind::Place;
  slot.command.place = command;
  slot.state = SlotState::Queued;
  return AdapterResult::Ok;
}

AdapterResult BinanceTradeAdapter::commit_cancel(
    AdapterReservation reservation,
    const AdapterCancelCommand& command) noexcept {
  if (!valid_reservation(reservation, AdapterCommandKind::Cancel))
    return AdapterResult::StaleReservation;
  SendSlot& slot = send_[reservation.slot];
  slot.command = {};
  slot.command.kind = AdapterCommandKind::Cancel;
  slot.command.cancel = command;
  slot.state = SlotState::Queued;
  return AdapterResult::Ok;
}

void BinanceTradeAdapter::release(std::size_t index) noexcept {
  const std::uint64_t generation = send_[index].generation;
  send_[index] = {};
  send_[index].generation = generation;
  refresh_backpressure();
}

void BinanceTradeAdapter::refresh_backpressure() noexcept {
  if (status_ != AdapterStatus::Backpressured ||
      receive_count_ == receive_.size() || rate_limit_until_ns_ != 0)
    return;
  for (const SendSlot& slot : send_) {
    if (slot.state == SlotState::Free) {
      status_ = user_session_ready_ && trading_session_ready_
                    ? AdapterStatus::Ready
                    : AdapterStatus::Authenticating;
      return;
    }
  }
}

AdapterResult BinanceTradeAdapter::queue_event(
    const AdapterEvent& event) noexcept {
  if (receive_count_ == receive_.size()) {
    status_ = AdapterStatus::Backpressured;
    return AdapterResult::WouldBlock;
  }
  const std::size_t tail = (receive_head_ + receive_count_) % receive_.size();
  receive_[tail] = event;
  ++receive_count_;
  return AdapterResult::Ok;
}

std::uint32_t BinanceTradeAdapter::next_request_id() noexcept {
  std::uint32_t result = next_transport_request_id_++;
  if (result == 0) result = next_transport_request_id_++;
  return result;
}

std::uint64_t BinanceTradeAdapter::request_deadline(
    std::uint64_t now_ns) const noexcept {
  return now_ns > std::numeric_limits<std::uint64_t>::max() -
                      config_.request_timeout_ns
             ? std::numeric_limits<std::uint64_t>::max()
             : now_ns + config_.request_timeout_ns;
}

BinanceTradeAdapter::SendSlot* BinanceTradeAdapter::find_request(
    std::uint32_t request_id) noexcept {
  for (SendSlot& slot : send_)
    if (slot.state == SlotState::InFlight &&
        slot.request.id == request_id)
      return &slot;
  return nullptr;
}

BinanceTradeAdapter::OrderBinding* BinanceTradeAdapter::find_binding(
    api::OrderHandle handle) noexcept {
  for (OrderBinding& binding : bindings_)
    if (binding.used && binding.handle == handle) return &binding;
  return nullptr;
}

BinanceTradeAdapter::OrderBinding* BinanceTradeAdapter::find_binding(
    const api::ClientOrderId& client_order_id,
    const api::VenueOrderId& venue_order_id) noexcept {
  for (OrderBinding& binding : bindings_) {
    if (!binding.used) continue;
    if ((client_order_id.length != 0 &&
         binding.client_order_id == client_order_id) ||
        (venue_order_id.length != 0 &&
         binding.venue_order_id == venue_order_id))
      return &binding;
  }
  return nullptr;
}

std::size_t BinanceTradeAdapter::next_uncertain_binding() const noexcept {
  for (std::size_t index = 0; index < bindings_.size(); ++index)
    if (bindings_[index].used && bindings_[index].uncertain) return index;
  return bindings_.size();
}

bool BinanceTradeAdapter::bind_place(
    api::OrderHandle handle, std::string_view symbol,
    const api::ClientOrderId& client_order_id,
    api::RequestToken token) noexcept {
  if (symbol.empty() || symbol.size() > kMaxSymbolBytes) return false;
  OrderBinding* binding = find_binding(handle);
  if (binding == nullptr) {
    for (OrderBinding& candidate : bindings_) {
      if (!candidate.used) {
        binding = &candidate;
        break;
      }
    }
  }
  if (binding == nullptr) return false;
  *binding = {};
  binding->used = true;
  binding->handle = handle;
  binding->token = token;
  std::copy(symbol.begin(), symbol.end(), binding->symbol.begin());
  binding->symbol_length = static_cast<std::uint8_t>(symbol.size());
  binding->client_order_id = client_order_id;
  return true;
}

bool BinanceTradeAdapter::resolve_stream_context(
    std::string_view json, ParseContext& context) noexcept {
  ParseContext probe{};
  probe.price_scale = config_.price_scale;
  probe.quantity_scale = config_.quantity_scale;
  api::VenueEvent decoded{};
  const ParseResult parsed =
      config_.product == Product::Spot
          ? parse_spot_execution_report(json, probe, decoded)
          : parse_usdm_order_trade_update(json, probe, decoded);
  if (parsed != ParseResult::Ok) return false;
  for (const OrderBinding& binding : bindings_) {
    if (!binding.used) continue;
    const bool client_matches =
        binding.client_order_id == decoded.client_order_id;
    const bool venue_matches =
        binding.venue_order_id.length != 0 &&
        binding.venue_order_id == decoded.venue_order_id;
    if (!client_matches && !venue_matches) continue;
    context = probe;
    context.handle = binding.handle;
    context.token = binding.token;
    return true;
  }
  return false;
}

void BinanceTradeAdapter::schedule_reconnect(std::uint64_t now_ns) noexcept {
  if (reconnect_attempts_ >= kMaximumReconnectAttempts) {
    reconnect_deadline_ns_ = 0;
    status_ = AdapterStatus::Failed;
    return;
  }
  std::uint64_t delay = kReconnectInitialBackoffNs;
  for (std::uint8_t attempt = 0; attempt < reconnect_attempts_; ++attempt) {
    if (delay >= kReconnectMaximumBackoffNs / 2U) {
      delay = kReconnectMaximumBackoffNs;
      break;
    }
    delay *= 2U;
  }
  if (delay > kReconnectMaximumBackoffNs)
    delay = kReconnectMaximumBackoffNs;
  ++reconnect_attempts_;
  reconnect_deadline_ns_ =
      now_ns > std::numeric_limits<std::uint64_t>::max() - delay
          ? std::numeric_limits<std::uint64_t>::max()
          : now_ns + delay;
  status_ = AdapterStatus::Reconnecting;
}

void BinanceTradeAdapter::reset_reconnect() noexcept {
  reconnect_attempts_ = 0;
  reconnect_deadline_ns_ = 0;
}

void BinanceTradeAdapter::start_reconcile_generation() noexcept {
  if (reconcile_generation_ != 0) return;
  if (next_reconnect_generation_ == 0) --next_reconnect_generation_;
  reconcile_generation_ = next_reconnect_generation_--;
  reconcile_cursor_ = {};
  reconcile_response_size_ = 0;
  reconcile_snapshot_complete_ = false;
  for (OrderBinding& binding : bindings_)
    if (binding.used) binding.uncertain = true;
  stream_reconcile_required_ = false;
  status_ = AdapterStatus::Reconciling;
}

AdapterResult BinanceTradeAdapter::process_one(std::uint64_t now_ns) noexcept {
  std::size_t index = send_.size();
  if (status_ == AdapterStatus::Reconnecting) {
    for (std::size_t offset = 0; offset < send_.size(); ++offset) {
      const std::size_t candidate = (next_send_ + offset) % send_.size();
      if (send_[candidate].state == SlotState::Queued &&
          send_[candidate].command.kind == AdapterCommandKind::Cancel) {
        index = candidate;
        break;
      }
    }
  }
  for (std::size_t offset = 0; offset < send_.size(); ++offset) {
    if (index != send_.size()) break;
    const std::size_t candidate = (next_send_ + offset) % send_.size();
    if (send_[candidate].state == SlotState::Queued) {
      index = candidate;
      break;
    }
  }
  if (index == send_.size()) return AdapterResult::Ok;

  SendSlot& slot = send_[index];
  const AdapterCommand& command = slot.command;
  if (!slot.request_built) {
    std::array<char, kMaxSymbolBytes> symbol{};
    std::uint8_t symbol_length = 0;
    bool built = false;
    slot.request = {};
    slot.request.id = next_request_id();
    slot.parse_context = {};
    if (command.kind == AdapterCommandKind::Place) {
      slot.parse_context.handle = command.place.handle;
      slot.parse_context.token = command.place.request.token;
      built = config_.callbacks.resolve_symbol(
          config_.callbacks.context, command.place.request.instrument_id,
          symbol.data(), symbol.size(), symbol_length);
      const std::string_view client_id =
          id_view(command.place.request.client_order_id);
      if (built && symbol_length != 0 && symbol_length <= symbol.size() &&
          !client_id.empty()) {
        const PlaceParameters parameters{
            {symbol.data(), symbol_length},
            command.place.request.side,
            command.place.request.type,
            command.place.request.time_in_force,
            command.place.request.flags,
            command.place.request.quantity,
            command.place.request.price,
            client_id,
            command.place.request.expire_time_ns};
        built = builder_.trading_place(
            parameters, slot.request.id,
            clock_.venue_time_ms(now_ns / 1000000U),
            config_.recv_window_ms, config_.credentials,
            slot.request.trading);
        slot.request.use_trading_websocket = true;
        if (built) {
          built = bind_place(command.place.handle,
                             {symbol.data(), symbol_length},
                             command.place.request.client_order_id,
                             command.place.request.token);
        }
      } else {
        built = false;
      }
    } else {
      slot.parse_context.handle = command.cancel.request.handle;
      slot.parse_context.token = command.cancel.request.target_token;
      ResolvedCancel resolved{};
      if (OrderBinding* binding =
              find_binding(command.cancel.request.handle);
          binding != nullptr) {
        resolved.symbol = binding->symbol;
        resolved.symbol_length = binding->symbol_length;
        resolved.client_order_id = binding->client_order_id;
        resolved.venue_order_id = binding->venue_order_id;
        built = true;
      } else {
        built = config_.callbacks.resolve_cancel(
            config_.callbacks.context, command.cancel.request.handle, resolved);
      }
      if (built && resolved.symbol_length != 0 &&
          resolved.symbol_length <= resolved.symbol.size()) {
        const CancelParameters parameters{
            {resolved.symbol.data(), resolved.symbol_length},
            id_view(resolved.client_order_id), id_view(resolved.venue_order_id)};
        built = builder_.trading_cancel(
            parameters, slot.request.id,
            clock_.venue_time_ms(now_ns / 1000000U),
            config_.recv_window_ms, config_.credentials,
            slot.request.trading);
        slot.request.use_trading_websocket = true;
      } else {
        built = false;
      }
    }
    slot.parse_context.price_scale = config_.price_scale;
    slot.parse_context.quantity_scale = config_.quantity_scale;
    if (!built) return fail_command(index, AdapterResult::InvalidArgument);
    slot.request_built = true;
  }

  const AdapterResult submitted = config_.transport->submit(slot.request);
  if (submitted == AdapterResult::WouldBlock) return submitted;
  if (submitted != AdapterResult::Ok)
    return fail_command(index, submitted, true);
  slot.state = SlotState::InFlight;
  slot.deadline_ns = request_deadline(now_ns);
  return AdapterResult::Ok;
}

AdapterResult BinanceTradeAdapter::fail_command(
    std::size_t index, AdapterResult reason, bool uncertain_place) noexcept {
  const AdapterCommand command = send_[index].command;
  AdapterEvent event = base_event(identity(), AdapterEventKind::CommandResult);
  event.command_result.kind = command.kind;
  event.command_result.command_id =
      command.kind == AdapterCommandKind::Place ? command.place.command_id
                                                : command.cancel.command_id;
  event.command_result.request_token =
      command.kind == AdapterCommandKind::Place
          ? command.place.request.token
          : command.cancel.request.request_token;
  event.command_result.result = reason;
  const AdapterResult queued = queue_event(event);
  if (uncertain_place && command.kind == AdapterCommandKind::Place) {
    if (OrderBinding* binding = find_binding(command.place.handle);
        binding != nullptr)
      binding->uncertain = true;
    stream_reconcile_required_ = true;
    status_ = AdapterStatus::Reconciling;
  }
  release(index);
  return queued == AdapterResult::Ok ? reason : queued;
}

AdapterResult BinanceTradeAdapter::finish_reconcile(
    AdapterResult reconcile_result, const AdapterEventSink& sink,
    std::uint32_t& events_processed) noexcept {
  AdapterEvent complete =
      base_event(identity(), AdapterEventKind::ReconcileComplete);
  complete.reconcile.generation = reconcile_generation_;
  complete.reconcile.result = reconcile_result;
  reconcile_generation_ = 0;
  reconcile_cursor_ = {};
  reconcile_response_size_ = 0;
  reconcile_snapshot_complete_ = false;
  status_ = reconcile_result == AdapterResult::Ok
                ? (user_session_ready_ && trading_session_ready_
                       ? AdapterStatus::Ready
                       : AdapterStatus::Authenticating)
                : AdapterStatus::Failed;
  if (reconcile_result == AdapterResult::Ok) reset_reconnect();
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  const AdapterResult offered = sink.on_event(sink.context, complete);
  if (offered == AdapterResult::Ok) {
    ++events_processed;
    return reconcile_result;
  }
  if (offered == AdapterResult::WouldBlock) {
    const AdapterResult queued = queue_event(complete);
    return queued == AdapterResult::Ok ? AdapterResult::WouldBlock : queued;
  }
  return offered;
}

AdapterResult BinanceTradeAdapter::continue_reconcile(
    const AdapterEventSink& sink, std::uint32_t event_budget,
    std::uint32_t& events_processed) noexcept {
  if (reconcile_generation_ == 0) return AdapterResult::Ok;
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  const std::string_view snapshot{response_buffer_.data(),
                                  reconcile_response_size_};
  while (events_processed < event_budget) {
    std::array<api::VenueEvent, 1> decoded{};
    std::size_t count = 0;
    const ParseResult parsed = parse_open_orders_page(
        snapshot, reconcile_cursor_, decoded.data(), decoded.size(), count);
    if (parsed != ParseResult::Ok)
      return finish_reconcile(AdapterResult::Failed, sink, events_processed);
    if (count == 0 && reconcile_cursor_.complete) {
      reconcile_response_size_ = 0;
      reconcile_snapshot_complete_ = true;
      return AdapterResult::Ok;
    }
    if (count == 0) continue;
    if (OrderBinding* binding =
            find_binding(decoded[0].client_order_id,
                         decoded[0].venue_order_id);
        binding != nullptr) {
      decoded[0].handle = binding->handle;
      decoded[0].token = binding->token;
      if (decoded[0].venue_order_id.length != 0)
        binding->venue_order_id = decoded[0].venue_order_id;
      binding->uncertain = false;
    }
    AdapterEvent event = base_event(identity(), AdapterEventKind::Venue);
    event.venue = decoded[0];
    const AdapterResult offered = sink.on_event(sink.context, event);
    if (offered == AdapterResult::Ok) {
      ++events_processed;
      continue;
    }
    if (offered == AdapterResult::WouldBlock) {
      const AdapterResult queued = queue_event(event);
      return queued == AdapterResult::Ok ? AdapterResult::WouldBlock : queued;
    }
    return offered;
  }
  return AdapterResult::WouldBlock;
}

AdapterResult BinanceTradeAdapter::submit_listen_key(
    ListenKeyAction action, std::uint64_t now_ns) noexcept {
  if (action == ListenKeyAction::None ||
      action == ListenKeyAction::ReconnectStream)
    return action == ListenKeyAction::None ? AdapterResult::Ok
                                           : AdapterResult::NotReady;
  if (control_.kind == ControlKind::None) {
    control_ = {};
    control_.kind = ControlKind::ListenKey;
    control_.listen_key_action = action;
    control_.request.id = next_request_id();
    bool built = false;
    if (action == ListenKeyAction::Create)
      built =
          builder_.create_listen_key(config_.credentials, control_.request.wire);
    else if (action == ListenKeyAction::Keepalive)
      built = builder_.keepalive_listen_key(
          config_.credentials, listen_key_.key(), control_.request.wire);
    else if (action == ListenKeyAction::Close)
      built = builder_.close_listen_key(
          config_.credentials, listen_key_.key(), control_.request.wire);
    if (!built) {
      control_ = {};
      return AdapterResult::InvalidArgument;
    }
  } else if (control_.kind != ControlKind::ListenKey ||
             control_.listen_key_action != action) {
    return AdapterResult::WouldBlock;
  }
  const AdapterResult submitted = config_.transport->submit(control_.request);
  if (submitted == AdapterResult::WouldBlock) return submitted;
  if (submitted != AdapterResult::Ok)
    return fail_control(submitted, now_ns);
  control_.deadline_ns = request_deadline(now_ns);
  lifecycle_action_ = ListenKeyAction::None;
  return AdapterResult::Ok;
}

AdapterResult BinanceTradeAdapter::submit_reconcile(
    std::uint64_t generation, std::uint64_t now_ns) noexcept {
  if (control_.kind != ControlKind::None) return AdapterResult::WouldBlock;
  control_ = {};
  control_.kind = ControlKind::Reconcile;
  control_.reconcile_generation = generation;
  control_.request.id = next_request_id();
  if (!builder_.open_orders({}, clock_.venue_time_ms(now_ns / 1000000U),
                            config_.recv_window_ms, config_.credentials,
                            control_.request.wire)) {
    control_ = {};
    return AdapterResult::InvalidArgument;
  }
  const AdapterResult submitted = config_.transport->submit(control_.request);
  if (submitted == AdapterResult::WouldBlock) {
    control_ = {};
    return submitted;
  }
  if (submitted != AdapterResult::Ok) {
    control_ = {};
    return submitted;
  }
  control_.deadline_ns = request_deadline(now_ns);
  return AdapterResult::Ok;
}

AdapterResult BinanceTradeAdapter::submit_time_sync(
    std::uint64_t now_ns) noexcept {
  if (control_.kind != ControlKind::None) return AdapterResult::WouldBlock;
  control_ = {};
  control_.kind = ControlKind::TimeSync;
  control_.request.id = next_request_id();
  control_.local_send_ms = now_ns / 1'000'000ULL;
  if (!builder_.server_time(control_.request.wire)) {
    control_ = {};
    return AdapterResult::InvalidArgument;
  }
  const AdapterResult submitted = config_.transport->submit(control_.request);
  if (submitted == AdapterResult::WouldBlock) {
    control_ = {};
    return submitted;
  }
  if (submitted != AdapterResult::Ok) {
    control_ = {};
    return submitted;
  }
  control_.deadline_ns = request_deadline(now_ns);
  return AdapterResult::Ok;
}

AdapterResult BinanceTradeAdapter::submit_query_order(
    std::size_t binding_index, std::uint64_t now_ns) noexcept {
  if (control_.kind != ControlKind::None) return AdapterResult::WouldBlock;
  if (binding_index >= bindings_.size() ||
      !bindings_[binding_index].used ||
      !bindings_[binding_index].uncertain)
    return AdapterResult::InvalidArgument;
  const OrderBinding& binding = bindings_[binding_index];
  control_ = {};
  control_.kind = ControlKind::QueryOrder;
  control_.binding_index = static_cast<std::uint32_t>(binding_index);
  control_.request.id = next_request_id();
  const CancelParameters parameters{
      {binding.symbol.data(), binding.symbol_length},
      id_view(binding.client_order_id), id_view(binding.venue_order_id)};
  if (!builder_.query_order(
          parameters, clock_.venue_time_ms(now_ns / 1'000'000ULL),
          config_.recv_window_ms, config_.credentials,
          control_.request.wire)) {
    control_ = {};
    return AdapterResult::InvalidArgument;
  }
  const AdapterResult submitted = config_.transport->submit(control_.request);
  if (submitted == AdapterResult::WouldBlock) {
    control_ = {};
    return submitted;
  }
  if (submitted != AdapterResult::Ok) {
    control_ = {};
    return submitted;
  }
  control_.deadline_ns = request_deadline(now_ns);
  return AdapterResult::Ok;
}

AdapterResult BinanceTradeAdapter::fail_control(
    AdapterResult reason, std::uint64_t now_ns) noexcept {
  const ControlSlot failed = control_;
  control_ = {};
  if (failed.kind == ControlKind::ListenKey) {
    listen_key_.request_failed();
    if (listen_key_.state() == ListenKeyState::RecreateRequired)
      schedule_reconnect(now_ns);
    else
      status_ = AdapterStatus::Failed;
    return reason;
  }
  if (failed.kind == ControlKind::Reconcile ||
      failed.kind == ControlKind::QueryOrder) {
    AdapterEvent complete =
        base_event(identity(), AdapterEventKind::ReconcileComplete);
    complete.reconcile.generation = reconcile_generation_;
    complete.reconcile.result = reason;
    reconcile_generation_ = 0;
    reconcile_cursor_ = {};
    reconcile_response_size_ = 0;
    reconcile_snapshot_complete_ = false;
    status_ = AdapterStatus::Failed;
    const AdapterResult queued = queue_event(complete);
    return queued == AdapterResult::Ok ? reason : queued;
  }
  if (failed.kind == ControlKind::TimeSync) {
    time_sync_required_ = true;
    status_ = AdapterStatus::Authenticating;
    return reason;
  }
  return AdapterResult::StaleReservation;
}

AdapterResult BinanceTradeAdapter::process_transport_event(
    const TransportEvent& event, std::uint64_t now_ns) noexcept {
  if (event.kind == TransportEventKind::SessionLost) {
    user_session_ready_ = false;
    stream_reconcile_required_ = true;
    schedule_reconnect(now_ns);
    return AdapterResult::NotReady;
  }
  if (event.kind == TransportEventKind::SessionReady) {
    user_session_ready_ = true;
    reset_reconnect();
    if (stream_reconcile_required_) {
      start_reconcile_generation();
    } else if (trading_session_ready_) {
      status_ = AdapterStatus::Ready;
    } else {
      status_ = AdapterStatus::Authenticating;
    }
    return AdapterResult::Ok;
  }
  if (event.kind == TransportEventKind::TradingSessionLost) {
    trading_session_ready_ = false;
    trading_start_required_ = true;
    stream_reconcile_required_ = true;
    for (std::size_t index = 0; index < send_.size(); ++index) {
      if (send_[index].state == SlotState::InFlight)
        (void)fail_command(index, AdapterResult::NotReady, true);
    }
    schedule_reconnect(now_ns);
    return AdapterResult::NotReady;
  }
  if (event.kind == TransportEventKind::TradingSessionReady) {
    trading_session_ready_ = true;
    trading_start_required_ = false;
    reset_reconnect();
    if (user_session_ready_ && stream_reconcile_required_ &&
        reconcile_generation_ == 0) {
      start_reconcile_generation();
    } else if (user_session_ready_ && reconcile_generation_ == 0 &&
               !time_sync_required_) {
      status_ = AdapterStatus::Ready;
    } else {
      status_ = AdapterStatus::Authenticating;
    }
    return AdapterResult::Ok;
  }
  if (event.kind == TransportEventKind::UserMessage) {
    ParseContext context{};
    context.price_scale = config_.price_scale;
    context.quantity_scale = config_.quantity_scale;
    bool resolved = resolve_stream_context(event.payload, context);
    if (!resolved && config_.callbacks.resolve_stream != nullptr) {
      resolved = config_.callbacks.resolve_stream(
          config_.callbacks.context, event.payload, context);
    }
    if (!resolved) {
      return AdapterResult::InvalidArgument;
    }
    return ingest_user_stream(event.payload, context);
  }
  if (control_.kind != ControlKind::None &&
      event.request_id == control_.request.id) {
    if (event.kind == TransportEventKind::Failure)
      return fail_control(event.result, now_ns);
    const ControlSlot completed = control_;
    if (event.metadata.http_status >= 400 &&
        completed.kind == ControlKind::QueryOrder) {
      VenueError error{};
      const ParseResult parsed = parse_error(
          event.payload, event.metadata.http_status, event.metadata, error);
      if (parsed != ParseResult::Ok || error.code != -2013)
        return fail_control(AdapterResult::Failed, now_ns);
      control_ = {};
      if (completed.binding_index >= bindings_.size() ||
          !bindings_[completed.binding_index].used)
        return AdapterResult::InvalidArgument;
      const OrderBinding binding = bindings_[completed.binding_index];
      AdapterEvent missing =
          base_event(identity(), AdapterEventKind::Venue);
      missing.venue.type = api::VenueEventType::NewReject;
      missing.venue.handle = binding.handle;
      missing.venue.token = binding.token;
      missing.venue.client_order_id = binding.client_order_id;
      missing.venue.venue_order_id = binding.venue_order_id;
      missing.venue.reconciled_status = api::OrderStatus::Rejected;
      const AdapterResult queued = queue_event(missing);
      if (queued == AdapterResult::Ok)
        bindings_[completed.binding_index] = {};
      return queued;
    }
    if (event.metadata.http_status >= 400)
      return fail_control(AdapterResult::Failed, now_ns);
    control_ = {};
    if (completed.kind == ControlKind::ListenKey) {
      const bool reconnecting = status_ == AdapterStatus::Reconnecting;
      if (completed.listen_key_action == ListenKeyAction::Create) {
        std::array<char, kMaxListenKeyBytes> key{};
        std::uint16_t length = 0;
        if (parse_listen_key(event.payload, key, length) != ParseResult::Ok ||
            !listen_key_.activated({key.data(), length}, now_ns)) {
          status_ = AdapterStatus::Failed;
          return AdapterResult::Failed;
        }
        const AdapterResult stream =
            config_.transport->start_user_stream(listen_key_.key());
        if (stream != AdapterResult::Ok) {
          schedule_reconnect(now_ns);
          return stream;
        }
      } else if (completed.listen_key_action == ListenKeyAction::Keepalive) {
        listen_key_.keepalive_succeeded(now_ns);
      }
      if (completed.listen_key_action == ListenKeyAction::Create) {
        stream_reconcile_required_ = reconnecting;
        status_ = reconnecting ? AdapterStatus::Reconnecting
                               : AdapterStatus::Authenticating;
      } else if (completed.listen_key_action != ListenKeyAction::Close) {
        reset_reconnect();
      }
      return AdapterResult::Ok;
    }
    if (completed.kind == ControlKind::TimeSync) {
      std::uint64_t server_ms = 0;
      if (parse_server_time(event.payload, server_ms) != ParseResult::Ok) {
        time_sync_required_ = true;
        status_ = AdapterStatus::Failed;
        return AdapterResult::Failed;
      }
      clock_.observe(completed.local_send_ms, now_ns / 1'000'000ULL,
                     server_ms);
      time_sync_required_ = false;
      if (user_session_ready_ && trading_session_ready_ &&
          reconcile_generation_ == 0)
        status_ = AdapterStatus::Ready;
      return AdapterResult::Ok;
    }
    if (completed.kind == ControlKind::Reconcile) {
      if (event.payload.size() > response_buffer_.size()) {
        control_ = completed;
        return fail_control(AdapterResult::Failed, now_ns);
      }
      std::memcpy(response_buffer_.data(), event.payload.data(),
                  event.payload.size());
      reconcile_response_size_ =
          static_cast<std::uint32_t>(event.payload.size());
      return AdapterResult::Ok;
    }
    if (completed.kind == ControlKind::QueryOrder) {
      if (completed.binding_index >= bindings_.size() ||
          !bindings_[completed.binding_index].used)
        return AdapterResult::InvalidArgument;
      OrderBinding& binding = bindings_[completed.binding_index];
      api::VenueEvent venue{};
      if (parse_order_query(event.payload, venue) != ParseResult::Ok) {
        control_ = completed;
        return fail_control(AdapterResult::Failed, now_ns);
      }
      venue.handle = binding.handle;
      venue.token = binding.token;
      if (venue.client_order_id.length == 0)
        venue.client_order_id = binding.client_order_id;
      if (venue.venue_order_id.length == 0)
        venue.venue_order_id = binding.venue_order_id;
      AdapterEvent reconciled =
          base_event(identity(), AdapterEventKind::Venue);
      reconciled.venue = venue;
      const AdapterResult queued = queue_event(reconciled);
      if (queued != AdapterResult::Ok) return queued;
      binding.uncertain = false;
      binding.venue_order_id = venue.venue_order_id;
      if (venue.reconciled_status == api::OrderStatus::Filled ||
          venue.reconciled_status == api::OrderStatus::Canceled ||
          venue.reconciled_status == api::OrderStatus::Rejected ||
          venue.reconciled_status == api::OrderStatus::Expired)
        binding = {};
      return AdapterResult::Ok;
    }
    return AdapterResult::Ok;
  }

  SendSlot* slot = find_request(event.request_id);
  if (slot == nullptr) return AdapterResult::Ok;
  const std::size_t index =
      static_cast<std::size_t>(slot - send_.data());
  if (event.kind == TransportEventKind::Failure)
    return fail_command(index, event.result, true);

  AdapterEvent command_event =
      base_event(identity(), AdapterEventKind::CommandResult);
  command_event.command_result.kind = slot->command.kind;
  command_event.command_result.command_id =
      slot->command.kind == AdapterCommandKind::Place
          ? slot->command.place.command_id
          : slot->command.cancel.command_id;
  command_event.command_result.request_token =
      slot->command.kind == AdapterCommandKind::Place
          ? slot->command.place.request.token
          : slot->command.cancel.request.request_token;
  if (event.metadata.http_status >= 400) {
    VenueError error{};
    const ParseResult parsed = parse_error(
        event.payload, event.metadata.http_status, event.metadata, error);
    command_event.command_result.result = AdapterResult::Failed;
    command_event.command_result.venue_code =
        parsed == ParseResult::Ok ? error.code : 0;
    (void)queue_event(command_event);
    if (event.metadata.http_status == 429 ||
        event.metadata.http_status == 418) {
      std::uint64_t delay_ms =
          event.metadata.has_retry_after ? event.metadata.retry_after_ms
                                         : 1000ULL;
      delay_ms = std::min<std::uint64_t>(delay_ms, 60'000ULL);
      const std::uint64_t delay_ns = delay_ms * 1'000'000ULL;
      rate_limit_until_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() - delay_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + delay_ns;
      status_ = AdapterStatus::Backpressured;
    }
    if (parsed == ParseResult::Ok &&
        (error.classification == ErrorClass::TimestampSkew ||
         (slot->command.kind == AdapterCommandKind::Place &&
          event.metadata.http_status >= 500) ||
         (slot->command.kind == AdapterCommandKind::Place &&
          retry_disposition(slot->command.kind, error.classification) ==
              RetryDisposition::ReconcileBeforeRetry))) {
      stream_reconcile_required_ = true;
      status_ = AdapterStatus::Reconciling;
    }
    if (parsed == ParseResult::Ok &&
        error.classification == ErrorClass::TimestampSkew)
      time_sync_required_ = true;
    release(index);
    return AdapterResult::Failed;
  }

  AdapterEvent venue_event = base_event(identity(), AdapterEventKind::Venue);
  const ParseResult parsed =
      slot->command.kind == AdapterCommandKind::Place
          ? parse_rest_place(event.payload, slot->parse_context,
                             venue_event.venue)
          : parse_rest_cancel(event.payload, slot->parse_context,
                              venue_event.venue);
  if (parsed != ParseResult::Ok)
    return fail_command(index, AdapterResult::Failed);
  if (slot->command.kind == AdapterCommandKind::Place) {
    if (OrderBinding* binding =
            find_binding(slot->command.place.handle);
        binding != nullptr) {
      binding->venue_order_id = venue_event.venue.venue_order_id;
    }
  }
  command_event.command_result.result = AdapterResult::Ok;
  (void)queue_event(command_event);
  (void)queue_event(venue_event);
  if (slot->command.kind == AdapterCommandKind::Cancel) {
    if (OrderBinding* binding =
            find_binding(slot->command.cancel.request.handle);
        binding != nullptr)
      *binding = {};
  }
  release(index);
  return AdapterResult::Ok;
}

AdapterServiceResult BinanceTradeAdapter::service_io(
    std::uint64_t now_ns, std::uint32_t event_budget,
    const AdapterEventSink& sink) noexcept {
  AdapterServiceResult result{};
  if (trading_start_required_ && reconnect_deadline_ns_ == 0) {
    const AdapterResult started = config_.transport->start_trading_stream();
    if (started == AdapterResult::Ok) {
      trading_start_required_ = false;
      status_ = AdapterStatus::Authenticating;
    } else {
      schedule_reconnect(now_ns);
      result.result = AdapterResult::NotReady;
      result.next_deadline_ns = reconnect_deadline_ns_;
      return result;
    }
  }
  if (rate_limit_until_ns_ != 0 && rate_limit_until_ns_ <= now_ns) {
    rate_limit_until_ns_ = 0;
    if (status_ == AdapterStatus::Backpressured)
      status_ = user_session_ready_ && trading_session_ready_
                    ? AdapterStatus::Ready
                    : AdapterStatus::Authenticating;
  }
  if (rate_limit_until_ns_ != 0)
    result.next_deadline_ns = rate_limit_until_ns_;
  if (reconnect_deadline_ns_ != 0 &&
      (result.next_deadline_ns == 0 ||
       reconnect_deadline_ns_ < result.next_deadline_ns))
    result.next_deadline_ns = reconnect_deadline_ns_;
  if (control_.kind != ControlKind::None && control_.deadline_ns != 0 &&
      control_.deadline_ns <= now_ns) {
    result.result = fail_control(AdapterResult::Failed, now_ns);
  }
  for (std::size_t index = 0; index < send_.size(); ++index) {
    if (send_[index].state != SlotState::InFlight ||
        send_[index].deadline_ns == 0 ||
        send_[index].deadline_ns > now_ns) {
      continue;
    }
    const bool uncertain =
        send_[index].command.kind == AdapterCommandKind::Place;
    result.result =
        fail_command(index, AdapterResult::Failed, uncertain);
    break;
  }
  if ((status_ == AdapterStatus::Failed || status_ == AdapterStatus::Stopped) &&
      receive_count_ == 0) {
    result.result = AdapterResult::NotReady;
    return result;
  }
  const std::uint64_t listen_key_deadline = listen_key_.next_deadline_ns();
  if (listen_key_deadline != 0 &&
      (result.next_deadline_ns == 0 ||
       listen_key_deadline < result.next_deadline_ns))
    result.next_deadline_ns = listen_key_deadline;
  for (const SendSlot& slot : send_) {
    if (slot.state != SlotState::InFlight) continue;
    if (result.next_deadline_ns == 0 ||
        slot.deadline_ns < result.next_deadline_ns)
      result.next_deadline_ns = slot.deadline_ns;
  }
  if (control_.kind != ControlKind::None &&
      (result.next_deadline_ns == 0 ||
       control_.deadline_ns < result.next_deadline_ns))
    result.next_deadline_ns = control_.deadline_ns;

  while (result.events_processed < event_budget) {
    if (receive_count_ != 0) {
      if (sink.on_event == nullptr) {
        result.result = AdapterResult::InvalidArgument;
        return result;
      }
      const AdapterResult offered =
          sink.on_event(sink.context, receive_[receive_head_]);
      if (offered == AdapterResult::WouldBlock) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      if (offered != AdapterResult::Ok) {
        result.result = offered;
        return result;
      }
      receive_head_ = (receive_head_ + 1) % receive_.size();
      --receive_count_;
      refresh_backpressure();
      ++result.events_processed;
      continue;
    }
    if (status_ == AdapterStatus::Failed ||
        status_ == AdapterStatus::Stopped) {
      result.result = AdapterResult::NotReady;
      return result;
    }
    if (time_sync_required_ && control_.kind == ControlKind::None) {
      const AdapterResult synchronized = submit_time_sync(now_ns);
      if (synchronized != AdapterResult::Ok) {
        result.result = synchronized;
        return result;
      }
    }
    if (stream_reconcile_required_ && user_session_ready_ &&
        trading_session_ready_ && !time_sync_required_ &&
        control_.kind == ControlKind::None &&
        reconcile_generation_ == 0)
      start_reconcile_generation();
    if (reconcile_generation_ != 0) {
      if (reconcile_snapshot_complete_) {
        if (control_.kind == ControlKind::None) {
          const std::size_t uncertain = next_uncertain_binding();
          if (uncertain != bindings_.size()) {
            const AdapterResult submitted =
                submit_query_order(uncertain, now_ns);
            if (submitted != AdapterResult::Ok) {
              result.result = submitted;
              return result;
            }
          } else {
            std::uint32_t completed_events = 0;
            const AdapterResult completed = finish_reconcile(
                AdapterResult::Ok, sink, completed_events);
            result.events_processed += completed_events;
            if (completed != AdapterResult::Ok) result.result = completed;
            return result;
          }
        }
      } else if (reconcile_response_size_ == 0) {
        if (control_.kind == ControlKind::None) {
          const AdapterResult submitted =
              submit_reconcile(reconcile_generation_, now_ns);
          if (submitted != AdapterResult::Ok) {
            result.result = submitted;
            return result;
          }
        }
      } else {
        std::uint32_t reconciled = 0;
        const AdapterResult continued = continue_reconcile(
            sink, event_budget - result.events_processed, reconciled);
        result.events_processed += reconciled;
        if (continued != AdapterResult::Ok) result.result = continued;
        if (continued == AdapterResult::WouldBlock ||
            continued == AdapterResult::InvalidArgument ||
            continued == AdapterResult::Failed)
          return result;
        continue;
      }
    }
    if (lifecycle_action_ != ListenKeyAction::None &&
        control_.kind == ControlKind::None) {
      const AdapterResult lifecycle =
          submit_listen_key(lifecycle_action_, now_ns);
      if (lifecycle != AdapterResult::Ok) {
        result.result = lifecycle;
        return result;
      }
    }
    if (control_.kind == ControlKind::None &&
        reconcile_generation_ == 0) {
      const AdapterResult submitted = process_one(now_ns);
      if (submitted == AdapterResult::WouldBlock) {
        result.result = submitted;
        return result;
      }
      if (submitted != AdapterResult::Ok) result.result = submitted;
    }
    for (const SendSlot& slot : send_) {
      if (slot.state == SlotState::InFlight &&
          (result.next_deadline_ns == 0 ||
           slot.deadline_ns < result.next_deadline_ns))
        result.next_deadline_ns = slot.deadline_ns;
    }
    if (control_.kind != ControlKind::None &&
        (result.next_deadline_ns == 0 ||
         control_.deadline_ns < result.next_deadline_ns))
      result.next_deadline_ns = control_.deadline_ns;
    if (receive_.size() - receive_count_ < 2) {
      result.result = AdapterResult::WouldBlock;
      return result;
    }
    TransportEvent transport_event{};
    const AdapterResult polled = config_.transport->poll(transport_event);
    if (polled == AdapterResult::WouldBlock) return result;
    if (polled != AdapterResult::Ok) {
      result.result = polled;
      return result;
    }
    const AdapterResult processed =
        process_transport_event(transport_event, now_ns);
    if (processed != AdapterResult::Ok) result.result = processed;
  }
  return result;
}

AdapterResult BinanceTradeAdapter::on_deadline(
    const AdapterDeadline& deadline, std::uint64_t now_ns,
    const AdapterEventSink& sink) noexcept {
  (void)sink;
  if (deadline.due_time_ns > now_ns) return AdapterResult::InvalidArgument;
  if (deadline.kind == AdapterDeadlineKind::Request) {
    if (control_.kind != ControlKind::None &&
        control_.deadline_ns != 0 && control_.deadline_ns <= now_ns &&
        (deadline.id == 0 || deadline.id == control_.request.id) &&
        (deadline.generation == 0 ||
         deadline.generation == control_.reconcile_generation))
      return fail_control(AdapterResult::Failed, now_ns);
    SendSlot* slot = nullptr;
    if (deadline.id == 0) {
      for (SendSlot& candidate : send_)
        if (candidate.state == SlotState::InFlight &&
            candidate.deadline_ns <= now_ns &&
            (slot == nullptr ||
             candidate.deadline_ns < slot->deadline_ns))
          slot = &candidate;
    } else {
      slot = find_request(deadline.id);
      if (slot != nullptr &&
          ((deadline.generation != 0 &&
            deadline.generation != slot->generation) ||
           slot->deadline_ns > now_ns))
        slot = nullptr;
    }
    if (slot == nullptr) return AdapterResult::StaleReservation;
    return fail_command(static_cast<std::size_t>(slot - send_.data()),
                        AdapterResult::Failed, true);
  }
  if (deadline.kind == AdapterDeadlineKind::Keepalive) {
    const ListenKeyAction action = listen_key_.on_deadline(now_ns);
    if (action == ListenKeyAction::None)
      return AdapterResult::StaleReservation;
    lifecycle_action_ = action;
    return submit_listen_key(action, now_ns);
  }
  if (deadline.kind == AdapterDeadlineKind::Reconnect &&
      (!trading_session_ready_ ||
       listen_key_.state() == ListenKeyState::RecreateRequired)) {
    if (reconnect_deadline_ns_ != 0 && now_ns < reconnect_deadline_ns_)
      return AdapterResult::NotReady;
    if (!trading_session_ready_) {
      const AdapterResult trading = config_.transport->start_trading_stream();
      if (trading != AdapterResult::Ok) {
        schedule_reconnect(now_ns);
        return trading;
      }
      trading_start_required_ = false;
      reconnect_deadline_ns_ = 0;
      status_ = AdapterStatus::Authenticating;
    }
    if (listen_key_.state() != ListenKeyState::RecreateRequired)
      return AdapterResult::Ok;
    lifecycle_action_ = listen_key_.start();
    const AdapterResult submitted =
        submit_listen_key(lifecycle_action_, now_ns);
    if (submitted != AdapterResult::Ok && submitted != AdapterResult::WouldBlock)
      schedule_reconnect(now_ns);
    return submitted;
  }
  return AdapterResult::Unsupported;
}

AdapterResult BinanceTradeAdapter::ingest_user_stream(
    std::string_view json, const ParseContext& context) noexcept {
  if (status_ == AdapterStatus::Failed || status_ == AdapterStatus::Stopped)
    return AdapterResult::NotReady;
  AdapterEvent event = base_event(identity(), AdapterEventKind::Venue);
  const ParseResult parsed =
      config_.product == Product::Spot
          ? parse_spot_execution_report(json, context, event.venue)
          : parse_usdm_order_trade_update(json, context, event.venue);
  if (parsed == ParseResult::Unsupported) return AdapterResult::Unsupported;
  if (parsed != ParseResult::Ok) return AdapterResult::InvalidArgument;
  const AdapterResult queued = queue_event(event);
  if (queued == AdapterResult::Ok &&
      (event.venue.type == api::VenueEventType::NewReject ||
       event.venue.type == api::VenueEventType::CancelAck ||
       event.venue.type == api::VenueEventType::Expire ||
       event.venue.reconciled_status == api::OrderStatus::Filled)) {
    if (OrderBinding* binding = find_binding(context.handle);
        binding != nullptr) {
      *binding = {};
    }
  }
  return queued;
}

AdapterResult BinanceTradeAdapter::emit_immediate(
    const AdapterEvent& event, const AdapterEventSink& sink) noexcept {
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  const AdapterResult result = sink.on_event(sink.context, event);
  if (result == AdapterResult::WouldBlock) return queue_event(event);
  return result;
}

AdapterResult BinanceTradeAdapter::begin_reconcile(
    std::uint64_t generation, std::uint64_t now_ns,
    const AdapterEventSink& sink) noexcept {
  (void)sink;
  if (generation == 0 || reconcile_generation_ != 0)
    return AdapterResult::InvalidArgument;
  // Do not overtake an already-decoded user-stream or REST event. The owner
  // drains that bounded queue and retries reconciliation.
  if (receive_count_ != 0) return AdapterResult::WouldBlock;
  reconcile_generation_ = generation;
  reconcile_cursor_ = {};
  reconcile_response_size_ = 0;
  reconcile_snapshot_complete_ = false;
  for (OrderBinding& binding : bindings_)
    if (binding.used) binding.uncertain = true;
  status_ = AdapterStatus::Reconciling;
  const AdapterResult submitted = submit_reconcile(generation, now_ns);
  if (submitted != AdapterResult::Ok) {
    reconcile_generation_ = 0;
    status_ = submitted == AdapterResult::WouldBlock
                  ? (user_session_ready_ && trading_session_ready_
                         ? AdapterStatus::Ready
                         : AdapterStatus::Authenticating)
                  : AdapterStatus::Failed;
  }
  return submitted;
}

AdapterResult BinanceTradeAdapter::shutdown(
    std::uint64_t now_ns, const AdapterEventSink& sink) noexcept {
  lifecycle_action_ = ListenKeyAction::None;
  (void)listen_key_.shutdown();
  config_.transport->close();
  control_ = {};
  for (std::size_t index = 0; index < send_.size(); ++index)
    release(index);
  status_ = AdapterStatus::Stopped;
  AdapterEvent event = base_event(identity(), AdapterEventKind::Status);
  event.status.identity = identity();
  event.status.status = status_;
  event.status.reason = AdapterResult::Ok;
  event.status.event_time_ns = now_ns;
  return emit_immediate(event, sink);
}

std::size_t BinanceTradeAdapter::pending_commands() const noexcept {
  std::size_t count = 0;
  for (const SendSlot& slot : send_)
    if (slot.state != SlotState::Free) ++count;
  return count;
}

}  // namespace oms::exchange::binance
