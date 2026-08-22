#include <chrono>
#include <atomic>
#include <array>
#include <cstring>
#include <iostream>
#include <stdexcept>
#include <string>
#include <thread>
#include <mutex>
#include <vector>

#include <poll.h>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <time.h>
#include <unistd.h>

#include "oms/api/execution_channel.h"

namespace {

#define REQUIRE(value)                                                        \
  do {                                                                        \
    if (!(value))                                                             \
      throw std::runtime_error(std::string("require failed: ") + #value);     \
  } while (false)

std::uint64_t NowNs() {
  timespec now{};
  REQUIRE(::clock_gettime(CLOCK_MONOTONIC, &now) == 0);
  return static_cast<std::uint64_t>(now.tv_sec) * 1'000'000'000ULL +
         static_cast<std::uint64_t>(now.tv_nsec);
}

oms::api::RuntimeConfig Config(oms::api::ExecutionMode mode,
                               std::uint32_t update_capacity = 8,
                               std::uint32_t lanes = 2) {
  oms::api::RuntimeConfig config{};
  config.mode = mode;
  config.lane_count = lanes;
  config.deadline_capacity = 16;
  for (std::uint32_t index = 0; index < lanes; ++index) {
    config.lanes[index].lane_id = index + 1;
    config.lanes[index].command_capacity = 8;
    config.lanes[index].update_capacity = update_capacity;
  }
  return config;
}

oms::api::InstrumentInit Instrument() {
  oms::api::InstrumentInit result{};
  result.instrument.instrument_id = 7;
  result.instrument.venue = utils::md::Venue::Binance;
  result.instrument.product_type = utils::md::ProductType::Spot;
  result.instrument.price_scale = 2;
  result.instrument.quantity_scale = 2;
  result.instrument.tick_size = 1;
  result.instrument.lot_size = 1;
  constexpr char key[] = "1:1:TEST";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  return result;
}

oms::api::InstrumentInit UsdmInstrument() {
  auto result = Instrument();
  result.instrument.instrument_id = 8;
  result.instrument.product_type = utils::md::ProductType::Perpetual;
  result.instrument.instrument_key.fill('\0');
  constexpr char key[] = "1:2:TESTPERP";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  return result;
}

oms::api::InstrumentInit PolymarketInstrument(std::uint8_t signature_type = 0) {
  auto result = Instrument();
  result.instrument.instrument_id = 9;
  result.instrument.venue = utils::md::Venue::Polymarket;
  result.instrument.product_type = utils::md::ProductType::BinaryOption;
  result.instrument.instrument_key.fill('\0');
  constexpr char key[] = "6:4:YES";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  result.polymarket_condition_id[0] = 1;
  result.polymarket_token_id[0] = 1;
  result.polymarket_outcome = oms::api::PolymarketOutcome::Yes;
  result.polymarket_signature_type = signature_type;
  result.minimum_order_size = 1;
  return result;
}

oms::api::PolymarketCredentialView PolymarketCredentials() {
  return {
      "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1",
      "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1",
      "4f3edf983ac63ad7c7a0f4a1c2e8b7f5f6f0f4f0a3a5b6c7d8e9f00112233445",
      "offline-key",
      "c2VjcmV0LWtleQ",
      "offline-passphrase",
  };
}

oms::api::ExecutionChannelConfig LiveConfig() {
  oms::api::ExecutionChannelConfig config{};
  config.runtime = Config(oms::api::ExecutionMode::Inline, 8, 1);
  config.binance_spot.account_id = 1;
  config.binance_usdm.account_id = 2;
  config.polymarket.account_id = 3;
  return config;
}

oms::api::NewOrderRequest Order(std::uint64_t id,
                                oms::api::TimeInForce tif =
                                    oms::api::TimeInForce::GTC) {
  oms::api::NewOrderRequest request{};
  const std::string client = "runtime-" + std::to_string(id);
  std::memcpy(request.client_order_id.value.data(), client.data(), client.size());
  request.client_order_id.length =
      static_cast<std::uint16_t>(client.size());
  request.instrument_id = 7;
  request.side = oms::api::Side::Buy;
  request.type = oms::api::OrderType::Limit;
  request.time_in_force = tif;
  request.quantity = {10, 2, {}};
  request.price = {50, 2, {}};
  if (tif == oms::api::TimeInForce::GTD)
    request.expire_time_ns = NowNs() + 2'000'000ULL;
  return request;
}

struct Capture {
  std::vector<oms::api::RuntimeUpdate> values;
  std::thread::id callback_thread{};
  static void Add(void* context,
                  const oms::api::RuntimeUpdate& update) noexcept {
    auto& capture = *static_cast<Capture*>(context);
    capture.callback_thread = std::this_thread::get_id();
    capture.values.push_back(update);
  }
};

class ContractAdapter final : public oms::exchange::TradeAdapter {
 public:
  oms::exchange::AdapterIdentity identity() const noexcept override {
    return {oms::exchange::AdapterKind::BinanceSpot, 0,
            utils::md::Venue::Binance, utils::md::ProductType::Spot, {}};
  }
  oms::exchange::AdapterStatus status() const noexcept override {
    return oms::exchange::AdapterStatus::Ready;
  }
  oms::exchange::AdapterCapabilities capabilities() const noexcept override {
    return {static_cast<std::uint64_t>(
                oms::exchange::AdapterCapability::Limit) |
            static_cast<std::uint64_t>(
                oms::exchange::AdapterCapability::TifGtc) |
            static_cast<std::uint64_t>(
                oms::exchange::AdapterCapability::TifGtd) |
            static_cast<std::uint64_t>(
                oms::exchange::AdapterCapability::ReconcileOpenOrders)};
  }
  oms::exchange::AdapterResult reserve_command(
      oms::exchange::AdapterCommandKind kind,
      oms::exchange::AdapterReservation& output) noexcept override {
    if (reserved_) return oms::exchange::AdapterResult::WouldBlock;
    reserved_ = true;
    reserved_kind_ = kind;
    output = {oms::exchange::AdapterKind::BinanceSpot, {}, 0, ++generation_};
    return oms::exchange::AdapterResult::Ok;
  }
  void cancel_reservation(
      oms::exchange::AdapterReservation reservation) noexcept override {
    if (reserved_ && reservation.generation == generation_) {
      reserved_ = false;
      canceled.fetch_add(1, std::memory_order_relaxed);
    }
  }
  oms::exchange::AdapterResult commit_place(
      oms::exchange::AdapterReservation reservation,
      const oms::exchange::AdapterPlaceCommand& command) noexcept override {
    if (!reserved_ ||
        reserved_kind_ != oms::exchange::AdapterCommandKind::Place ||
        reservation.generation != generation_)
      return oms::exchange::AdapterResult::StaleReservation;
    reserved_ = false;
    {
      std::lock_guard lock(mutex_);
      handle_ = command.handle;
      token_ = command.request.token;
    }
    committed.store(true, std::memory_order_release);
    return oms::exchange::AdapterResult::Ok;
  }
  oms::exchange::AdapterResult commit_cancel(
      oms::exchange::AdapterReservation reservation,
      const oms::exchange::AdapterCancelCommand&) noexcept override {
    if (!reserved_ ||
        reserved_kind_ != oms::exchange::AdapterCommandKind::Cancel ||
        reservation.generation != generation_)
      return oms::exchange::AdapterResult::StaleReservation;
    reserved_ = false;
    return oms::exchange::AdapterResult::Ok;
  }
  oms::exchange::AdapterServiceResult service_io(
      std::uint64_t, std::uint32_t budget,
      const oms::exchange::AdapterEventSink& sink) noexcept override {
    oms::exchange::AdapterServiceResult result{};
    std::lock_guard lock(mutex_);
    if (!pending_ || budget == 0) return result;
    const auto offered = sink.on_event(sink.context, event_);
    if (offered != oms::exchange::AdapterResult::Ok) {
      result.result = offered;
      return result;
    }
    pending_ = false;
    result.events_processed = 1;
    return result;
  }
  oms::exchange::AdapterResult on_deadline(
      const oms::exchange::AdapterDeadline&, std::uint64_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    return oms::exchange::AdapterResult::Unsupported;
  }
  oms::exchange::AdapterResult begin_reconcile(
      std::uint64_t generation, std::uint64_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    if (generation == 0) return oms::exchange::AdapterResult::InvalidArgument;
    reconciles.fetch_add(1, std::memory_order_release);
    return oms::exchange::AdapterResult::Ok;
  }
  oms::exchange::AdapterResult shutdown(
      std::uint64_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    return oms::exchange::AdapterResult::Ok;
  }

  void PushAck() {
    std::lock_guard lock(mutex_);
    event_ = {};
    event_.kind = oms::exchange::AdapterEventKind::Venue;
    event_.source = identity();
    event_.venue.type = oms::api::VenueEventType::NewAck;
    event_.venue.handle = handle_;
    event_.venue.token = token_;
    pending_ = true;
  }
  bool pending() {
    std::lock_guard lock(mutex_);
    return pending_;
  }

  std::atomic<bool> committed{false};
  std::atomic<unsigned> canceled{0};
  std::atomic<unsigned> reconciles{0};

 private:
  std::mutex mutex_;
  oms::exchange::AdapterEvent event_{};
  oms::api::OrderHandle handle_{};
  oms::api::RequestToken token_{};
  std::uint64_t generation_{};
  oms::exchange::AdapterCommandKind reserved_kind_{};
  bool reserved_{};
  bool pending_{};
};

class ContractIoDriver final : public oms::exchange::AsyncIoDriver {
 public:
  ContractIoDriver() {
    fd_ = ::eventfd(0, EFD_CLOEXEC | EFD_NONBLOCK);
    REQUIRE(fd_ >= 0);
  }
  ~ContractIoDriver() override { ::close(fd_); }

  std::size_t descriptor_capacity() const noexcept override { return 1; }
  std::size_t snapshot_descriptors(
      std::span<oms::exchange::AsyncIoDescriptor> output) const noexcept
      override {
    if (output.empty()) return 0;
    output[0] = {fd_, EPOLLIN, generation_.load(std::memory_order_acquire)};
    return 1;
  }
  void service_io(int fd, std::uint64_t generation, std::uint32_t events,
                  std::uint64_t) noexcept override {
    if (fd != fd_ || generation != generation_.load(std::memory_order_acquire) ||
        (events & EPOLLIN) == 0) {
      stale_calls.fetch_add(1, std::memory_order_release);
      return;
    }
    eventfd_t value{};
    (void)::eventfd_read(fd_, &value);
    readiness_calls.fetch_add(1, std::memory_order_release);
    serviced_generation.store(generation, std::memory_order_release);
  }
  void check_timeouts(std::uint64_t now_ns) noexcept override {
    std::uint64_t due = deadline_ns.load(std::memory_order_acquire);
    if (due != 0 && due <= now_ns &&
        deadline_ns.compare_exchange_strong(due, 0,
                                            std::memory_order_acq_rel)) {
      timeout_calls.fetch_add(1, std::memory_order_release);
    }
  }
  std::uint64_t next_deadline_ns() const noexcept override {
    return deadline_ns.load(std::memory_order_acquire);
  }

  void ReplaceAndSignal() {
    generation_.fetch_add(1, std::memory_order_acq_rel);
    REQUIRE(::eventfd_write(fd_, 1) == 0);
  }

  std::atomic<unsigned> readiness_calls{0};
  std::atomic<unsigned> stale_calls{0};
  std::atomic<unsigned> timeout_calls{0};
  std::atomic<std::uint64_t> serviced_generation{0};
  std::atomic<std::uint64_t> deadline_ns{0};

 private:
  int fd_{-1};
  std::atomic<std::uint64_t> generation_{1};
};

void Pump(oms::api::OmsApi& api, oms::api::ExecutionMode mode,
          std::uint32_t lane, Capture& capture, std::size_t target,
          int timeout_ms = 1000) {
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
  while (capture.values.size() < target &&
         std::chrono::steady_clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline) {
      REQUIRE(api.service_io(1) == oms::api::Error::Ok);
    } else {
      pollfd descriptor{api.update_fd(lane), POLLIN, 0};
      (void)::poll(&descriptor, 1, 1);
    }
    (void)api.drain_updates(lane, &Capture::Add, &capture);
  }
  if (capture.values.size() < target) {
    const auto metrics = api.metrics();
    std::cerr << "pump timeout lane=" << lane << " have="
              << capture.values.size() << " target=" << target
              << " command_depth=" << metrics.lanes[lane - 1].command_queue.depth
              << " update_depth=" << metrics.lanes[lane - 1].update_queue.depth
              << '\n';
  }
  REQUIRE(capture.values.size() >= target);
}

void TestMode(oms::api::ExecutionMode mode) {
  const auto instrument = Instrument();
  const auto created = oms::api::OmsApi::Create(Config(mode), {&instrument, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 101));
  REQUIRE(api.initialize_lane(2, 202));
  REQUIRE(!api.initialize_lane(1, 303));
  if (mode == oms::api::ExecutionMode::DedicatedIo) {
    REQUIRE(api.service_io(0) == oms::api::Error::InvalidMode);
    REQUIRE(api.update_fd(1) >= 0);
  } else {
    REQUIRE(api.update_fd(1) == -1);
  }

  const auto first = api.submit_order(1, Order(1));
  const auto second = api.submit_order(2, Order(2));
  REQUIRE(first && second);
  Capture lane1;
  Capture lane2;
  Pump(api, mode, 1, lane1, 1);
  Pump(api, mode, 2, lane2, 1);
  REQUIRE(lane1.values[0].kind == oms::api::RuntimeUpdateKind::Order);
  REQUIRE(lane1.values[0].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(lane1.values[0].order.token == first.value);
  REQUIRE(lane1.callback_thread == std::this_thread::get_id());
  REQUIRE(lane2.values[0].order.token == second.value);

  const auto cancel = api.cancel_order(1, first.value);
  REQUIRE(cancel);
  Pump(api, mode, 1, lane1, 3);
  REQUIRE(lane1.values[1].order.type ==
          oms::api::UpdateType::CancelRequested);
  REQUIRE(lane1.values[2].kind ==
          oms::api::RuntimeUpdateKind::CommandResult);
  REQUIRE(lane1.values[2].command_result.correlation.target_token ==
          first.value);
  REQUIRE(lane1.values[2].command_result.correlation.request_token ==
          cancel.value);

  const auto gtd = api.submit_order(2, Order(3, oms::api::TimeInForce::GTD));
  REQUIRE(gtd);
  Pump(api, mode, 2, lane2, 3);
  REQUIRE(lane2.values.back().order.type == oms::api::UpdateType::Expired);

  const auto metrics = api.metrics();
  REQUIRE(metrics.lane_count == 2);
  REQUIRE(metrics.lanes[0].enqueue_to_owner.sample_count >= 2);
  REQUIRE(metrics.lanes[0].owner_to_drain.sample_count >= 3);
  REQUIRE(api.shutdown() == oms::api::Error::Ok);
  REQUIRE(api.submit_order(1, Order(9)).error ==
          oms::api::Error::ShuttingDown);
}

void TestReserveBeforeMutate() {
  const auto instrument = Instrument();
  const auto created = oms::api::OmsApi::Create(
      Config(oms::api::ExecutionMode::Inline, 2, 1), {&instrument, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 1));
  const auto placed = api.submit_order(1, Order(10));
  REQUIRE(placed);

  oms::api::ReplayStep ack{};
  ack.event.type = oms::api::VenueEventType::NewAck;
  ack.event.token = placed.value;
  REQUIRE(api.inject(ack) == oms::api::Error::Ok);
  REQUIRE(api.service_io(0) == oms::api::Error::Ok);

  Capture capture;
  REQUIRE(api.drain_updates(1, &Capture::Add, &capture, 1) == 1);
  REQUIRE(capture.values[0].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(api.service_io(0) == oms::api::Error::Ok);
  REQUIRE(api.drain_updates(1, &Capture::Add, &capture) == 1);
  REQUIRE(capture.values[1].order.type == oms::api::UpdateType::Accepted);

  oms::api::ReplayStep unmatched{};
  unmatched.event.type = oms::api::VenueEventType::Fill;
  unmatched.event.fill_quantity = {1, 0, {}};
  unmatched.event.fill_price = {1, 0, {}};
  REQUIRE(api.inject(unmatched) == oms::api::Error::Ok);
  REQUIRE(api.service_io(0) == oms::api::Error::Ok);
  const auto metrics = api.metrics();
  REQUIRE(metrics.unmatched_venue_events == 1);
  REQUIRE(metrics.unmatched_fills == 1);
}

void TestAccountRoutedQueryUnsupported() {
  const auto instrument = Instrument();
  ContractAdapter adapter;
  std::array<oms::exchange::TradeAdapter*, 1> adapters{&adapter};
  const std::array<oms::api::AdapterRuntimeConfig::AccountRoute, 1> routes{{
      {41, oms::exchange::AdapterKind::BinanceSpot},
  }};
  oms::api::AdapterRuntimeConfig adapter_config{};
  adapter_config.adapters = adapters;
  adapter_config.enable_fake_fallback = false;
  adapter_config.account_routes = routes;
  const auto created = oms::api::OmsApi::Create(
      Config(oms::api::ExecutionMode::Inline, 8, 1), {&instrument, 1}, {},
      adapter_config);
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 3));
  const auto query = api.query_open_orders(1, 41);
  REQUIRE(query);
  const auto missing = api.query_positions(1, 99);
  REQUIRE(!missing);
  REQUIRE(missing.error == oms::api::Error::NotFound);
  Capture capture;
  REQUIRE(api.drain_updates(1, &Capture::Add, &capture) == 1);
  REQUIRE(capture.values[0].kind ==
          oms::api::RuntimeUpdateKind::QueryComplete);
  REQUIRE(capture.values[0].query_complete.query_token == query.value);
  REQUIRE(capture.values[0].query_complete.account_id == 41);
  REQUIRE(capture.values[0].query_complete.error ==
          oms::api::Error::Unsupported);
}

void TestInlineTimeoutsAndThreadGuard() {
  const auto instrument = Instrument();
  const auto created = oms::api::OmsApi::Create(
      Config(oms::api::ExecutionMode::Inline, 8, 1), {&instrument, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 5));
  REQUIRE(api.service_io(0) == oms::api::Error::Ok);

  const auto before = std::chrono::steady_clock::now();
  REQUIRE(api.service_io(2) == oms::api::Error::Ok);
  REQUIRE(std::chrono::steady_clock::now() - before >=
          std::chrono::milliseconds(1));

  oms::api::Error foreign_result = oms::api::Error::Ok;
  std::thread foreign([&] { foreign_result = api.service_io(0); });
  foreign.join();
  REQUIRE(foreign_result == oms::api::Error::InvalidTransition);

  oms::api::Error wake_result = oms::api::Error::NotReady;
  std::thread wake([&] {
    std::this_thread::sleep_for(std::chrono::milliseconds(2));
    oms::api::ReplayStep step{};
    step.control = oms::api::ReplayControl::Reconnect;
    wake_result = api.inject(step);
  });
  REQUIRE(api.service_io(-1) == oms::api::Error::Ok);
  wake.join();
  REQUIRE(wake_result == oms::api::Error::Ok);
}

void TestOwnedIoDriver(oms::api::ExecutionMode mode) {
  const auto instrument = Instrument();
  ContractIoDriver driver;
  driver.deadline_ns.store(NowNs() + 2'000'000ULL,
                           std::memory_order_release);
  std::array<oms::exchange::AsyncIoDriver*, 1> drivers{&driver};
  oms::api::AdapterRuntimeConfig adapter_config{};
  adapter_config.io_drivers = drivers;
  const auto created = oms::api::OmsApi::Create(
      Config(mode, 8, 1), {&instrument, 1}, {}, adapter_config);
  REQUIRE(created);
  auto& api = *created.value;

  if (mode == oms::api::ExecutionMode::Inline) {
    REQUIRE(api.service_io(-1) == oms::api::Error::Ok);
  } else {
    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::seconds(1);
    while (driver.timeout_calls.load(std::memory_order_acquire) == 0 &&
           std::chrono::steady_clock::now() < deadline) {
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
  }
  REQUIRE(driver.timeout_calls.load(std::memory_order_acquire) == 1);

  std::thread replace([&driver] {
    std::this_thread::sleep_for(std::chrono::milliseconds(2));
    driver.ReplaceAndSignal();
  });
  if (mode == oms::api::ExecutionMode::Inline) {
    REQUIRE(api.service_io(-1) == oms::api::Error::Ok);
    REQUIRE(api.service_io(10) == oms::api::Error::Ok);
  }
  replace.join();
  const auto readiness_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (driver.readiness_calls.load(std::memory_order_acquire) == 0 &&
         std::chrono::steady_clock::now() < readiness_deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(1) == oms::api::Error::Ok);
    else
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  REQUIRE(driver.readiness_calls.load(std::memory_order_acquire) == 1);
  REQUIRE(driver.stale_calls.load(std::memory_order_acquire) == 0);
  REQUIRE(driver.serviced_generation.load(std::memory_order_acquire) == 2);
}

void TestBlockedLaneDoesNotStarvePeers() {
  const auto instrument = Instrument();
  const auto created = oms::api::OmsApi::Create(
      Config(oms::api::ExecutionMode::DedicatedIo, 2, 2),
      {&instrument, 1});
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 11));
  REQUIRE(api.initialize_lane(2, 22));
  REQUIRE(api.submit_order(1, Order(19, oms::api::TimeInForce::GTD)).error ==
          oms::api::Error::QueueFull);

