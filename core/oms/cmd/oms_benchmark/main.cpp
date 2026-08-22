#include <algorithm>
#include <array>
#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <limits>
#include <span>
#include <string_view>
#include <vector>

#include <poll.h>
#include <time.h>

#include "oms/api/execution_channel.h"

namespace {

using Clock = std::chrono::steady_clock;

struct Options {
  std::uint32_t samples{5'000};
  std::uint32_t warmup{250};
  std::uint32_t idle_ms{50};
};

bool ParseUnsigned(std::string_view text, std::uint32_t& output) noexcept {
  std::uint32_t value{};
  const auto result =
      std::from_chars(text.data(), text.data() + text.size(), value);
  if (result.ec != std::errc{} || result.ptr != text.data() + text.size())
    return false;
  output = value;
  return true;
}

bool Parse(int argc, char** argv, Options& output) noexcept {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (index + 1 >= argc) return false;
    const std::string_view value(argv[++index]);
    if (argument == "--samples") {
      if (!ParseUnsigned(value, output.samples) || output.samples == 0)
        return false;
    } else if (argument == "--warmup") {
      if (!ParseUnsigned(value, output.warmup)) return false;
    } else if (argument == "--idle-ms") {
      if (!ParseUnsigned(value, output.idle_ms)) return false;
    } else {
      return false;
    }
  }
  return output.samples <= 1'000'000 && output.warmup <= 1'000'000 &&
         output.samples + output.warmup <= 1'000'000;
}

std::uint64_t ProcessCpuNs() noexcept {
  timespec value{};
  if (::clock_gettime(CLOCK_PROCESS_CPUTIME_ID, &value) != 0) return 0;
  return static_cast<std::uint64_t>(value.tv_sec) * 1'000'000'000ULL +
         static_cast<std::uint64_t>(value.tv_nsec);
}

std::uint32_t NextPowerOfTwo(std::uint32_t value) noexcept {
  if (value <= 2) return 2;
  --value;
  value |= value >> 1U;
  value |= value >> 2U;
  value |= value >> 4U;
  value |= value >> 8U;
  value |= value >> 16U;
  return value + 1U;
}

oms::api::InstrumentInit MakeInstrument() noexcept {
  oms::api::InstrumentInit result{};
  result.instrument.instrument_id = 7;
  result.instrument.venue = utils::md::Venue::Binance;
  result.instrument.product_type = utils::md::ProductType::Spot;
  result.instrument.price_scale = 2;
  result.instrument.quantity_scale = 2;
  result.instrument.tick_size = 1;
  result.instrument.lot_size = 1;
  constexpr char key[] = "1:1:OMS_BENCH";
  std::memcpy(result.instrument.instrument_key.data(), key, sizeof(key) - 1);
  return result;
}

oms::api::NewOrderRequest MakeOrder(std::uint64_t sequence) noexcept {
  oms::api::NewOrderRequest request{};
  constexpr std::string_view prefix = "bench-";
  std::memcpy(request.client_order_id.value.data(), prefix.data(),
              prefix.size());
  char* first = request.client_order_id.value.data() + prefix.size();
  char* last = request.client_order_id.value.data() +
               request.client_order_id.value.size();
  const auto converted = std::to_chars(first, last, sequence);
  request.client_order_id.length = static_cast<std::uint16_t>(
      converted.ec == std::errc{} ? converted.ptr -
                                        request.client_order_id.value.data()
                                  : prefix.size());
  request.instrument_id = 7;
  request.side = oms::api::Side::Buy;
  request.type = oms::api::OrderType::Limit;
  request.time_in_force = oms::api::TimeInForce::GTC;
  request.quantity = {100, 2, {}};
  request.price = {10'000, 2, {}};
  return request;
}

struct Capture {
  oms::api::RequestToken target{};
  Clock::time_point observed{};
  bool matched{};

  static void OnUpdate(void* context,
                       const oms::api::RuntimeUpdate& update) noexcept {
    auto& capture = *static_cast<Capture*>(context);
    if (!capture.matched &&
        update.kind == oms::api::RuntimeUpdateKind::Order &&
        update.order.type == oms::api::UpdateType::Submitted &&
        update.order.token == capture.target) {
      capture.observed = Clock::now();
      capture.matched = true;
    }
  }
};

bool AwaitSubmitted(oms::api::ExecutionChannel& channel,
                    oms::api::ExecutionMode mode, Capture& capture) noexcept {
  const auto deadline = Clock::now() + std::chrono::seconds(2);
  while (!capture.matched && Clock::now() < deadline) {
    if (mode == oms::api::ExecutionMode::Inline) {
      if (channel.service_io(0) != oms::api::Error::Ok) return false;
    } else {
      pollfd descriptor{channel.notification_fd(1), POLLIN, 0};
      const int result = ::poll(&descriptor, 1, 100);
      if (result < 0) return false;
    }
    channel.drain_updates(1, &Capture::OnUpdate, &capture);
  }
  return capture.matched;
}

std::uint64_t Quantile(const std::vector<std::uint64_t>& sorted,
                       std::uint32_t numerator,
                       std::uint32_t denominator) noexcept {
  const std::uint64_t count = sorted.size();
  const std::uint64_t rank =
      (count * numerator + denominator - 1U) / denominator;
  return sorted[static_cast<std::size_t>(std::max<std::uint64_t>(1, rank) - 1)];
}

struct IdleObservation {
  std::uint64_t wall_ns{};
  std::uint64_t process_cpu_ns{};
  std::uint32_t waits{};
};

IdleObservation ObserveIdle(oms::api::ExecutionChannel& channel,
                            oms::api::ExecutionMode mode,
                            std::uint32_t idle_ms) noexcept {
  IdleObservation result{};
  if (idle_ms == 0) return result;
  constexpr std::uint32_t waits = 5;
  const int timeout_ms = static_cast<int>(
      std::max<std::uint32_t>(1, idle_ms / waits));
  const auto wall_start = Clock::now();
  const std::uint64_t cpu_start = ProcessCpuNs();
  for (std::uint32_t index = 0; index < waits; ++index) {
    if (mode == oms::api::ExecutionMode::Inline) {
      (void)channel.service_io(timeout_ms);
    } else {
      pollfd descriptor{channel.notification_fd(1), POLLIN, 0};
      (void)::poll(&descriptor, 1, timeout_ms);
    }
  }
  const std::uint64_t cpu_end = ProcessCpuNs();
  result.wall_ns = static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(Clock::now() -
                                                           wall_start)
          .count());
  result.process_cpu_ns = cpu_end >= cpu_start ? cpu_end - cpu_start : 0;
  result.waits = waits;
  return result;
}

bool RunMode(const Options& options, oms::api::ExecutionMode mode) {
  const std::uint32_t total = options.samples + options.warmup;
  oms::api::RuntimeConfig config{};
  config.mode = mode;
  config.lane_count = 1;
  config.deadline_capacity = 64;
  config.lanes[0] = {1, 1024, 1024, -1};
  config.order_capacity = NextPowerOfTwo(total + 1U);
  config.fill_dedup_capacity = NextPowerOfTwo(total + 1U);
  config.pending_event_capacity = 1024;

  const auto instrument = MakeInstrument();
  auto created =
      oms::api::ExecutionChannel::Create(config, std::span{&instrument, 1U});
  if (!created || !created.value->initialize_lane(1, 0xBEEF)) return false;
  auto& channel = *created.value;

  std::vector<std::uint64_t> samples;
  samples.reserve(options.samples);
  for (std::uint32_t index = 0; index < total; ++index) {
    Capture capture;
    const auto start = Clock::now();
    const auto placed = channel.place_order(1, MakeOrder(index + 1U));
    if (!placed) return false;
    capture.target = placed.value;
    if (!AwaitSubmitted(channel, mode, capture)) return false;
    if (index >= options.warmup) {
      samples.push_back(static_cast<std::uint64_t>(
          std::chrono::duration_cast<std::chrono::nanoseconds>(
              capture.observed - start)
              .count()));
    }
  }

  std::sort(samples.begin(), samples.end());
  const IdleObservation idle = ObserveIdle(channel, mode, options.idle_ms);
  const auto metrics = channel.metrics();
  const auto shutdown = channel.shutdown();
  if (shutdown != oms::api::Error::Ok) return false;

  const std::string_view mode_name =
      mode == oms::api::ExecutionMode::Inline ? "Inline" : "DedicatedIo";
  std::cout << "{\"benchmark\":\"oms_place_to_submitted\","
               "\"environment\":\"offline_fake_loopback\","
               "\"production_slo\":false,\"mode\":\""
            << mode_name << "\",\"samples\":" << samples.size()
            << ",\"latency_ns\":{\"p50\":" << Quantile(samples, 50, 100)
            << ",\"p95\":" << Quantile(samples, 95, 100)
            << ",\"p99\":" << Quantile(samples, 99, 100)
            << ",\"p99_9\":" << Quantile(samples, 999, 1000)
            << ",\"max\":" << samples.back()
            << "},\"idle_observation\":{\"requested_ms\":"
            << options.idle_ms << ",\"waits\":" << idle.waits
            << ",\"wall_ns\":" << idle.wall_ns
            << ",\"process_cpu_ns\":" << idle.process_cpu_ns
            << "},\"queue_high_water\":{\"command\":"
            << metrics.lanes[0].command_queue.high_water << ",\"update\":"
            << metrics.lanes[0].update_queue.high_water << "}}\n";
  return true;
}

}  // namespace

int main(int argc, char** argv) {
  Options options;
  if (!Parse(argc, argv, options)) {
    std::cerr << "usage: oms_benchmark [--samples N] [--warmup N] "
                 "[--idle-ms N]\n";
    return 2;
  }
  std::cout
      << "NOTICE: offline fake-adapter loopback benchmark; results include "
         "host scheduling and are not production SLO evidence.\n";
  if (!RunMode(options, oms::api::ExecutionMode::Inline)) {
    std::cerr << "Inline benchmark failed\n";
    return 1;
  }
  if (!RunMode(options, oms::api::ExecutionMode::DedicatedIo)) {
    std::cerr << "DedicatedIo benchmark failed\n";
    return 1;
  }
  return 0;
}
