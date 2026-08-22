#pragma once

#include <atomic>
#include <cstdint>
#include <string>
#include <thread>

#include "polymm/types.h"
#include "utils/queue/spsc_ring.h"

namespace polymm {

struct SidecarConfig {
  std::string fairprice_host{"127.0.0.1"};
  std::string fairprice_service{"8080"};
  std::string fairprice_target{"/v1/stream"};
  std::string fairprice_origin{};
  bool fairprice_secure{true};
  std::string fairprice_token{};
  std::string fairprice_profile{};
  std::string fairprice_symbol{"BTCUSDT"};
  std::uint32_t connect_timeout_ms{3'000};
};

struct SidecarMetrics {
  std::uint64_t fairprice_messages{};
  std::uint64_t fairprice_parse_errors{};
  std::uint64_t fairprice_reconnects{};
  std::uint64_t queue_drops{};
  std::uint64_t last_fairprice_lag_ns{};
};

class FairPriceClient {
 public:
  explicit FairPriceClient(SidecarConfig config);
  ~FairPriceClient();
  FairPriceClient(const FairPriceClient&) = delete;
  FairPriceClient& operator=(const FairPriceClient&) = delete;

  bool start();
  void stop() noexcept;
  [[nodiscard]] bool try_pop(SidecarEvent& event) noexcept;
  [[nodiscard]] SidecarMetrics metrics() const noexcept;

 private:
  void run(std::stop_token stop);
  bool publish(const SidecarEvent& event) noexcept;

  SidecarConfig config_;
  utils::queue::SpscRing<SidecarEvent, 1024> queue_;
  std::jthread thread_;
  std::atomic<bool> started_{false};
  std::atomic<std::uint64_t> fairprice_messages_{0};
  std::atomic<std::uint64_t> fairprice_parse_errors_{0};
  std::atomic<std::uint64_t> fairprice_reconnects_{0};
  std::atomic<std::uint64_t> queue_drops_{0};
  std::atomic<std::uint64_t> last_fairprice_lag_ns_{0};
};

}  // namespace polymm
