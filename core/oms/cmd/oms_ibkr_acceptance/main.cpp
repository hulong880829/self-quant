#include "oms/api/execution_channel.h"

#include <chrono>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <string>
#include <string_view>
#include <thread>

namespace {

struct Options {
  std::string host{"127.0.0.1"};
  std::string service{"4002"};
  std::string account;
  std::string security{"STK"};
  std::string limit{"0.01"};
  int client_id{32};
  std::int64_t con_id{};
  bool ioc{};
};

bool take(int& index, int argc, char** argv, std::string& output) {
  if (index + 1 >= argc) return false;
  output = argv[++index];
  return true;
}

bool parse(int argc, char** argv, Options& options) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    std::string value;
    if (argument == "--host") {
      if (!take(index, argc, argv, options.host)) return false;
    } else if (argument == "--service") {
      if (!take(index, argc, argv, options.service)) return false;
    } else if (argument == "--account") {
      if (!take(index, argc, argv, options.account)) return false;
    } else if (argument == "--security") {
      if (!take(index, argc, argv, value)) return false;
      options.security = value;
    } else if (argument == "--limit") {
      if (!take(index, argc, argv, options.limit)) return false;
    } else if (argument == "--client-id") {
      if (!take(index, argc, argv, value)) return false;
      options.client_id = std::stoi(value);
    } else if (argument == "--con-id") {
      if (!take(index, argc, argv, value)) return false;
      options.con_id = std::stoll(value);
    } else if (argument == "--ioc") {
      options.ioc = true;
    } else {
      return false;
    }
  }
  return options.con_id > 0 && !options.account.empty();
}

struct QueryWatch {
  oms::api::QueryToken orders{};
  oms::api::QueryToken positions{};
  oms::api::QueryToken balances{};
  bool orders_done{};
  bool positions_done{};
  bool balances_done{};
  std::size_t open_orders{};
  std::int64_t position_total{};
  std::int64_t available_total{};

  [[nodiscard]] bool done() const noexcept {
    return orders_done && positions_done && balances_done;
  }

  static void on(void* context,
                 const oms::api::RuntimeUpdate& update) noexcept {
    auto* self = static_cast<QueryWatch*>(context);
    if (update.kind == oms::api::RuntimeUpdateKind::OpenOrderSnapshot &&
        update.open_order.query_token == self->orders) {
      ++self->open_orders;
    } else if (update.kind ==
                   oms::api::RuntimeUpdateKind::PositionSnapshot &&
               update.position.query_token == self->positions) {
      self->position_total += update.position.quantity.value;
    } else if (update.kind ==
                   oms::api::RuntimeUpdateKind::BalanceSnapshot &&
               update.balance.query_token == self->balances) {
      self->available_total += update.balance.available_quantity;
    } else if (update.kind == oms::api::RuntimeUpdateKind::QueryComplete) {
      const auto& complete = update.query_complete;
      if (complete.query_token == self->orders)
        self->orders_done = complete.error == oms::api::Error::Ok;
      else if (complete.query_token == self->positions)
        self->positions_done = complete.error == oms::api::Error::Ok;
      else if (complete.query_token == self->balances)
        self->balances_done = complete.error == oms::api::Error::Ok;
    }
  }
};

bool capture_snapshot(oms::api::ExecutionChannel& channel,
                      const oms::api::ResolvedInstrument& routing,
                      QueryWatch& watch) {
  oms::api::QueryRequest instrument_query{};
  instrument_query.account_id = 1;
  instrument_query.scope = oms::api::QueryScope::SingleInstrument;
  instrument_query.instrument_id = 1;
  instrument_query.instrument.kind = routing.kind;
  instrument_query.instrument.venue = routing.venue;
  instrument_query.instrument.product_type = routing.product_type;
  instrument_query.instrument.ibkr = routing.ibkr;
  const auto orders = channel.query_open_orders(1, instrument_query);
  const auto positions = channel.query_positions(1, instrument_query);
  const auto balances = channel.query_balances(1, 1);
  if (!orders || !positions || !balances) return false;
  watch.orders = orders.value;
  watch.positions = positions.value;
  watch.balances = balances.value;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(20);
  while (std::chrono::steady_clock::now() < deadline && !watch.done()) {
    if (channel.service_io(50) != oms::api::Error::Ok) return false;
    (void)channel.drain_updates(1, &QueryWatch::on, &watch);
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  return watch.done();
}

}  // namespace

