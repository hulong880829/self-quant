#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <cstdint>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

#include <sched.h>

#include "strategyframe/manager_access.h"

namespace {

using Clock = std::chrono::steady_clock;

std::uint64_t Quantile(const std::vector<std::uint64_t>& values,
                       std::size_t numerator,
                       std::size_t denominator) {
  const std::size_t rank =
      (values.size() * numerator + denominator - 1U) / denominator;
  return values[std::max<std::size_t>(1, rank) - 1U];
}

class Callback {
 public:
  void on_bbo_update(const strategyframe::BboUpdate& update) noexcept {
    checksum_ += static_cast<std::uint64_t>(update.bid.price.value);
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
  strategyframe::BboUpdate update;
  update.bid.price = {10000, 2, {}};
  std::vector<std::uint64_t> samples;
  samples.reserve(kSamples);

  for (std::size_t index = 0; index < kWarmup + kSamples; ++index) {
    const auto start = Clock::now();
    callback.on_bbo_update(update);
    const auto end = Clock::now();
    if (index >= kWarmup) {
      samples.push_back(static_cast<std::uint64_t>(
          std::chrono::duration_cast<std::chrono::nanoseconds>(end - start)
              .count()));
    }
  }
  std::sort(samples.begin(), samples.end());
  const std::uint64_t p99 = Quantile(samples, 99, 100);
  std::cout << "{\"benchmark\":\"strategyframe_direct_bbo_callback\","
               "\"production_slo\":false,\"samples\":"
            << samples.size() << ",\"p50_ns\":"
            << Quantile(samples, 50, 100) << ",\"p95_ns\":"
            << Quantile(samples, 95, 100) << ",\"p99_ns\":"
            << p99 << ",\"p99_9_ns\":"
            << Quantile(samples, 999, 1000) << ",\"max_ns\":"
            << samples.back() << ",\"checksum\":" << callback.checksum()
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