  const auto first = api.submit_order(1, Order(20));
  REQUIRE(first);
  REQUIRE(api.cancel_order(1, first.value));
  REQUIRE(api.submit_order(2, Order(21)));

  Capture lane2;
  Pump(api, oms::api::ExecutionMode::DedicatedIo, 2, lane2, 1, 5000);
  REQUIRE(lane2.values[0].order.type == oms::api::UpdateType::Submitted);

  Capture lane1;
  Pump(api, oms::api::ExecutionMode::DedicatedIo, 1, lane1, 1, 5000);
  Pump(api, oms::api::ExecutionMode::DedicatedIo, 1, lane1, 3, 5000);
  REQUIRE(lane1.values[0].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(lane1.values[1].order.type ==
          oms::api::UpdateType::CancelRequested);
  REQUIRE(lane1.values[2].kind ==
          oms::api::RuntimeUpdateKind::CommandResult);
}

void TestInvalidModeRejected() {
  const auto instrument = Instrument();
  auto config = Config(oms::api::ExecutionMode::Inline, 8, 1);
  config.mode = static_cast<oms::api::ExecutionMode>(255);
  REQUIRE(oms::api::OmsApi::Create(config, {&instrument, 1}).error ==
          oms::api::Error::InvalidMode);
}

void TestAdapterCountAndSignatureValidation() {
  const auto instrument = Instrument();
  ContractAdapter adapter;
  std::array<oms::exchange::TradeAdapter*, 5> adapters{
      &adapter, &adapter, &adapter, &adapter, &adapter};
  const oms::api::AdapterRuntimeConfig too_many{adapters, false, 8};
  REQUIRE(oms::api::OmsApi::Create(
              Config(oms::api::ExecutionMode::Inline, 8, 1),
              {&instrument, 1}, {}, too_many)
              .error == oms::api::Error::InvalidArgument);

  auto polymarket = Instrument();
  polymarket.instrument.venue = utils::md::Venue::Polymarket;
  polymarket.instrument.product_type = utils::md::ProductType::BinaryOption;
  polymarket.polymarket_signature_type = 1;
  REQUIRE(oms::api::OmsApi::Create(
              Config(oms::api::ExecutionMode::Inline, 8, 1),
              {&polymarket, 1})
              .error == oms::api::Error::InvalidArgument);
}

void TestExecutionChannelFacade() {
  const auto instrument = Instrument();
  const auto created = oms::api::ExecutionChannel::Create(
      Config(oms::api::ExecutionMode::Inline, 8, 1), {&instrument, 1});
  REQUIRE(created);
  auto& channel = *created.value;
  REQUIRE(channel.initialize_lane(1, 77));
  REQUIRE(channel.notification_fd(1) == -1);
  REQUIRE(channel.place_order(1, Order(50)));
  REQUIRE(channel.service_io(0) == oms::api::Error::Ok);
  Capture capture;
  REQUIRE(channel.drain_updates(1, &Capture::Add, &capture) != 0);
  REQUIRE(channel.shutdown() == oms::api::Error::Ok);
}

struct BinanceProviderContext {
  std::string api_key{"provider-api-key"};
  std::string secret_key{"provider-secret-key"};
  std::size_t calls{};

