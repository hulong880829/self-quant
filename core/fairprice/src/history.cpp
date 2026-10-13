#include "fairprice/history.h"

#include "fairprice/json.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <limits>
#include <map>

namespace fairprice {
namespace {

struct TaggedSample {
  LiveSample sample;
  std::uint8_t source_priority{};
};

void fill_window(HistoryResponse &response, std::uint64_t now_ns) {
  response.end_ns = now_ns;
  constexpr auto day_ns = 24ULL * 3600ULL * 1'000'000'000ULL;
  response.start_ns = now_ns > day_ns ? now_ns - day_ns : 0;
}

void fill_timeline(std::map<std::uint64_t, HistoryBucket> buckets,
                   std::uint64_t start_ns, std::uint64_t end_ns,
                   std::uint64_t resolution_ns, std::uint64_t sample_interval_ns,
                   HistoryResponse &response) {
  const double expected = sample_interval_ns == 0
                              ? 1.0
                              : static_cast<double>(resolution_ns) /
                                    static_cast<double>(sample_interval_ns);
  response.buckets.clear();
  response.gaps.clear();
  for (auto bucket_start = start_ns; bucket_start < end_ns;
       bucket_start += resolution_ns) {
    auto it = buckets.find(bucket_start);
    if (it != buckets.end()) {
      it->second.coverage =
          std::min(1.0, static_cast<double>(it->second.samples) / expected);
      it->second.start_ns = bucket_start;
      response.buckets.push_back(it->second);
    } else if (!response.gaps.empty() &&
               response.gaps.back().end_ns == bucket_start) {
      response.gaps.back().end_ns =
          std::min(end_ns, bucket_start + resolution_ns);
    } else {
      response.gaps.push_back(
          {bucket_start, std::min(end_ns, bucket_start + resolution_ns)});
    }
    if (std::numeric_limits<std::uint64_t>::max() - bucket_start <
        resolution_ns) {
      break;
    }
  }
}

void collect_samples(const std::vector<LiveSample> &samples, std::uint64_t start_ns,
                     std::uint64_t end_ns, std::uint64_t resolution_ns,
                     std::string_view bps_type, bool fill_distribution,
                     std::map<std::uint64_t, HistoryBucket> &buckets,
                     HistoryResponse &response) {
  for (const auto &sample : samples) {
    if (sample.wall_ns < start_ns || sample.wall_ns > end_ns ||
        resolution_ns == 0) {
      continue;
    }
    const auto bucket_start =
        start_ns + ((sample.wall_ns - start_ns) / resolution_ns) * resolution_ns;
    const bool raw = bps_type == "raw";
    const double value = raw ? sample.raw_bps : sample.gated_bps;
    const double minimum = raw ? sample.raw_min : sample.gated_min;
    const double maximum = raw ? sample.raw_max : sample.gated_max;
    auto &bucket = buckets[bucket_start];
    if (bucket.samples == 0) {
      bucket.start_ns = bucket_start;
      bucket.open = bucket.high = bucket.low = bucket.close = value;
      bucket.window_min = minimum;
      bucket.window_max = maximum;
    } else {
      bucket.high = std::max(bucket.high, value);
      bucket.low = std::min(bucket.low, value);
      bucket.close = value;
      bucket.window_min = std::min(bucket.window_min, minimum);
      bucket.window_max = std::max(bucket.window_max, maximum);
    }
    bucket.samples++;
    if (fill_distribution) {
      response.distribution.push_back(value);
    }
  }
}

void aggregate(const std::vector<LiveSample> &samples, std::uint64_t start_ns,
               std::uint64_t end_ns, std::uint64_t resolution_ns,
               std::uint64_t sample_interval_ns, std::string_view bps_type,
               HistoryResponse &response) {
  std::map<std::uint64_t, HistoryBucket> buckets;
  collect_samples(samples, start_ns, end_ns, resolution_ns, bps_type, true,
                  buckets, response);
  fill_timeline(std::move(buckets), start_ns, end_ns, resolution_ns,
                sample_interval_ns, response);
  std::sort(response.distribution.begin(), response.distribution.end());
  response.distribution = downsample_distribution(std::move(response.distribution));
}

}  // namespace

std::vector<LiveSample> merge_history_samples(
    const std::vector<LiveSample> &shard, const std::vector<LiveSample> &live) {
  std::vector<TaggedSample> tagged;
  tagged.reserve(shard.size() + live.size());
  for (const auto &sample : shard) {
    tagged.push_back({sample, 0});
  }
  for (const auto &sample : live) {
    tagged.push_back({sample, 1});
  }
  std::sort(tagged.begin(), tagged.end(),
            [](const TaggedSample &a, const TaggedSample &b) {
              if (a.sample.wall_ns != b.sample.wall_ns) {
                return a.sample.wall_ns < b.sample.wall_ns;
              }
              return a.source_priority < b.source_priority;
            });
  std::vector<LiveSample> out;
  out.reserve(tagged.size());
  for (const auto &item : tagged) {
    if (!out.empty() && out.back().wall_ns == item.sample.wall_ns) {
      out.back() = item.sample;
      continue;
    }
    out.push_back(item.sample);
  }
  return out;
}

std::vector<double> downsample_distribution(std::vector<double> source) {
  if (source.size() <= kMaximumDistributionSamples) {
    return source;
  }
  std::vector<double> sampled;
  sampled.resize(kMaximumDistributionSamples);
  const auto last = source.size() - 1U;
  for (std::size_t index = 0; index < kMaximumDistributionSamples; ++index) {
    const auto origin = index * last / (kMaximumDistributionSamples - 1U);
    sampled[index] = source[origin];
  }
  return sampled;
}

HistoryBucket merge_cut_ohlc_bucket(const HistoryBucket &earlier,
                                    const HistoryBucket &later) {
  HistoryBucket out = earlier;
  out.high = std::max(earlier.high, later.high);
  out.low = std::min(earlier.low, later.low);
  out.close = later.close;
  out.window_min = std::min(earlier.window_min, later.window_min);
  out.window_max = std::max(earlier.window_max, later.window_max);
  out.samples = earlier.samples + later.samples;
  return out;
}

void overlay_ohlc_live(HistoryResponse &stored, const std::vector<LiveSample> &live,
                       std::uint64_t cut_ns, std::uint64_t sample_interval_ms) {
  if (live.empty() || stored.resolution_ns == 0) {
    return;
  }
  std::vector<LiveSample> after_cut;
  after_cut.reserve(live.size());
  for (const auto &sample : live) {
    if (sample.wall_ns >= cut_ns) {
      after_cut.push_back(sample);
    }
  }
  if (after_cut.empty()) {
    return;
  }
  HistoryResponse live_part;
  live_part.type = stored.type;
  live_part.resolution_ns = stored.resolution_ns;
  std::map<std::uint64_t, HistoryBucket> live_buckets;
  collect_samples(after_cut, stored.start_ns, stored.end_ns, stored.resolution_ns,
                  stored.type, false, live_buckets, live_part);
  std::map<std::uint64_t, HistoryBucket> merged;
  for (const auto &bucket : stored.buckets) {
    merged[bucket.start_ns] = bucket;
  }
  for (const auto &[start, bucket] : live_buckets) {
    auto it = merged.find(start);
    if (it == merged.end()) {
      merged[start] = bucket;
    } else {
      it->second = merge_cut_ohlc_bucket(it->second, bucket);
    }
  }
  fill_timeline(std::move(merged), stored.start_ns, stored.end_ns,
                stored.resolution_ns, sample_interval_ms * 1'000'000ULL,
                stored);
}

HistoryResponse aggregate_history(std::string_view symbol,
                                  std::string_view bps_type,
                                  std::uint64_t now_ns,
                                  std::uint64_t resolution_ns,
                                  std::uint64_t sample_interval_ms,
                                  const std::vector<LiveSample> &samples) {
  HistoryResponse response;
  response.symbol = std::string(symbol);
  response.type = std::string(bps_type);
  response.resolution_ns = resolution_ns;
  fill_window(response, now_ns);
  aggregate(samples, response.start_ns, response.end_ns, resolution_ns,
            sample_interval_ms * 1'000'000ULL, bps_type, response);
  return response;
}

History::History(MarketDataRepository *repository, std::uint64_t resolution_ns)
    : repository_(repository), resolution_ns_(resolution_ns) {}

HistoryResponse History::query(const CatalogMarket &market,
                               std::uint64_t sample_interval_ms,
                               const std::vector<LiveSample> &live,
                               std::string_view bps_type,
                               std::uint64_t now_ns) {
  HistoryResponse response;
  response.symbol = market.symbol;
  response.type = std::string(bps_type);
  response.resolution_ns = resolution_ns_;
  fill_window(response, now_ns);

  const std::uint64_t cut_ns =
      live.empty() ? response.end_ns : live.front().wall_ns;
  if (!live.empty()) {
    response.current_hour_from_ns = cut_ns;
  }
  if (repository_ != nullptr && !market.bbo_segment.empty()) {
    const auto ohlc = repository_->query_bbo_history_ohlc(
        market.bbo_segment, response.start_ns, cut_ns, resolution_ns_,
        bps_type);
    if (!ohlc) {
      std::fprintf(stderr,
                   "fairprice: history ohlc query failed segment=%s error=%s "
                   "%s\n",
                   market.bbo_segment.c_str(),
                   std::string(market_data_error_name(ohlc.error)).c_str(),
                   ohlc.message.c_str());
      response.unavailable_shards.push_back(market.bbo_segment);
    } else {
      std::map<std::uint64_t, HistoryBucket> buckets;
      for (const auto &row : ohlc.value) {
        HistoryBucket bucket{};
        bucket.start_ns = row.bucket_start_ns;
        bucket.open = row.open;
        bucket.high = row.high;
        bucket.low = row.low;
        bucket.close = row.close;
        bucket.window_min = row.window_min;
        bucket.window_max = row.window_max;
        bucket.samples = row.samples;
        buckets[row.bucket_start_ns] = bucket;
      }
      fill_timeline(std::move(buckets), response.start_ns, response.end_ns,
                    resolution_ns_, sample_interval_ms * 1'000'000ULL,
                    response);
    }
    const auto dist = repository_->query_bbo_history_distribution(
        market.bbo_segment, response.start_ns, response.end_ns, bps_type);
    if (!dist) {
      std::fprintf(stderr,
                   "fairprice: history distribution query failed segment=%s "
                   "error=%s %s\n",
                   market.bbo_segment.c_str(),
                   std::string(market_data_error_name(dist.error)).c_str(),
                   dist.message.c_str());
      if (response.unavailable_shards.empty() ||
          response.unavailable_shards.back() != market.bbo_segment) {
        response.unavailable_shards.push_back(market.bbo_segment);
      }
    } else {
      response.distribution = dist.value;
    }
  }
  overlay_ohlc_live(response, live, cut_ns, sample_interval_ms);
  return response;
}

std::string encode_history_json(const HistoryResponse &response) {
  std::string out;
  out.reserve(256U + response.buckets.size() * 160U +
              response.gaps.size() * 64U +
              response.distribution.size() * 24U +
              response.unavailable_shards.size() * 48U);
  out += "{\"symbol\":\"";
  out += json_escape(response.symbol);
  out += "\",\"type\":\"";
  out += json_escape(response.type);
  out += "\",\"start_ns\":\"";
  append_u64(out, response.start_ns);
  out += "\",\"end_ns\":\"";
  append_u64(out, response.end_ns);
  out += "\",\"resolution_ns\":\"";
  append_u64(out, response.resolution_ns);
  out += "\",\"buckets\":[";
  for (std::size_t i = 0; i < response.buckets.size(); ++i) {
    if (i) {
      out += ',';
    }
    const auto &bucket = response.buckets[i];
    out += "{\"start_ns\":\"";
    append_u64(out, bucket.start_ns);
    out += "\",\"open\":";
    append_f64(out, bucket.open);
    out += ",\"high\":";
    append_f64(out, bucket.high);
    out += ",\"low\":";
    append_f64(out, bucket.low);
    out += ",\"close\":";
    append_f64(out, bucket.close);
    out += ",\"window_min\":";
    append_f64(out, bucket.window_min);
    out += ",\"window_max\":";
    append_f64(out, bucket.window_max);
    out += ",\"samples\":";
    append_u64(out, bucket.samples);
    out += ",\"coverage\":";
    append_f64(out, bucket.coverage);
    out += '}';
  }
  out += "],\"gaps\":[";
  for (std::size_t i = 0; i < response.gaps.size(); ++i) {
    if (i) {
      out += ',';
    }
    out += "{\"start_ns\":\"";
    append_u64(out, response.gaps[i].start_ns);
    out += "\",\"end_ns\":\"";
    append_u64(out, response.gaps[i].end_ns);
    out += "\"}";
  }
  out += "],\"distribution_bps\":[";
  for (std::size_t i = 0; i < response.distribution.size(); ++i) {
    if (i) {
      out += ',';
    }
    append_f64(out, response.distribution[i]);
  }
  out += "]";
  if (!response.unavailable_shards.empty()) {
    out += ",\"unavailable_shards\":[";
    for (std::size_t i = 0; i < response.unavailable_shards.size(); ++i) {
      if (i) {
        out += ',';
      }
      out += '"';
      out += json_escape(response.unavailable_shards[i]);
      out += '"';
    }
    out += ']';
  }
  if (response.current_hour_from_ns) {
    out += ",\"current_hour_from_ns\":\"";
    append_u64(out, response.current_hour_from_ns);
    out += '"';
  }
  out += "}\n";
  return out;
}

}  // namespace fairprice
