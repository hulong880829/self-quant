#include <chrono>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <memory>
#include <span>
#include <thread>

#include <poll.h>

#include "oms/api/execution_channel.h"

namespace {

constexpr std::uint32_t kLaneId = 1;

oms::api::InstrumentInit MakeInstrument() noexcept {
  oms::api::InstrumentInit result{};
  result.instrument.instrument_id = 42;
  result.instrument.venue = utils::md::Venue::Binance;
  result.instrument.product_type = utils::md::ProductType::Spot;
  result.instrument.price_scale = 2;
  result.instrument.quantity_scale = 3;
  result.instrument.tick_size = 1;
  result.instrument.lot_size = 1;
  constexpr char key[] = "1:1:STRATEGY_FRAME";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  return result;
}

// A minimal stand-in for the StrategyFrame owner. The frame owns one lane and
// drains that lane only on its strategy thread, so callbacks may mutate
// strategy state without another lock.
class StrategyFrame {
 public:
  explicit StrategyFrame(oms::api::ExecutionMode mode) : mode_(mode) {}

  bool Start() {
    oms::api::RuntimeConfig config{};
    config.mode = mode_;
    config.lane_count = 1;
    config.deadline_capacity = 64;
    config.lanes[0] = {kLaneId, 256, 256, -1};

    instrument_ = MakeInstrument();
    auto created = oms::api::ExecutionChannel::Create(
        config, std::span{&instrument_, 1U});
    if (!created) return false;
    execution_ = std::move(created.value);
    return static_cast<bool>(execution_->initialize_lane(kLaneId, 1));
  }

  bool PlaceOne() {
    oms::api::SubmitOrderRequest submitted{};
    auto& request = submitted.order;
    constexpr char client_id[] = "strategy-frame-1";
    std::memcpy(request.client_order_id.value.data(), client_id,
                sizeof(client_id) - 1);
    request.client_order_id.length = sizeof(client_id) - 1;
    request.instrument_id = instrument_.instrument.instrument_id;
    request.side = oms::api::Side::Buy;
    request.type = oms::api::OrderType::Limit;
    request.time_in_force = oms::api::TimeInForce::GTC;
    request.quantity = {10, 3, {}};
    request.price = {25'000, 2, {}};
    submitted.routing.kind = oms::api::ExecutionRouteKind::Crypto;
    submitted.routing.venue =
        static_cast<std::uint8_t>(utils::md::Venue::Binance);
    submitted.routing.product_type =
        static_cast<std::uint8_t>(utils::md::ProductType::Spot);
    submitted.routing.price_scale = 2;
    submitted.routing.quantity_scale = 3;
    submitted.routing.catalog_revision = 1;
    submitted.routing.tick_size = 1;
    submitted.routing.lot_size = 1;
    constexpr char symbol[] = "STRATEGY_FRAME";
    std::memcpy(submitted.routing.crypto.venue_symbol.value.data(), symbol,
                sizeof(symbol) - 1);
    submitted.routing.crypto.venue_symbol.length = sizeof(symbol) - 1;

    const auto placed = execution_->place_order(kLaneId, submitted);
    if (!placed) return false;
    expected_ = placed.value;
    return true;
  }

  bool RunUntilSubmitted() {
    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::seconds(2);
    while (!submitted_ && std::chrono::steady_clock::now() < deadline) {
      if (mode_ == oms::api::ExecutionMode::Inline) {
        // Inline ownership is fixed to the thread that created the channel.
        // service_io must be called on that same thread.
        if (execution_->service_io(10) != oms::api::Error::Ok) return false;
      } else {
        // DedicatedIo owns adapter/state work on its worker. StrategyFrame
        // waits on the lane fd and never calls service_io in this mode.
        pollfd descriptor{execution_->notification_fd(kLaneId), POLLIN, 0};
        if (::poll(&descriptor, 1, 10) < 0) return false;
      }
      execution_->drain_updates(kLaneId, &StrategyFrame::OnUpdate, this);
    }
    return submitted_;
  }

  bool Stop() {
    // Stop strategy submissions first. Drain any already-published updates
    // before shutdown; shutdown is explicit and idempotent.
    execution_->drain_updates(kLaneId, &StrategyFrame::OnUpdate, this);
    return execution_->shutdown() == oms::api::Error::Ok;
  }

 private:
  static void OnUpdate(void* context,
                       const oms::api::ExecutionUpdate& update) noexcept {
    auto& frame = *static_cast<StrategyFrame*>(context);
    // This callback runs synchronously inside drain_updates, hence on the
    // StrategyFrame thread rather than the DedicatedIo worker.
    if (update.kind == oms::api::RuntimeUpdateKind::Order &&
        update.order.token == frame.expected_) {
      frame.submitted_ =
          update.order.type == oms::api::UpdateType::Submitted;
      std::cout << "order update type="
                << static_cast<unsigned>(update.order.type)
                << " sequence=" << update.order.token.sequence << '\n';
    }
  }

  oms::api::ExecutionMode mode_;
  oms::api::InstrumentInit instrument_{};
  std::unique_ptr<oms::api::ExecutionChannel> execution_;
  oms::api::RequestToken expected_{};
  bool submitted_{};
};

}  // namespace

int main() {
  // Change to DedicatedIo to use the worker + notification-fd integration.
  StrategyFrame frame(oms::api::ExecutionMode::Inline);
  if (!frame.Start() || !frame.PlaceOne() || !frame.RunUntilSubmitted() ||
      !frame.Stop()) {
    std::cerr << "StrategyFrame execution example failed\n";
    return 1;
  }
  return 0;
}
