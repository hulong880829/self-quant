#include <chrono>
#include <cstring>
#include <iostream>
#include <stdexcept>
#include <string>
#include <string_view>
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

oms::api::SubmitOrderRequest Order(
    std::uint64_t instrument_id = 9,
    std::string_view symbol = "DUAL") {
  oms::api::SubmitOrderRequest submitted{};
  auto& request = submitted.order;
  constexpr char client[] = "dual-client";
  std::memcpy(request.client_order_id.value.data(), client,
              sizeof(client) - 1);
  request.client_order_id.length = sizeof(client) - 1;
  request.instrument_id = instrument_id;
  request.side = oms::api::Side::Buy;
  request.type = oms::api::OrderType::Limit;
  request.time_in_force = oms::api::TimeInForce::GTC;
  request.quantity = {10, 2, {}};
  request.price = {50, 2, {}};
  submitted.routing.kind = oms::api::ExecutionRouteKind::Crypto;
  submitted.routing.venue =
      static_cast<std::uint8_t>(utils::md::Venue::Binance);
  submitted.routing.product_type =
      static_cast<std::uint8_t>(utils::md::ProductType::Spot);
  submitted.routing.price_scale = 2;
  submitted.routing.quantity_scale = 2;
  submitted.routing.catalog_revision = 1;
  submitted.routing.tick_size = 1;
  submitted.routing.lot_size = 1;
  std::memcpy(submitted.routing.crypto.venue_symbol.value.data(),
              symbol.data(), symbol.size());
  submitted.routing.crypto.venue_symbol.length =
      static_cast<std::uint16_t>(symbol.size());
  return submitted;
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

void TestRegisterSubmitRetireOrdering(oms::api::ExecutionMode mode) {
  const auto initial = Instrument();
  auto created = oms::api::OmsApi::Create(Config(mode), {&initial, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 88));

  constexpr oms::api::InstrumentId dynamic_id = 777;
  auto order = Order(dynamic_id, "DYNAMIC");
  constexpr char client[] = "dynamic-client";
  order.order.client_order_id = {};
  std::memcpy(order.order.client_order_id.value.data(), client,
              sizeof(client) - 1);
  order.order.client_order_id.length = sizeof(client) - 1;
  oms::api::RegisterInstrumentRequest registration{};
  registration.instrument_id = dynamic_id;
  registration.routing = order.routing;

  const std::uint64_t before = api.execution_directory_access_count();
  const auto registered = api.register_instrument(1, registration);
  const auto submitted = api.submit_order(1, order);
  const auto retired = api.retire_instrument(1, dynamic_id);
  REQUIRE(registered);
  REQUIRE(submitted);
  REQUIRE(retired);

  Capture capture;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.size() < 3 &&
         std::chrono::steady_clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  REQUIRE(capture.values.size() >= 3);
  REQUIRE(capture.values[0].kind ==
          oms::api::RuntimeUpdateKind::CommandResult);
  REQUIRE(capture.values[0].command_result.kind ==
          oms::api::RuntimeCommandResultKind::RegisterInstrument);
  REQUIRE(capture.values[0].command_result.error == oms::api::Error::Ok);
  REQUIRE(capture.values[1].kind == oms::api::RuntimeUpdateKind::Order);
  REQUIRE(capture.values[1].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(capture.values[2].kind ==
          oms::api::RuntimeUpdateKind::CommandResult);
  REQUIRE(capture.values[2].command_result.kind ==
          oms::api::RuntimeCommandResultKind::RetireInstrument);
  REQUIRE(capture.values[2].command_result.error ==
          oms::api::Error::Deferred);
  // Exactly Register and Retire touched the directory; Submit did not.
  REQUIRE(api.execution_directory_access_count() == before + 2);
}

void TestSubmitCancelDirectoryIsolation(oms::api::ExecutionMode mode) {
  const auto initial = Instrument();
  auto created = oms::api::OmsApi::Create(Config(mode), {&initial, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 99));
  const std::uint64_t before = api.execution_directory_access_count();
  const auto submitted = api.submit_order(1, Order());
  REQUIRE(submitted);
  REQUIRE(api.cancel_order(1, submitted.value));

  Capture capture;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (capture.values.size() < 3 &&
         std::chrono::steady_clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    (void)api.drain_updates(1, &Capture::Add, &capture);
  }
  REQUIRE(capture.values.size() >= 3);
  REQUIRE(capture.values[0].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(capture.values[1].order.type ==
          oms::api::UpdateType::CancelRequested);
  REQUIRE(capture.values[2].command_result.kind ==
          oms::api::RuntimeCommandResultKind::Cancel);
  REQUIRE(api.execution_directory_access_count() == before);
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
  TestRegisterSubmitRetireOrdering(oms::api::ExecutionMode::Inline);
  TestRegisterSubmitRetireOrdering(oms::api::ExecutionMode::DedicatedIo);
  TestSubmitCancelDirectoryIsolation(oms::api::ExecutionMode::Inline);
  TestSubmitCancelDirectoryIsolation(oms::api::ExecutionMode::DedicatedIo);
}
