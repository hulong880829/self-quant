#include "fairprice/publisher.h"

#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <cstring>

namespace fairprice {
namespace {

template <std::size_t N>
bool fill_fixed(std::array<char, N> &dest, std::string_view value) {
  if (value.size() >= N) {
    return false;
  }
  dest = {};
  std::memcpy(dest.data(), value.data(), value.size());
  return true;
}

}  // namespace

FairPublisher::FairPublisher(std::string prefix,
                             mds::transport::RingOptions options)
    : prefix_(std::move(prefix)), options_(options) {}

mds::publish::WirePublisher *FairPublisher::ensure(const std::string &key,
                                                   std::string_view profile,
                                                   std::string_view symbol) {
  auto it = publishers_.find(key);
  if (it != publishers_.end()) {
    return it->second.get();
  }
  auto name = mds::publish::make_publisher_segment_name(prefix_, profile, symbol,
                                                        "fairprice");
  if (name.empty()) {
    return nullptr;
  }
  auto ring_options = options_;
  ring_options.name = name;
  ring_options.create = true;
  auto opened = mds::transport::SharedRing::open(ring_options);
  if (!opened) {
    return nullptr;
  }
  auto inserted = publishers_.emplace(
      key, std::make_unique<mds::publish::WirePublisher>(std::move(opened.value)));
  return inserted.first->second.get();
}

void FairPublisher::publish(const std::string &key, std::string_view profile,
                            std::string_view symbol, const FairPricePod &pod) {
  auto *publisher = ensure(key, profile, symbol);
  if (!publisher) {
    ++dropped_;
    return;
  }
  utils::md::wire::FairPriceRecordV2 record{};
  if (!fill_fixed(record.profile, profile) || !fill_fixed(record.symbol, symbol) ||
      !fill_fixed(record.model_id, cstr_view(pod.model_id)) ||
      !fill_fixed(record.reset_reason, cstr_view(pod.reset_reason))) {
    ++dropped_;
    return;
  }
  record.ring_epoch = pod.ring_epoch;
  record.ring_sequence = pod.ring_sequence;
  record.generation = pod.generation;
  record.wall_ns = pod.wall_ns;
  record.exchange_ts_ns = pod.exchange_ts_ns;
  record.price = pod.price;
  record.price_raw = pod.price_raw;
  record.mid = pod.mid;
  record.microprice = pod.microprice;
  record.best_bid = pod.best_bid;
  record.best_ask = pod.best_ask;
  record.effective_bid = pod.effective_bid;
  record.effective_ask = pod.effective_ask;
  record.impact_bid = pod.impact_bid;
  record.impact_ask = pod.impact_ask;
  record.crossed_quantity = pod.crossed_quantity;
  record.imbalance = pod.imbalance;
  record.spread_bps = pod.spread_bps;
  record.cross_bps = pod.cross_bps;
  record.weighted_bid_depth = pod.weighted_bid_depth;
  record.weighted_ask_depth = pod.weighted_ask_depth;
  record.depth_bid_notional = pod.depth_bid_notional;
  record.depth_ask_notional = pod.depth_ask_notional;
  record.active_venue_mask = pod.active_venue_mask;
  record.degraded_mask = pod.degraded_mask;
  record.price_scale = pod.price_scale;
  record.quantity_scale = pod.quantity_scale;
  record.output_scale = pod.output_scale;
  record.ready = pod.ready;
  record.crossed = pod.crossed;
  record.degraded = pod.degraded;
  record.has_impact_bid = pod.has_impact_bid;
  record.has_impact_ask = pod.has_impact_ask;
  record.has_crossed_quantity = pod.has_crossed_quantity;
  record.has_cross_bps = pod.has_cross_bps;
  utils::md::EventHeader header{};
  header.book_generation = static_cast<std::uint32_t>(pod.generation);
  header.exchange_ts_ns = pod.exchange_ts_ns;
  header.publish_tsc = utils::runtime::Timestamp::NowMono();
  header.state = pod.ready ? utils::md::BookState::Live
                           : utils::md::BookState::Invalid;
  auto published = publisher->publish_fair_price_v2(header, record);
  if (!published) {
    ++dropped_;
  }
}

void FairPublisher::close_missing(
    const std::unordered_map<std::string, bool> &active) {
  for (auto it = publishers_.begin(); it != publishers_.end();) {
    if (active.find(it->first) == active.end()) {
      it = publishers_.erase(it);
    } else {
      ++it;
    }
  }
}

}  // namespace fairprice
