#pragma once

#include "fairprice/market_data_repository.h"
#include "fairprice/store.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace fairprice {

inline constexpr std::size_t kMaximumDistributionSamples = 10'000;

struct HistoryBucket {
  std::uint64_t start_ns{};
  double open{};
  double high{};
  double low{};
  double close{};
  double window_min{};
  double window_max{};
  std::uint64_t samples{};
  double coverage{};
};

struct HistoryGap {
  std::uint64_t start_ns{};
  std::uint64_t end_ns{};
};

struct HistoryResponse {
  std::string symbol;
  std::string type;
  std::uint64_t start_ns{};
  std::uint64_t end_ns{};
  std::uint64_t resolution_ns{};
  std::vector<HistoryBucket> buckets;
  std::vector<HistoryGap> gaps;
  std::vector<double> distribution;
  std::vector<std::string> unavailable_shards;
  std::uint64_t current_hour_from_ns{};
};

class HistoryProvider {
 public:
  virtual ~HistoryProvider() = default;
  virtual HistoryResponse query(const CatalogMarket &market,
                                std::uint64_t sample_interval_ms,
                                const std::vector<LiveSample> &live,
                                std::string_view bps_type,
                                std::uint64_t now_ns) = 0;
};

std::vector<LiveSample> merge_history_samples(
    const std::vector<LiveSample> &shard, const std::vector<LiveSample> &live);
std::vector<double> downsample_distribution(std::vector<double> source);
HistoryBucket merge_cut_ohlc_bucket(const HistoryBucket &earlier,
                                    const HistoryBucket &later);
void overlay_ohlc_live(HistoryResponse &stored, const std::vector<LiveSample> &live,
                       std::uint64_t cut_ns, std::uint64_t sample_interval_ms);
HistoryResponse aggregate_history(std::string_view symbol,
                                  std::string_view bps_type,
                                  std::uint64_t now_ns,
                                  std::uint64_t resolution_ns,
                                  std::uint64_t sample_interval_ms,
                                  const std::vector<LiveSample> &samples);

class History : public HistoryProvider {
 public:
  History(MarketDataRepository *repository, std::uint64_t resolution_ns);
  HistoryResponse query(const CatalogMarket &market,
                        std::uint64_t sample_interval_ms,
                        const std::vector<LiveSample> &live,
                        std::string_view bps_type,
                        std::uint64_t now_ns) override;

 private:
  MarketDataRepository *repository_{};
  std::uint64_t resolution_ns_{};
};

std::string encode_history_json(const HistoryResponse &response);

}  // namespace fairprice
