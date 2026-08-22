#include <chrono>
#include <cstring>
#include <iostream>
#include <stdexcept>
#include <string>
#include <vector>

#include <poll.h>

#include "oms/api/oms_api.h"
#include "oms/runtime/replay_script.h"

namespace {

#define REQUIRE(value)                                                        \
  do {                                                                        \
    if (!(value))                                                             \
      throw std::runtime_error(std::string("require failed: ") + #value);     \
  } while (false)

oms::api::InstrumentInit Instrument() {
  oms::api::InstrumentInit result{};
  result.instrument.instrument_id = 9;
  result.instrument.venue = utils::md::Venue::Binance;
  result.instrument.product_type = utils::md::ProductType::Spot;
  result.instrument.price_scale = 2;
  result.instrument.quantity_scale = 2;
  result.instrument.tick_size = 1;
  result.instrument.lot_size = 1;
  constexpr char key[] = "1:1:DUAL";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  return result;
}

oms::api::InstrumentInit PolymarketInstrument() {
  auto result = Instrument();
  result.instrument.instrument_id = 10;
  result.instrument.venue = utils::md::Venue::Polymarket;
  result.instrument.product_type = utils::md::ProductType::BinaryOption;
  result.instrument.instrument_key.fill('\0');
  constexpr char key[] = "6:4:DUAL";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  result.polymarket_condition_id[0] = 1;
  result.polymarket_token_id[0] = 2;
  result.polymarket_outcome = oms::api::PolymarketOutcome::Yes;
  result.minimum_order_size = 1;
  return result;
}

oms::api::RuntimeConfig Config(oms::api::ExecutionMode mode) {
  oms::api::RuntimeConfig config{};
  config.mode = mode;
  config.lane_count = 1;
  config.deadline_capacity = 8;
  config.lanes[0] = {1, 16, 16, -1};
  return config;
}

oms::api::NewOrderRequest Order() {
  oms::api::NewOrderRequest request{};
  constexpr char client[] = "dual-client";
  std::memcpy(request.client_order_id.value.data(), client,
              sizeof(client) - 1);
  request.client_order_id.length = sizeof(client) - 1;
  request.instrument_id = 9;
  request.side = oms::api::Side::Buy;
  request.type = oms::api::OrderType::Limit;
  request.time_in_force = oms::api::TimeInForce::GTC;
  request.quantity = {10, 2, {}};
  request.price = {50, 2, {}};
  return request;
}

struct Capture {
  std::vector<oms::api::RuntimeUpdate> values;
  static void Add(void* context,
                  const oms::api::RuntimeUpdate& update) noexcept {
    auto copy = update;
    copy.published_time_ns = 0;
    static_cast<Capture*>(context)->values.push_back(copy);
  }
};

std::vector<oms::api::RuntimeUpdate> Run(
    oms::api::ExecutionMode mode,
    const std::vector<oms::api::ReplayStep>& replay) {
  const auto instrument = Instrument();
  auto created =
      oms::api::OmsApi::Create(Config(mode), {&instrument, 1}, replay);
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 77));
  const auto submitted = api.submit_order(1, Order());
  REQUIRE(submitted);
  REQUIRE(submitted.value == (oms::api::RequestToken{1, 77, 1}));

  Capture capture;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.size() < 4 &&
         std::chrono::steady_clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline) {
      REQUIRE(api.service_io(1) == oms::api::Error::Ok);
    } else {
      pollfd fd{api.update_fd(1), POLLIN, 0};
      (void)::poll(&fd, 1, 1);
    }
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  std::cerr << "oms_dual_mode mode=" << static_cast<int>(mode)
            << " updates=" << capture.values.size() << '\n';
  REQUIRE(capture.values.size() == 4);
  REQUIRE(capture.values[0].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(capture.values[1].order.type == oms::api::UpdateType::Accepted);
  REQUIRE(capture.values[2].kind == oms::api::RuntimeUpdateKind::Fill);
  REQUIRE(capture.values[2].fill.cumulative_quantity.value == 4);
  REQUIRE(capture.values[3].fill.cumulative_quantity.value == 10);
  return capture.values;
}

