#pragma once

#include "fairprice/engine.h"

#include <cstdint>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

namespace fairprice {

inline constexpr std::uint64_t kOrderbookLogSummaryPeriodNs =
    600ULL * 1'000'000'000ULL;

struct OrderbookLogCounters {
  std::uint64_t frames{};
  std::uint64_t coalesced_drains{};
  std::uint64_t coalesced_extra_frames{};
  std::uint64_t target_depth_shortfall_count{};
  int actual_bid_depth{};
  int actual_ask_depth{};

  [[nodiscard]] bool nonempty() const noexcept {
    return frames != 0 || coalesced_drains != 0 ||
           coalesced_extra_frames != 0 || target_depth_shortfall_count != 0;
  }
};

struct OrderbookLogSummaryLine {
  std::string key;
  std::string profile;
  std::string symbol;
  OrderbookLogCounters counters;
};

class OrderbookLogSummary {
 public:
  void note_apply(std::string_view key, std::string_view profile,
                  std::string_view symbol, int frames,
                  const ComputeStats *stats);
  std::vector<OrderbookLogSummaryLine> poll(std::uint64_t now_mono_ns);
  std::vector<OrderbookLogSummaryLine> flush_on_stop();

 private:
  struct Slot {
    std::string key;
    std::string profile;
    std::string symbol;
    OrderbookLogCounters counters;
  };

  std::vector<OrderbookLogSummaryLine> take_nonempty();

  std::unordered_map<std::string, Slot> slots_;
  std::uint64_t window_start_mono_ns_{};
  bool started_{};
};

}  // namespace fairprice
