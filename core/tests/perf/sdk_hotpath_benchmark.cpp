#include "mds/transport/shared_ring.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <stdexcept>
#include <string>
#include <unistd.h>
#include <vector>

namespace {

using Clock = std::chrono::steady_clock;

constexpr std::size_t kWarmupIterations = 10'000;
constexpr std::size_t kMeasuredIterations = 100'000;

std::uint64_t now_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          Clock::now().time_since_epoch())
          .count());
}

struct Summary {
  double throughput{};
  double p50{};
  double p95{};
  double p99{};
  double p999{};
  double maximum{};
};

Summary summarize(const char *name, std::vector<double> samples,
                  double elapsed_seconds) {
  std::sort(samples.begin(), samples.end());
  if (const char *directory =
          std::getenv("SELF_QUANT_BENCHMARK_HISTOGRAM_DIR")) {
    std::filesystem::create_directories(directory);
    std::ofstream output(std::filesystem::path(directory) /
                         (std::string(name) + "-" +
                          std::to_string(::getpid()) + ".csv"));
    output << "latency_ns\n";
    for (const auto sample : samples) {
      output << sample << '\n';
    }
  }
  const auto percentile = [&samples](double fraction) {
    const auto index = std::min(
        samples.size() - 1,
        static_cast<std::size_t>(fraction * static_cast<double>(samples.size())));
    return samples[index];
  };
  return {
      .throughput = static_cast<double>(samples.size()) / elapsed_seconds,
      .p50 = percentile(0.50),
      .p95 = percentile(0.95),
      .p99 = percentile(0.99),
      .p999 = percentile(0.999),
      .maximum = samples.back(),
  };
}

void print_summary(const char *name, const Summary &summary) {
  std::cout << std::fixed << std::setprecision(2) << name
            << " iterations=" << kMeasuredIterations
            << " throughput_ops_s=" << summary.throughput
            << " p50_ns=" << summary.p50 << " p95_ns=" << summary.p95
            << " p99_ns=" << summary.p99 << " p99_9_ns=" << summary.p999
            << " max_ns=" << summary.maximum << '\n';
}

utils::md::wire::HeaderFields header_fields() {
  return {.instrument_id = 42,
          .bus_seq = 100,
          .source_seq = 777,
          .exchange_ts_ns = 1'000'000,
          .receive_tsc = 2'000'000,
          .publish_tsc = 3'000'000,
          .book_generation = 9,
          .state = utils::md::BookState::Live,
          .source_id = 2};
}

utils::md::TickerEvent ticker_event() {
  utils::md::TickerEvent event;
  event.bid = {.price = 100, .quantity = 10};
  event.ask = {.price = 101, .quantity = 11};
  event.last_price = 100;
  event.last_quantity = 3;
  event.open_price = 95;
  event.high_price = 110;
  event.low_price = 90;
  event.close_price = 105;
  return event;
}

Summary benchmark_wire_encode() {
  std::array<std::byte, 512> buffer{};
  const auto header = header_fields();
  const auto event = ticker_event();
  std::uint64_t checksum = 0;

  for (std::size_t i = 0; i < kWarmupIterations; ++i) {
    const auto encoded = utils::md::wire::EncodeTicker(buffer, header, event);
    if (!encoded) {
      throw std::runtime_error("EncodeTicker warmup failed");
    }
    checksum += encoded.size;
  }

  std::vector<double> samples;
  samples.reserve(kMeasuredIterations);
  const auto total_start = Clock::now();
  for (std::size_t i = 0; i < kMeasuredIterations; ++i) {
    const auto start = Clock::now();
    const auto encoded = utils::md::wire::EncodeTicker(buffer, header, event);
    const auto stop = Clock::now();
    if (!encoded) {
      throw std::runtime_error("EncodeTicker benchmark failed");
    }
    checksum += encoded.size;
    samples.push_back(
        std::chrono::duration<double, std::nano>(stop - start).count());
  }
  const auto elapsed =
      std::chrono::duration<double>(Clock::now() - total_start).count();
  if (checksum == 0) {
    std::abort();
  }
  return summarize("wire_encode_ticker", std::move(samples), elapsed);
}

Summary benchmark_shared_ring() {
  std::array<std::byte, 512> payload{};
  const auto encoded = utils::md::wire::EncodeTicker(
      payload, header_fields(), ticker_event());
  if (!encoded) {
    throw std::runtime_error("failed to prepare ring payload");
  }

  mds::transport::RingOptions options;
  options.name = "/selfquant.benchmark." + std::to_string(::getpid());
  options.mode = mds::api::RingMode::Lossless;
  options.ring_bytes = 1U << 20U;
  options.max_record_bytes = payload.size();
  options.max_readers = 1;
  options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(options);
  if (!opened) {
    throw std::runtime_error(opened.message);
  }
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      mds::transport::process_start_marker(
          static_cast<std::uint32_t>(::getpid())),
      now_ns());
  if (!registered) {
    throw std::runtime_error(registered.message);
  }
  auto reader = registered.value;
  const auto bytes = std::span<const std::byte>(payload).first(encoded.size);
  std::uint64_t checksum = 0;

  const auto one_round_trip = [&] {
    const auto published = ring.publish(2, bytes);
    if (!published) {
      throw std::runtime_error(published.message);
    }
    auto read = ring.read(reader);
    if (!read) {
      throw std::runtime_error(read.message);
    }
    checksum += read.value->sequence;
    const auto committed = read.value.commit();
    if (!committed) {
      throw std::runtime_error(committed.message);
    }
  };

  for (std::size_t i = 0; i < kWarmupIterations; ++i) {
    one_round_trip();
  }

  std::vector<double> samples;
  samples.reserve(kMeasuredIterations);
  const auto total_start = Clock::now();
  for (std::size_t i = 0; i < kMeasuredIterations; ++i) {
    const auto start = Clock::now();
    one_round_trip();
    const auto stop = Clock::now();
    samples.push_back(
        std::chrono::duration<double, std::nano>(stop - start).count());
  }
  const auto elapsed =
      std::chrono::duration<double>(Clock::now() - total_start).count();
  if (checksum == 0) {
    std::abort();
  }
  return summarize("shared_ring_round_trip", std::move(samples), elapsed);
}

} // namespace

int main() {
  try {
    print_summary("wire_encode_ticker", benchmark_wire_encode());
    print_summary("shared_ring_round_trip", benchmark_shared_ring());
    return 0;
  } catch (const std::exception &error) {
    std::cerr << "benchmark failed: " << error.what() << '\n';
    return 1;
  }
}
