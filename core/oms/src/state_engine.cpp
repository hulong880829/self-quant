#include "oms/state_engine.h"

#include <algorithm>
#include <limits>
#include <stdexcept>

namespace oms {
namespace {

std::size_t NextPowerOfTwo(std::size_t value) {
  if (value > std::numeric_limits<std::size_t>::max() / 2)
    throw std::invalid_argument("unsupported OMS fill dedup capacity");
  std::size_t result = 1;
  while (result < value) {
    if (result > std::numeric_limits<std::size_t>::max() / 2)
      throw std::invalid_argument("unsupported OMS fill dedup capacity");
    result *= 2;
  }
  return result;
}

std::size_t FillHash(api::OrderHandle handle,
                     const api::TradeId& trade_id) noexcept {
  std::size_t hash =
      static_cast<std::size_t>(handle.slot) * 0x9e3779b97f4a7c15ULL +
      handle.generation;
  for (std::size_t i = 0; i < trade_id.length; ++i)
    hash = (hash ^ static_cast<unsigned char>(trade_id.value[i])) *
           1099511628211ULL;
  return hash;
}

bool IsTerminal(api::OrderStatus status) noexcept {
  return status == api::OrderStatus::Filled ||
         status == api::OrderStatus::Canceled ||
         status == api::OrderStatus::Rejected ||
         status == api::OrderStatus::Expired;
}

bool HasVenueId(const api::VenueOrderId& id) noexcept {
  return id.length > 0 && id.length <= id.value.size();
}

bool HasClientId(const api::ClientOrderId& id) noexcept {
  return id.length > 0 && id.length <= id.value.size();
}

bool ValidSide(api::Side side) noexcept {
  return side == api::Side::Buy || side == api::Side::Sell;
}

bool ValidOrderType(api::OrderType type) noexcept {
  return type == api::OrderType::Limit || type == api::OrderType::Market;
}

bool ValidTimeInForce(api::TimeInForce value) noexcept {
  switch (value) {
    case api::TimeInForce::GTC:
    case api::TimeInForce::GTD:
    case api::TimeInForce::IOC:
    case api::TimeInForce::FOK:
    case api::TimeInForce::FAK:
      return true;
  }
  return false;
}

template <std::size_t Size>
bool HasAnyByte(const std::array<std::uint8_t, Size>& value) noexcept {
  return std::any_of(value.begin(), value.end(),
                     [](std::uint8_t byte) { return byte != 0; });
}

}  // namespace

StateEngine::StateEngine(OrderTable& orders,
                         const InstrumentRegistry& instruments,
                         std::size_t fill_dedup_capacity)
    : orders_(orders), instruments_(instruments),
      fill_dedup_(NextPowerOfTwo(
          std::max<std::size_t>(4, fill_dedup_capacity * 2))) {}

void StateEngine::EmitOrder(api::UpdateType type, api::OrderHandle handle,
                            const OrderRecord& record,
                            const api::UpdateSink& sink) noexcept {
  if (sink.on_order == nullptr) return;
  api::OrderUpdate update{};
  update.type = type;
  update.handle = handle;
  update.token = record.request.token;
  update.client_order_id = record.request.client_order_id;
  update.venue_order_id = record.venue_order_id;
  update.status = record.status;
  update.inflight = record.inflight;
  sink.on_order(sink.context, update);
}

api::Error StateEngine::Submit(const api::NewOrderRequest& request,
                               api::OrderHandle& handle,
                               const api::UpdateSink& sink) noexcept {
  if (!instruments_.frozen()) return api::Error::InvalidArgument;
  constexpr std::uint16_t known_flags =
      api::PostOnly | api::ReduceOnly | api::ClosePosition |
      api::QuoteQuantity;
  if (!ValidSide(request.side) || !ValidOrderType(request.type) ||
      !ValidTimeInForce(request.time_in_force) ||
      (request.flags & static_cast<std::uint16_t>(~known_flags)) != 0U)
    return api::Error::InvalidArgument;
  const auto* instrument = instruments_.Find(request.instrument_id);
  if (instrument == nullptr) return api::Error::NotFound;
  if (!instruments_.IsTradeable(request.instrument_id))
    return api::Error::Untradeable;
  if (request.quantity.scale != instrument->quantity_scale ||
      request.price.scale != instrument->price_scale)
    return api::Error::InvalidScale;
  if (request.quantity.value <= 0 ||
      ((request.flags & api::QuoteQuantity) == 0U &&
       request.quantity.value % instrument->lot_size != 0))
    return api::Error::InvalidArgument;
  if (request.type == api::OrderType::Limit &&
      (request.price.value <= 0 ||
       request.price.value % instrument->tick_size != 0))
    return api::Error::InvalidArgument;
  if (request.type == api::OrderType::Market &&
      (request.flags & api::PostOnly) != 0U)
    return api::Error::InvalidArgument;
  if ((request.time_in_force == api::TimeInForce::GTD) !=
      (request.expire_time_ns != 0))
    return api::Error::InvalidArgument;
  if (const auto* trading =
          instruments_.FindTradingMetadata(request.instrument_id);
      instrument->venue == utils::md::Venue::Polymarket &&
      trading != nullptr &&
      request.quantity.value < trading->polymarket.minimum_order_size)
    return api::Error::InvalidArgument;
  const api::Error result = orders_.Insert(request, handle);
  if (result != api::Error::Ok) return result;
  EmitOrder(api::UpdateType::Submitted, handle, *orders_.Lookup(handle), sink);
  return api::Error::Ok;
}

api::Error StateEngine::Submit(const api::PreparedOrderRequest& prepared,
                               api::OrderHandle& handle,
                               const api::UpdateSink& sink) noexcept {
  const auto& request = prepared.order;
  const auto& routing = prepared.routing;
  constexpr std::uint16_t known_flags =
      api::PostOnly | api::ReduceOnly | api::ClosePosition |
      api::QuoteQuantity;
  const bool polymarket =
      routing.kind == api::ExecutionRouteKind::Polymarket;
  const bool polymarket_venue =
      routing.venue ==
      static_cast<std::uint8_t>(utils::md::Venue::Polymarket);
  if ((routing.kind != api::ExecutionRouteKind::Generic && !polymarket) ||
      routing.venue == 0 || routing.product_type == 0 ||
      routing.catalog_generation == 0 ||
      polymarket != polymarket_venue ||
      routing.price_scale > 18 || routing.quantity_scale > 18 ||
      routing.tick_size <= 0 || routing.lot_size <= 0 ||
      !ValidSide(request.side) || !ValidOrderType(request.type) ||
      !ValidTimeInForce(request.time_in_force) ||
      (request.flags & static_cast<std::uint16_t>(~known_flags)) != 0U)
    return api::Error::InvalidArgument;
  if (request.quantity.scale != routing.quantity_scale ||
      request.price.scale != routing.price_scale)
    return api::Error::InvalidScale;
  if (request.quantity.value <= 0 ||
      ((request.flags & api::QuoteQuantity) == 0U &&
       request.quantity.value % routing.lot_size != 0))
    return api::Error::InvalidArgument;
  if (request.type == api::OrderType::Limit &&
      (request.price.value <= 0 ||
       request.price.value % routing.tick_size != 0))
    return api::Error::InvalidArgument;
  if (request.type == api::OrderType::Market &&
      (request.flags & api::PostOnly) != 0U)
    return api::Error::InvalidArgument;
  if ((request.time_in_force == api::TimeInForce::GTD) !=
      (request.expire_time_ns != 0))
    return api::Error::InvalidArgument;
  if (polymarket &&
      (routing.minimum_order_size <= 0 ||
       request.quantity.value < routing.minimum_order_size ||
       (routing.outcome != api::PolymarketOutcome::Yes &&
        routing.outcome != api::PolymarketOutcome::No) ||
       (routing.signature_type != 0 && routing.signature_type != 3) ||
       !HasAnyByte(routing.condition_id) ||
       !HasAnyByte(routing.token_id)))
    return api::Error::InvalidArgument;
  const api::Error result = orders_.Insert(prepared, handle);
  if (result != api::Error::Ok) return result;
  EmitOrder(api::UpdateType::Submitted, handle, *orders_.Lookup(handle), sink);
  return api::Error::Ok;
}

api::Error StateEngine::RejectLocal(api::OrderHandle handle,
                                    const api::UpdateSink& sink) noexcept {
  OrderRecord* record = orders_.Lookup(handle);
  if (record == nullptr) return api::Error::StaleHandle;
  if (record->status != api::OrderStatus::PendingSubmit ||
      record->inflight != api::InflightAction::Submit)
    return api::Error::InvalidTransition;
  record->status = api::OrderStatus::Rejected;
  record->inflight = api::InflightAction::None;
  EmitOrder(api::UpdateType::Rejected, handle, *record, sink);
  return api::Error::Ok;
}

api::Error StateEngine::RequestCancel(api::RequestToken token,
                                      const api::UpdateSink& sink) noexcept {
  OrderRecord* record = orders_.Find(token);
  if (record == nullptr) return api::Error::NotFound;
  if (IsTerminal(record->status) ||
      record->inflight == api::InflightAction::Cancel)
    return api::Error::InvalidTransition;
  record->inflight = api::InflightAction::Cancel;
  EmitOrder(api::UpdateType::CancelRequested, orders_.HandleOf(*record),
            *record, sink);
  return api::Error::Ok;
}

api::Error StateEngine::Resolve(const api::VenueEvent& event,
                                OrderRecord*& record) noexcept {
  record = nullptr;
  const auto merge = [&record](OrderRecord* candidate,
                               api::Error missing) noexcept {
    if (candidate == nullptr)
      return record == nullptr ? missing : api::Error::ProtocolConflict;
    if (record != nullptr && record != candidate)
      return api::Error::ProtocolConflict;
    record = candidate;
    return api::Error::Ok;
  };

  if (event.handle.generation != 0) {
    const api::Error result =
        merge(orders_.Lookup(event.handle), api::Error::StaleHandle);
    if (result != api::Error::Ok) return result;
  }
  if (event.token.sequence != 0) {
    const api::Error result =
        merge(orders_.Find(event.token), api::Error::NotFound);
    if (result != api::Error::Ok) return result;
  }
  if (event.client_order_id.length != 0) {
    if (!HasClientId(event.client_order_id))
      return api::Error::InvalidArgument;
    const api::Error result =
        merge(orders_.Find(event.client_order_id), api::Error::NotFound);
    if (result != api::Error::Ok) return result;
  }

  OrderRecord* venue_record = nullptr;
  if (event.venue_order_id.length != 0) {
    if (!HasVenueId(event.venue_order_id))
      return api::Error::InvalidArgument;
    venue_record = orders_.Find(event.venue_order_id);
    if (venue_record != nullptr) {
      const api::Error result = merge(venue_record, api::Error::NotFound);
      if (result != api::Error::Ok) return result;
    }
  }
  if (record == nullptr) return api::Error::NotFound;
  if (event.venue_order_id.length != 0 && venue_record == nullptr) {
    return orders_.BindVenueId(orders_.HandleOf(*record),
                               event.venue_order_id);
  }
  return api::Error::Ok;
}

api::Error StateEngine::CheckDuplicate(
    api::OrderHandle handle, const api::VenueEvent& event,
    std::size_t& insert_slot) const noexcept {
  if (event.trade_id.length == 0 ||
      event.trade_id.length > event.trade_id.value.size())
    return api::Error::InvalidArgument;
  const std::size_t mask = fill_dedup_.size() - 1;
  std::size_t index = FillHash(handle, event.trade_id) & mask;
  for (std::size_t probe = 0; probe < fill_dedup_.size(); ++probe) {
    const FillDedupEntry& entry = fill_dedup_[index];
    if (entry.state == 0) {
      insert_slot = index;
      return api::Error::Ok;
    }
    if (entry.handle == handle && entry.trade_id == event.trade_id) {
      return entry.quantity == event.fill_quantity.value &&
                     entry.price == event.fill_price.value
                 ? api::Error::Duplicate
                 : api::Error::ProtocolConflict;
    }
    index = (index + 1) & mask;
  }
  return api::Error::CapacityExceeded;
}

void StateEngine::RecordFill(std::size_t slot, api::OrderHandle handle,
                             const api::VenueEvent& event) noexcept {
  fill_dedup_[slot] = {handle, event.trade_id, event.fill_quantity.value,
                       event.fill_price.value, 1};
}

api::Error StateEngine::Apply(const api::VenueEvent& event,
                              const api::UpdateSink& sink) noexcept {
  OrderRecord* record = nullptr;
  const api::Error resolve = Resolve(event, record);
  if (resolve != api::Error::Ok) return resolve;
  const api::OrderHandle handle = orders_.HandleOf(*record);

  switch (event.type) {
    case api::VenueEventType::NewAck:
      if (record->status == api::OrderStatus::Rejected)
        return api::Error::ProtocolConflict;
      {
      bool changed = false;
      if (record->status == api::OrderStatus::PendingSubmit ||
          record->status == api::OrderStatus::Unknown) {
        record->status =
            record->cumulative_quantity == 0
                ? api::OrderStatus::Open
                : (record->remaining_quantity == 0
                       ? api::OrderStatus::Filled
                       : api::OrderStatus::PartiallyFilled);
        changed = true;
      }
      if (record->inflight == api::InflightAction::Submit) {
        record->inflight = api::InflightAction::None;
        changed = true;
      }
      if (record->status == api::OrderStatus::Filled &&
          record->inflight != api::InflightAction::None) {
        record->inflight = api::InflightAction::None;
        changed = true;
      }
      if (!changed) return api::Error::Ok;
      EmitOrder(api::UpdateType::Accepted, handle, *record, sink);
      return api::Error::Ok;
      }

    case api::VenueEventType::NewReject:
      if (record->status == api::OrderStatus::Rejected)
        return api::Error::Ok;
      if (record->status != api::OrderStatus::PendingSubmit ||
          record->cumulative_quantity != 0)
        return api::Error::ProtocolConflict;
      record->status = api::OrderStatus::Rejected;
      record->inflight = api::InflightAction::None;
      EmitOrder(api::UpdateType::Rejected, handle, *record, sink);
      return api::Error::Ok;

    case api::VenueEventType::CancelAck:
      if (record->status == api::OrderStatus::Canceled)
        return api::Error::Ok;
      if (record->inflight != api::InflightAction::Cancel ||
          record->status == api::OrderStatus::Filled)
        return api::Error::InvalidTransition;
      record->status = api::OrderStatus::Canceled;
      record->inflight = api::InflightAction::None;
      EmitOrder(api::UpdateType::Canceled, handle, *record, sink);
      return api::Error::Ok;

    case api::VenueEventType::CancelReject:
      if (record->inflight != api::InflightAction::Cancel) {
        return IsTerminal(record->status) ? api::Error::ProtocolConflict
                                          : api::Error::Ok;
      }
      record->inflight = record->status == api::OrderStatus::PendingSubmit
                             ? api::InflightAction::Submit
                             : api::InflightAction::None;
      EmitOrder(api::UpdateType::CancelRejected, handle, *record, sink);
      return api::Error::Ok;

    case api::VenueEventType::Expire:
      if (record->status == api::OrderStatus::Expired)
        return api::Error::Ok;
      if (IsTerminal(record->status))
        return api::Error::ProtocolConflict;
      record->status = api::OrderStatus::Expired;
      record->inflight = api::InflightAction::None;
      EmitOrder(api::UpdateType::Expired, handle, *record, sink);
      return api::Error::Ok;

    case api::VenueEventType::ReconcileOpen:
      if (record->remaining_quantity == 0 ||
          (IsTerminal(record->status) &&
           record->status != api::OrderStatus::Unknown))
        return api::Error::ProtocolConflict;
      record->status = record->cumulative_quantity == 0
                           ? api::OrderStatus::Open
                           : api::OrderStatus::PartiallyFilled;
      record->inflight = api::InflightAction::None;
      EmitOrder(api::UpdateType::Reconciled, handle, *record, sink);
      return api::Error::Ok;

    case api::VenueEventType::ReconcileTerminal:
      if (!IsTerminal(event.reconciled_status))
        return api::Error::InvalidArgument;
      if (event.reconciled_status == api::OrderStatus::Filled &&
          record->remaining_quantity != 0)
        return api::Error::ProtocolConflict;
      if (IsTerminal(record->status) &&
          record->status != event.reconciled_status)
        return api::Error::ProtocolConflict;
      if (record->status == event.reconciled_status)
        return api::Error::Ok;
      record->status = event.reconciled_status;
      record->inflight = api::InflightAction::None;
      EmitOrder(api::UpdateType::Reconciled, handle, *record, sink);
      return api::Error::Ok;

    case api::VenueEventType::Fill:
      break;
  }

  if (event.fill_quantity.scale != record->request.quantity.scale ||
      event.fill_price.scale != record->request.price.scale)
    return api::Error::InvalidScale;
  if (event.fill_quantity.value <= 0 || event.fill_price.value <= 0)
    return api::Error::InvalidArgument;

  std::size_t fill_slot = 0;
  const api::Error duplicate = CheckDuplicate(handle, event, fill_slot);
  if (duplicate == api::Error::Duplicate) return api::Error::Ok;
  if (duplicate != api::Error::Ok) return duplicate;
  if (event.fill_quantity.value > record->remaining_quantity)
    return api::Error::Overfill;
  if (record->cumulative_quantity >
      std::numeric_limits<std::int64_t>::max() - event.fill_quantity.value)
    return api::Error::ArithmeticOverflow;

#if defined(__SIZEOF_INT128__)
  const WideNotional new_quantity =
      static_cast<WideNotional>(record->cumulative_quantity) +
      event.fill_quantity.value;
  const WideNotional notional =
      record->cumulative_notional +
      static_cast<WideNotional>(event.fill_price.value) *
          event.fill_quantity.value;
  const WideNotional average = notional / new_quantity;
  if (average > std::numeric_limits<std::int64_t>::max() ||
      average < std::numeric_limits<std::int64_t>::min())
    return api::Error::ArithmeticOverflow;
  const std::int64_t new_average = static_cast<std::int64_t>(average);
#else
  if (event.fill_price.value >
          std::numeric_limits<std::int64_t>::max() /
              event.fill_quantity.value)
    return api::Error::ArithmeticOverflow;
  const std::int64_t fill_notional =
      event.fill_price.value * event.fill_quantity.value;
  if (record->cumulative_notional >
      std::numeric_limits<std::int64_t>::max() - fill_notional)
    return api::Error::ArithmeticOverflow;
  const std::int64_t notional =
      record->cumulative_notional + fill_notional;
  const std::int64_t new_average =
      notional /
      (record->cumulative_quantity + event.fill_quantity.value);
#endif

  record->cumulative_quantity += event.fill_quantity.value;
  record->remaining_quantity -= event.fill_quantity.value;
  record->cumulative_notional = notional;
  record->average_price = new_average;
  if (record->status == api::OrderStatus::Open ||
      record->status == api::OrderStatus::PartiallyFilled ||
      record->status == api::OrderStatus::Unknown) {
    record->status = record->remaining_quantity == 0
                         ? api::OrderStatus::Filled
                         : api::OrderStatus::PartiallyFilled;
    if (record->status == api::OrderStatus::Filled)
      record->inflight = api::InflightAction::None;
  }
  RecordFill(fill_slot, handle, event);

  if (sink.on_fill != nullptr) {
    api::FillUpdate update{};
    update.handle = handle;
    update.token = record->request.token;
    update.client_order_id = record->request.client_order_id;
    update.venue_order_id = record->venue_order_id;
    update.trade_id = event.trade_id;
    update.quantity = event.fill_quantity;
    update.price = event.fill_price;
    update.cumulative_quantity = {
        record->cumulative_quantity, record->request.quantity.scale, {}};
    update.remaining_quantity = {
        record->remaining_quantity, record->request.quantity.scale, {}};
    update.average_price = {record->average_price, record->request.price.scale,
                            {}};
    sink.on_fill(sink.context, update);
  }
  return api::Error::Ok;
}

}  // namespace oms
