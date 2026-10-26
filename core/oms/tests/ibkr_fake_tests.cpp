#include "oms/api/execution_channel.h"
#include "oms/api/oms_api.h"
#include "oms/exchange/hyperliquid/trade_adapter.h"
#include "oms/exchange/ibkr/exec_client.h"
#include "oms/exchange/ibkr/trade_adapter.h"

#include <array>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <stdexcept>
#include <string>
#include <string_view>

namespace {

void require(bool condition, const char* message) {
  if (!condition) throw std::runtime_error(message);
}

void copy_text(char* destination, std::size_t capacity, std::string_view value) {
  const std::size_t count =
      value.size() < capacity - 1 ? value.size() : capacity - 1;
  if (count != 0) std::memcpy(destination, value.data(), count);
  destination[count] = '\0';
}

oms::api::ClientOrderId order_ref(std::string_view text) {
  oms::api::ClientOrderId id;
  id.length = static_cast<std::uint16_t>(text.size());
  std::memcpy(id.value.data(), text.data(), text.size());
  return id;
}

oms::api::ResolvedInstrument route(std::int64_t con_id,
                                   utils::md::IbkrSecurityType security,
                                   std::uint8_t price_scale = 2,
                                   std::uint8_t quantity_scale = 0,
                                   std::int64_t tick = 1,
                                   std::int64_t lot = 1,
                                   std::int64_t multiplier = 0,
                                   std::uint8_t multiplier_scale = 0) {
  oms::api::ResolvedInstrument routing{};
  routing.kind = oms::api::ExecutionRouteKind::Ibkr;
  routing.venue = static_cast<std::uint8_t>(utils::md::Venue::Ibkr);
  routing.product_type = static_cast<std::uint8_t>(utils::md::ProductType::Equity);
  routing.catalog_revision = 1;
  routing.price_scale = price_scale;
  routing.quantity_scale = quantity_scale;
  routing.tick_size = tick;
  routing.lot_size = lot;
  routing.ibkr.con_id = con_id;
  routing.ibkr.security_type = security;
  routing.ibkr.contract_multiplier = multiplier;
  routing.ibkr.contract_multiplier_scale = multiplier_scale;
  return routing;
}

struct Capture {
  std::array<oms::exchange::AdapterEvent, 24> events{};
  std::size_t count{};

  static oms::exchange::AdapterResult on(
      void* context, const oms::exchange::AdapterEvent& event) noexcept {
    auto* self = static_cast<Capture*>(context);
    if (self->count < self->events.size()) self->events[self->count++] = event;
    return oms::exchange::AdapterResult::Ok;
  }

  [[nodiscard]] oms::exchange::AdapterEventSink sink() noexcept {
    return {this, &Capture::on};
  }
};

struct CommandSeen {
  bool seen{};
  oms::api::Error error{oms::api::Error::Ok};

