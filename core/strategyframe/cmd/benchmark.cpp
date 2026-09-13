#include <algorithm>
#include <array>
#include <chrono>
#include <cstdlib>
#include <cstdint>
#include <fstream>
#include <iostream>
#include <span>
#include <string>
#include <vector>

#include <sched.h>
#if defined(__x86_64__) || defined(__i386__)
#include <x86intrin.h>
#endif

#include "strategyframe/market_data_source.h"
#include "utils/md/wire_codec.h"

namespace {

using Clock = std::chrono::steady_clock;

std::uint64_t ReadCycles() noexcept {
#if defined(__x86_64__) || defined(__i386__)
  unsigned auxiliary{};
  return __rdtscp(&auxiliary);
#else
  return 0;
#endif
}

std::uint64_t Quantile(const std::vector<std::uint64_t>& values,
                       std::size_t numerator,
                       std::size_t denominator) {
  const std::size_t rank =
      (values.size() * numerator + denominator - 1U) / denominator;
  return values[std::max<std::size_t>(1, rank) - 1U];
}

class Callback {
 public:
  static bool OnInstrument(void*, const utils::md::Instrument&) noexcept {
    return true;
  }
  static bool OnCatalog(void*, const utils::md::InstrumentCatalog&,
                        std::uint32_t) noexcept {
    return true;
  }
  static bool OnBbo(void* context,
                    const strategyframe::BboUpdate& update) noexcept {
    auto& self = *static_cast<Callback*>(context);
    self.checksum_ +=
        static_cast<std::uint64_t>(update.bid.price.value);
    return true;
  }
  [[nodiscard]] std::uint64_t checksum() const noexcept { return checksum_; }

 private:
  std::uint64_t checksum_{};
};

std::string CpuModel() {
  std::ifstream input("/proc/cpuinfo");
  std::string line;
  while (std::getline(input, line)) {
    constexpr std::string_view key = "model name";
    if (!line.starts_with(key)) continue;
    const std::size_t separator = line.find(':');
    if (separator == std::string::npos) break;
    const std::size_t value = line.find_first_not_of(" \t", separator + 1);
    return value == std::string::npos ? std::string{} : line.substr(value);
  }
  return "unknown";
}

}  // namespace

int main() {
  constexpr std::size_t kWarmup = 1000;
  constexpr std::size_t kSamples = 10000;
  Callback callback;
  strategyframe::MdsConfig config;
  config.source = strategyframe::MdsSourceMode::Replay;
  config.bbo_policy = strategyframe::BboPolicy::TickerOnly;
  strategyframe::MarketDataSource source(
      config,
      {&callback, &Callback::OnInstrument, &Callback::OnCatalog,
       &Callback::OnBbo, nullptr, nullptr, nullptr, nullptr});
  std::array<std::byte, sizeof(utils::md::wire::InstrumentUpdateRecord)>
      instrument_bytes{};
  utils::md::Instrument instrument{};
  instrument.instrument_id = 7;
  instrument.venue = utils::md::Venue::Binance;
  instrument.product_type = utils::md::ProductType::Spot;
  instrument.price_scale = 2;
  instrument.quantity_scale = 2;
  utils::md::wire::HeaderFields instrument_header{};
  instrument_header.instrument_id = instrument.instrument_id;
  instrument_header.state = utils::md::BookState::Building;
  const auto encoded_instrument = utils::md::wire::EncodeInstrument(
      instrument_bytes, instrument_header, instrument);
  if (!encoded_instrument ||
      source.enqueue_replay(std::span(instrument_bytes).first(
          encoded_instrument.size)) != strategyframe::Error::Ok ||
      source.start() != strategyframe::Error::Ok) {
    return 1;
  }
  std::size_t dispatched{};
  if (source.poll(1, dispatched) != strategyframe::Error::Ok ||
      dispatched != 1) {
    return 1;
  }
  std::vector<std::uint64_t> samples;
  std::vector<std::uint64_t> cycle_samples;
  samples.reserve(kSamples);
  cycle_samples.reserve(kSamples);

  for (std::size_t index = 0; index < kWarmup + kSamples; ++index) {
    std::array<std::byte, sizeof(utils::md::wire::BboRecord)> bytes{};
    utils::md::wire::HeaderFields header{};
    header.instrument_id = instrument.instrument_id;
    header.source_seq = index + 1;
    header.exchange_ts_ns = index + 1;
    header.book_generation = 1;
    header.state = utils::md::BookState::Live;
    header.flags = utils::md::wire::kBboOriginTickerStream;
    const auto encoded = utils::md::wire::EncodeBbo(
        bytes, header, {10'000, 10}, {10'001, 10});
    if (!encoded ||
        source.enqueue_replay(
            std::span(bytes).first(encoded.size)) !=
            strategyframe::Error::Ok) {
      return 1;
    }
    const auto start = Clock::now();
    const std::uint64_t cycle_start = ReadCycles();
    dispatched = 0;
    if (source.poll(1, dispatched) != strategyframe::Error::Ok ||
        dispatched != 1) {
      return 1;
    }
    const std::uint64_t cycle_end = ReadCycles();
    const auto end = Clock::now();
    if (index >= kWarmup) {
      samples.push_back(static_cast<std::uint64_t>(
          std::chrono::duration_cast<std::chrono::nanoseconds>(end - start)
              .count()));
      cycle_samples.push_back(cycle_end >= cycle_start
                                  ? cycle_end - cycle_start
                                  : 0);
    }
  }
  std::sort(samples.begin(), samples.end());
  std::sort(cycle_samples.begin(), cycle_samples.end());
  std::uint64_t cycle_total{};
  for (const std::uint64_t value : cycle_samples) cycle_total += value;
  const std::uint64_t p99 = Quantile(samples, 99, 100);
  source.stop();
  std::cout << "{\"benchmark\":\"strategyframe_ticker_bbo_dispatch\","
               "\"production_slo\":false,\"samples\":"
            << samples.size() << ",\"p50_ns\":"
            << Quantile(samples, 50, 100) << ",\"p95_ns\":"
            << Quantile(samples, 95, 100) << ",\"p99_ns\":"
            << p99 << ",\"p99_9_ns\":"
            << Quantile(samples, 999, 1000) << ",\"max_ns\":"
            << samples.back() << ",\"checksum\":" << callback.checksum()
            << ",\"cycles\":{\"avg\":"
            << cycle_total / cycle_samples.size()
            << ",\"p99\":" << Quantile(cycle_samples, 99, 100)
            << ",\"p99_9\":"
            << Quantile(cycle_samples, 999, 1000) << "}"
            << ",\"cpu\":" << ::sched_getcpu()
            << ",\"cpu_model\":\"" << CpuModel()
            << "\",\"compiler\":\"" << __VERSION__
            << "\",\"network\":\"in_process_no_socket\""
            << "}\n";
  if (const char* threshold =
          std::getenv("STRATEGYFRAME_MAX_CALLBACK_P99_NS");
      threshold != nullptr) {
    char* end = nullptr;
    const auto maximum = std::strtoull(threshold, &end, 10);
    if (end == threshold || *end != '\0' || p99 > maximum) return 1;
  }
  return 0;
}