  static bool Load(void* context,
                   oms::api::BinanceCredentialView& output) noexcept {
    auto& self = *static_cast<BinanceProviderContext*>(context);
    ++self.calls;
    output = {self.api_key, self.secret_key};
    return true;
  }
};

void TestExecutionChannelLiveValidationAndOwnership() {
  const auto instrument = Instrument();

  auto no_venues = LiveConfig();
  REQUIRE(oms::api::ExecutionChannel::Create(no_venues, {&instrument, 1})
              .error == oms::api::Error::InvalidArgument);

  auto invalid_endpoint = LiveConfig();
  invalid_endpoint.binance_spot.enabled = true;
  invalid_endpoint.binance_spot.credentials = {"key", "secret"};
  invalid_endpoint.binance_spot.endpoints.rest.host =
      "https://api.binance.com/path";
  REQUIRE(oms::api::ExecutionChannel::Create(invalid_endpoint,
                                             {&instrument, 1})
              .error == oms::api::Error::InvalidArgument);

  auto invalid_service = LiveConfig();
  invalid_service.binance_spot.enabled = true;
  invalid_service.binance_spot.credentials = {"key", "secret"};
  invalid_service.binance_spot.endpoints.websocket.service = "70000";
  REQUIRE(oms::api::ExecutionChannel::Create(invalid_service,
                                             {&instrument, 1})
              .error == oms::api::Error::InvalidArgument);

  auto invalid_trading_service = LiveConfig();
  invalid_trading_service.binance_spot.enabled = true;
  invalid_trading_service.binance_spot.credentials = {"key", "secret"};
  invalid_trading_service.binance_spot.trading_websocket.service = "70000";
  REQUIRE(oms::api::ExecutionChannel::Create(invalid_trading_service,
                                             {&instrument, 1})
              .error == oms::api::Error::InvalidArgument);

  BinanceProviderContext provider;
  auto provided = LiveConfig();
  provided.binance_spot.enabled = true;
  provided.binance_spot.credential_provider =
      &BinanceProviderContext::Load;
  provided.binance_spot.credential_context = &provider;
  const auto created =
      oms::api::ExecutionChannel::Create(provided, {&instrument, 1});
  REQUIRE(created);
  REQUIRE(provider.calls == 1);
  provider.api_key.assign("changed");
  provider.secret_key.clear();
  REQUIRE(created.value->venue_status(
              oms::exchange::AdapterKind::BinanceSpot)
              .value.status == oms::exchange::AdapterStatus::Authenticating);
  REQUIRE(created.value->shutdown() == oms::api::Error::Ok);
}

void TestExecutionChannelLiveMultiAdapterAndTeardown() {
  std::array instruments{Instrument(), UsdmInstrument(),
                         PolymarketInstrument()};
  auto config = LiveConfig();
  config.binance_spot.enabled = true;
  config.binance_spot.credentials = {"spot-key", "spot-secret"};
  config.binance_usdm.enabled = true;
  config.binance_usdm.credentials = {"usdm-key", "usdm-secret"};
  config.polymarket.enabled = true;
  config.polymarket.credentials = PolymarketCredentials();

  auto created = oms::api::ExecutionChannel::Create(config, instruments);
  REQUIRE(created);
  REQUIRE(created.value->venue_status(
              oms::exchange::AdapterKind::BinanceSpot));
  REQUIRE(created.value->venue_status(
              oms::exchange::AdapterKind::BinanceUsdm));
  REQUIRE(created.value->venue_status(
              oms::exchange::AdapterKind::Polymarket));
  REQUIRE(created.value->shutdown() == oms::api::Error::Ok);
  created.value.reset();

  instruments[2] = PolymarketInstrument(1);
  REQUIRE(oms::api::ExecutionChannel::Create(config, instruments).error ==
          oms::api::Error::InvalidArgument);
}

void TestIntegratedAdapter(oms::api::ExecutionMode mode) {
  const auto instrument = Instrument();
  auto config = Config(mode, 2, 1);
  ContractAdapter adapter;
  std::array<oms::exchange::TradeAdapter*, 1> adapters{&adapter};
  const oms::api::AdapterRuntimeConfig adapter_config{adapters, false, 8};
  const auto created =
      oms::api::OmsApi::Create(config, {&instrument, 1}, {}, adapter_config);
  REQUIRE(created);
  auto& api = *created.value;
  REQUIRE(api.initialize_lane(1, 91));
  const auto snapshot =
      api.adapter_status(oms::exchange::AdapterKind::BinanceSpot);
  REQUIRE(snapshot);
  REQUIRE(snapshot.value.status == oms::exchange::AdapterStatus::Ready);

  const auto submitted = api.submit_order(1, Order(40));
  REQUIRE(submitted);
  const auto commit_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (!adapter.committed.load(std::memory_order_acquire) &&
         std::chrono::steady_clock::now() < commit_deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    else
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  REQUIRE(adapter.committed.load(std::memory_order_acquire));

  // Submitted occupies one of two output slots. Venue processing requires the
  // worst-case two slots and therefore must retain the adapter event.
  adapter.PushAck();
  if (mode == oms::api::ExecutionMode::Inline)
    REQUIRE(api.service_io(0) == oms::api::Error::Ok);
  else
    std::this_thread::sleep_for(std::chrono::milliseconds(5));
  REQUIRE(adapter.pending());

  Capture capture;
  REQUIRE(api.drain_updates(1, &Capture::Add, &capture, 1) == 1);
  Pump(api, mode, 1, capture, 2);
  REQUIRE(capture.values[0].order.type == oms::api::UpdateType::Submitted);
  REQUIRE(capture.values[1].order.type == oms::api::UpdateType::Accepted);
  REQUIRE(!adapter.pending());

  auto invalid = Order(41);
  invalid.quantity.value = 0;
  REQUIRE(api.submit_order(1, invalid));
  Pump(api, mode, 1, capture, 3);
  REQUIRE(capture.values.back().kind ==
          oms::api::RuntimeUpdateKind::CommandResult);
  REQUIRE(capture.values.back().command_result.error ==
          oms::api::Error::InvalidArgument);
  REQUIRE(adapter.canceled.load(std::memory_order_relaxed) == 1);

  // Capability rejection happens before reservation and StateEngine insertion:
  // only the command error is published, never a Submitted/Rejected pair.
  const std::size_t before_unsupported = capture.values.size();
  auto unsupported = Order(42);
  unsupported.type = oms::api::OrderType::Market;
  unsupported.price.value = 0;
  REQUIRE(api.submit_order(1, unsupported));
  Pump(api, mode, 1, capture, before_unsupported + 1);
  REQUIRE(capture.values.size() == before_unsupported + 1);
  REQUIRE(capture.values.back().kind ==
          oms::api::RuntimeUpdateKind::CommandResult);
  REQUIRE(capture.values.back().command_result.error ==
          oms::api::Error::Unsupported);
  REQUIRE(adapter.canceled.load(std::memory_order_relaxed) == 1);

  REQUIRE(api.reconcile(oms::exchange::AdapterKind::BinanceSpot) ==
          oms::api::Error::Ok);
  const auto reconcile_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(1);
  while (adapter.reconciles.load(std::memory_order_acquire) == 0 &&
         std::chrono::steady_clock::now() < reconcile_deadline) {
    if (mode == oms::api::ExecutionMode::Inline)
      REQUIRE(api.service_io(0) == oms::api::Error::Ok);
    else
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  REQUIRE(adapter.reconciles.load(std::memory_order_acquire) == 1);
}

}  // namespace

int main() {
  TestMode(oms::api::ExecutionMode::Inline);
  TestMode(oms::api::ExecutionMode::DedicatedIo);
  TestReserveBeforeMutate();
  TestAccountRoutedQueryUnsupported();
  TestInlineTimeoutsAndThreadGuard();
  TestOwnedIoDriver(oms::api::ExecutionMode::Inline);
  TestOwnedIoDriver(oms::api::ExecutionMode::DedicatedIo);
  TestBlockedLaneDoesNotStarvePeers();
  TestInvalidModeRejected();
  TestAdapterCountAndSignatureValidation();
  TestExecutionChannelFacade();
  TestExecutionChannelLiveValidationAndOwnership();
  TestExecutionChannelLiveMultiAdapterAndTeardown();
  TestIntegratedAdapter(oms::api::ExecutionMode::Inline);
  TestIntegratedAdapter(oms::api::ExecutionMode::DedicatedIo);
}
