#pragma once

#include <atomic>
#include <cstdint>
#include <string>
#include <thread>

#include "polymm/gamma_resolution.h"
#include "polymm/types.h"
#include "utils/queue/spsc_ring.h"

namespace polymm {

enum class SubscribePhase : std::uint8_t { Idle = 0, Sent = 1, Acked = 2 };

struct SubscribeGate {
  SubscribePhase phase{SubscribePhase::Idle};
  std::uint64_t deadline_ns{};

  [[nodiscard]] bool should_send(bool socket_open,
                                 bool can_send) const noexcept {
    return socket_open && can_send && phase == SubscribePhase::Idle;
  }

  void mark_sent(std::uint64_t now_ns, std::uint32_t timeout_ms) noexcept {
    phase = SubscribePhase::Sent;
    const std::uint32_t timeout = timeout_ms == 0 ? 1'000 : timeout_ms;
    deadline_ns =
        now_ns + static_cast<std::uint64_t>(timeout) * 1'000'000ULL;
  }

  [[nodiscard]] bool note_ack(bool ok) noexcept {
    if (phase != SubscribePhase::Sent) return false;
    deadline_ns = 0;
    if (!ok) {
      phase = SubscribePhase::Idle;
      return true;
    }
    phase = SubscribePhase::Acked;
    return false;
  }

  [[nodiscard]] bool timed_out(std::uint64_t now_ns) const noexcept {
    return phase == SubscribePhase::Sent && deadline_ns != 0 &&
           now_ns >= deadline_ns;
  }

  void reset() noexcept { *this = {}; }
};

struct SidecarConfig {
  std::string fairprice_host{"127.0.0.1"};
  std::string fairprice_service{};
  std::string fairprice_target{"/v1/stream"};
  std::string fairprice_origin{};
  bool fairprice_secure{true};
  std::string fairprice_token{};
  std::string fairprice_profile{};
  std::string fairprice_symbol{"BTCUSDT"};
  std::uint32_t connect_timeout_ms{1'000};
};

struct ResolutionRequest {
  std::uint64_t request_id{};
  strategyframe::ConditionId condition_id{};
};

struct ResolutionResult {
  std::uint64_t request_id{};
  strategyframe::ConditionId condition_id{};
  bool transport_error{};
  ParsedGammaResolution parsed{};
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
  [[nodiscard]] bool try_push_resolution(
      const ResolutionRequest& request) noexcept;
  [[nodiscard]] bool try_pop_resolution(ResolutionResult& result) noexcept;
  [[nodiscard]] SidecarMetrics metrics() const noexcept;

 private:
  void run(std::stop_token stop);
  bool publish(const SidecarEvent& event) noexcept;
  bool publish_resolution(const ResolutionResult& result) noexcept;

  SidecarConfig config_;
  utils::queue::SpscRing<SidecarEvent, 1024> queue_;
  utils::queue::SpscRing<ResolutionRequest, 16> resolution_requests_;
  utils::queue::SpscRing<ResolutionResult, 16> resolution_results_;
  std::atomic<std::uint32_t> resolution_unconsumed_{0};
  std::jthread thread_;
  std::atomic<bool> started_{false};
  std::atomic<std::uint64_t> fairprice_messages_{0};
  std::atomic<std::uint64_t> fairprice_parse_errors_{0};
  std::atomic<std::uint64_t> fairprice_reconnects_{0};
  std::atomic<std::uint64_t> queue_drops_{0};
  std::atomic<std::uint64_t> last_fairprice_lag_ns_{0};
};

}  // namespace polymm
