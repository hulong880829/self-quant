#include "oms/exchange/polymarket/trade_adapter.h"

#include <array>
#include <charconv>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <string_view>

namespace oms::exchange::polymarket {
namespace {

template <typename Id>
std::string_view IdView(const Id& id) noexcept {
  return id.length <= id.value.size()
             ? std::string_view{id.value.data(), id.length}
             : std::string_view{};
}

AdapterEvent Event(AdapterIdentity identity, AdapterEventKind kind) noexcept {
  AdapterEvent event{};
  event.kind = kind;
  event.source = identity;
  return event;
}

AdapterResult ProtocolAdapterResult(ProtocolResult result) noexcept {
  switch (result) {
    case ProtocolResult::Ok:
      return AdapterResult::Ok;
    case ProtocolResult::InvalidArgument:
    case ProtocolResult::Malformed:
      return AdapterResult::InvalidArgument;
    case ProtocolResult::Unsupported:
      return AdapterResult::Unsupported;
    case ProtocolResult::BoundsExceeded:
      return AdapterResult::WouldBlock;
  }
  return AdapterResult::Failed;
}

AdapterResult CryptoAdapterResult(CryptoResult result) noexcept {
  switch (result) {
    case CryptoResult::Ok:
      return AdapterResult::Ok;
    case CryptoResult::InvalidArgument:
      return AdapterResult::InvalidArgument;
    case CryptoResult::BufferTooSmall:
      return AdapterResult::WouldBlock;
    case CryptoResult::UnsupportedSignatureType:
      return AdapterResult::Unsupported;
    case CryptoResult::CryptoFailure:
      return AdapterResult::Failed;
  }
  return AdapterResult::Failed;
}

std::uint64_t Power10(std::uint8_t exponent) noexcept {
  std::uint64_t value = 1;
  while (exponent-- != 0) value *= 10U;
  return value;
}

bool AtScale(api::FixedPoint value, std::uint8_t scale,
             std::uint64_t& output) noexcept {
  output = 0;
  if (value.value <= 0 || value.scale > 18 || scale > 18)
    return false;
  std::uint64_t converted = static_cast<std::uint64_t>(value.value);
  if (value.scale < scale) {
    const std::uint64_t multiplier =
        Power10(static_cast<std::uint8_t>(scale - value.scale));
    if (converted > std::numeric_limits<std::uint64_t>::max() / multiplier)
      return false;
    converted *= multiplier;
  } else if (value.scale > scale) {
    const std::uint64_t divisor =
        Power10(static_cast<std::uint8_t>(value.scale - scale));
    if (converted % divisor != 0) return false;
    converted /= divisor;
  }
  output = converted;
  return true;
}

}  // namespace

PolymarketTradeAdapter::PolymarketTradeAdapter(
    const AdapterConfig& config) noexcept
    : config_(config),
      signing_context_(config.credentials.private_key.view()) {
  Address address{};
  std::array<char, 45> signature{};
  std::size_t signature_size = 0;
  const bool valid =
      config_.instruments != nullptr && config_.transport != nullptr &&
      config_.now_ms != nullptr && config_.request_timeout_ns != 0 &&
      config_.reconnect_initial_ns != 0 && config_.reconnect_max_ns != 0 &&
      config_.reconnect_initial_ns <= config_.reconnect_max_ns &&
      config_.heartbeat_interval_ns != 0 &&
      config_.liveness_timeout_ns > config_.heartbeat_interval_ns &&
      !config_.credentials.signer_address.view().empty() &&
      !config_.credentials.funder_address.view().empty() &&
      !config_.credentials.private_key.view().empty() &&
      !config_.credentials.api_key.view().empty() &&
      !config_.credentials.api_secret.view().empty() &&
      !config_.credentials.passphrase.view().empty() &&
      DecodeAddress(config_.credentials.signer_address.view(), address) ==
          CryptoResult::Ok &&
      DecodeAddress(config_.credentials.funder_address.view(), address) ==
          CryptoResult::Ok &&
      signing_context_.status() == CryptoResult::Ok &&
      L2Signature(config_.credentials.api_secret.view(), "1", "GET", "/",
                  "", signature.data(), signature.size(), signature_size) ==
          CryptoResult::Ok;
  status_ = valid ? AdapterStatus::Connecting : AdapterStatus::Failed;
  resume_status_ = status_;
}

AdapterIdentity PolymarketTradeAdapter::identity() const noexcept {
  AdapterIdentity result{};
  result.kind = AdapterKind::Polymarket;
  result.venue = utils::md::Venue::Polymarket;
  result.product_type = utils::md::ProductType::BinaryOption;
  return result;
}

AdapterStatus PolymarketTradeAdapter::status() const noexcept { return status_; }

AdapterCapabilities PolymarketTradeAdapter::capabilities() const noexcept {
  AdapterCapabilities result{};
  result.bits =
      static_cast<std::uint64_t>(AdapterCapability::Limit) |
      static_cast<std::uint64_t>(AdapterCapability::TifGtc) |
      static_cast<std::uint64_t>(AdapterCapability::TifGtd) |
      static_cast<std::uint64_t>(AdapterCapability::TifIoc) |
      static_cast<std::uint64_t>(AdapterCapability::TifFok) |
      static_cast<std::uint64_t>(AdapterCapability::TifFak) |
      static_cast<std::uint64_t>(AdapterCapability::PostOnly) |
      static_cast<std::uint64_t>(AdapterCapability::ReconcileOpenOrders) |
      static_cast<std::uint64_t>(AdapterCapability::UserOrderStream) |
      static_cast<std::uint64_t>(AdapterCapability::UserFillStream);
  return result;
}

AdapterResult PolymarketTradeAdapter::reserve_command(
    AdapterCommandKind kind, AdapterReservation& reservation) noexcept {
  reservation = {};
  const bool ready =
      status_ == AdapterStatus::Ready ||
      (status_ == AdapterStatus::Backpressured &&
       resume_status_ == AdapterStatus::Ready);
  const bool reconnect_cancel =
      kind == AdapterCommandKind::Cancel &&
      (status_ == AdapterStatus::Reconnecting ||
       (status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Reconnecting));
  if (!ready && !reconnect_cancel)
    return AdapterResult::NotReady;
  for (std::size_t index = 0; index < commands_.size(); ++index) {
    CommandSlot& slot = commands_[index];
    if (slot.reserved || slot.inflight) continue;
    if (++slot.generation == 0) ++slot.generation;
    slot.reserved = true;
    slot.kind = kind;
    reservation.adapter = AdapterKind::Polymarket;
    reservation.slot = static_cast<std::uint32_t>(index);
    reservation.generation = slot.generation;
    return AdapterResult::Ok;
  }
  if (status_ != AdapterStatus::Backpressured) resume_status_ = status_;
  status_ = AdapterStatus::Backpressured;
  return AdapterResult::WouldBlock;
}

bool PolymarketTradeAdapter::valid(AdapterReservation reservation,
                                   AdapterCommandKind kind) const noexcept {
  if (reservation.adapter != AdapterKind::Polymarket ||
      reservation.slot >= commands_.size() || reservation.generation == 0)
    return false;
  const CommandSlot& slot = commands_[reservation.slot];
  return slot.reserved && !slot.inflight && slot.kind == kind &&
         slot.generation == reservation.generation;
}

void PolymarketTradeAdapter::release(std::size_t slot) noexcept {
  const std::uint64_t generation = commands_[slot].generation;
  commands_[slot] = {};
  commands_[slot].generation = generation;
  refresh_backpressure();
}

void PolymarketTradeAdapter::refresh_backpressure() noexcept {
  if (status_ != AdapterStatus::Backpressured ||
      pending_size_ == pending_events_.size())
    return;
  for (const CommandSlot& slot : commands_) {
    if (!slot.reserved && !slot.inflight) {
      status_ = resume_status_;
      return;
    }
  }
}

void PolymarketTradeAdapter::cancel_reservation(
    AdapterReservation reservation) noexcept {
  if (reservation.adapter != AdapterKind::Polymarket ||
      reservation.slot >= commands_.size())
    return;
  CommandSlot& slot = commands_[reservation.slot];
  if (slot.reserved && !slot.inflight &&
      slot.generation == reservation.generation)
    release(reservation.slot);
}

AdapterResult PolymarketTradeAdapter::authorize(
    TransportRequest& request) noexcept {
  const std::uint64_t timestamp = config_.now_ms(config_.clock_context) / 1000U;
  std::array<char, 24> text{};
  const auto converted =
      std::to_chars(text.data(), text.data() + text.size(), timestamp);
  if (converted.ec != std::errc{} ||
      !request.poly_timestamp.assign(
          {text.data(), static_cast<std::size_t>(converted.ptr - text.data())}) ||
      !request.poly_address.assign(config_.credentials.signer_address.view()) ||
      !request.poly_api_key.assign(config_.credentials.api_key.view()) ||
      !request.poly_passphrase.assign(config_.credentials.passphrase.view()))
    return AdapterResult::InvalidArgument;
  std::size_t signature_size = 0;
  std::string_view signed_path{request.wire.path.data(),
                               request.wire.path_size};
  if (const std::size_t query = signed_path.find('?');
      query != std::string_view::npos)
    signed_path = signed_path.substr(0, query);
  const CryptoResult signed_result = L2Signature(
      config_.credentials.api_secret.view(), request.poly_timestamp.view(),
      {request.wire.method.data(), request.wire.method_size},
      signed_path,
      {request.wire.body.data(), request.wire.body_size},
      request.poly_signature.value.data(), request.poly_signature.value.size(),
      signature_size);
  if (signed_result != CryptoResult::Ok)
    return CryptoAdapterResult(signed_result);
  request.poly_signature.size = static_cast<std::uint16_t>(signature_size);
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::commit_place(
    AdapterReservation reservation,
    const AdapterPlaceCommand& command) noexcept {
  if (!valid(reservation, AdapterCommandKind::Place))
    return AdapterResult::StaleReservation;
  const std::size_t index = reservation.slot;
  const auto finish = [&](AdapterResult result) noexcept {
    release(index);
    return result;
  };
  if (status_ != AdapterStatus::Ready &&
      !(status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Ready))
    return finish(AdapterResult::NotReady);
  const api::NewOrderRequest& request = command.request;
  std::size_t order_slots = 0;
  for (const OrderBinding& binding : orders_)
    if (binding.used) ++order_slots;
  for (const CommandSlot& slot : commands_)
    if (slot.inflight && slot.kind == AdapterCommandKind::Place) ++order_slots;
  if (order_slots >= orders_.size())
    return finish(AdapterResult::WouldBlock);
  if (request.type != api::OrderType::Limit ||
      (request.flags & ~(static_cast<std::uint16_t>(api::OrderFlag::PostOnly))) !=
          0)
    return finish(AdapterResult::Unsupported);
  const std::uint64_t now_ms = config_.now_ms(config_.clock_context);
  std::uint64_t expiration_seconds = 0;
  if (request.time_in_force == api::TimeInForce::GTD) {
    if (request.expire_time_ns == 0 ||
        request.expire_time_ns % 1000000000ULL != 0 ||
        request.expire_time_ns / 1000000ULL <= now_ms)
      return finish(AdapterResult::InvalidArgument);
    expiration_seconds = request.expire_time_ns / 1000000000ULL;
  } else if (request.expire_time_ns != 0) {
    return finish(AdapterResult::InvalidArgument);
  }
  const bool prepared =
      command.routing.kind == api::ExecutionRouteKind::Polymarket;
  const TradingMetadata* trading =
      prepared ? nullptr
               : config_.instruments->FindTradingMetadata(
                     request.instrument_id);
  const utils::md::Instrument* instrument =
      prepared ? nullptr : config_.instruments->Find(request.instrument_id);
  if ((!prepared &&
       (trading == nullptr || instrument == nullptr ||
        trading->kind != MetadataKind::Polymarket ||
        instrument->venue != utils::md::Venue::Polymarket)) ||
      (prepared &&
       command.routing.venue !=
           static_cast<std::uint8_t>(utils::md::Venue::Polymarket)))
    return finish(AdapterResult::InvalidArgument);
  const std::uint8_t signature_type =
      prepared ? command.routing.signature_type
               : trading->polymarket.signature_type;
  const bool negative_risk =
      prepared ? command.routing.negative_risk
               : trading->polymarket.negative_risk;
  const std::int64_t minimum_order_size =
      prepared ? command.routing.minimum_order_size
               : trading->polymarket.minimum_order_size;
  const std::uint32_t taker_delay_ms =
      prepared ? command.routing.taker_delay_ms
               : trading->polymarket.taker_delay_ms;
  const std::uint8_t price_scale =
      prepared ? command.routing.price_scale : instrument->price_scale;
  const std::uint8_t quantity_scale =
      prepared ? command.routing.quantity_scale : instrument->quantity_scale;
  const std::int64_t tick_size =
      prepared ? command.routing.tick_size : instrument->tick_size;
  const std::int64_t lot_size =
      prepared ? command.routing.lot_size : instrument->lot_size;
  if (signature_type == 1)
    return finish(AdapterResult::Unsupported);
  if (prepared && command.routing.instrument_expiry_ns != 0 &&
      now_ms >= command.routing.instrument_expiry_ns / 1'000'000ULL)
    return finish(AdapterResult::InvalidArgument);
  std::uint64_t quantity_units = 0;
  std::uint64_t price_units = 0;
  if (lot_size <= 0 || tick_size <= 0 ||
      !AtScale(request.quantity, quantity_scale, quantity_units) ||
      !AtScale(request.price, price_scale, price_units) ||
      quantity_units %
              static_cast<std::uint64_t>(lot_size) !=
          0 ||
      price_units % static_cast<std::uint64_t>(tick_size) != 0 ||
      quantity_units <
          static_cast<std::uint64_t>(minimum_order_size) ||
      price_units >= Power10(price_scale) ||
      ((request.flags & api::OrderFlag::PostOnly) != 0 &&
       request.time_in_force != api::TimeInForce::GTC &&
       request.time_in_force != api::TimeInForce::GTD))
    return finish(AdapterResult::InvalidArgument);
  if (request.time_in_force == api::TimeInForce::GTD) {
    if (now_ms > std::numeric_limits<std::uint64_t>::max() -
                     taker_delay_ms ||
        request.expire_time_ns / 1000000ULL <=
            now_ms + taker_delay_ms)
      return finish(AdapterResult::InvalidArgument);
  }

  Order order{};
  if (DecodeAddress(config_.credentials.funder_address.view(), order.maker) !=
      CryptoResult::Ok)
    return finish(AdapterResult::InvalidArgument);
  if (signature_type == 3) {
    order.signer = order.maker;
  } else if (DecodeAddress(config_.credentials.signer_address.view(),
                           order.signer) != CryptoResult::Ok) {
    return finish(AdapterResult::InvalidArgument);
  }
  order.token_id =
      prepared ? command.routing.token_id : trading->polymarket.token_id;
  order.side = request.side == api::Side::Buy ? 0 : 1;
  if (request.side != api::Side::Buy && request.side != api::Side::Sell)
    return finish(AdapterResult::InvalidArgument);
  order.signature_type = signature_type;
  order.timestamp_ms = now_ms;
  order.negative_risk = negative_risk;
  CryptoResult crypto = GenerateSafeSalt(order.salt);
  if (crypto != CryptoResult::Ok)
    return finish(CryptoAdapterResult(crypto));
  const ProtocolResult amounts =
      OrderAmounts(request.side, request.quantity, request.price,
                   order.maker_amount, order.taker_amount);
  if (amounts != ProtocolResult::Ok)
    return finish(ProtocolAdapterResult(amounts));
  Signature signature{};
  crypto = SignOrder(order, signing_context_, signature);
  if (crypto != CryptoResult::Ok)
    return finish(CryptoAdapterResult(crypto));

  TransportRequest transport{};
  transport.id = next_request_id_++;
  if (transport.id == 0) transport.id = next_request_id_++;
  const PlaceOrderInput input{order, signature,
                              config_.credentials.api_key.view(),
                              request.time_in_force,
                              (request.flags & api::OrderFlag::PostOnly) != 0,
                              expiration_seconds};
  const ProtocolResult built = BuildPlaceOrder(input, transport.wire);
  if (built != ProtocolResult::Ok)
    return finish(ProtocolAdapterResult(built));
  AdapterResult result = authorize(transport);
  if (result != AdapterResult::Ok) return finish(result);

  CommandSlot& slot = commands_[index];
  slot.request_id = transport.id;
  slot.command_id = command.command_id;
  slot.handle = command.handle;
  slot.token = request.token;
  slot.instrument_id = request.instrument_id;
  slot.reserved = false;
  slot.inflight = true;
  result = config_.transport->submit(transport);
  if (result != AdapterResult::Ok) return finish(result);
  return AdapterResult::Ok;
}

const api::VenueOrderId* PolymarketTradeAdapter::find_order(
    api::OrderHandle handle) const noexcept {
  for (const OrderBinding& binding : orders_)
    if (binding.used && binding.handle == handle) return &binding.venue_order_id;
  return nullptr;
}

void PolymarketTradeAdapter::bind_order(
    api::OrderHandle handle, const api::VenueOrderId& venue_order_id,
    api::InstrumentId instrument_id) noexcept {
  for (OrderBinding& binding : orders_) {
    if (binding.used && binding.handle == handle) {
      binding.venue_order_id = venue_order_id;
      binding.instrument_id = instrument_id;
      return;
    }
  }
  for (OrderBinding& binding : orders_) {
    if (!binding.used) {
      binding.used = true;
      binding.reconcile_seen = false;
      binding.handle = handle;
      binding.venue_order_id = venue_order_id;
      binding.instrument_id = instrument_id;
      return;
    }
  }
}

AdapterResult PolymarketTradeAdapter::validate_rebind(
    const api::RebindPolymarketInstrumentRequest& request) const noexcept {
  if (config_.instruments == nullptr ||
      config_.instruments->ValidateRebind(request) != api::Error::Ok ||
      status_ != AdapterStatus::Ready || reconcile_generation_ != 0 ||
      reconcile_request_id_ != 0 || pending_size_ != 0) {
    return AdapterResult::InvalidArgument;
  }
  for (const CommandSlot& slot : commands_)
    if (slot.reserved || slot.inflight) return AdapterResult::WouldBlock;
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::apply_rebind(
    const api::RebindPolymarketInstrumentRequest& request) noexcept {
  const AdapterResult valid = validate_rebind(request);
  if (valid != AdapterResult::Ok) return valid;
  for (OrderBinding& binding : orders_)
    if (binding.used && binding.instrument_id == request.instrument_id)
      binding = {};
  return config_.instruments->Rebind(request) == api::Error::Ok
             ? AdapterResult::Ok
             : AdapterResult::Failed;
}

AdapterResult PolymarketTradeAdapter::commit_cancel(
    AdapterReservation reservation,
    const AdapterCancelCommand& command) noexcept {
  if (!valid(reservation, AdapterCommandKind::Cancel))
    return AdapterResult::StaleReservation;
  const std::size_t index = reservation.slot;
  const auto finish = [&](AdapterResult result) noexcept {
    release(index);
    return result;
  };
  if (status_ != AdapterStatus::Ready &&
      !(status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Ready) &&
      status_ != AdapterStatus::Reconnecting &&
      !(status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Reconnecting))
    return finish(AdapterResult::NotReady);
  const api::VenueOrderId* venue_order_id =
      find_order(command.request.handle);
  if (venue_order_id == nullptr) return finish(AdapterResult::InvalidArgument);
  TransportRequest transport{};
  transport.id = next_request_id_++;
  if (transport.id == 0) transport.id = next_request_id_++;
  const ProtocolResult built =
      BuildCancelOrder(IdView(*venue_order_id), transport.wire);
  if (built != ProtocolResult::Ok)
    return finish(ProtocolAdapterResult(built));
  AdapterResult result = authorize(transport);
  if (result != AdapterResult::Ok) return finish(result);
  CommandSlot& slot = commands_[index];
  slot.request_id = transport.id;
  slot.command_id = command.command_id;
  slot.handle = command.request.handle;
  slot.token = command.request.request_token;
  slot.venue_order_id = *venue_order_id;
  slot.reserved = false;
  slot.inflight = true;
  result = config_.transport->submit(transport);
  if (result != AdapterResult::Ok) return finish(result);
  return AdapterResult::Ok;
}

PolymarketTradeAdapter::CommandSlot* PolymarketTradeAdapter::find_request(
    std::uint32_t request_id) noexcept {
  for (CommandSlot& slot : commands_)
    if (slot.inflight && slot.request_id == request_id) return &slot;
  return nullptr;
}

AdapterResult PolymarketTradeAdapter::queue(
    const AdapterEvent& event) noexcept {
  if (pending_size_ == pending_events_.size()) {
    if (status_ != AdapterStatus::Backpressured) resume_status_ = status_;
    status_ = AdapterStatus::Backpressured;
    return AdapterResult::WouldBlock;
  }
  const std::size_t tail =
      (static_cast<std::size_t>(pending_begin_) + pending_size_) %
      pending_events_.size();
  pending_events_[tail] = event;
  ++pending_size_;
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::offer(
    const AdapterEventSink& sink, const AdapterEvent& event) noexcept {
  if (pending_size_ != 0) return queue(event);
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  const AdapterResult result = sink.on_event(sink.context, event);
  return result == AdapterResult::WouldBlock ? queue(event) : result;
}

AdapterResult PolymarketTradeAdapter::submit_reconcile_page() noexcept {
  TransportRequest request{};
  request.id = next_request_id_++;
  if (request.id == 0) request.id = next_request_id_++;
  const ProtocolResult built = BuildOpenOrdersPage(pagination_, request.wire);
  if (built != ProtocolResult::Ok) return ProtocolAdapterResult(built);
  AdapterResult result = authorize(request);
  if (result != AdapterResult::Ok) return result;
  result = config_.transport->submit(request);
  if (result == AdapterResult::Ok) {
    reconcile_request_id_ = request.id;
    reconcile_deadline_ns_ = 0;
  }
  return result;
}

AdapterResult PolymarketTradeAdapter::submit_query_page() noexcept {
  if (!query_.active) return AdapterResult::InvalidArgument;
  TransportRequest request{};
  request.id = next_request_id_++;
  if (request.id == 0) request.id = next_request_id_++;
  ProtocolResult built = ProtocolResult::InvalidArgument;
  Transport* transport = config_.transport;
  if (query_.kind == api::QueryKind::OpenOrders) {
    built = BuildOpenOrdersPage(query_.pagination, request.wire);
    if (built == ProtocolResult::Ok) {
      const AdapterResult authorized = authorize(request);
      if (authorized != AdapterResult::Ok) return authorized;
    }
  } else {
    built = BuildPositions(config_.credentials.funder_address.view(),
                           request.wire);
    transport = config_.data_transport;
  }
  if (built != ProtocolResult::Ok) return ProtocolAdapterResult(built);
  if (transport == nullptr) return AdapterResult::Unsupported;
  const AdapterResult submitted = transport->submit(request);
  if (submitted == AdapterResult::Ok) {
    query_.request_id = request.id;
    query_.deadline_ns = 0;
  }
  return submitted;
}

AdapterResult PolymarketTradeAdapter::query_open_orders(
    const AdapterQueryRequest& request,
    const AdapterEventSink& sink) noexcept {
  (void)sink;
  if (request.account_id == 0 || request.token.sequence == 0)
    return AdapterResult::InvalidArgument;
  if (query_.active) return AdapterResult::WouldBlock;
  query_ = {};
  query_.active = true;
  query_.kind = api::QueryKind::OpenOrders;
  query_.request = request;
  const AdapterResult result = submit_query_page();
  if (result != AdapterResult::Ok) query_ = {};
  return result;
}

AdapterResult PolymarketTradeAdapter::query_positions(
    const AdapterQueryRequest& request,
    const AdapterEventSink& sink) noexcept {
  (void)sink;
  if (request.account_id == 0 || request.token.sequence == 0)
    return AdapterResult::InvalidArgument;
  if (config_.data_transport == nullptr) return AdapterResult::Unsupported;
  if (query_.active) return AdapterResult::WouldBlock;
  query_ = {};
  query_.active = true;
  query_.kind = api::QueryKind::Positions;
  query_.request = request;
  const AdapterResult result = submit_query_page();
  if (result != AdapterResult::Ok) query_ = {};
  return result;
}

AdapterServiceResult PolymarketTradeAdapter::service_io(
    std::uint64_t now_ns, std::uint32_t event_budget,
    const AdapterEventSink& sink) noexcept {
  AdapterServiceResult result{};
  if (status_ == AdapterStatus::Failed || status_ == AdapterStatus::Stopped ||
      config_.transport == nullptr) {
    result.result = AdapterResult::NotReady;
    return result;
  }
  const bool session_ready =
      status_ == AdapterStatus::Ready ||
      (status_ == AdapterStatus::Backpressured &&
       resume_status_ == AdapterStatus::Ready);
  if (session_ready && last_session_activity_ns_ != 0 &&
      now_ns - last_session_activity_ns_ >= config_.liveness_timeout_ns) {
    config_.transport->close();
    reconnect_reconcile_required_ = true;
    heartbeat_due_ns_ = 0;
    reconnect_delay_ns_ = config_.reconnect_initial_ns;
    reconnect_due_ns_ =
        now_ns > std::numeric_limits<std::uint64_t>::max() -
                     reconnect_delay_ns_
            ? std::numeric_limits<std::uint64_t>::max()
            : now_ns + reconnect_delay_ns_;
    status_ = AdapterStatus::Reconnecting;
    resume_status_ = status_;
    AdapterEvent status_event = Event(identity(), AdapterEventKind::Status);
    status_event.status.identity = identity();
    status_event.status.status = status_;
    status_event.status.reason = AdapterResult::NotReady;
    status_event.status.event_time_ns = now_ns;
    if (queue(status_event) != AdapterResult::Ok) {
      result.result = AdapterResult::WouldBlock;
      return result;
    }
  } else if (session_ready && heartbeat_due_ns_ != 0 &&
             heartbeat_due_ns_ <= now_ns) {
    const AdapterResult heartbeat = config_.transport->send_heartbeat();
    if (heartbeat != AdapterResult::Ok) {
      result.result = heartbeat;
      return result;
    }
    heartbeat_due_ns_ =
        now_ns > std::numeric_limits<std::uint64_t>::max() -
                     config_.heartbeat_interval_ns
            ? std::numeric_limits<std::uint64_t>::max()
            : now_ns + config_.heartbeat_interval_ns;
  }
  if (heartbeat_due_ns_ != 0)
    result.next_deadline_ns = heartbeat_due_ns_;
  if (session_ready && last_session_activity_ns_ != 0) {
    const std::uint64_t liveness_due =
        last_session_activity_ns_ >
                std::numeric_limits<std::uint64_t>::max() -
                    config_.liveness_timeout_ns
            ? std::numeric_limits<std::uint64_t>::max()
            : last_session_activity_ns_ + config_.liveness_timeout_ns;
    if (result.next_deadline_ns == 0 ||
        liveness_due < result.next_deadline_ns)
      result.next_deadline_ns = liveness_due;
  }
  for (CommandSlot& slot : commands_) {
    if (!slot.inflight) continue;
    if (slot.deadline_ns == 0) {
      slot.deadline_ns =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.request_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.request_timeout_ns;
    }
    if (result.next_deadline_ns == 0 ||
        slot.deadline_ns < result.next_deadline_ns)
      result.next_deadline_ns = slot.deadline_ns;
  }
  if (reconcile_request_id_ != 0) {
    if (reconcile_deadline_ns_ == 0) {
      reconcile_deadline_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.request_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.request_timeout_ns;
    }
    if (result.next_deadline_ns == 0 ||
        reconcile_deadline_ns_ < result.next_deadline_ns)
      result.next_deadline_ns = reconcile_deadline_ns_;
  }
  if (query_.active && query_.request_id != 0) {
    if (query_.deadline_ns == 0) {
      query_.deadline_ns =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.request_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.request_timeout_ns;
    }
    if (result.next_deadline_ns == 0 ||
        query_.deadline_ns < result.next_deadline_ns)
      result.next_deadline_ns = query_.deadline_ns;
  }
  while (result.events_processed < event_budget) {
    if (pending_size_ != 0) {
      if (sink.on_event == nullptr) {
        result.result = AdapterResult::InvalidArgument;
        return result;
      }
      const AdapterResult delivered =
          sink.on_event(sink.context, pending_events_[pending_begin_]);
      if (delivered != AdapterResult::Ok) {
        result.result = delivered;
        return result;
      }
      pending_begin_ = static_cast<std::uint16_t>(
          (pending_begin_ + 1U) % pending_events_.size());
      --pending_size_;
      ++result.events_processed;
      refresh_backpressure();
      continue;
    }

    TransportEvent transport_event{};
    AdapterResult polled = config_.transport->poll(transport_event);
    if (polled == AdapterResult::WouldBlock &&
        config_.data_transport != nullptr)
      polled = config_.data_transport->poll(transport_event);
    if (polled == AdapterResult::WouldBlock) return result;
    if (polled != AdapterResult::Ok) {
      result.result = polled;
      return result;
    }
    last_session_activity_ns_ = now_ns;

    if (transport_event.kind == TransportEventKind::SessionReady ||
        transport_event.kind == TransportEventKind::SessionLost) {
      const bool ready =
          transport_event.kind == TransportEventKind::SessionReady;
      AdapterResult status_reason =
          ready ? AdapterResult::Ok : AdapterResult::NotReady;
      if (!ready) {
        reconnect_reconcile_required_ = true;
        reconcile_request_id_ = 0;
        reconcile_deadline_ns_ = 0;
        heartbeat_due_ns_ = 0;
        reconnect_delay_ns_ = config_.reconnect_initial_ns;
        reconnect_due_ns_ =
            now_ns > std::numeric_limits<std::uint64_t>::max() -
                         reconnect_delay_ns_
                ? std::numeric_limits<std::uint64_t>::max()
                : now_ns + reconnect_delay_ns_;
        if (reconcile_generation_ != 0) {
          AdapterEvent interrupted =
              Event(identity(), AdapterEventKind::ReconcileComplete);
          interrupted.reconcile.generation = reconcile_generation_;
          interrupted.reconcile.result = AdapterResult::Failed;
          reconcile_generation_ = 0;
          if (queue(interrupted) != AdapterResult::Ok) {
            result.result = AdapterResult::WouldBlock;
            return result;
          }
        }
        status_ = AdapterStatus::Reconnecting;
      } else if (reconnect_reconcile_required_ &&
                 reconcile_generation_ == 0) {
        reconnect_due_ns_ = 0;
        reconnect_delay_ns_ = config_.reconnect_initial_ns;
        heartbeat_due_ns_ =
            now_ns > std::numeric_limits<std::uint64_t>::max() -
                         config_.heartbeat_interval_ns
                ? std::numeric_limits<std::uint64_t>::max()
                : now_ns + config_.heartbeat_interval_ns;
        pagination_ = {};
        reconcile_uncertain_ = false;
        for (OrderBinding& binding : orders_)
          if (binding.used) binding.reconcile_seen = false;
        reconcile_generation_ = next_reconnect_generation_--;
        if (reconcile_generation_ == 0)
          reconcile_generation_ = next_reconnect_generation_--;
        status_ = AdapterStatus::Reconciling;
        const AdapterResult submitted = submit_reconcile_page();
        if (submitted != AdapterResult::Ok) {
          reconcile_generation_ = 0;
          status_ = AdapterStatus::Failed;
          status_reason = submitted;
        }
      } else if (!reconnect_reconcile_required_) {
        status_ = AdapterStatus::Ready;
        reconnect_due_ns_ = 0;
        reconnect_delay_ns_ = config_.reconnect_initial_ns;
        heartbeat_due_ns_ =
            now_ns > std::numeric_limits<std::uint64_t>::max() -
                         config_.heartbeat_interval_ns
                ? std::numeric_limits<std::uint64_t>::max()
                : now_ns + config_.heartbeat_interval_ns;
      }
      resume_status_ = status_;
      AdapterEvent status_event = Event(identity(), AdapterEventKind::Status);
      status_event.status.identity = identity();
      status_event.status.status = status_;
      status_event.status.reason = status_reason;
      status_event.status.event_time_ns = now_ns;
      if (queue(status_event) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    if (transport_event.kind == TransportEventKind::UserMessage) {
      std::array<api::VenueEvent, kMaximumTradeEvents> parsed_events{};
      std::size_t parsed_count = 0;
      const ProtocolResult parsed = ParseUserMessageEvents(
          transport_event.payload, parsed_events.data(), parsed_events.size(),
          parsed_count);
      if (parsed == ProtocolResult::Unsupported) continue;
      if (parsed != ProtocolResult::Ok) {
        result.result = AdapterResult::Failed;
        return result;
      }
      for (std::size_t parsed_index = 0; parsed_index < parsed_count;
           ++parsed_index) {
        AdapterEvent venue_event = Event(identity(), AdapterEventKind::Venue);
        venue_event.venue = parsed_events[parsed_index];
        const std::string_view venue_id =
            IdView(venue_event.venue.venue_order_id);
        if (!venue_id.empty()) {
          for (OrderBinding& binding : orders_) {
            if (binding.used && IdView(binding.venue_order_id) == venue_id) {
              venue_event.venue.handle = binding.handle;
              if (venue_event.venue.type == api::VenueEventType::CancelAck ||
                  venue_event.venue.type ==
                      api::VenueEventType::ReconcileTerminal)
                binding.used = false;
              break;
            }
          }
        }
        if (queue(venue_event) != AdapterResult::Ok) {
          result.result = AdapterResult::WouldBlock;
          return result;
        }
      }
      continue;
    }

    if (transport_event.kind != TransportEventKind::HttpResponse) {
      result.result = AdapterResult::Failed;
      return result;
    }

    if (query_.active &&
        transport_event.request_id == query_.request_id) {
      query_.request_id = 0;
      query_.deadline_ns = 0;
      AdapterResult query_result = AdapterResult::Ok;
      if (transport_event.status_code >= 400 ||
          transport_event.status_code == 0) {
        query_result = AdapterResult::Failed;
      } else if (query_.kind == api::QueryKind::OpenOrders) {
        std::array<OpenOrderSnapshot, kMaximumOpenOrders> parsed_items{};
        std::size_t parsed_count = 0;
        const ProtocolResult parsed = ParseOpenOrderSnapshots(
            transport_event.payload, query_.pagination, parsed_items.data(),
            parsed_items.size(), parsed_count);
        if (parsed != ProtocolResult::Ok) {
          query_result = ProtocolAdapterResult(parsed);
        } else {
          for (std::size_t index = 0; index < parsed_count; ++index) {
            const auto& source = parsed_items[index];
            AdapterEvent item =
                Event(identity(), AdapterEventKind::OpenOrderSnapshot);
            item.open_order.query_token = query_.request.token;
            item.open_order.account_id = query_.request.account_id;
            item.open_order.instrument_id =
                config_.instruments->FindPolymarketToken(source.token_id);
            item.open_order.venue_order_id = source.venue_order_id;
            item.open_order.side = source.side;
            item.open_order.status = source.status;
            item.open_order.quantity = source.quantity;
            item.open_order.price = source.price;
            item.open_order.matched_quantity = source.matched_quantity;
            const std::uint8_t scale =
                std::max(source.quantity.scale,
                         source.matched_quantity.scale);
            std::uint64_t quantity = 0, matched = 0;
            if (item.open_order.instrument_id == 0 ||
                !AtScale(source.quantity, scale, quantity) ||
                (source.matched_quantity.value != 0 &&
                 !AtScale(source.matched_quantity, scale, matched)) ||
                matched > quantity) {
              query_result = AdapterResult::InvalidArgument;
              break;
            }
            item.open_order.remaining_quantity.value =
                static_cast<std::int64_t>(quantity - matched);
            item.open_order.remaining_quantity.scale = scale;
            if (queue(item) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
          if (query_result == AdapterResult::Ok &&
              !query_.pagination.complete) {
            query_result = submit_query_page();
            if (query_result == AdapterResult::Ok) continue;
          }
        }
      } else {
        std::array<PositionSnapshot, kMaximumOpenOrders> parsed_items{};
        std::size_t parsed_count = 0;
        const ProtocolResult parsed =
            ParsePositions(transport_event.payload, parsed_items.data(),
                           parsed_items.size(), parsed_count);
        if (parsed != ProtocolResult::Ok) {
          query_result = ProtocolAdapterResult(parsed);
        } else {
          for (std::size_t index = 0; index < parsed_count; ++index) {
            AdapterEvent item =
                Event(identity(), AdapterEventKind::PositionSnapshot);
            item.position.query_token = query_.request.token;
            item.position.account_id = query_.request.account_id;
            item.position.instrument_id =
                config_.instruments->FindPolymarketToken(
                    parsed_items[index].token_id);
            item.position.quantity = parsed_items[index].quantity;
            if (item.position.instrument_id == 0) {
              query_result = AdapterResult::InvalidArgument;
              break;
            }
            if (queue(item) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
        }
      }
      AdapterEvent complete =
          Event(identity(), AdapterEventKind::QueryComplete);
      complete.query_complete.query_token = query_.request.token;
      complete.query_complete.account_id = query_.request.account_id;
      complete.query_complete.kind = query_.kind;
      complete.query_complete.error =
          query_result == AdapterResult::Ok
              ? api::Error::Ok
              : (query_result == AdapterResult::Unsupported
                     ? api::Error::Unsupported
                     : api::Error::NotReady);
      query_ = {};
      if (queue(complete) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    if (transport_event.request_id == reconcile_request_id_ &&
        reconcile_generation_ != 0) {
      reconcile_request_id_ = 0;
      reconcile_deadline_ns_ = 0;
      AdapterResult reconcile_result = AdapterResult::Ok;
      if (transport_event.status_code >= 400) {
        reconcile_result = AdapterResult::Failed;
      } else {
        std::array<api::VenueEvent, kOrderCapacity> parsed_events{};
        std::size_t parsed_count = 0;
        const ProtocolResult parsed = ParseOpenOrdersPage(
            transport_event.payload, pagination_, parsed_events.data(),
            parsed_events.size(), parsed_count);
        if (parsed != ProtocolResult::Ok) {
          reconcile_result = AdapterResult::Failed;
        } else {
          for (std::size_t index = 0; index < parsed_count; ++index) {
            AdapterEvent venue_event =
                Event(identity(), AdapterEventKind::Venue);
            venue_event.venue = parsed_events[index];
            const std::string_view venue_id =
                IdView(venue_event.venue.venue_order_id);
            bool local = false;
            for (OrderBinding& binding : orders_) {
              if (binding.used && IdView(binding.venue_order_id) == venue_id) {
                venue_event.venue.handle = binding.handle;
                binding.reconcile_seen = true;
                local = true;
                break;
              }
            }
            if (!local) reconcile_uncertain_ = true;
            if (queue(venue_event) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
          if (!pagination_.complete) {
            reconcile_result = submit_reconcile_page();
            if (reconcile_result == AdapterResult::Ok) continue;
          }
        }
      }
      AdapterEvent complete =
          Event(identity(), AdapterEventKind::ReconcileComplete);
      complete.reconcile.generation = reconcile_generation_;
      if (reconcile_result == AdapterResult::Ok) {
        for (const OrderBinding& binding : orders_)
          if (binding.used && !binding.reconcile_seen)
            reconcile_uncertain_ = true;
      }
      complete.reconcile.result =
          reconcile_result == AdapterResult::Ok && reconcile_uncertain_
              ? AdapterResult::Failed
              : reconcile_result;
      reconcile_generation_ = 0;
      status_ = reconcile_result == AdapterResult::Ok ? AdapterStatus::Ready
                                                       : AdapterStatus::Failed;
      resume_status_ = status_;
      if (reconcile_result == AdapterResult::Ok)
        reconnect_reconcile_required_ = false;
      if (queue(complete) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    CommandSlot* slot = find_request(transport_event.request_id);
    if (slot == nullptr) continue;
    const std::size_t slot_index =
        static_cast<std::size_t>(slot - commands_.data());
    AdapterEvent command_event =
        Event(identity(), AdapterEventKind::CommandResult);
    command_event.command_result.command_id = slot->command_id;
    command_event.command_result.kind = slot->kind;
    command_event.command_result.request_token = slot->token;
    command_event.command_result.result = AdapterResult::Failed;
    AdapterEvent venue_event = Event(identity(), AdapterEventKind::Venue);
    ProtocolResult parsed = ProtocolResult::Malformed;
    if (transport_event.status_code < 400) {
      parsed =
          slot->kind == AdapterCommandKind::Place
              ? ParsePlaceResponse(transport_event.payload, slot->handle,
                                   slot->token, venue_event.venue)
              : ParseCancelResponse(transport_event.payload,
                                    IdView(slot->venue_order_id), slot->handle,
                                    slot->token, venue_event.venue);
    } else if (transport_event.status_code < 500) {
      venue_event.venue.handle = slot->handle;
      venue_event.venue.token = slot->token;
      if (slot->kind == AdapterCommandKind::Place) {
        venue_event.venue.type = api::VenueEventType::NewReject;
      } else {
        venue_event.venue.type = api::VenueEventType::CancelReject;
        venue_event.venue.venue_order_id = slot->venue_order_id;
      }
      parsed = ProtocolResult::Ok;
    }
    if (parsed == ProtocolResult::Ok) {
      command_event.command_result.result =
          transport_event.status_code < 400 ? AdapterResult::Ok
                                            : AdapterResult::Failed;
      if (transport_event.status_code < 400 &&
          slot->kind == AdapterCommandKind::Place &&
          venue_event.venue.type == api::VenueEventType::NewAck)
        bind_order(slot->handle, venue_event.venue.venue_order_id,
                   slot->instrument_id);
      if (transport_event.status_code < 400 &&
          slot->kind == AdapterCommandKind::Cancel &&
          venue_event.venue.type == api::VenueEventType::CancelAck) {
        for (OrderBinding& binding : orders_)
          if (binding.used && binding.handle == slot->handle)
            binding.used = false;
      }
    }
    if (queue(command_event) != AdapterResult::Ok ||
        (parsed == ProtocolResult::Ok && queue(venue_event) != AdapterResult::Ok)) {
      result.result = AdapterResult::WouldBlock;
      return result;
    }
    release(slot_index);
  }
  return result;
}

AdapterResult PolymarketTradeAdapter::on_deadline(
    const AdapterDeadline& deadline, std::uint64_t now_ns,
    const AdapterEventSink& sink) noexcept {
  if (deadline.due_time_ns > now_ns) return AdapterResult::InvalidArgument;
  if (deadline.kind == AdapterDeadlineKind::Heartbeat) {
    const bool ready =
        status_ == AdapterStatus::Ready ||
        (status_ == AdapterStatus::Backpressured &&
         resume_status_ == AdapterStatus::Ready);
    if (!ready) return AdapterResult::NotReady;
    if (heartbeat_due_ns_ == 0 || heartbeat_due_ns_ > now_ns)
      return AdapterResult::StaleReservation;
    const AdapterResult result = config_.transport->send_heartbeat();
    if (result == AdapterResult::Ok) {
      heartbeat_due_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.heartbeat_interval_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.heartbeat_interval_ns;
    }
    return result;
  }
  if (deadline.kind == AdapterDeadlineKind::Reconnect) {
    const bool reconnecting =
        status_ == AdapterStatus::Reconnecting ||
        (status_ == AdapterStatus::Backpressured &&
         resume_status_ == AdapterStatus::Reconnecting);
    if (!reconnecting) return AdapterResult::NotReady;
    if (reconnect_due_ns_ == 0 || reconnect_due_ns_ > now_ns)
      return AdapterResult::WouldBlock;
    const AdapterResult result = config_.transport->request_reconnect();
    if (result == AdapterResult::Ok) {
      const std::uint64_t delay = reconnect_delay_ns_;
      const std::uint64_t next_delay =
          delay >= config_.reconnect_max_ns - delay
              ? config_.reconnect_max_ns
              : std::min(delay * 2U, config_.reconnect_max_ns);
      reconnect_due_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() - next_delay
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + next_delay;
      reconnect_delay_ns_ = next_delay;
    }
    return result;
  }
  if (deadline.kind != AdapterDeadlineKind::Request)
    return AdapterResult::Unsupported;
  const bool query_due =
      query_.active && query_.request_id != 0 && query_.deadline_ns != 0 &&
      query_.deadline_ns <= now_ns &&
      (deadline.id == 0 || deadline.id == query_.request_id);
  if (query_due) {
    AdapterEvent complete = Event(identity(), AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = query_.request.token;
    complete.query_complete.account_id = query_.request.account_id;
    complete.query_complete.kind = query_.kind;
    complete.query_complete.error = api::Error::NotReady;
    query_ = {};
    return offer(sink, complete);
  }
  const bool reconcile_due =
      reconcile_request_id_ != 0 && reconcile_generation_ != 0 &&
      reconcile_deadline_ns_ != 0 && reconcile_deadline_ns_ <= now_ns &&
      (deadline.id == 0 || deadline.id == reconcile_request_id_) &&
      (deadline.generation == 0 ||
       deadline.generation == reconcile_generation_);
  CommandSlot* slot = nullptr;
  if (deadline.id == 0) {
    for (CommandSlot& candidate : commands_) {
      if (candidate.inflight && candidate.deadline_ns != 0 &&
          candidate.deadline_ns <= now_ns &&
          (slot == nullptr || candidate.deadline_ns < slot->deadline_ns))
        slot = &candidate;
    }
  } else {
    slot = find_request(deadline.id);
    if (slot != nullptr && deadline.generation != 0 &&
        slot->generation != deadline.generation)
      slot = nullptr;
    if (slot != nullptr &&
        (slot->deadline_ns == 0 || slot->deadline_ns > now_ns))
      slot = nullptr;
  }
  if (reconcile_due &&
      (slot == nullptr || reconcile_deadline_ns_ <= slot->deadline_ns)) {
    AdapterEvent complete =
        Event(identity(), AdapterEventKind::ReconcileComplete);
    complete.reconcile.generation = reconcile_generation_;
    complete.reconcile.result = AdapterResult::Failed;
    reconcile_generation_ = 0;
    reconcile_request_id_ = 0;
    reconcile_deadline_ns_ = 0;
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
    return offer(sink, complete);
  }
  if (slot == nullptr) return AdapterResult::StaleReservation;
  const std::size_t index = static_cast<std::size_t>(slot - commands_.data());
  AdapterEvent event = Event(identity(), AdapterEventKind::CommandResult);
  event.command_result.command_id = slot->command_id;
  event.command_result.kind = slot->kind;
  event.command_result.request_token = slot->token;
  event.command_result.result = AdapterResult::Failed;
  release(index);
  return offer(sink, event);
}

AdapterResult PolymarketTradeAdapter::begin_reconcile(
    std::uint64_t generation, std::uint64_t now_ns,
    const AdapterEventSink& sink) noexcept {
  (void)now_ns;
  (void)sink;
  if (generation == 0 || reconcile_generation_ != 0 ||
      (status_ != AdapterStatus::Ready &&
       status_ != AdapterStatus::Backpressured))
    return AdapterResult::InvalidArgument;
  pagination_ = {};
  reconcile_uncertain_ = false;
  for (OrderBinding& binding : orders_)
    if (binding.used) binding.reconcile_seen = false;
  reconcile_generation_ = generation;
  status_ = AdapterStatus::Reconciling;
  resume_status_ = status_;
  const AdapterResult result = submit_reconcile_page();
  if (result != AdapterResult::Ok) {
    reconcile_generation_ = 0;
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
  }
  return result;
}

AdapterResult PolymarketTradeAdapter::shutdown(
    std::uint64_t now_ns, const AdapterEventSink& sink) noexcept {
  if (config_.transport != nullptr) config_.transport->close();
  if (config_.data_transport != nullptr) config_.data_transport->close();
  status_ = AdapterStatus::Stopped;
  resume_status_ = status_;
  AdapterEvent event = Event(identity(), AdapterEventKind::Status);
  event.status.identity = identity();
  event.status.status = status_;
  event.status.reason = AdapterResult::Ok;
  event.status.event_time_ns = now_ns;
  return offer(sink, event);
}

}  // namespace oms::exchange::polymarket
