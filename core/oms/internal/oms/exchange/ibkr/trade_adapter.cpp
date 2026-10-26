#include "oms/exchange/ibkr/trade_adapter.h"

#include <cerrno>
#include <charconv>
#include <cmath>
#include <cstring>
#include <ctime>
#include <exception>
#include <limits>
#include <unistd.h>

namespace oms::exchange::ibkr {
namespace {

AdapterEvent MakeEvent(AdapterEventKind kind) noexcept {
  AdapterEvent event;
  event.kind = kind;
  event.source = {AdapterKind::Ibkr, 0, utils::md::Venue::Ibkr,
                  utils::md::ProductType::Unknown, {}};
  return event;
}

template <std::size_t Capacity, typename Tag>
void copy_id(api::FixedId<Capacity, Tag>& output,
             std::string_view text) noexcept {
  const std::size_t length =
      text.size() < output.value.size() ? text.size() : output.value.size();
  std::memcpy(output.value.data(), text.data(), length);
  output.length = static_cast<std::uint16_t>(length);
}

bool supported_order(const api::NewOrderRequest& request) noexcept {
  if (request.type != api::OrderType::Limit) return false;
  if ((request.flags & api::PostOnly) != 0U) return false;
  return request.time_in_force == api::TimeInForce::IOC ||
         request.time_in_force == api::TimeInForce::GTC ||
         request.time_in_force == api::TimeInForce::GTD;
}

const char* tif_name(api::TimeInForce tif) noexcept {
  switch (tif) {
    case api::TimeInForce::IOC:
      return "IOC";
    case api::TimeInForce::GTD:
      return "GTD";
    case api::TimeInForce::GTC:
      return "GTC";
    case api::TimeInForce::FOK:
      return "FOK";
    case api::TimeInForce::FAK:
      return "FAK";
  }
  return "GTC";
}

bool format_fixed(std::int64_t value, std::uint8_t scale, std::string& out) {
  if (value < 0 || scale > 18) return false;
  std::string digits = std::to_string(value);
  if (scale == 0) {
    out = std::move(digits);
    return true;
  }
  if (digits.size() <= scale)
    digits.insert(0, static_cast<std::size_t>(scale) + 1 - digits.size(), '0');
  digits.insert(digits.size() - scale, 1, '.');
  out = std::move(digits);
  return true;
}

bool rescale_exact(std::int64_t value, std::uint8_t from, std::uint8_t to,
                   std::int64_t& out) noexcept {
  if (from == to) {
    out = value;
    return true;
  }
  if (to > from) {
    std::int64_t scaled = value;
    for (std::uint8_t index = from; index < to; ++index) {
      if (scaled > std::numeric_limits<std::int64_t>::max() / 10 ||
          scaled < std::numeric_limits<std::int64_t>::min() / 10)
        return false;
      scaled *= 10;
    }
    out = scaled;
    return true;
  }
  std::int64_t divisor = 1;
  for (std::uint8_t index = to; index < from; ++index) {
    if (divisor > std::numeric_limits<std::int64_t>::max() / 10) return false;
    divisor *= 10;
  }
  if (divisor == 0 || value % divisor != 0) return false;
  out = value / divisor;
  return true;
}

bool parse_decimal(std::string_view text, std::uint8_t scale,
                   std::int64_t& out) noexcept {
  if (text.empty() || scale > 18) return false;
  const auto dot = text.find('.');
  const std::string_view whole =
      dot == std::string_view::npos ? text : text.substr(0, dot);
  const std::string_view fraction =
      dot == std::string_view::npos ? std::string_view{} : text.substr(dot + 1);
  if (fraction.size() > scale) {
    for (std::size_t index = scale; index < fraction.size(); ++index) {
      if (fraction[index] != '0') return false;
    }
  }
  std::int64_t value = 0;
  if (!whole.empty()) {
    const auto parsed =
        std::from_chars(whole.data(), whole.data() + whole.size(), value);
    if (parsed.ec != std::errc{} || parsed.ptr != whole.data() + whole.size())
      return false;
  }
  std::uint8_t seen = 0;
  for (const char digit : fraction) {
    if (digit < '0' || digit > '9') return false;
    if (seen >= scale) break;
    if (value > std::numeric_limits<std::int64_t>::max() / 10) return false;
    value = value * 10 + (digit - '0');
    ++seen;
  }
  while (seen < scale) {
    if (value > std::numeric_limits<std::int64_t>::max() / 10) return false;
    value *= 10;
    ++seen;
  }
  out = value;
  return true;
}

bool parse_decimal_auto(std::string_view text, std::int64_t& out,
                        std::uint8_t& scale) noexcept {
  if (text.empty()) return false;
  bool negative = false;
  if (text.front() == '-' || text.front() == '+') {
    negative = text.front() == '-';
    text.remove_prefix(1);
  }
  if (text.empty()) return false;
  const auto dot = text.find('.');
  if (dot != std::string_view::npos &&
      text.find('.', dot + 1) != std::string_view::npos)
    return false;
  const auto whole =
      dot == std::string_view::npos ? text : text.substr(0, dot);
  auto fraction =
      dot == std::string_view::npos ? std::string_view{} : text.substr(dot + 1);
  while (!fraction.empty() && fraction.back() == '0')
    fraction.remove_suffix(1);
  if (fraction.size() > 18 || (whole.empty() && fraction.empty()))
    return false;
  std::uint64_t magnitude = 0;
  const auto append = [&magnitude](char digit) noexcept {
    if (digit < '0' || digit > '9') return false;
    const std::uint64_t value = static_cast<std::uint64_t>(digit - '0');
    if (magnitude >
        (std::numeric_limits<std::uint64_t>::max() - value) / 10)
      return false;
    magnitude = magnitude * 10 + value;
    return true;
  };
  for (const char digit : whole)
    if (!append(digit)) return false;
  for (const char digit : fraction)
    if (!append(digit)) return false;
  const std::uint64_t limit =
      negative
          ? static_cast<std::uint64_t>(
                std::numeric_limits<std::int64_t>::max()) +
                1
          : static_cast<std::uint64_t>(
                std::numeric_limits<std::int64_t>::max());
  if (magnitude > limit) return false;
  out = negative
            ? (magnitude == limit
                   ? std::numeric_limits<std::int64_t>::min()
                   : -static_cast<std::int64_t>(magnitude))
            : static_cast<std::int64_t>(magnitude);
  scale = static_cast<std::uint8_t>(fraction.size());
  return true;
}

bool double_to_fixed(double value, std::uint8_t scale, std::int64_t& out) noexcept {
  if (!(value > 0)) return false;
  long double scaled = value;
  for (std::uint8_t index = 0; index < scale; ++index) scaled *= 10.0L;
  const long double nearest = std::round(scaled);
  if (std::fabs(scaled - nearest) > 1e-4L) return false;
  if (nearest > static_cast<long double>(std::numeric_limits<std::int64_t>::max()))
    return false;
  out = static_cast<std::int64_t>(nearest);
  return true;
}

utils::md::IbkrSecurityType security_of(std::string_view text) noexcept {
  if (text == "STK") return utils::md::IbkrSecurityType::Stock;
  if (text == "CMDTY") return utils::md::IbkrSecurityType::Commodity;
  if (text == "CASH") return utils::md::IbkrSecurityType::Cash;
  return utils::md::IbkrSecurityType::Unknown;
}

bool same_text(std::string_view left, const api::ClientOrderId& right) noexcept {
  return left.size() == right.length &&
         (left.empty() ||
          std::memcmp(left.data(), right.value.data(), left.size()) == 0);
}

std::string utc_gtc(std::uint64_t expire_ns) {
  if (expire_ns == 0) return {};
  const std::time_t seconds = static_cast<std::time_t>(expire_ns / 1000000000ULL);
  std::tm utc{};
  if (gmtime_r(&seconds, &utc) == nullptr) return {};
  char buffer[32];
  if (std::strftime(buffer, sizeof(buffer), "%Y%m%d %H:%M:%S UTC", &utc) == 0)
    return {};
  return buffer;
}

void copy_field(char* destination, std::size_t capacity, std::string_view value) {
  const std::size_t count =
      value.size() < capacity - 1 ? value.size() : capacity - 1;
  if (count != 0) std::memcpy(destination, value.data(), count);
  destination[count] = '\0';
}

}  // namespace

IbkrTradeAdapter::IbkrTradeAdapter(int client_id, std::string account,
                                   std::string_view strategy_instance)
    : session_(client_id, std::move(account)) {
  if (!strategy_instance.empty()) {
    strategy_order_ref_prefix_ = "tf-";
    strategy_order_ref_prefix_.append(strategy_instance);
    strategy_order_ref_prefix_.push_back('-');
  }
  session_.set_listener(&IbkrTradeAdapter::on_callback, this);
}

void IbkrTradeAdapter::attach_client(ExecClient* client) noexcept {
  client_ = client;
}

void IbkrTradeAdapter::configure_gateway(std::string host, int port,
                                         std::size_t order_capacity,
                                         std::size_t exec_capacity,
                                         std::size_t query_waiter_capacity) noexcept {
  host_ = std::move(host);
  port_ = port;
  session_.set_capacities(order_capacity, exec_capacity);
  if (query_waiter_capacity > 0 && query_waiter_capacity < order_waiters_.size())
    query_waiter_capacity_ = query_waiter_capacity;
}

AdapterIdentity IbkrTradeAdapter::identity() const noexcept {
  return {AdapterKind::Ibkr, 0, utils::md::Venue::Ibkr,
          utils::md::ProductType::Unknown, {}};
}

AdapterStatus IbkrTradeAdapter::status() const noexcept {
  return session_.status();
}

AdapterCapabilities IbkrTradeAdapter::capabilities() const noexcept {
  AdapterCapabilities capabilities;
  capabilities.bits = static_cast<std::uint64_t>(AdapterCapability::Limit) |
                      static_cast<std::uint64_t>(AdapterCapability::TifGtc) |
                      static_cast<std::uint64_t>(AdapterCapability::TifGtd) |
                      static_cast<std::uint64_t>(AdapterCapability::TifIoc) |
                      static_cast<std::uint64_t>(
                          AdapterCapability::ReconcileOpenOrders);
  return capabilities;
}

AdapterResult IbkrTradeAdapter::reserve_command(
    AdapterCommandKind, AdapterReservation& reservation) noexcept {
  if (session_.status() == AdapterStatus::Failed || reserved_)
    return AdapterResult::NotReady;
  reserved_ = true;
  reservation.adapter = AdapterKind::Ibkr;
  reservation.slot = 0;
  reservation.generation = generation_;
  return AdapterResult::Ok;
}

void IbkrTradeAdapter::cancel_reservation(AdapterReservation reservation) noexcept {
  if (reservation.generation == generation_) reserved_ = false;
}

bool IbkrTradeAdapter::queue_reject(const AdapterPlaceCommand& command) noexcept {
  if (pending_count_ >= pending_.size()) return false;
  AdapterEvent event = MakeEvent(AdapterEventKind::Venue);
  event.venue.type = api::VenueEventType::NewReject;
  event.venue.handle = command.handle;
  event.venue.token = command.request.token;
  event.venue.client_order_id = command.request.client_order_id;
  event.venue.reconciled_status = api::OrderStatus::Rejected;
  pending_[pending_count_++] = event;
  return true;
}

void IbkrTradeAdapter::push_event(const AdapterEvent& event) noexcept {
  if (pending_count_ < pending_.size()) pending_[pending_count_++] = event;
  else session_.fail();
}

AdapterResult IbkrTradeAdapter::register_instrument(
    api::InstrumentId instrument_id,
    const api::ResolvedInstrument& routing) noexcept {
  const AdapterResult result = session_.register_instrument(instrument_id, routing);
  if (result != AdapterResult::Ok) return result;
  Contract* contract = session_.find_contract(routing.ibkr.con_id);
  if (contract == nullptr) return AdapterResult::Failed;
  if (contract->state == InstrumentExecutionState::Ready ||
      contract->state == InstrumentExecutionState::RetryPending ||
      contract->details_sent)
    return AdapterResult::Ok;
  if (client_ == nullptr || !client_->connected()) return AdapterResult::Ok;
  const int request_id = next_request_id_++;
  if (!client_->request_contract_details(request_id, contract->con_id)) {
    session_.abandon_resolving(instrument_id, contract->con_id);
    return AdapterResult::NotReady;
  }
  contract->details_sent = true;
  contract->details_request_id = request_id;
  contract->detail_count = 0;
  contract->request_generation = session_.connection_generation();
  contract->deadline_ns = now_ns_ + 5000000000ULL;
  ++contract_requests_;
  return AdapterResult::Ok;
}

AdapterResult IbkrTradeAdapter::retire_instrument(
    api::InstrumentId instrument_id,
    const api::ResolvedInstrument& routing) noexcept {
  return session_.retire_instrument(instrument_id, routing);
}

bool IbkrTradeAdapter::instrument_ready(
    const api::ResolvedInstrument& routing) const noexcept {
  return session_.execution_ready(routing.ibkr.con_id);
}

AdapterResult IbkrTradeAdapter::commit_place(
    AdapterReservation reservation, const AdapterPlaceCommand& command) noexcept {
  if (reservation.generation != generation_ || !reserved_)
    return AdapterResult::StaleReservation;
  reserved_ = false;
  ++generation_;
  if (!supported_order(command.request))
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  if (!session_.new_orders_allowed() || client_ == nullptr || !client_->connected())
    return AdapterResult::NotReady;
  if (command.routing.kind != api::ExecutionRouteKind::Ibkr ||
      !session_.execution_ready(command.routing.ibkr.con_id))
    return AdapterResult::NotReady;
  const Contract* contract = session_.find_contract(command.routing.ibkr.con_id);
  if (contract == nullptr) return AdapterResult::NotReady;
  if (command.request.time_in_force == api::TimeInForce::IOC &&
      contract->ioc_capability == IocCapability::Unsupported)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  if (command.request.client_order_id.length == 0 ||
      command.request.client_order_id.length > kOrderRefCapacity)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  for (const TrackedOrder& existing : session_.orders()) {
    if (existing.occupied &&
        same_text(std::string_view(existing.order_ref.value.data(),
                                   existing.order_ref.length),
                  command.request.client_order_id))
      return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  }
  std::int64_t quantity = 0;
  std::int64_t price = 0;
  if (!rescale_exact(command.request.quantity.value,
                     command.request.quantity.scale, contract->quantity_scale,
                     quantity) ||
      !rescale_exact(command.request.price.value, command.request.price.scale,
                     contract->price_scale, price) ||
      quantity <= 0 || price <= 0)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  if (contract->min_size > 0 && quantity < contract->min_size)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  if (contract->size_increment > 0 && quantity % contract->size_increment != 0)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  if (contract->min_tick > 0 && price % contract->min_tick != 0)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  std::string quantity_text;
  std::string price_text;
  if (!format_fixed(quantity, contract->quantity_scale, quantity_text) ||
      !format_fixed(price, contract->price_scale, price_text))
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  const int order_id = session_.allocate_order_id();
  if (order_id < 0) return AdapterResult::Failed;
  std::string order_ref = strategy_order_ref_prefix_;
  order_ref.append(command.request.client_order_id.value.data(),
                   command.request.client_order_id.length);
  if (order_ref.size() >= sizeof(ExecEvent::order_ref))
    return queue_reject(command) ? AdapterResult::Ok
                                 : AdapterResult::WouldBlock;
  const std::string good_till =
      command.request.time_in_force == api::TimeInForce::GTD
          ? utc_gtc(command.request.expire_time_ns)
          : std::string{};
  if (command.request.time_in_force == api::TimeInForce::GTD && good_till.empty())
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  double boundary_price = 0;
  try {
    boundary_price = std::stod(price_text);
  } catch (const std::exception&) {
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  }
  std::int64_t round_trip = 0;
  if (!double_to_fixed(boundary_price, contract->price_scale, round_trip) ||
      round_trip != price)
    return queue_reject(command) ? AdapterResult::Ok : AdapterResult::WouldBlock;
  TrackedOrder tracked;
  tracked.client_id = session_.client_id();
  tracked.order_id = order_id;
  tracked.handle = command.handle;
  tracked.token = command.request.token;
  tracked.client_order_id = command.request.client_order_id;
  copy_id(tracked.order_ref, order_ref);
  tracked.instrument_id = command.request.instrument_id;
  tracked.con_id = contract->con_id;
  tracked.quantity = quantity;
  tracked.quantity_scale = contract->quantity_scale;
  tracked.price_scale = contract->price_scale;
  tracked.tif = command.request.time_in_force;
  tracked.ours = true;
  tracked.active = true;
  if (session_.insert_order(tracked) == nullptr) return AdapterResult::Failed;
  const char* action = command.request.side == api::Side::Buy ? "BUY" : "SELL";
  if (!client_->place_limit(order_id, contract->con_id, action, quantity_text,
                            price_text, tif_name(command.request.time_in_force),
                            session_.account(), order_ref, good_till)) {
    if (TrackedOrder* placed = session_.find_order(session_.client_id(), order_id);
        placed != nullptr)
      placed->occupied = false;
    return AdapterResult::NotReady;
  }
  return AdapterResult::Ok;
}

AdapterResult IbkrTradeAdapter::commit_cancel(
    AdapterReservation reservation, const AdapterCancelCommand& command) noexcept {
  if (reservation.generation != generation_ || !reserved_)
    return AdapterResult::StaleReservation;
  reserved_ = false;
  ++generation_;
  if (client_ == nullptr || !client_->connected()) return AdapterResult::NotReady;
  for (TrackedOrder& order : session_.orders()) {
    if (!order.occupied || order.handle != command.request.handle) continue;
    if (!client_->cancel_order(order.order_id)) return AdapterResult::NotReady;
    return AdapterResult::Ok;
  }
  return AdapterResult::NotReady;
}

AdapterResult IbkrTradeAdapter::commit_cancel_venue(
    AdapterReservation reservation,
    const AdapterCancelVenueCommand& command) noexcept {
  if (reservation.generation != generation_ || !reserved_)
    return AdapterResult::StaleReservation;
  reserved_ = false;
  ++generation_;
  if (client_ == nullptr || !client_->connected())
    return AdapterResult::NotReady;
  const std::string_view order_ref(
      command.request.client_order_id.value.data(),
      command.request.client_order_id.length);
  if (strategy_order_ref_prefix_.empty() ||
      order_ref.size() <= strategy_order_ref_prefix_.size() ||
      !order_ref.starts_with(strategy_order_ref_prefix_) ||
      command.request.routing.kind != api::ExecutionRouteKind::Ibkr)
    return AdapterResult::InvalidArgument;
  const Contract* contract =
      session_.find_contract(command.request.routing.ibkr.con_id);
  if (contract == nullptr ||
      contract->instrument_id != command.request.instrument_id)
    return AdapterResult::InvalidArgument;
  const std::string_view venue_id(
      command.request.venue_order_id.value.data(),
      command.request.venue_order_id.length);
  const std::size_t colon = venue_id.find(':');
  if (colon == std::string_view::npos ||
      venue_id.find(':', colon + 1) != std::string_view::npos)
    return AdapterResult::InvalidArgument;
  int client_id = -1;
  int order_id = -1;
  const auto client_result =
      std::from_chars(venue_id.data(), venue_id.data() + colon, client_id);
  const auto order_result =
      std::from_chars(venue_id.data() + colon + 1,
                      venue_id.data() + venue_id.size(), order_id);
  if (client_result.ec != std::errc{} ||
      client_result.ptr != venue_id.data() + colon ||
      order_result.ec != std::errc{} ||
      order_result.ptr != venue_id.data() + venue_id.size() ||
      client_id < 0 || order_id < 0)
    return AdapterResult::InvalidArgument;
  return client_->cancel_order(order_id) ? AdapterResult::Ok
                                         : AdapterResult::NotReady;
}

bool IbkrTradeAdapter::enqueue_query(const QueryWaiter& waiter) noexcept {
  std::size_t* count = nullptr;
  QueryWaiter* slots = nullptr;
  std::size_t capacity = 0;
  if (waiter.kind == api::QueryKind::OpenOrders) {
    count = &order_waiter_count_;
    slots = order_waiters_.data();
    capacity = order_waiters_.size();
  } else if (waiter.kind == api::QueryKind::Positions) {
    count = &position_waiter_count_;
    slots = position_waiters_.data();
    capacity = position_waiters_.size();
  } else {
    count = &balance_waiter_count_;
    slots = balance_waiters_.data();
    capacity = balance_waiters_.size();
  }
  if (*count >= query_waiter_capacity_ || *count >= capacity) return false;
  slots[(*count)++] = waiter;
  return true;
}

AdapterResult IbkrTradeAdapter::query_open_orders(
    const AdapterQueryRequest& request, const AdapterEventSink& sink) noexcept {
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  if (client_ == nullptr || !client_->connected()) {
    AdapterEvent complete = MakeEvent(AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = request.token;
    complete.query_complete.account_id = request.request.account_id;
    complete.query_complete.kind = api::QueryKind::OpenOrders;
    complete.query_complete.error = api::Error::NotReady;
    return sink.on_event(sink.context, complete);
  }
  const bool join_incomplete_stream =
      orders_in_flight_ && order_waiter_count_ == 0;
  QueryWaiter waiter{request.token, request.request.account_id,
                     api::QueryKind::OpenOrders,
                     request.request.instrument_id,
                     request.request.scope ==
                             api::QueryScope::SingleInstrument
                         ? request.request.instrument.ibkr.con_id
                         : 0};
  if (!enqueue_query(waiter)) return AdapterResult::WouldBlock;
  if (join_incomplete_stream) orders_requery_needed_ = true;
  if (!orders_in_flight_) {
    if (!client_->request_all_open_orders()) {
      --order_waiter_count_;
      return AdapterResult::NotReady;
    }
    orders_in_flight_ = true;
  }
  return AdapterResult::Ok;
}

AdapterResult IbkrTradeAdapter::query_positions(
    const AdapterQueryRequest& request, const AdapterEventSink& sink) noexcept {
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  if (client_ == nullptr || !client_->connected()) {
    AdapterEvent complete = MakeEvent(AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = request.token;
    complete.query_complete.account_id = request.request.account_id;
    complete.query_complete.kind = api::QueryKind::Positions;
    complete.query_complete.error = api::Error::NotReady;
    return sink.on_event(sink.context, complete);
  }
  const bool join_incomplete_stream =
      positions_in_flight_ && position_waiter_count_ == 0;
  QueryWaiter waiter{request.token, request.request.account_id,
                     api::QueryKind::Positions,
                     request.request.instrument_id,
                     request.request.scope ==
                             api::QueryScope::SingleInstrument
                         ? request.request.instrument.ibkr.con_id
                         : 0};
  if (!enqueue_query(waiter)) return AdapterResult::WouldBlock;
  if (join_incomplete_stream) positions_requery_needed_ = true;
  if (!positions_in_flight_) {
    if (!client_->request_positions()) {
      --position_waiter_count_;
      return AdapterResult::NotReady;
    }
    positions_in_flight_ = true;
  }
  return AdapterResult::Ok;
}

AdapterResult IbkrTradeAdapter::query_balances(
    const AdapterQueryRequest& request, const AdapterEventSink& sink) noexcept {
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  if (request.request.scope != api::QueryScope::All)
    return AdapterResult::InvalidArgument;
  if (client_ == nullptr || !client_->connected()) {
    AdapterEvent complete = MakeEvent(AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = request.token;
    complete.query_complete.account_id = request.request.account_id;
    complete.query_complete.kind = api::QueryKind::Balances;
    complete.query_complete.error = api::Error::NotReady;
    return sink.on_event(sink.context, complete);
  }
  QueryWaiter waiter{request.token, request.request.account_id,
                     api::QueryKind::Balances, 0, 0};
  if (!enqueue_query(waiter)) return AdapterResult::WouldBlock;
  if (!balances_in_flight_) {
    balance_value_count_ = 0;
    balance_request_id_ = next_request_id_++;
    if (!client_->request_account_updates(
            balance_request_id_, true, session_.account())) {
      --balance_waiter_count_;
      balance_request_id_ = 0;
      return AdapterResult::NotReady;
    }
    balances_in_flight_ = true;
  }
  return AdapterResult::Ok;
}

void IbkrTradeAdapter::finish_queries(api::QueryKind kind) noexcept {
  auto& count = kind == api::QueryKind::OpenOrders ? order_waiter_count_
                                                   : position_waiter_count_;
  auto& slots = kind == api::QueryKind::OpenOrders ? order_waiters_
                                                   : position_waiters_;
  for (std::size_t index = 0; index < count; ++index) {
    AdapterEvent complete = MakeEvent(AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = slots[index].token;
    complete.query_complete.account_id = slots[index].account_id;
    complete.query_complete.kind = kind;
    push_event(complete);
  }
  count = 0;
  if (kind == api::QueryKind::OpenOrders) {
    orders_in_flight_ = false;
    orders_done_ = true;
  } else {
    positions_in_flight_ = false;
    positions_done_ = true;
  }
  complete_reconcile_if_ready();
}

void IbkrTradeAdapter::finish_balance_queries() noexcept {
  for (std::size_t waiter_index = 0; waiter_index < balance_waiter_count_;
       ++waiter_index) {
    const auto& waiter = balance_waiters_[waiter_index];
    for (std::size_t value_index = 0; value_index < balance_value_count_;
         ++value_index) {
      const auto& value = balance_values_[value_index];
      if (!value.has_total || !value.has_available) continue;
      std::int64_t total = 0;
      std::int64_t available = 0;
      std::uint8_t total_scale = 0;
      std::uint8_t available_scale = 0;
      if (!parse_decimal_auto(value.total.data(), total, total_scale) ||
          !parse_decimal_auto(value.available.data(), available,
                              available_scale))
        continue;
      const std::uint8_t scale =
          total_scale > available_scale ? total_scale : available_scale;
      if (!rescale_exact(total, total_scale, scale, total) ||
          !rescale_exact(available, available_scale, scale, available))
        continue;
      if ((available < 0 &&
           total > std::numeric_limits<std::int64_t>::max() + available) ||
          (available > 0 &&
           total < std::numeric_limits<std::int64_t>::min() + available))
        continue;
      AdapterEvent snapshot =
          MakeEvent(AdapterEventKind::BalanceSnapshot);
      snapshot.balance.query_token = waiter.token;
      snapshot.balance.account_id = waiter.account_id;
      snapshot.balance.asset_or_currency = value.currency;
      snapshot.balance.total_quantity = total;
      snapshot.balance.available_quantity = available;
      snapshot.balance.held_quantity = total - available;
      snapshot.balance.scale = scale;
      push_event(snapshot);
    }
    AdapterEvent complete = MakeEvent(AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = waiter.token;
    complete.query_complete.account_id = waiter.account_id;
    complete.query_complete.kind = api::QueryKind::Balances;
    push_event(complete);
  }
  balance_waiter_count_ = 0;
  balance_value_count_ = 0;
  balances_in_flight_ = false;
  balance_request_id_ = 0;
}

void IbkrTradeAdapter::request_registered_contracts() noexcept {
  if (client_ == nullptr || !client_->connected()) return;
  session_.for_each_contract([&](Contract& entry) {
    if (entry.state != InstrumentExecutionState::Resolving || entry.details_sent)
      return;
    const int request_id = next_request_id_++;
    if (!client_->request_contract_details(request_id, entry.con_id)) return;
    entry.details_sent = true;
    entry.details_request_id = request_id;
    entry.detail_count = 0;
    entry.request_generation = session_.connection_generation();
    entry.deadline_ns = now_ns_ + 5000000000ULL;
    ++contract_requests_;
  });
}

void IbkrTradeAdapter::start_reconcile() noexcept {
  orders_done_ = false;
  positions_done_ = false;
  executions_done_ = false;
  reconcile_emitted_ = false;
  if (client_ == nullptr || !client_->connected()) return;
  if (!orders_in_flight_ && client_->request_all_open_orders())
    orders_in_flight_ = true;
  if (!positions_in_flight_ && client_->request_positions())
    positions_in_flight_ = true;
  if (!executions_in_flight_ && client_->request_executions(session_.account()))
    executions_in_flight_ = true;
  if (orders_in_flight_ || positions_in_flight_ || executions_in_flight_)
    query_deadline_ns_ = now_ns_ + 5000000000ULL;
  session_.for_each_contract([&](Contract& entry) {
    if (entry.state != InstrumentExecutionState::RetryPending) return;
    const int request_id = next_request_id_++;
    if (client_->request_contract_details(request_id, entry.con_id)) {
      entry.details_sent = true;
      entry.details_request_id = request_id;
      entry.detail_count = 0;
      entry.state = InstrumentExecutionState::Resolving;
      entry.request_generation = session_.connection_generation();
      entry.deadline_ns = now_ns_ + 5000000000ULL;
    }
  });
}

void IbkrTradeAdapter::complete_reconcile_if_ready() noexcept {
  if (reconcile_emitted_ || !orders_done_ || !positions_done_ ||
      !executions_done_)
    return;
  if (session_.status() == AdapterStatus::Reconciling &&
      !session_.confirm_reconnect())
    return;
  if (session_.status() != AdapterStatus::Ready) return;
  AdapterEvent complete = MakeEvent(AdapterEventKind::ReconcileComplete);
  complete.reconcile.generation = generation_;
  complete.reconcile.result = AdapterResult::Ok;
  push_event(complete);
  reconcile_emitted_ = true;
}

void IbkrTradeAdapter::handle_event(const ExecEvent& event) noexcept {
  if (event.kind == ExecEvent::Kind::OpenOrder ||
      event.kind == ExecEvent::Kind::OrderStatus ||
      event.kind == ExecEvent::Kind::ExecDetails) {
    if (event.order_id > 0) session_.observe_order_id(event.order_id);
  }
  switch (event.kind) {
    case ExecEvent::Kind::NextValidId:
      session_.note_next_valid_id(event.order_id);
      break;
    case ExecEvent::Kind::ConnectionClosed:
      session_.note_connected(false);
      session_.bump_connection_generation();
      if (client_ != nullptr)
        client_->set_generation(session_.connection_generation());
      connect_started_ = false;
      session_.for_each_contract([&](Contract& contract) {
        if (contract.state == InstrumentExecutionState::Resolving)
          contract.state = InstrumentExecutionState::RetryPending;
      });
      break;
    case ExecEvent::Kind::Error: {
      Callback callback;
      callback.kind = Callback::Kind::Error;
      callback.code = event.error_code;
      callback.order_id = event.order_id;
      (void)session_.push(callback);
      session_.service();
      if (event.error_code == 201 && event.order_id > 0) {
        if (TrackedOrder* order =
                session_.find_order(session_.client_id(), event.order_id);
            order != nullptr && order->tif == api::TimeInForce::IOC)
          session_.note_ioc_unsupported(order->con_id);
      }
      break;
    }
    case ExecEvent::Kind::ContractDetails: {
      Contract* contract = nullptr;
      session_.for_each_contract([&](Contract& entry) {
        if (event.request_id != 0 && entry.details_request_id == event.request_id)
          contract = &entry;
      });
      if (contract == nullptr) contract = session_.find_contract(event.con_id);
      if (contract == nullptr) break;
      if (event.generation != 0 &&
          event.generation != contract->request_generation) {
        if (contract->state == InstrumentExecutionState::Resolving)
          contract->state = InstrumentExecutionState::RetryPending;
        break;
      }
      ++contract->detail_count;
      if (contract->detail_count > 1 ||
          (event.con_id != 0 && event.con_id != contract->con_id)) {
        contract->multiple_details = true;
        contract->unique = false;
        break;
      }
      const auto reported_security = security_of(event.sec_type);
      const bool usd = std::strcmp(event.currency, "USD") == 0;
      std::int64_t tick = 0;
      std::int64_t min_size = 0;
      std::int64_t increment = 0;
      std::int64_t multiplier = 1;
      std::uint8_t multiplier_scale = 0;
      const bool tick_ok =
          double_to_fixed(event.min_tick, contract->price_scale, tick);
      const bool min_ok =
          event.min_size[0] == '\0' ||
          parse_decimal(event.min_size, contract->quantity_scale, min_size);
      const bool step_ok =
          event.size_increment[0] == '\0' ||
          parse_decimal(event.size_increment, contract->quantity_scale,
                        increment);
      const std::string_view multiplier_text = event.multiplier;
      bool multiplier_present = false;
      if (multiplier_text.empty()) {
        multiplier_present = reported_security != utils::md::IbkrSecurityType::Commodity;
        multiplier = 1;
        multiplier_scale = 0;
      } else {
        const auto dot = multiplier_text.find('.');
        const std::size_t fraction =
            dot == std::string_view::npos ? 0 : multiplier_text.size() - dot - 1;
        multiplier_scale = fraction > 18 ? static_cast<std::uint8_t>(255)
                                         : static_cast<std::uint8_t>(fraction);
        multiplier_present = multiplier_scale <= 18 &&
                             parse_decimal(multiplier_text, multiplier_scale,
                                           multiplier);
      }
      const bool identity_ok =
          usd && reported_security == contract->security && tick_ok && min_ok &&
          step_ok && multiplier_present &&
          (contract->catalog_tick == 0 || contract->catalog_tick == tick) &&
          (contract->catalog_lot == 0 || contract->catalog_lot == increment) &&
          (contract->catalog_multiplier == 0 ||
           (contract->catalog_multiplier == multiplier &&
            contract->catalog_multiplier_scale == multiplier_scale));
      contract->min_tick = tick_ok ? tick : 0;
      contract->min_size = min_ok ? min_size : 0;
      contract->size_increment = step_ok ? increment : 0;
      contract->multiplier = multiplier;
      contract->multiplier_scale = multiplier_present ? multiplier_scale : 0;
      contract->multiplier_present = multiplier_present;
      contract->units_per_native = multiplier_scale == 0 ? multiplier : 0;
      contract->unique = identity_ok;
      break;
    }
    case ExecEvent::Kind::ContractDetailsEnd: {
      Contract* contract = nullptr;
      session_.for_each_contract([&](Contract& entry) {
        if (event.request_id != 0 && entry.details_request_id == event.request_id)
          contract = &entry;
      });
      if (contract == nullptr && detail_con_id_ != 0)
        contract = session_.find_contract(detail_con_id_);
      if (contract == nullptr) break;
      if (event.generation != 0 &&
          event.generation != contract->request_generation) {
        if (contract->state == InstrumentExecutionState::Resolving)
          contract->state = InstrumentExecutionState::RetryPending;
        break;
      }
      if (contract->detail_count == 1 && contract->unique &&
          contract->multiplier_present && !contract->multiple_details &&
          contract->security != utils::md::IbkrSecurityType::Unknown)
        contract->state = InstrumentExecutionState::Ready;
      else
        contract->state = InstrumentExecutionState::Rejected;
      break;
    }
    case ExecEvent::Kind::OpenOrder: {
      const bool ours = event.client_id == session_.client_id();
      TrackedOrder* order = session_.find_order(event.client_id, event.order_id);
      const bool tracked =
          order != nullptr && same_text(event.order_ref, order->order_ref);
      const bool owned_external =
          order == nullptr && !strategy_order_ref_prefix_.empty() &&
          std::string_view(event.order_ref).size() >
              strategy_order_ref_prefix_.size() &&
          std::string_view(event.order_ref)
              .starts_with(strategy_order_ref_prefix_);
      const Contract* contract = session_.find_contract(event.con_id);
      if (!orders_requery_needed_ &&
          (event.account[0] == '\0' ||
           std::strcmp(event.account, session_.account().c_str()) == 0)) {
        for (std::size_t index = 0; index < order_waiter_count_; ++index) {
          const QueryWaiter& waiter = order_waiters_[index];
          if (waiter.con_id != 0 && waiter.con_id != event.con_id) continue;
          AdapterEvent snapshot =
              MakeEvent(AdapterEventKind::OpenOrderSnapshot);
          snapshot.open_order.query_token = waiter.token;
          snapshot.open_order.account_id = waiter.account_id;
          snapshot.open_order.instrument_id =
              waiter.instrument_id != 0
                  ? waiter.instrument_id
                  : (contract == nullptr ? 0 : contract->instrument_id);
          if (snapshot.open_order.instrument_id == 0)
            snapshot.open_order.reserved |= api::SnapshotUnmapped;
          if (owned_external)
            snapshot.open_order.reserved |= api::SnapshotOwnedExternal;
          else if (tracked) {
            snapshot.open_order.reserved |= api::SnapshotTracked;
            snapshot.open_order.tracked_token = order->token;
          } else if (!tracked)
            snapshot.open_order.reserved |= api::SnapshotUnmapped;
          copy_id(snapshot.open_order.client_order_id, event.order_ref);
          const std::string venue_id =
              std::to_string(event.client_id) + ":" +
              std::to_string(event.order_id);
          copy_id(snapshot.open_order.venue_order_id, venue_id);
          snapshot.open_order.side =
              std::strcmp(event.action, "BUY") == 0 ? api::Side::Buy
                                                    : api::Side::Sell;
          snapshot.open_order.status = api::OrderStatus::Open;
          if (contract != nullptr) {
            std::int64_t quantity = 0;
            std::int64_t price = 0;
            if (parse_decimal(event.quantity, contract->quantity_scale,
                              quantity) &&
                double_to_fixed(event.limit_price, contract->price_scale,
                                price)) {
              snapshot.open_order.quantity = {
                  quantity, contract->quantity_scale, {}};
              snapshot.open_order.price = {
                  price, contract->price_scale, {}};
              snapshot.open_order.remaining_quantity =
                  snapshot.open_order.quantity;
            } else {
              snapshot.open_order.reserved |= api::SnapshotUnmapped;
            }
          }
          push_event(snapshot);
        }
      }
      if (!ours) {
        if (!owned_external) session_.mark_unknown_order();
        break;
      }
      if (!tracked) {
        if (order != nullptr) {
          session_.fail();
        } else if (!owned_external) {
          session_.mark_unknown_order();
        }
        // A prior process can own an order with this API client id. The query
        // snapshot above carries orderRef so the strategy can classify it.
        break;
      }
      if (event.perm_id != 0 && order->perm_id != 0 &&
          order->perm_id != event.perm_id) {
        session_.fail();
        break;
      }
      if (event.perm_id != 0) order->perm_id = event.perm_id;
      AdapterEvent ack = MakeEvent(AdapterEventKind::Venue);
      ack.venue.type = api::VenueEventType::NewAck;
      ack.venue.handle = order->handle;
      ack.venue.token = order->token;
      ack.venue.client_order_id = order->client_order_id;
      push_event(ack);
      break;
    }
    case ExecEvent::Kind::OpenOrderEnd:
      if (orders_requery_needed_) {
        orders_requery_needed_ = false;
        if (client_ == nullptr || !client_->request_all_open_orders()) {
          session_.note_query_timeout();
          orders_in_flight_ = false;
        }
      } else {
        finish_queries(api::QueryKind::OpenOrders);
      }
      break;
    case ExecEvent::Kind::OrderStatus: {
      TrackedOrder* order = session_.find_order(event.client_id, event.order_id);
      if (order == nullptr) break;
      if (event.perm_id != 0 && order->perm_id != 0 &&
          order->perm_id != event.perm_id) {
        session_.fail();
        break;
      }
      const std::string_view status = event.status;
      if (status == "Submitted" || status == "PreSubmitted") {
        AdapterEvent ack = MakeEvent(AdapterEventKind::Venue);
        ack.venue.type = api::VenueEventType::NewAck;
        ack.venue.handle = order->handle;
        ack.venue.token = order->token;
        ack.venue.client_order_id = order->client_order_id;
        push_event(ack);
      } else if (status == "Cancelled" || status == "ApiCancelled") {
        AdapterEvent ack = MakeEvent(AdapterEventKind::Venue);
        ack.venue.type = api::VenueEventType::CancelAck;
        ack.venue.handle = order->handle;
        ack.venue.token = order->token;
        ack.venue.client_order_id = order->client_order_id;
        order->active = false;
        order->occupied = false;
        push_event(ack);
      } else if (status == "Filled" && !order->terminal_sent &&
                 order->filled >= order->quantity && order->quantity > 0) {
        AdapterEvent terminal = MakeEvent(AdapterEventKind::Venue);
        terminal.venue.type = api::VenueEventType::ReconcileTerminal;
        terminal.venue.reconciled_status = api::OrderStatus::Filled;
        terminal.venue.handle = order->handle;
        terminal.venue.token = order->token;
        terminal.venue.client_order_id = order->client_order_id;
        order->active = false;
        order->occupied = false;
        order->terminal_sent = true;
        push_event(terminal);
      }
      break;
    }
    case ExecEvent::Kind::ExecDetails: {
      if (event.client_id != 0 && event.client_id != session_.client_id()) break;
      if (!session_.remember_exec(event.exec_id)) break;
      TrackedOrder* order =
          session_.find_order(event.client_id == 0 ? session_.client_id()
                                                   : event.client_id,
                              event.order_id);
      if (order == nullptr) {
        const Contract* contract = session_.find_contract(event.con_id);
        if (contract != nullptr) {
          std::int64_t quantity = 0;
          std::int64_t price = 0;
          if (parse_decimal(event.quantity, contract->quantity_scale,
                            quantity) &&
              double_to_fixed(event.limit_price, contract->price_scale,
                              price)) {
            AdapterEvent unmatched = MakeEvent(AdapterEventKind::Venue);
            unmatched.venue.type = api::VenueEventType::Fill;
            unmatched.venue.fill_quantity = {
                quantity, contract->quantity_scale, {}};
            unmatched.venue.fill_price = {
                price, contract->price_scale, {}};
            copy_field(unmatched.venue.trade_id.value.data(),
                       unmatched.venue.trade_id.value.size(),
                       event.exec_id);
            unmatched.venue.trade_id.length =
                static_cast<std::uint16_t>(
                    std::char_traits<char>::length(event.exec_id));
            push_event(unmatched);
          }
        }
        break;
      }
      if (event.perm_id != 0 && order->perm_id != 0 &&
          event.perm_id != order->perm_id) {
        session_.fail();
        break;
      }
      std::int64_t quantity = 0;
      std::int64_t price = 0;
      if (!parse_decimal(event.quantity, order->quantity_scale, quantity) ||
          !double_to_fixed(event.limit_price, order->price_scale, price)) {
        session_.fail();
        break;
      }
      order->filled += quantity;
      AdapterEvent fill = MakeEvent(AdapterEventKind::Venue);
      fill.venue.type = api::VenueEventType::Fill;
      fill.venue.handle = order->handle;
      fill.venue.token = order->token;
      fill.venue.client_order_id = order->client_order_id;
      fill.venue.fill_quantity = {quantity, order->quantity_scale, {}};
      fill.venue.fill_price = {price, order->price_scale, {}};
      copy_field(fill.venue.trade_id.value.data(), fill.venue.trade_id.value.size(),
                 event.exec_id);
      fill.venue.trade_id.length = static_cast<std::uint16_t>(
          std::char_traits<char>::length(event.exec_id));
      push_event(fill);
      break;
    }
    case ExecEvent::Kind::ExecutionEnd:
      executions_in_flight_ = false;
      executions_done_ = true;
      complete_reconcile_if_ready();
      break;
    case ExecEvent::Kind::Position: {
      if (std::strcmp(event.account, session_.account().c_str()) != 0) break;
      Contract* contract = session_.find_contract(event.con_id);
      std::int64_t quantity = 0;
      const std::uint8_t quantity_scale =
          contract == nullptr ? 0 : contract->quantity_scale;
      if (!parse_decimal(event.quantity, quantity_scale, quantity))
        quantity = event.position;
      for (std::size_t index = 0;
           !positions_requery_needed_ &&
           index < position_waiter_count_;
           ++index) {
        const QueryWaiter& waiter = position_waiters_[index];
        if (waiter.con_id != 0 && waiter.con_id != event.con_id) continue;
        AdapterEvent snapshot =
            MakeEvent(AdapterEventKind::PositionSnapshot);
        snapshot.position.query_token = waiter.token;
        snapshot.position.account_id = waiter.account_id;
        snapshot.position.instrument_id =
            waiter.instrument_id != 0
                ? waiter.instrument_id
                : (contract == nullptr ? 0 : contract->instrument_id);
        snapshot.position.quantity = {quantity, quantity_scale, {}};
        if (snapshot.position.instrument_id == 0)
          snapshot.position.reserved |= api::SnapshotUnmapped;
        push_event(snapshot);
      }
      if (contract == nullptr) {
        break;
      }
      session_.set_position(contract->instrument_id, quantity);
      break;
    }
    case ExecEvent::Kind::PositionEnd:
      if (positions_requery_needed_) {
        positions_requery_needed_ = false;
        if (client_ == nullptr || !client_->request_positions()) {
          session_.note_query_timeout();
          positions_in_flight_ = false;
        }
      } else {
        finish_queries(api::QueryKind::Positions);
      }
      break;
    case ExecEvent::Kind::AccountValue: {
      if (!balances_in_flight_ ||
          std::strcmp(event.account, session_.account().c_str()) != 0 ||
          event.currency[0] == '\0')
        break;
      BalanceValue* balance = nullptr;
      for (std::size_t index = 0; index < balance_value_count_; ++index) {
        if (std::strcmp(balance_values_[index].currency.data(),
                        event.currency) == 0) {
          balance = &balance_values_[index];
          break;
        }
      }
      if (balance == nullptr &&
          balance_value_count_ < balance_values_.size()) {
        balance = &balance_values_[balance_value_count_++];
        *balance = {};
        copy_field(balance->currency.data(), balance->currency.size(),
                   event.currency);
      }
      if (balance == nullptr) break;
      if (std::strcmp(event.tag, "CashBalance") == 0 ||
          std::strcmp(event.tag, "TotalCashBalance") == 0) {
        copy_field(balance->total.data(), balance->total.size(), event.value);
        balance->has_total = true;
      } else if (std::strcmp(event.tag, "SettledCash") == 0) {
        copy_field(balance->available.data(), balance->available.size(),
                   event.value);
        balance->has_available = true;
      }
      break;
    }
    case ExecEvent::Kind::AccountDownloadEnd:
      if (balances_in_flight_ &&
          std::strcmp(event.account, session_.account().c_str()) == 0) {
        if (client_ != nullptr)
          (void)client_->request_account_updates(
              balance_request_id_, false, session_.account());
        finish_balance_queries();
      }
      break;
  }
}

AdapterResult IbkrTradeAdapter::emit(const AdapterEventSink& sink) noexcept {
  std::size_t index = 0;
  while (index < pending_count_) {
    if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
    const AdapterResult result = sink.on_event(sink.context, pending_[index]);
    if (result != AdapterResult::Ok) {
      for (std::size_t next = index; next < pending_count_; ++next)
        pending_[next - index] = pending_[next];
      pending_count_ -= index;
      return result;
    }
    ++index;
  }
  pending_count_ = 0;
  return AdapterResult::Ok;
}

void IbkrTradeAdapter::on_callback(const Callback& callback, void* context) noexcept {
  auto* self = static_cast<IbkrTradeAdapter*>(context);
  if (self == nullptr) return;
  if (callback.kind == Callback::Kind::Error &&
      (callback.code == 1100 || callback.code == 1101 || callback.code == 1102 ||
       callback.code == 326)) {
    AdapterEvent event = MakeEvent(AdapterEventKind::Status);
    event.status.identity = self->identity();
    event.status.status = self->session_.status();
    self->push_event(event);
    if (callback.code == 1101 && self->client_ != nullptr &&
        self->client_->connected())
      (void)self->client_->request_ids();
    if ((callback.code == 1101 || callback.code == 1102) &&
        self->client_ != nullptr && self->client_->connected())
      self->start_reconcile();
  }
}

AdapterServiceResult IbkrTradeAdapter::service_io(
    std::uint64_t now_ns, std::uint32_t, const AdapterEventSink& sink) noexcept {
  now_ns_ = now_ns;
  sink_ = sink;
  if (client_ != nullptr && !client_->connected() && !connect_started_ &&
      !host_.empty()) {
    connect_started_ = true;
    if (client_->connect(host_, port_, session_.client_id())) {
      session_.note_connected(true);
      if (client_ != nullptr)
        client_->set_generation(session_.connection_generation());
      request_registered_contracts();
    } else {
      connect_started_ = false;
    }
  }
  if (client_ != nullptr && client_->overflowed()) session_.fail();
  if (client_ != nullptr) {
    ExecEvent event;
    while (client_->pop(event)) handle_event(event);
  }
  session_.for_each_contract([&](Contract& contract) {
    if (contract.state == InstrumentExecutionState::Resolving &&
        contract.deadline_ns != 0 && now_ns > contract.deadline_ns)
      contract.state = InstrumentExecutionState::RetryPending;
  });
  session_.service();
  if ((orders_in_flight_ || positions_in_flight_ || executions_in_flight_) &&
      query_deadline_ns_ != 0 && now_ns > query_deadline_ns_)
    session_.note_query_timeout();
  if (!orders_in_flight_ && !positions_in_flight_ && !executions_in_flight_)
    query_deadline_ns_ = 0;
  if (session_.begin_session_reconcile()) {
    request_registered_contracts();
    start_reconcile();
  }
  AdapterServiceResult result;
  result.result = emit(sink);
  return result;
}

AdapterResult IbkrTradeAdapter::on_deadline(
    const AdapterDeadline& deadline, std::uint64_t now_ns,
    const AdapterEventSink&) noexcept {
  now_ns_ = now_ns;
  if (deadline.kind == AdapterDeadlineKind::Reconnect && client_ != nullptr &&
      !client_->connected() && !host_.empty()) {
    if (client_->connect(host_, port_, session_.client_id())) {
      connect_started_ = true;
      session_.note_connected(true);
      client_->set_generation(session_.connection_generation());
      request_registered_contracts();
    }
  }
  return AdapterResult::Ok;
}

AdapterResult IbkrTradeAdapter::begin_reconcile(
    std::uint64_t generation, std::uint64_t now_ns,
    const AdapterEventSink&) noexcept {
  now_ns_ = now_ns;
  generation_ = generation;
  start_reconcile();
  return AdapterResult::Ok;
}

AdapterResult IbkrTradeAdapter::shutdown(std::uint64_t,
                                        const AdapterEventSink&) noexcept {
  if (client_ != nullptr) client_->disconnect();
  Callback closed;
  closed.kind = Callback::Kind::Closed;
  (void)session_.push(closed);
  session_.service();
  return AdapterResult::Ok;
}

IbkrWakeDriver::IbkrWakeDriver(ExecClient& client) noexcept : client_(client) {}

std::size_t IbkrWakeDriver::descriptor_capacity() const noexcept { return 1; }

std::size_t IbkrWakeDriver::snapshot_descriptors(
    std::span<AsyncIoDescriptor> output) const noexcept {
  const int fd = client_.event_fd();
  if (fd < 0 || output.empty()) return fd < 0 ? 0 : 1;
  output[0] = {fd, 0x001U, generation_};
  return 1;
}

void IbkrWakeDriver::service_io(int fd, std::uint64_t, std::uint32_t,
                                std::uint64_t) noexcept {
  if (fd < 0) return;
  std::uint64_t value = 0;
  const ssize_t read_count = ::read(fd, &value, sizeof(value));
  if (read_count < 0 && errno != EAGAIN && errno != EWOULDBLOCK) return;
}

void IbkrWakeDriver::check_timeouts(std::uint64_t) noexcept {}

std::uint64_t IbkrWakeDriver::next_deadline_ns() const noexcept { return 0; }

}  // namespace oms::exchange::ibkr
