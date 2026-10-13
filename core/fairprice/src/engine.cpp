#include "fairprice/engine.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <limits>
#include <openssl/sha.h>
#include <sstream>

namespace fairprice {
namespace {

bool finite(double value) noexcept {
  return std::isfinite(value);
}

double clamp(double value, double lower, double upper) noexcept {
  return std::max(lower, std::min(upper, value));
}

std::string format_g12(double value) {
  char buffer[64]{};
  std::snprintf(buffer, sizeof(buffer), "%.12g", value);
  return buffer;
}

bool fixed_from_float(double value, std::uint8_t scale, std::int64_t &out) {
  if (scale > 18 || !finite(value)) {
    return false;
  }
  const double scaled = value * std::pow(10.0, static_cast<int>(scale));
  const double rounded = std::round(scaled);
  if (!finite(rounded) || rounded >= static_cast<double>(std::numeric_limits<std::int64_t>::max()) ||
      rounded <= static_cast<double>(std::numeric_limits<std::int64_t>::min())) {
    return false;
  }
  out = static_cast<std::int64_t>(rounded);
  return true;
}

void consume_residual(ResidualLevel &level, std::int64_t quantity) {
  level.quantity -= quantity;
  auto remaining = quantity;
  for (auto &contribution : level.venue_quantity) {
    const auto consumed = std::min(contribution, remaining);
    contribution -= consumed;
    remaining -= consumed;
    if (remaining == 0) {
      return;
    }
  }
}

bool virtual_uncross(ResidualLevel *bids, int &bid_count, ResidualLevel *asks,
                     int &ask_count, std::int64_t &crossed) {
  int bid_index = 0;
  int ask_index = 0;
  crossed = 0;
  while (bid_index < bid_count && ask_index < ask_count &&
         bids[bid_index].price >= asks[ask_index].price) {
    const auto matched =
        std::min(bids[bid_index].quantity, asks[ask_index].quantity);
    if (matched <= 0 ||
        crossed > std::numeric_limits<std::int64_t>::max() - matched) {
      return false;
    }
    crossed += matched;
    consume_residual(bids[bid_index], matched);
    consume_residual(asks[ask_index], matched);
    if (bids[bid_index].quantity == 0) {
      ++bid_index;
    }
    if (asks[ask_index].quantity == 0) {
      ++ask_index;
    }
  }
  if (bid_index >= bid_count || ask_index >= ask_count) {
    return false;
  }
  if (bid_index > 0) {
    std::memmove(bids, bids + bid_index,
                 static_cast<std::size_t>(bid_count - bid_index) *
                     sizeof(ResidualLevel));
  }
  if (ask_index > 0) {
    std::memmove(asks, asks + ask_index,
                 static_cast<std::size_t>(ask_count - ask_index) *
                     sizeof(ResidualLevel));
  }
  bid_count -= bid_index;
  ask_count -= ask_index;
  return true;
}

double impact_price(std::span<const ResidualLevel> levels, double notional,
                    double price_factor, double quantity_factor, bool &ok) {
  double accumulated = 0;
  double base_quantity = 0;
  ok = false;
  for (const auto &level : levels) {
    const double price = static_cast<double>(level.price) / price_factor;
    const double quantity = static_cast<double>(level.quantity) / quantity_factor;
    const double available = price * quantity;
    const double used = std::min(notional - accumulated, available);
    if (used > 0) {
      accumulated += used;
      base_quantity += used / price;
    }
    if (accumulated >= notional) {
      ok = finite(base_quantity) && base_quantity > 0;
      return ok ? accumulated / base_quantity : 0;
    }
  }
  if (base_quantity > 0 && finite(base_quantity)) {
    ok = false;
    return accumulated / base_quantity;
  }
  return 0;
}

bool venue_dominant(std::span<const ResidualLevel> bids,
                    std::span<const ResidualLevel> asks, int depth,
                    double threshold) {
  std::array<double, kMaxVenues> totals{};
  double total = 0;
  auto accumulate = [&](std::span<const ResidualLevel> levels) {
    const int count = std::min(depth, static_cast<int>(levels.size()));
    for (int i = 0; i < count; ++i) {
      for (int v = 0; v < kMaxVenues; ++v) {
        totals[static_cast<std::size_t>(v)] +=
            static_cast<double>(levels[static_cast<std::size_t>(i)]
                                    .venue_quantity[static_cast<std::size_t>(v)]);
        total += static_cast<double>(
            levels[static_cast<std::size_t>(i)]
                .venue_quantity[static_cast<std::size_t>(v)]);
      }
    }
  };
  accumulate(bids);
  accumulate(asks);
  if (total <= 0) {
    return false;
  }
  for (double quantity : totals) {
    if (quantity / total >= threshold) {
      return true;
    }
  }
  return false;
}

FairPricePod not_ready(std::string_view reason, const SnapshotView &snapshot,
                       std::string_view model_id) {
  FairPricePod pod{};
  copy_cstr(pod.reset_reason, reason);
  copy_cstr(pod.model_id, model_id);
  pod.ring_epoch = snapshot.ring_epoch;
  pod.ring_sequence = snapshot.ring_sequence;
  pod.generation = snapshot.generation;
  pod.wall_ns = snapshot.wall_ns;
  if (snapshot.book) {
    pod.exchange_ts_ns = snapshot.book->exchange_ts_ns;
    pod.price_scale = snapshot.book->price_scale;
    pod.quantity_scale = snapshot.book->quantity_scale;
    pod.active_venue_mask = snapshot.book->active_mask;
  }
  return pod;
}

}  // namespace

void copy_cstr(std::array<char, 32> &dest, std::string_view value) noexcept {
  dest = {};
  const auto n = std::min(value.size(), dest.size() - 1);
  std::memcpy(dest.data(), value.data(), n);
}

std::string_view cstr_view(const std::array<char, 32> &value) noexcept {
  return {value.data(), std::strlen(value.data())};
}

void append_degraded_reasons(std::uint32_t mask, std::string &out) {
  bool first = true;
  auto add = [&](std::uint32_t bit, std::string_view name) {
    if ((mask & bit) == 0) {
      return;
    }
    if (!first) {
      out += ',';
    }
    first = false;
    out += '"';
    out += name;
    out += '"';
  };
  add(kDegradedBookCrossed, "book_crossed");
  add(kDegradedInsufficientDepth, "insufficient_depth");
  add(kDegradedImpactDepth, "impact_depth_insufficient");
  add(kDegradedInactiveVenues, "inactive_venues");
  add(kDegradedVenueDominant, "single_venue_dominant");
}

std::optional<Engine> Engine::create(const FairPriceConfig &config,
                                     std::string &error) {
  if (config.depth_k <= 0 || config.depth_k > kMaxDepth) {
    error = "fair price depth must be within 1..50";
    return std::nullopt;
  }
  if (config.lambda_per_bp < 0 || !finite(config.lambda_per_bp)) {
    error = "fair price lambda must be finite and non-negative";
    return std::nullopt;
  }
  if (config.imbalance_alpha < 0 || config.imbalance_alpha > 1 ||
      !finite(config.imbalance_alpha)) {
    error = "fair price imbalance alpha must be within 0..1";
    return std::nullopt;
  }
  if (config.ewma_tau_ns == 0) {
    error = "fair price EWMA tau must be positive";
    return std::nullopt;
  }
  if (config.impact_notional < 0 || !finite(config.impact_notional)) {
    error = "fair price impact notional must be finite and non-negative";
    return std::nullopt;
  }
  if (config.venue_dominance_ratio <= 0 || config.venue_dominance_ratio > 1 ||
      !finite(config.venue_dominance_ratio)) {
    error = "fair price venue dominance ratio must be within (0,1]";
    return std::nullopt;
  }
  std::ostringstream canonical;
  canonical << "fp-v1|k=" << config.depth_k
            << "|lambda=" << format_g12(config.lambda_per_bp)
            << "|alpha=" << format_g12(config.imbalance_alpha)
            << "|tau_ns=" << config.ewma_tau_ns
            << "|impact=" << format_g12(config.impact_notional)
            << "|dominance=" << format_g12(config.venue_dominance_ratio);
  const auto text = canonical.str();
  unsigned char hash[SHA256_DIGEST_LENGTH]{};
  SHA256(reinterpret_cast<const unsigned char *>(text.data()), text.size(),
         hash);
  Engine engine;
  engine.config_ = config;
  engine.model_id_ = "fp-v1:";
  static constexpr char hex[] = "0123456789abcdef";
  for (int i = 0; i < 8; ++i) {
    engine.model_id_.push_back(hex[hash[i] >> 4]);
    engine.model_id_.push_back(hex[hash[i] & 0x0f]);
  }
  return engine;
}

FairPricePod Engine::compute(const SnapshotView &snapshot,
                             const ModelState *previous,
                             ModelState *next, ComputeStats *stats) const {
  if (stats) {
    *stats = {};
    stats->recorded = true;
  }
  if (next) {
    *next = previous ? *previous : ModelState{};
  }
  if (!snapshot.ready || snapshot.book == nullptr) {
    return not_ready("orderbook_not_ready", snapshot, model_id_);
  }
  const auto &book = *snapshot.book;
  auto pod = not_ready("", snapshot, model_id_);
  pod.reset_reason = {};
  if (book.active_mask == 0) {
    return not_ready("no_active_venues", snapshot, model_id_);
  }
  if (book.bids.empty() || book.asks.empty()) {
    if (stats) {
      stats->actual_bid_depth = static_cast<int>(book.bids.size());
      stats->actual_ask_depth = static_cast<int>(book.asks.size());
      stats->target_depth_shortfall = true;
    }
    return not_ready("empty_side", snapshot, model_id_);
  }
  if (book.price_scale > 18 || book.quantity_scale > 18) {
    return not_ready("invalid_scale", snapshot, model_id_);
  }

  const int bid_depth =
      std::min(kMaxDepth, static_cast<int>(book.bids.size()));
  const int ask_depth =
      std::min(kMaxDepth, static_cast<int>(book.asks.size()));
  ResidualLevel bids[kMaxDepth];
  ResidualLevel asks[kMaxDepth];
  for (int i = 0; i < bid_depth; ++i) {
    const auto &level = book.bids[static_cast<std::size_t>(i)];
    if (level.price <= 0 || level.quantity <= 0) {
      return not_ready("invalid_level", snapshot, model_id_);
    }
    bids[i] = level;
  }
  for (int i = 0; i < ask_depth; ++i) {
    const auto &level = book.asks[static_cast<std::size_t>(i)];
    if (level.price <= 0 || level.quantity <= 0) {
      return not_ready("invalid_level", snapshot, model_id_);
    }
    asks[i] = level;
  }
  int bid_count = bid_depth;
  int ask_count = ask_depth;
  const auto original_bid = bids[0].price;
  const auto original_ask = asks[0].price;
  pod.crossed = original_bid >= original_ask ? 1 : 0;
  std::int64_t crossed_quantity = 0;
  if (pod.crossed) {
    if (!virtual_uncross(bids, bid_count, asks, ask_count, crossed_quantity)) {
      if (stats) {
        stats->target_depth_shortfall = true;
      }
      auto failed = not_ready("uncross_depth_exhausted", snapshot, model_id_);
      failed.crossed = 1;
      return failed;
    }
    pod.crossed_quantity = crossed_quantity;
    pod.has_crossed_quantity = 1;
  }
  if (bid_count == 0 || ask_count == 0 || bids[0].price >= asks[0].price) {
    if (stats) {
      stats->actual_bid_depth = bid_count;
      stats->actual_ask_depth = ask_count;
      stats->target_depth_shortfall = true;
    }
    auto failed = not_ready("uncross_depth_exhausted", snapshot, model_id_);
    failed.crossed = pod.crossed;
    return failed;
  }

  const double price_factor = std::pow(10.0, static_cast<int>(book.price_scale));
  const double quantity_factor =
      std::pow(10.0, static_cast<int>(book.quantity_scale));
  const double bid = static_cast<double>(bids[0].price) / price_factor;
  const double ask = static_cast<double>(asks[0].price) / price_factor;
  const double mid = (bid + ask) / 2.0;
  if (mid <= 0 || !finite(mid)) {
    return not_ready("invalid_numeric", snapshot, model_id_);
  }
  const double top_bid_quantity =
      static_cast<double>(bids[0].quantity) / quantity_factor;
  const double top_ask_quantity =
      static_cast<double>(asks[0].quantity) / quantity_factor;
  const double top_total = top_bid_quantity + top_ask_quantity;
  if (top_total <= 0 || !finite(top_total)) {
    return not_ready("invalid_numeric", snapshot, model_id_);
  }
  const double top_imbalance = top_bid_quantity / top_total;
  const double microprice = top_imbalance * ask + (1.0 - top_imbalance) * bid;

  const int depth = config_.depth_k;
  const int bid_used = std::min(depth, bid_count);
  const int ask_used = std::min(depth, ask_count);
  if (stats) {
    stats->actual_bid_depth = bid_count;
    stats->actual_ask_depth = ask_count;
    stats->target_depth_shortfall = bid_used < depth || ask_used < depth;
  }
  double weighted_bid = 0;
  double weighted_ask = 0;
  for (int i = 0; i < bid_used; ++i) {
    const double price = static_cast<double>(bids[i].price) / price_factor;
    const double quantity =
        static_cast<double>(bids[i].quantity) / quantity_factor;
    const double weight =
        std::exp(-config_.lambda_per_bp * std::abs(price - mid) / mid * 10000.0);
    weighted_bid += weight * quantity;
    pod.depth_bid_notional += price * quantity;
  }
  for (int i = 0; i < ask_used; ++i) {
    const double price = static_cast<double>(asks[i].price) / price_factor;
    const double quantity =
        static_cast<double>(asks[i].quantity) / quantity_factor;
    const double weight =
        std::exp(-config_.lambda_per_bp * std::abs(price - mid) / mid * 10000.0);
    weighted_ask += weight * quantity;
    pod.depth_ask_notional += price * quantity;
  }
  const double weighted_total = weighted_bid + weighted_ask;
  if (weighted_total <= 0 || !finite(weighted_total)) {
    return not_ready("invalid_numeric", snapshot, model_id_);
  }
  const double imbalance = weighted_bid / weighted_total;
  double raw = mid + (imbalance - 0.5) * (ask - bid) * config_.imbalance_alpha;
  raw = clamp(raw, bid, ask);
  if (!finite(raw) || !finite(microprice)) {
    return not_ready("invalid_numeric", snapshot, model_id_);
  }

  double smoothed = raw;
  if (previous && snapshot.wall_ns > previous->wall_ns) {
    const double delta =
        static_cast<double>(snapshot.wall_ns - previous->wall_ns);
    const double weight =
        1.0 - std::exp(-delta / static_cast<double>(config_.ewma_tau_ns));
    smoothed = previous->price + weight * (raw - previous->price);
  }
  smoothed = clamp(smoothed, bid, ask);
  if (next) {
    *next = ModelState{snapshot.wall_ns, smoothed};
  }

  auto output_scale = book.price_scale;
  if (output_scale <= 15) {
    output_scale = static_cast<std::uint8_t>(output_scale + 3);
  } else {
    output_scale = 18;
  }
  pod.output_scale = output_scale;
  if (!fixed_from_float(smoothed, output_scale, pod.price)) {
    return not_ready("fixed_overflow", snapshot, model_id_);
  }
  auto assign = [&](std::int64_t &target, double value) {
    return fixed_from_float(value, output_scale, target);
  };
  if (!assign(pod.price_raw, raw) || !assign(pod.mid, mid) ||
      !assign(pod.microprice, microprice) ||
      !assign(pod.best_bid, static_cast<double>(original_bid) / price_factor) ||
      !assign(pod.best_ask, static_cast<double>(original_ask) / price_factor) ||
      !assign(pod.effective_bid, bid) || !assign(pod.effective_ask, ask)) {
    return not_ready("fixed_overflow", snapshot, model_id_);
  }

  if (config_.impact_notional > 0) {
    bool bid_ok = false;
    bool ask_ok = false;
    const double impact_bid =
        impact_price({bids, static_cast<std::size_t>(bid_count)},
                     config_.impact_notional, price_factor, quantity_factor,
                     bid_ok);
    const double impact_ask =
        impact_price({asks, static_cast<std::size_t>(ask_count)},
                     config_.impact_notional, price_factor, quantity_factor,
                     ask_ok);
    if (impact_bid > 0) {
      if (!fixed_from_float(impact_bid, output_scale, pod.impact_bid)) {
        return not_ready("fixed_overflow", snapshot, model_id_);
      }
      pod.has_impact_bid = 1;
    }
    if (impact_ask > 0) {
      if (!fixed_from_float(impact_ask, output_scale, pod.impact_ask)) {
        return not_ready("fixed_overflow", snapshot, model_id_);
      }
      pod.has_impact_ask = 1;
    }
    if (!bid_ok || !ask_ok) {
      pod.degraded_mask |= kDegradedImpactDepth;
    }
  }
  if (book.active_mask != book.member_mask) {
    pod.degraded_mask |= kDegradedInactiveVenues;
  }
  if (venue_dominant({bids, static_cast<std::size_t>(bid_count)},
                     {asks, static_cast<std::size_t>(ask_count)},
                     config_.depth_k, config_.venue_dominance_ratio)) {
    pod.degraded_mask |= kDegradedVenueDominant;
  }
  pod.imbalance = imbalance;
  pod.weighted_bid_depth = weighted_bid;
  pod.weighted_ask_depth = weighted_ask;
  pod.spread_bps = (ask - bid) / mid * 10000.0;
  if (pod.crossed) {
    pod.cross_bps = (static_cast<double>(original_bid) -
                     static_cast<double>(original_ask)) *
                    20000.0 /
                    (static_cast<double>(original_bid) +
                     static_cast<double>(original_ask));
    pod.has_cross_bps = 1;
  }
  pod.degraded = pod.degraded_mask != 0 ? 1 : 0;
  pod.ready = 1;
  copy_cstr(pod.model_id, model_id_);
  return pod;
}

}  // namespace fairprice