int main(int argc, char** argv) {
  Options options;
  if (!parse(argc, argv, options)) {
    std::cerr << "usage: oms_ibkr_acceptance --account DU... --con-id N "
                 "[--security STK|CMDTY|CASH] [--host 127.0.0.1] "
                 "[--service 4002] [--client-id 32] [--limit 0.01] [--ioc]\n";
    return 2;
  }
  if (options.account.rfind("DU", 0) != 0) {
    std::cerr << "oms_ibkr_acceptance accepts only DU paper accounts\n";
    return 2;
  }
#if !defined(OMS_HAS_IBKR)
  std::cerr << "IBKR OMS support is not linked\n";
  return 2;
#else
  utils::md::IbkrSecurityType security = utils::md::IbkrSecurityType::Stock;
  if (options.security == "CMDTY")
    security = utils::md::IbkrSecurityType::Commodity;
  else if (options.security == "CASH")
    security = utils::md::IbkrSecurityType::Cash;
  else if (options.security != "STK") {
    std::cerr << "security must be STK, CMDTY, or CASH\n";
    return 2;
  }

  oms::api::ExecutionChannelConfig config;
  config.runtime.mode = oms::api::ExecutionMode::Inline;
  config.runtime.lane_count = 1;
  config.runtime.deadline_capacity = 8;
  config.runtime.lanes[0] = {1, 16, 16, -1};
  config.ibkr.enabled = true;
  config.ibkr.account_id = 1;
  config.ibkr.client_id = options.client_id;
  config.ibkr.account = options.account;
  config.ibkr.strategy_instance = "oms-ibkr-acceptance";
  config.ibkr.strategy_instance = "acceptance";
  config.ibkr.gateway.host = options.host;
  config.ibkr.gateway.service = options.service;
  oms::api::InstrumentInit instrument{};
  instrument.instrument.instrument_id = 1;
  instrument.instrument.venue = utils::md::Venue::Ibkr;
  instrument.instrument.product_type = utils::md::ProductType::Equity;
  instrument.instrument.price_scale = 2;
  instrument.instrument.tick_size = 1;
  instrument.instrument.lot_size = 1;
  instrument.ibkr_security_type = security;
  const std::string con_id = std::to_string(options.con_id);
  std::memcpy(instrument.instrument.venue_symbol.data(), con_id.data(),
              con_id.size());
  auto created = oms::api::ExecutionChannel::Create(
      config, std::span<const oms::api::InstrumentInit>(&instrument, 1));
  if (!created) {
    std::cerr << "execution channel rejected the IBKR configuration\n";
    return 1;
  }
  if (!created.value->initialize_lane(1, 1)) {
    std::cerr << "lane initialization failed\n";
    return 1;
  }

  oms::api::ResolvedInstrument routing{};
  routing.kind = oms::api::ExecutionRouteKind::Ibkr;
  routing.venue = static_cast<std::uint8_t>(utils::md::Venue::Ibkr);
  routing.product_type = static_cast<std::uint8_t>(utils::md::ProductType::Equity);
  routing.catalog_revision = 1;
  routing.price_scale = 2;
  routing.quantity_scale = 0;
  routing.tick_size = 1;
  routing.lot_size = 1;
  routing.ibkr.con_id = options.con_id;
  routing.ibkr.security_type = security;
  oms::api::RegisterInstrumentRequest registration;
  registration.instrument_id = 1;
  registration.routing = routing;
  if (!created.value->register_instrument(1, registration)) {
    std::cerr << "register was not accepted\n";
    return 1;
  }

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(20);
  bool ready = false;
  while (std::chrono::steady_clock::now() < deadline) {
    if (created.value->service_io(50) != oms::api::Error::Ok) {
      std::cerr << "service failed\n";
      return 1;
    }
    ready = created.value->instrument_ready(routing);
    if (ready) break;
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
  }
  const auto status = created.value->venue_status(oms::exchange::AdapterKind::Ibkr);
  const bool session_ready =
      status && status.value.status == oms::exchange::AdapterStatus::Ready;
  std::cout << "connected=" << session_ready
            << " order_id_ready=" << session_ready
            << " contract_resolved=" << ready
            << " reconciles=" << session_ready
            << " session_ready=" << session_ready << '\n';
  if (!ready) {
    std::cerr << "IBKR session did not become ready\n";
    return 1;
  }

  QueryWatch before;
  if (!capture_snapshot(*created.value, routing, before)) {
    std::cerr << "initial open-orders/positions/balances query failed\n";
    return 1;
  }
  if (before.open_orders != 0) {
    std::cerr << "paper account has an existing order on the target contract\n";
    return 1;
  }

  oms::api::SubmitOrderRequest order;
  order.order.instrument_id = 1;
  order.order.type = oms::api::OrderType::Limit;
  order.order.time_in_force =
      options.ioc ? oms::api::TimeInForce::IOC : oms::api::TimeInForce::GTC;
  order.order.side = oms::api::Side::Buy;
  std::int64_t limit_value = 0;
  std::uint8_t limit_scale = 0;
  {
    const std::string& text = options.limit;
    const auto dot = text.find('.');
    const std::string digits = dot == std::string::npos
                                   ? text
                                   : text.substr(0, dot) + text.substr(dot + 1);
    if (digits.empty() || text.find('.') != text.rfind('.') ||
        (dot != std::string::npos && text.size() - dot - 1 > 18)) {
      std::cerr << "limit must be a decimal price\n";
      return 2;
    }
    for (char character : digits) {
      if (character < '0' || character > '9') {
        std::cerr << "limit must be a decimal price\n";
        return 2;
      }
    }
    limit_value = std::stoll(digits);
    limit_scale = dot == std::string::npos
                      ? 0
                      : static_cast<std::uint8_t>(text.size() - dot - 1);
  }
  order.order.price = {limit_value, limit_scale, {}};
  order.order.quantity = {1, 0, {}};
  order.order.client_order_id.length = 6;
  std::memcpy(order.order.client_order_id.value.data(), "paper1", 6);
  order.routing = routing;
  if (!created.value->place_order(1, order)) {
    std::cerr << "place was not accepted\n";
    return 1;
  }
  struct Watch {
    bool acknowledged{};
    bool canceled{};
    oms::api::OrderHandle handle{};
    static void on(void* context, const oms::api::RuntimeUpdate& update) noexcept {
      auto* self = static_cast<Watch*>(context);
      if (update.kind != oms::api::RuntimeUpdateKind::Order) return;
      if (update.order.type == oms::api::UpdateType::Accepted) {
        self->acknowledged = true;
        self->handle = update.order.handle;
      }
      if (update.order.type == oms::api::UpdateType::Canceled)
        self->canceled = true;
    }
  } watch;
  bool cancel_sent = false;
  const auto trade_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(20);
  while (std::chrono::steady_clock::now() < trade_deadline &&
         (!watch.acknowledged ||
          (!options.ioc && (!cancel_sent || !watch.canceled)))) {
    if (created.value->service_io(50) != oms::api::Error::Ok) return 1;
    (void)created.value->drain_updates(1, &Watch::on, &watch);
    if (watch.acknowledged && !options.ioc && !cancel_sent) {
      cancel_sent = static_cast<bool>(
          created.value->cancel_order(1, {}, watch.handle));
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
  }
  QueryWatch after;
  const bool snapshots_restored =
      capture_snapshot(*created.value, routing, after) &&
      after.open_orders == 0 &&
      after.position_total == before.position_total &&
      after.available_total == before.available_total;
  const auto shutdown = created.value->shutdown();
  if (!watch.acknowledged || (!options.ioc && !watch.canceled) ||
      !snapshots_restored || shutdown != oms::api::Error::Ok) {
    std::cerr << "paper lifecycle or final snapshot verification failed\n";
    return 1;
  }
  std::cout << "ack=1 cancel_terminal="
            << (!options.ioc && watch.canceled)
            << " snapshots_restored=1\n";
  return 0;
#endif
}
