#include "fairprice/log_summary.h"

namespace fairprice {

void OrderbookLogSummary::note_apply(std::string_view key,
                                     std::string_view profile,
                                     std::string_view symbol, int frames,
                                     const ComputeStats *stats) {
  auto &slot = slots_[std::string(key)];
  slot.key = key;
  slot.profile = profile;
  slot.symbol = symbol;
  const auto drained =
      frames > 0 ? static_cast<std::uint64_t>(frames) : std::uint64_t{0};
  slot.counters.frames += drained;
  if (frames > 1) {
    slot.counters.coalesced_drains += 1;
    slot.counters.coalesced_extra_frames +=
        static_cast<std::uint64_t>(frames - 1);
  }
  if (stats && stats->recorded) {
    if (stats->target_depth_shortfall) {
      slot.counters.target_depth_shortfall_count += 1;
    }
    slot.counters.actual_bid_depth = stats->actual_bid_depth;
    slot.counters.actual_ask_depth = stats->actual_ask_depth;
  }
}

std::vector<OrderbookLogSummaryLine> OrderbookLogSummary::take_nonempty() {
  std::vector<OrderbookLogSummaryLine> lines;
  for (auto &[_, slot] : slots_) {
    if (!slot.counters.nonempty()) {
      continue;
    }
    lines.push_back(OrderbookLogSummaryLine{
        .key = slot.key,
        .profile = slot.profile,
        .symbol = slot.symbol,
        .counters = slot.counters,
    });
    slot.counters = {};
  }
  return lines;
}

std::vector<OrderbookLogSummaryLine> OrderbookLogSummary::poll(
    std::uint64_t now_mono_ns) {
  if (!started_) {
    started_ = true;
    window_start_mono_ns_ = now_mono_ns;
    return {};
  }
  if (now_mono_ns < window_start_mono_ns_ ||
      now_mono_ns - window_start_mono_ns_ < kOrderbookLogSummaryPeriodNs) {
    return {};
  }
  auto lines = take_nonempty();
  window_start_mono_ns_ = now_mono_ns;
  return lines;
}

std::vector<OrderbookLogSummaryLine> OrderbookLogSummary::flush_on_stop() {
  auto lines = take_nonempty();
  started_ = false;
  window_start_mono_ns_ = 0;
  return lines;
}

}  // namespace fairprice