void TestRebind(oms::api::ExecutionMode mode) {
  const auto instrument = PolymarketInstrument();
  const std::array instruments{instrument, Instrument()};
  auto created = oms::api::OmsApi::Create(Config(mode), instruments);
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 88));

  oms::api::RebindPolymarketInstrumentRequest request{};
  request.instrument_id = instrument.instrument.instrument_id;
  request.condition_id[0] = 3;
  request.token_id[0] = 4;
  request.outcome = oms::api::PolymarketOutcome::No;
  request.signature_type = 3;
  request.minimum_order_size = 2;
  const auto submitted = api.rebind_polymarket_instrument(1, request);
  REQUIRE(submitted);

  Capture capture;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.empty() &&
         std::chrono::steady_clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  REQUIRE(capture.values.size() == 1);
  const auto& result = capture.values[0].command_result;
  REQUIRE(result.kind ==
          oms::api::RuntimeCommandResultKind::RebindInstrument);
  REQUIRE(result.error == oms::api::Error::Ok);
  REQUIRE(result.rebind.request_token == submitted.value);
  REQUIRE(result.rebind.instrument_id == request.instrument_id);

  auto non_polymarket = request;
  non_polymarket.instrument_id = 9;
  const auto invalid =
      api.rebind_polymarket_instrument(1, non_polymarket);
  REQUIRE(invalid);
  const auto invalid_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.size() < 2 &&
         std::chrono::steady_clock::now() < invalid_deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  REQUIRE(capture.values.size() == 2);
  REQUIRE(capture.values.back().command_result.error ==
          oms::api::Error::InvalidArgument);

  auto active_order = Order();
  active_order.instrument_id = request.instrument_id;
  REQUIRE(api.submit_order(1, active_order));
  const auto conflicted =
      api.rebind_polymarket_instrument(1, request);
  REQUIRE(conflicted);
  const auto conflict_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.size() < 4 &&
         std::chrono::steady_clock::now() < conflict_deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  REQUIRE(capture.values.size() == 4);
  REQUIRE(capture.values.back().command_result.kind ==
          oms::api::RuntimeCommandResultKind::RebindInstrument);
  REQUIRE(capture.values.back().command_result.error ==
          oms::api::Error::Conflict);
}

void TestPreparedWithoutRegistry(oms::api::ExecutionMode mode) {
  const auto instrument = Instrument();
  auto created =
      oms::api::OmsApi::Create(Config(mode), {&instrument, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 99));
  oms::api::PreparedOrderRequest prepared{};
  prepared.order = Order();
  prepared.order.instrument_id = 777;
  constexpr char client[] = "prepared-catalog-only";
  std::memcpy(prepared.order.client_order_id.value.data(), client,
              sizeof(client) - 1);
  prepared.order.client_order_id.length = sizeof(client) - 1;
  prepared.routing.kind = oms::api::ExecutionRouteKind::Generic;
  prepared.routing.venue =
      static_cast<std::uint8_t>(utils::md::Venue::Binance);
  prepared.routing.product_type =
      static_cast<std::uint8_t>(utils::md::ProductType::Spot);
  prepared.routing.price_scale = 2;
  prepared.routing.quantity_scale = 2;
  prepared.routing.tick_size = 1;
  prepared.routing.lot_size = 1;
  prepared.routing.catalog_generation = 41;
  const auto submitted = api.submit_prepared_order(1, prepared);
  REQUIRE(submitted);

  Capture capture;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.empty() &&
         std::chrono::steady_clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  REQUIRE(!capture.values.empty());
  REQUIRE(capture.values.front().kind ==
          oms::api::RuntimeUpdateKind::Order);
  REQUIRE(capture.values.front().order.type ==
          oms::api::UpdateType::Submitted);
}

}  // namespace

int main() {
  const auto loaded = oms::runtime::ReplayScript::Load(
      std::string(OMS_REPLAY_FIXTURE_DIR) + "/basic_normalized.replay");
  REQUIRE(loaded);
  std::cerr << "replay steps=" << loaded.value.steps().size() << '\n';
  for (const auto& step : loaded.value.steps())
    std::cerr << "control=" << static_cast<int>(step.control)
              << " event=" << static_cast<int>(step.event.type)
              << " token=" << step.event.token.sequence << '\n';
  const auto inline_updates =
      Run(oms::api::ExecutionMode::Inline, loaded.value.steps());
  const auto dedicated_updates =
      Run(oms::api::ExecutionMode::DedicatedIo, loaded.value.steps());
  REQUIRE(inline_updates.size() == dedicated_updates.size());
  REQUIRE(std::memcmp(inline_updates.data(), dedicated_updates.data(),
                      inline_updates.size() *
                          sizeof(oms::api::RuntimeUpdate)) == 0);
  TestRebind(oms::api::ExecutionMode::Inline);
  TestRebind(oms::api::ExecutionMode::DedicatedIo);
  TestPreparedWithoutRegistry(oms::api::ExecutionMode::Inline);
  TestPreparedWithoutRegistry(oms::api::ExecutionMode::DedicatedIo);
}