  static void on(void* context, const oms::api::RuntimeUpdate& update) noexcept {
    auto* self = static_cast<CommandSeen*>(context);
    if (update.kind != oms::api::RuntimeUpdateKind::CommandResult) return;
    self->seen = true;
    self->error = update.command_result.error;
  }
};

void pump(oms::exchange::ibkr::IbkrTradeAdapter& adapter, std::uint64_t now,
          Capture& capture) {
  capture.count = 0;
  require(adapter.service_io(now, 16, capture.sink()).result ==
              oms::exchange::AdapterResult::Ok,
          "service");
}

void push_details(oms::exchange::ibkr::FakeExecClient& client, int request_id,
                  std::int64_t con_id, std::string_view sec_type,
                  std::string_view currency, std::string_view multiplier,
                  double min_tick, std::string_view min_size,
                  std::string_view size_increment, std::uint64_t generation = 0) {
  oms::exchange::ibkr::ExecEvent details;
  details.kind = oms::exchange::ibkr::ExecEvent::Kind::ContractDetails;
  details.request_id = request_id;
  details.con_id = con_id;
  details.min_tick = min_tick;
  details.generation = generation;
  copy_text(details.sec_type, sizeof(details.sec_type), sec_type);
  copy_text(details.currency, sizeof(details.currency), currency);
  copy_text(details.multiplier, sizeof(details.multiplier), multiplier);
  copy_text(details.min_size, sizeof(details.min_size), min_size);
  copy_text(details.size_increment, sizeof(details.size_increment),
            size_increment);
  require(client.push(details), "details queued");
  oms::exchange::ibkr::ExecEvent end;
  end.kind = oms::exchange::ibkr::ExecEvent::Kind::ContractDetailsEnd;
  end.request_id = request_id;
  end.generation = generation;
  require(client.push(end), "details end queued");
}

void push_reconcile_ends(oms::exchange::ibkr::FakeExecClient& client) {
  oms::exchange::ibkr::ExecEvent orders;
  orders.kind = oms::exchange::ibkr::ExecEvent::Kind::OpenOrderEnd;
  oms::exchange::ibkr::ExecEvent positions;
  positions.kind = oms::exchange::ibkr::ExecEvent::Kind::PositionEnd;
  oms::exchange::ibkr::ExecEvent executions;
  executions.kind = oms::exchange::ibkr::ExecEvent::Kind::ExecutionEnd;
  require(client.push(orders) && client.push(positions) && client.push(executions),
          "reconcile ends");
}

void arm(oms::exchange::ibkr::IbkrTradeAdapter& adapter,
         oms::exchange::ibkr::FakeExecClient& client, Capture& capture) {
  adapter.session().note_connected(true);
  oms::exchange::ibkr::ExecEvent next_id;
  next_id.kind = oms::exchange::ibkr::ExecEvent::Kind::NextValidId;
  next_id.order_id = 50;
  require(client.push(next_id), "next id");
  pump(adapter, 1, capture);
  push_reconcile_ends(client);
  pump(adapter, 2, capture);
  require(adapter.session().session_ready(), "session ready");
  require(adapter.session().new_orders_allowed(), "new orders");
}

oms::exchange::AdapterPlaceCommand place_command(
    const oms::api::ResolvedInstrument& routing, oms::api::TimeInForce tif,
    std::int64_t price, std::uint8_t price_scale, std::int64_t quantity,
    std::uint8_t quantity_scale, std::string_view reference,
    oms::api::OrderType type = oms::api::OrderType::Limit) {
  oms::exchange::AdapterPlaceCommand command;
  command.handle = {1, 0, 1};
  command.request.token = {1, 1, 1};
  command.request.instrument_id = 7;
  command.request.type = type;
  command.request.time_in_force = tif;
  command.request.side = oms::api::Side::Buy;
  command.request.price = {price, price_scale, {}};
  command.request.quantity = {quantity, quantity_scale, {}};
  command.request.client_order_id = order_ref(reference);
  command.routing = routing;
  return command;
}

oms::api::RuntimeConfig runtime_config() {
  oms::api::RuntimeConfig config{};
  config.mode = oms::api::ExecutionMode::Inline;
  config.lane_count = 1;
  config.deadline_capacity = 8;
  config.lanes[0] = {1, 16, 16, -1};
  return config;
}

oms::api::Error register_via_oms(oms::api::OmsApi& api,
                                 const oms::api::ResolvedInstrument& routing,
                                 oms::api::InstrumentId instrument_id) {
  oms::api::RegisterInstrumentRequest request;
  request.instrument_id = instrument_id;
  request.routing = routing;
  require(static_cast<bool>(api.register_instrument(1, request)), "token");
  require(api.service_io(0) == oms::api::Error::Ok, "oms service");
  CommandSeen seen;
  (void)api.drain_updates(1, &CommandSeen::on, &seen);
  require(seen.seen, "register result");
  return seen.error;
}

void test_router_and_rollback() {
  oms::exchange::ibkr::FakeExecClient client;
  oms::exchange::ibkr::IbkrTradeAdapter adapter(32, "DU123");
  adapter.attach_client(&client);
  require(client.connect("127.0.0.1", 4002, 32), "connect");
  oms::exchange::TradeAdapter* adapters[] = {&adapter};
  oms::api::AdapterRuntimeConfig runtime;
  runtime.adapters = adapters;
  runtime.enable_fake_fallback = false;
  auto created = oms::api::OmsApi::Create(runtime_config(), {}, {}, runtime);
  require(static_cast<bool>(created), "oms");
  require(static_cast<bool>(created.value->initialize_lane(1, 1)), "lane");

  const auto stock = route(265598, utils::md::IbkrSecurityType::Stock);
  client.set_fail_contract_details(true);
  require(register_via_oms(*created.value, stock, 7) == oms::api::Error::NotReady,
          "sync register failure");
  require(adapter.session().find_contract(265598) == nullptr, "mapping rolled back");
  require(client.contract_requests() == 0, "failed request is not counted");
  require(register_via_oms(*created.value, stock, 7) == oms::api::Error::Ok,
          "register after rollback");
  require(client.contract_requests() == 1, "router sent one details request");
  require(register_via_oms(*created.value, stock, 7) == oms::api::Error::Ok,
          "duplicate register");
  require(client.contract_requests() == 1, "duplicate does not resend");

  adapter.session().set_position(7, 3);
  require(static_cast<bool>(created.value->retire_instrument(1, 7)), "retire token");
  require(created.value->service_io(0) == oms::api::Error::Ok, "retire service");
  CommandSeen retired;
  (void)created.value->drain_updates(1, &CommandSeen::on, &retired);
  require(retired.seen && retired.error != oms::api::Error::Ok, "position blocks retire");
  require(adapter.session().find_contract(265598) != nullptr, "adapter kept mapping");
  require(register_via_oms(*created.value, stock, 7) == oms::api::Error::Ok,
          "directory still holds the instrument");
  require(client.contract_requests() == 1, "blocked retire did not resend");
  adapter.session().set_position(7, 0);
  require(static_cast<bool>(created.value->retire_instrument(1, 7)), "retire again");
  require(created.value->service_io(0) == oms::api::Error::Ok, "clear retire");
  CommandSeen cleared;
  (void)created.value->drain_updates(1, &CommandSeen::on, &cleared);
  require(cleared.seen && cleared.error == oms::api::Error::Ok, "retire both sides");
  require(adapter.session().find_contract(265598) == nullptr, "adapter mapping removed");
}

void test_contracts_and_orders() {
  oms::exchange::ibkr::FakeExecClient client;
  oms::exchange::ibkr::IbkrTradeAdapter adapter(32, "DU123");
  adapter.attach_client(&client);
  adapter.configure_gateway("127.0.0.1", 4002);
  require(client.connect("127.0.0.1", 4002, 32), "connect");
  Capture capture;
  const auto stock = route(100, utils::md::IbkrSecurityType::Stock);
  oms::exchange::AdapterReservation reservation;
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve before ready");
  auto early = place_command(stock, oms::api::TimeInForce::GTC, 100, 2, 1, 0, "early");
  require(adapter.commit_place(reservation, early) ==
              oms::exchange::AdapterResult::NotReady,
          "no order id is not ready");

  arm(adapter, client, capture);
  require(adapter.register_instrument(7, stock) == oms::exchange::AdapterResult::Ok,
          "register stock");
  const int stock_request = client.last_contract_request();
  push_details(client, stock_request, 100, "STK", "USD", "", 0.01, "1", "1");
  pump(adapter, 3, capture);
  require(adapter.session().instrument_state(100) ==
              oms::exchange::ibkr::InstrumentExecutionState::Ready,
          "stk empty multiplier");
  require(adapter.instrument_ready(stock), "stk ready");
  const auto* stk = adapter.session().find_contract(100);
  require(stk != nullptr && stk->multiplier == 1 && stk->multiplier_scale == 0,
          "stk multiplier one");

  const auto cmdty = route(200, utils::md::IbkrSecurityType::Commodity, 2, 0, 1, 1,
                           150, 2);
  require(adapter.register_instrument(8, cmdty) == oms::exchange::AdapterResult::Ok,
          "register cmdty");
  push_details(client, client.last_contract_request(), 200, "CMDTY", "USD", "", 0.01,
               "1", "1");
  pump(adapter, 4, capture);
  require(adapter.session().instrument_state(200) ==
              oms::exchange::ibkr::InstrumentExecutionState::Rejected,
          "cmdty missing multiplier");
  require(adapter.instrument_ready(stock), "rejected cmdty leaves stk ready");

  const auto cmdty_ok =
      route(201, utils::md::IbkrSecurityType::Commodity, 2, 0, 1, 1, 150, 2);
  require(adapter.register_instrument(9, cmdty_ok) == oms::exchange::AdapterResult::Ok,
          "register fractional cmdty");
  push_details(client, client.last_contract_request(), 201, "CMDTY", "USD", "1.50",
               0.01, "2", "1");
  pump(adapter, 5, capture);
  const auto* fractional = adapter.session().find_contract(201);
  require(fractional != nullptr && fractional->multiplier == 150 &&
              fractional->multiplier_scale == 2 &&
              fractional->state == oms::exchange::ibkr::InstrumentExecutionState::Ready,
          "fractional multiplier");

  const auto cash = route(300, utils::md::IbkrSecurityType::Cash);
  require(adapter.register_instrument(10, cash) == oms::exchange::AdapterResult::Ok,
          "register cash");
  push_details(client, client.last_contract_request(), 300, "CASH", "USD", "", 0.01,
               "1", "1");
  pump(adapter, 6, capture);
  require(adapter.session().instrument_state(300) ==
              oms::exchange::ibkr::InstrumentExecutionState::Ready,
          "cash ready");

  const auto conflict = route(400, utils::md::IbkrSecurityType::Stock);
  require(adapter.register_instrument(11, conflict) ==
              oms::exchange::AdapterResult::Ok,
          "register conflict");
  push_details(client, client.last_contract_request(), 400, "STK", "EUR", "", 0.01,
               "1", "1");
  pump(adapter, 7, capture);
  require(adapter.session().instrument_state(400) ==
              oms::exchange::ibkr::InstrumentExecutionState::Rejected,
          "currency conflict rejected");
  require(adapter.instrument_ready(stock), "conflict stays local");

  const auto retry = route(500, utils::md::IbkrSecurityType::Stock);
  require(adapter.register_instrument(12, retry) == oms::exchange::AdapterResult::Ok,
          "register retry");
  const int retry_request = client.last_contract_request();
  const int requests_before_timeout = client.contract_requests();
  pump(adapter, 6000000000ULL, capture);
  require(adapter.session().instrument_state(500) ==
              oms::exchange::ibkr::InstrumentExecutionState::RetryPending,
          "details timeout");
  push_details(client, retry_request, 500, "STK", "USD", "", 0.01, "1", "1", 99);
  pump(adapter, 6000000001ULL, capture);
  require(adapter.session().instrument_state(500) ==
              oms::exchange::ibkr::InstrumentExecutionState::RetryPending,
          "old generation does not reject");
  require(adapter.begin_reconcile(1, 6000000002ULL, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "retry reconcile");
  require(client.contract_requests() > requests_before_timeout, "retry sends details");
  push_details(client, client.last_contract_request(), 500, "STK", "USD", "", 0.01,
               "1", "1");
  pump(adapter, 6000000003ULL, capture);
  require(adapter.session().instrument_state(500) ==
              oms::exchange::ibkr::InstrumentExecutionState::Ready,
          "retry recovers");

  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve small");
  auto small = place_command(cmdty_ok, oms::api::TimeInForce::GTC, 100, 2, 1, 0,
                             "too-small");
  small.request.instrument_id = 9;
  require(adapter.commit_place(reservation, small) == oms::exchange::AdapterResult::Ok,
          "below min size queued");
  pump(adapter, 9, capture);
  require(capture.count == 1 &&
              capture.events[0].venue.type == oms::api::VenueEventType::NewReject,
          "min size reject");
  require(client.last_order_id() == 0, "min size was not enlarged");

  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve gtc");
  auto gtc = place_command(stock, oms::api::TimeInForce::GTC, 123, 2, 1, 0, "gtc-1");
  require(adapter.commit_place(reservation, gtc) == oms::exchange::AdapterResult::Ok,
          "gtc placed");
  require(client.last_tif() == "GTC" && client.last_price() == "1.23" &&
              client.last_quantity() == "1" && client.last_order_id() == 50,
          "gtc fields");

  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve scale4");
  const auto fine = route(100, utils::md::IbkrSecurityType::Stock, 4, 0, 1, 1);
  auto scaled = place_command(fine, oms::api::TimeInForce::GTC, 12300, 4, 1, 0,
                              "scale-4");
  require(adapter.commit_place(reservation, scaled) == oms::exchange::AdapterResult::Ok,
          "scale 4 placed");
  require(client.last_price() == "1.23" && client.last_order_id() == 51,
          "scale 4 rescales to the contract");

  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve gtd");
  auto gtd = place_command(stock, oms::api::TimeInForce::GTD, 100, 2, 1, 0, "gtd-1");
  gtd.request.expire_time_ns = 1000000000ULL;
  require(adapter.commit_place(reservation, gtd) == oms::exchange::AdapterResult::Ok,
          "gtd placed");
  require(client.last_tif() == "GTD" &&
              client.last_good_till() == "19700101 00:00:01 UTC",
          "gtd utc");

  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve ioc");
  auto ioc = place_command(stock, oms::api::TimeInForce::IOC, 100, 2, 1, 0, "ioc-1");
  require(adapter.commit_place(reservation, ioc) == oms::exchange::AdapterResult::Ok,
          "ioc placed");
  require(client.last_tif() == "IOC", "ioc field");
  oms::exchange::ibkr::ExecEvent ioc_reject;
  ioc_reject.kind = oms::exchange::ibkr::ExecEvent::Kind::Error;
  ioc_reject.error_code = 201;
  ioc_reject.order_id = client.last_order_id();
  require(client.push(ioc_reject), "ioc reject");
  pump(adapter, 10, capture);
  require(adapter.session().ioc_capability(100) ==
                  oms::exchange::ibkr::IocCapability::Unsupported &&
              adapter.session().instrument_state(100) ==
                  oms::exchange::ibkr::InstrumentExecutionState::Ready,
          "ioc unsupported stays ready");
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve rejected ioc");
  auto ioc_again =
      place_command(stock, oms::api::TimeInForce::IOC, 100, 2, 1, 0, "ioc-2");
  require(adapter.commit_place(reservation, ioc_again) ==
              oms::exchange::AdapterResult::Ok,
          "later ioc queued");
  pump(adapter, 11, capture);
  require(capture.count == 1 &&
              capture.events[0].venue.type == oms::api::VenueEventType::NewReject,
          "later ioc rejected locally");
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve gtc after ioc");
  auto gtc_after =
      place_command(stock, oms::api::TimeInForce::GTC, 100, 2, 1, 0, "gtc-2");
  require(adapter.commit_place(reservation, gtc_after) ==
              oms::exchange::AdapterResult::Ok,
          "gtc after ioc");
  require(client.last_tif() == "GTC", "gtc still sent");

  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve market");
  auto market = place_command(stock, oms::api::TimeInForce::GTC, 100, 2, 1, 0,
                              "mkt", oms::api::OrderType::Market);
  require(adapter.commit_place(reservation, market) == oms::exchange::AdapterResult::Ok,
          "market queued");
  pump(adapter, 12, capture);
  require(capture.events[0].venue.type == oms::api::VenueEventType::NewReject,
          "market reject");

  oms::exchange::ibkr::ExecEvent other;
  other.kind = oms::exchange::ibkr::ExecEvent::Kind::OpenOrder;
  other.client_id = 99;
  other.order_id = 80;
  other.con_id = 100;
  copy_text(other.order_ref, sizeof(other.order_ref), "foreign");
  require(client.push(other), "foreign order");
  pump(adapter, 13, capture);
  require(adapter.session().find_order(99, 80) == nullptr, "foreign not mapped");
  require(adapter.session().find_order(32, 50) != nullptr, "same id stays ours");
  require(!adapter.session().session_ready(), "foreign blocks ready");
  require(adapter.session().next_order_id() > 80, "next id above foreign");

  adapter.session().clear_unknown_order();
  adapter.session().note_reconciled();
  oms::exchange::ibkr::ExecEvent mismatch;
  mismatch.kind = oms::exchange::ibkr::ExecEvent::Kind::OpenOrder;
  mismatch.client_id = 32;
  mismatch.order_id = 50;
  mismatch.perm_id = 9;
  copy_text(mismatch.order_ref, sizeof(mismatch.order_ref), "other-ref");
  require(client.push(mismatch), "ref mismatch");
  pump(adapter, 14, capture);
  require(adapter.session().status() == oms::exchange::AdapterStatus::Failed,
          "order ref mismatch fails closed");
}

void test_fills_queries_and_disconnect() {
  oms::exchange::ibkr::FakeExecClient client;
  oms::exchange::ibkr::IbkrTradeAdapter adapter(32, "DU9");
  adapter.attach_client(&client);
  require(client.connect("127.0.0.1", 4002, 32), "connect");
  Capture capture;
  arm(adapter, client, capture);
  const auto stock = route(100, utils::md::IbkrSecurityType::Stock);
  require(adapter.register_instrument(7, stock) == oms::exchange::AdapterResult::Ok,
          "register");
  push_details(client, client.last_contract_request(), 100, "STK", "USD", "", 0.01,
               "1", "1");
  pump(adapter, 3, capture);

  oms::exchange::AdapterReservation reservation;
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "reserve");
  auto command = place_command(stock, oms::api::TimeInForce::GTC, 100, 2, 5, 0, "fill");
  require(adapter.commit_place(reservation, command) == oms::exchange::AdapterResult::Ok,
          "place");
  const int order_id = client.last_order_id();
  oms::exchange::ibkr::ExecEvent ack;
  ack.kind = oms::exchange::ibkr::ExecEvent::Kind::OpenOrder;
  ack.client_id = 32;
  ack.order_id = order_id;
  ack.perm_id = 7;
  copy_text(ack.order_ref, sizeof(ack.order_ref), "fill");
  require(client.push(ack), "ack");
  pump(adapter, 4, capture);
  require(capture.count == 1 &&
              capture.events[0].venue.type == oms::api::VenueEventType::NewAck,
          "new ack");

  oms::exchange::ibkr::ExecEvent premature;
  premature.kind = oms::exchange::ibkr::ExecEvent::Kind::OrderStatus;
  premature.client_id = 32;
  premature.order_id = order_id;
  premature.perm_id = 7;
  copy_text(premature.status, sizeof(premature.status), "Filled");
  require(client.push(premature), "premature filled");
  pump(adapter, 5, capture);
  require(capture.count == 0, "filled waits for executions");

  oms::exchange::ibkr::ExecEvent partial;
  partial.kind = oms::exchange::ibkr::ExecEvent::Kind::ExecDetails;
  partial.client_id = 32;
  partial.order_id = order_id;
  partial.perm_id = 7;
  partial.limit_price = 1.0;
  copy_text(partial.exec_id, sizeof(partial.exec_id), "exec-1");
  copy_text(partial.quantity, sizeof(partial.quantity), "2");
  require(client.push(partial) && client.push(partial), "duplicate exec");
  pump(adapter, 6, capture);
  require(capture.count == 1 &&
              capture.events[0].venue.type == oms::api::VenueEventType::Fill &&
              capture.events[0].venue.fill_quantity.value == 2,
          "partial fill dedup");
  require(client.push(premature), "filled still early");
  pump(adapter, 7, capture);
  require(capture.count == 0, "partial is not terminal");
  oms::exchange::ibkr::ExecEvent rest;
  rest.kind = oms::exchange::ibkr::ExecEvent::Kind::ExecDetails;
  rest.client_id = 32;
  rest.order_id = order_id;
  rest.perm_id = 7;
  rest.limit_price = 1.0;
  copy_text(rest.exec_id, sizeof(rest.exec_id), "exec-2");
  copy_text(rest.quantity, sizeof(rest.quantity), "3");
  require(client.push(rest), "rest");
  pump(adapter, 8, capture);
  require(capture.count == 1 && capture.events[0].venue.type == oms::api::VenueEventType::Fill,
          "residual fill");
  require(client.push(premature), "filled after residual");
  pump(adapter, 9, capture);
  require(capture.count == 1 &&
              capture.events[0].venue.type == oms::api::VenueEventType::ReconcileTerminal &&
              capture.events[0].venue.reconciled_status == oms::api::OrderStatus::Filled,
          "terminal after fills");

  oms::api::QueryToken first{1, 1, 1};
  oms::api::QueryToken second{1, 1, 2};
  oms::exchange::AdapterQueryRequest left;
  left.token = first;
  left.request.account_id = 1;
  oms::exchange::AdapterQueryRequest right;
  right.token = second;
  right.request.account_id = 1;
  require(adapter.query_open_orders(left, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "query one");
  require(adapter.query_open_orders(right, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "query two");
  require(client.open_order_requests() == 2, "second query joins the snapshot");
  push_reconcile_ends(client);
  pump(adapter, 10, capture);
  int completed = 0;
  for (std::size_t index = 0; index < capture.count; ++index) {
    if (capture.events[index].kind == oms::exchange::AdapterEventKind::QueryComplete)
      ++completed;
  }
  require(completed == 2, "query fanout");

  const auto connection = adapter.session().connection_generation();
  const auto generation = adapter.session().generation();
  oms::exchange::ibkr::ExecEvent lost;
  lost.kind = oms::exchange::ibkr::ExecEvent::Kind::Error;
  lost.error_code = 1100;
  require(client.push(lost), "1100");
  pump(adapter, 11, capture);
  require(client.connected() && !client.saw_disconnect(), "1100 keeps socket");
  require(!adapter.session().new_orders_allowed() &&
              adapter.session().connection_generation() == connection &&
              adapter.session().generation() == generation,
          "1100 does not reconnect");

  const int ids_before = client.id_requests();
  oms::exchange::ibkr::ExecEvent restored;
  restored.kind = oms::exchange::ibkr::ExecEvent::Kind::Error;
  restored.error_code = 1101;
  require(client.push(restored), "1101");
  pump(adapter, 12, capture);
  require(client.id_requests() == ids_before + 1, "1101 requests ids");
  require(adapter.session().generation() == generation + 1 &&
              adapter.session().connection_generation() == connection,
          "1101 bumps reconciliation only");

  push_reconcile_ends(client);
  pump(adapter, 13, capture);
  oms::exchange::ibkr::ExecEvent maintained;
  maintained.kind = oms::exchange::ibkr::ExecEvent::Kind::Error;
  maintained.error_code = 1102;
  const auto generation_1102 = adapter.session().generation();
  require(client.push(maintained), "1102");
  pump(adapter, 14, capture);
  require(adapter.session().generation() == generation_1102 &&
              adapter.session().connection_generation() == connection &&
              !adapter.session().new_orders_allowed(),
          "1102 keeps generation");

  oms::exchange::ibkr::FakeExecClient duplicate_client;
  oms::exchange::ibkr::IbkrTradeAdapter duplicate(32, "DU9");
  duplicate.attach_client(&duplicate_client);
  require(duplicate_client.connect("127.0.0.1", 4002, 32), "dup connect");
  oms::exchange::ibkr::ExecEvent used;
  used.kind = oms::exchange::ibkr::ExecEvent::Kind::Error;
  used.error_code = 326;
  require(duplicate_client.push(used), "326");
  pump(duplicate, 15, capture);
  require(duplicate.session().status() == oms::exchange::AdapterStatus::Failed,
          "duplicate client id");

  constexpr char key[] =
      "0000000000000000000000000000000000000000000000000000000000000001";
  oms::exchange::hyperliquid::HyperliquidTradeAdapter hyperliquid(key);
  require(hyperliquid.tracks_instrument_readiness(),
          "hyperliquid tracks metadata readiness");
  require(!hyperliquid.instrument_ready(stock),
          "disconnected or wrong-route hyperliquid is not ready");
  require(sizeof(oms::api::ResolvedInstrument) == 112, "routing size");
}

void test_balance_queries() {
  oms::exchange::ibkr::FakeExecClient client;
  oms::exchange::ibkr::IbkrTradeAdapter adapter(32, "DU9");
  adapter.attach_client(&client);
  require(client.connect("127.0.0.1", 4002, 32), "balance connect");
  Capture capture;

  oms::exchange::AdapterQueryRequest first{};
  first.token = {1, 1, 10};
  first.request.account_id = 7;
  oms::exchange::AdapterQueryRequest second{};
  second.token = {1, 1, 11};
  second.request.account_id = 7;
  require(adapter.query_balances(first, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "first balance query");
  require(adapter.query_balances(second, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "second balance query");
  require(client.account_summary_requests() == 1,
          "balance queries share summary request");

  oms::exchange::ibkr::ExecEvent total{};
  total.kind = oms::exchange::ibkr::ExecEvent::Kind::AccountValue;
  copy_text(total.account, sizeof(total.account), "DU9");
  copy_text(total.currency, sizeof(total.currency), "USD");
  copy_text(total.tag, sizeof(total.tag), "CashBalance");
  copy_text(total.value, sizeof(total.value), "1234.50");
  oms::exchange::ibkr::ExecEvent available = total;
  copy_text(available.tag, sizeof(available.tag), "SettledCash");
  copy_text(available.value, sizeof(available.value), "1000.25");
  oms::exchange::ibkr::ExecEvent end{};
  end.kind = oms::exchange::ibkr::ExecEvent::Kind::AccountDownloadEnd;
  copy_text(end.account, sizeof(end.account), "DU9");
  require(client.push(total) && client.push(available) && client.push(end),
          "balance callbacks");
  pump(adapter, 1, capture);
  require(capture.count == 4, "balance fanout events");
  for (std::size_t index : {std::size_t{0}, std::size_t{2}}) {
    const auto& event = capture.events[index];
    require(event.kind ==
                oms::exchange::AdapterEventKind::BalanceSnapshot,
            "balance snapshot kind");
    require(event.balance.account_id == 7 &&
                std::strcmp(event.balance.asset_or_currency.data(), "USD") ==
                    0,
            "balance identity");
    require(event.balance.total_quantity == 123450 &&
                event.balance.held_quantity == 23425 &&
                event.balance.available_quantity == 100025 &&
                event.balance.scale == 2,
            "balance quantities");
    require(capture.events[index + 1].kind ==
                oms::exchange::AdapterEventKind::QueryComplete &&
                capture.events[index + 1].query_complete.kind ==
                    oms::api::QueryKind::Balances,
            "balance completion");
  }
}

void test_cmdty_and_cash_orders() {
  oms::exchange::ibkr::FakeExecClient client;
  oms::exchange::ibkr::IbkrTradeAdapter adapter(32, "DU123");
  adapter.attach_client(&client);
  require(client.connect("127.0.0.1", 4002, 32), "connect");
  Capture capture;
  arm(adapter, client, capture);
  const auto cmdty =
      route(201, utils::md::IbkrSecurityType::Commodity, 2, 0, 1, 1, 150, 2);
  const auto cash = route(300, utils::md::IbkrSecurityType::Cash);
  require(adapter.register_instrument(9, cmdty) == oms::exchange::AdapterResult::Ok,
          "cmdty register");
  push_details(client, client.last_contract_request(), 201, "CMDTY", "USD", "1.50",
               0.01, "1", "1");
  require(adapter.register_instrument(10, cash) == oms::exchange::AdapterResult::Ok,
          "cash register");
  push_details(client, client.last_contract_request(), 300, "CASH", "USD", "", 0.01,
               "1", "1");
  pump(adapter, 3, capture);
  require(adapter.instrument_ready(cmdty) && adapter.instrument_ready(cash),
          "cmdty and cash ready");
  oms::exchange::AdapterReservation reservation;
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "cmdty reserve");
  auto commodity = place_command(cmdty, oms::api::TimeInForce::GTC, 100, 2, 1, 0,
                                 "cmdty");
  commodity.request.instrument_id = 9;
  require(adapter.commit_place(reservation, commodity) ==
              oms::exchange::AdapterResult::Ok,
          "cmdty place");
  require(client.last_quantity() == "1" && client.last_price() == "1.00",
          "cmdty order");
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                  reservation) == oms::exchange::AdapterResult::Ok,
          "cash reserve");
  auto fx = place_command(cash, oms::api::TimeInForce::IOC, 100, 2, 1, 0, "cash");
  fx.request.instrument_id = 10;
  require(adapter.commit_place(reservation, fx) == oms::exchange::AdapterResult::Ok,
          "cash place");
  require(client.last_tif() == "IOC" && client.last_quantity() == "1", "cash order");
  require(adapter.begin_reconcile(1, 3, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "query restart");
  pump(adapter, 6000000000ULL, capture);
  require(!adapter.session().session_ready(), "query timeout blocks ready");
}

void test_owned_legacy_order_cancel() {
  oms::exchange::ibkr::FakeExecClient client;
  oms::exchange::ibkr::IbkrTradeAdapter adapter(32, "DU123", "alpha");
  adapter.attach_client(&client);
  require(client.connect("127.0.0.1", 4002, 32), "legacy connect");
  Capture capture;
  arm(adapter, client, capture);
  const auto stock = route(100, utils::md::IbkrSecurityType::Stock);
  require(adapter.register_instrument(7, stock) ==
              oms::exchange::AdapterResult::Ok,
          "legacy register");
  push_details(client, client.last_contract_request(), 100, "STK", "USD", "",
               0.01, "1", "1");
  pump(adapter, 3, capture);

  oms::exchange::AdapterQueryRequest query{};
  query.token = {1, 1, 9};
  query.request.account_id = 1;
  query.request.scope = oms::api::QueryScope::SingleInstrument;
  query.request.instrument_id = 7;
  query.request.instrument.kind = oms::api::ExecutionRouteKind::Ibkr;
  query.request.instrument.ibkr.con_id = 100;
  require(adapter.query_open_orders(query, capture.sink()) ==
              oms::exchange::AdapterResult::Ok,
          "legacy query");
  const auto push_open = [&](int order_id, std::string_view reference) {
    oms::exchange::ibkr::ExecEvent event{};
    event.kind = oms::exchange::ibkr::ExecEvent::Kind::OpenOrder;
    event.client_id = 32;
    event.order_id = order_id;
    event.con_id = 100;
    copy_text(event.account, sizeof(event.account), "DU123");
    copy_text(event.order_ref, sizeof(event.order_ref), reference);
    copy_text(event.action, sizeof(event.action), "BUY");
    copy_text(event.quantity, sizeof(event.quantity), "1");
    event.limit_price = 10.0;
    require(client.push(event), "legacy open");
  };
  push_open(77, "tf-alpha-old");
  push_open(78, "manual");
  pump(adapter, 4, capture);
  bool saw_owned = false;
  bool saw_unknown = false;
  for (std::size_t index = 0; index < capture.count; ++index) {
    if (capture.events[index].kind !=
        oms::exchange::AdapterEventKind::OpenOrderSnapshot)
      continue;
    const auto& item = capture.events[index].open_order;
    const std::string_view venue(item.venue_order_id.value.data(),
                                 item.venue_order_id.length);
    if (venue == "32:77")
      saw_owned =
          item.instrument_id == 7 &&
          (item.reserved & oms::api::SnapshotOwnedExternal) != 0 &&
          (item.reserved & oms::api::SnapshotUnmapped) == 0;
    if (venue == "32:78")
      saw_unknown =
          (item.reserved & oms::api::SnapshotUnmapped) != 0 &&
          (item.reserved & oms::api::SnapshotOwnedExternal) == 0;
  }
  require(saw_owned && saw_unknown, "legacy ownership classification");

  oms::exchange::AdapterReservation reservation{};
  require(adapter.reserve_command(oms::exchange::AdapterCommandKind::Cancel,
                                  reservation) ==
              oms::exchange::AdapterResult::Ok,
          "legacy cancel reserve");
  oms::exchange::AdapterCancelVenueCommand cancel{};
  cancel.request.account_id = 1;
  cancel.request.instrument_id = 7;
  cancel.request.routing = stock;
  cancel.request.client_order_id = order_ref("tf-alpha-old");
  const std::string_view venue = "32:77";
  std::memcpy(cancel.request.venue_order_id.value.data(), venue.data(),
              venue.size());
  cancel.request.venue_order_id.length =
      static_cast<std::uint16_t>(venue.size());
  require(adapter.commit_cancel_venue(reservation, cancel) ==
              oms::exchange::AdapterResult::Ok &&
              client.last_order_id() == 77,
          "legacy native cancel");
}

void test_ibkr_disabled() {
  oms::api::ExecutionChannelConfig config;
  config.runtime = runtime_config();
  config.ibkr.enabled = true;
  config.ibkr.account_id = 1;
  config.ibkr.client_id = 32;
  config.ibkr.account = "DU123";
  config.ibkr.strategy_instance = "test";
  config.ibkr.gateway.host = "127.0.0.1";
  config.ibkr.gateway.service = "4002";
  oms::api::InstrumentInit instrument{};
  instrument.instrument.instrument_id = 1;
  instrument.instrument.venue = utils::md::Venue::Ibkr;
  instrument.instrument.product_type = utils::md::ProductType::Equity;
  instrument.instrument.price_scale = 2;
  instrument.instrument.tick_size = 1;
  instrument.instrument.lot_size = 1;
  instrument.ibkr_security_type = utils::md::IbkrSecurityType::Stock;
  std::memcpy(instrument.instrument.venue_symbol.data(), "265598", 6);
  const auto created = oms::api::ExecutionChannel::Create(
      config, std::span<const oms::api::InstrumentInit>(&instrument, 1));
#if defined(OMS_HAS_IBKR)
  require(static_cast<bool>(created), "linked ibkr config");
  if (created.value != nullptr) require(created.value->shutdown() == oms::api::Error::Ok,
                                        "shutdown");
#else
  require(created.error == oms::api::Error::Unsupported, "ibkr disabled");
#endif
}

}  // namespace

int main() {
  try {
    test_router_and_rollback();
    test_contracts_and_orders();
    test_owned_legacy_order_cancel();
    test_fills_queries_and_disconnect();
    test_balance_queries();
    test_cmdty_and_cash_orders();
    test_ibkr_disabled();
  } catch (const std::exception& error) {
    std::cerr << error.what() << '\n';
    return 1;
  }
  std::cout << "ibkr fake tests passed\n";
  return 0;
}
