#include "mds/agg/aggregation_engine.h"

#include <algorithm>
#include <cstring>
#include <limits>

namespace mds::agg {
namespace {

using utils::md::Side;
using utils::md::Venue;
using utils::md::wire::AggBboRawSide;
using utils::md::wire::AggBboSide;
using utils::md::wire::AggLevel;

__extension__ typedef __int128 Int128;

std::uint64_t age_us(std::uint64_t now_ns, std::uint64_t ingress_ns) noexcept {
  return now_ns >= ingress_ns ? (now_ns - ingress_ns) / 1'000U
                              : std::numeric_limits<std::uint64_t>::max();
}

std::uint32_t clamp_u32(std::uint64_t value) noexcept {
  return value > std::numeric_limits<std::uint32_t>::max()
             ? std::numeric_limits<std::uint32_t>::max()
             : static_cast<std::uint32_t>(value);
}

std::int64_t saturating_add(std::int64_t left, std::int64_t right) noexcept {
  if (right > 0 && left > std::numeric_limits<std::int64_t>::max() - right) {
    return std::numeric_limits<std::int64_t>::max();
  }
  if (right < 0 && left < std::numeric_limits<std::int64_t>::min() - right) {
    return std::numeric_limits<std::int64_t>::min();
  }
  return left + right;
}

bool pow10(std::uint8_t exponent, Int128 &value) noexcept {
  value = 1;
  for (std::uint8_t index = 0; index < exponent; ++index) {
    if (value > std::numeric_limits<std::int64_t>::max() / 10) {
      return false;
    }
    value *= 10;
  }
  return true;
}

bool rescale(std::int64_t input, std::uint8_t from, std::uint8_t to,
             std::int64_t &output) noexcept {
  Int128 factor{};
  if (from <= to) {
    if (!pow10(static_cast<std::uint8_t>(to - from), factor)) {
      return false;
    }
    const Int128 converted = static_cast<Int128>(input) * factor;
    if (converted < std::numeric_limits<std::int64_t>::min() ||
        converted > std::numeric_limits<std::int64_t>::max()) {
      return false;
    }
    output = static_cast<std::int64_t>(converted);
    return true;
  }
  if (!pow10(static_cast<std::uint8_t>(from - to), factor) ||
      static_cast<Int128>(input) % factor != 0) {
    return false;
  }
  output = static_cast<std::int64_t>(static_cast<Int128>(input) / factor);
  return true;
}

bool multiply_scaled(std::int64_t price, std::uint8_t price_scale,
                     std::int64_t fx, std::uint8_t fx_scale,
                     std::uint8_t output_scale, bool round_up,
                     std::int64_t &output) noexcept {
  const int exponent = static_cast<int>(price_scale) +
                       static_cast<int>(fx_scale) -
                       static_cast<int>(output_scale);
  Int128 divisor = 1;
  Int128 multiplier = 1;
  if (exponent > 0) {
    if (!pow10(static_cast<std::uint8_t>(exponent), divisor)) {
      return false;
    }
  } else if (exponent < 0) {
    if (!pow10(static_cast<std::uint8_t>(-exponent), multiplier)) {
      return false;
    }
  }
  const Int128 numerator =
      static_cast<Int128>(price) * static_cast<Int128>(fx) * multiplier;
  Int128 converted = numerator / divisor;
  if (round_up && numerator > 0 && numerator % divisor != 0) {
    ++converted;
  }
  if (converted < std::numeric_limits<std::int64_t>::min() ||
      converted > std::numeric_limits<std::int64_t>::max()) {
    return false;
  }
  output = static_cast<std::int64_t>(converted);
  return true;
}

std::int32_t cross_bps(std::int64_t bid, std::int64_t ask) noexcept {
  if (bid <= ask || ask <= 0) {
    return 0;
  }
  const Int128 value =
      (static_cast<Int128>(bid) - ask) * 10'000 / ask;
  return value > std::numeric_limits<std::int32_t>::max()
             ? std::numeric_limits<std::int32_t>::max()
             : static_cast<std::int32_t>(value);
}

std::uint8_t venue_id(Venue venue) noexcept {
  return static_cast<std::uint8_t>(venue);
}

bool semantic_equal(utils::md::wire::AggBboRecord left,
                    utils::md::wire::AggBboRecord right,
                    std::uint16_t left_flags,
                    std::uint16_t right_flags) noexcept {
  left.header = {};
  right.header = {};
  left.gated_bid.worst_ingress_age_us = 0;
  right.gated_bid.worst_ingress_age_us = 0;
  left.gated_ask.worst_ingress_age_us = 0;
  right.gated_ask.worst_ingress_age_us = 0;
  left.fx_age_us = 0;
  right.fx_age_us = 0;
  return left_flags == right_flags &&
         std::memcmp(&left, &right, sizeof(left)) == 0;
}

bool semantic_equal(
    const utils::md::wire::AggOrderBookRecord &left,
    const utils::md::wire::AggOrderBookRecord &right) noexcept {
  const auto *left_payload =
      reinterpret_cast<const std::byte *>(&left) +
      sizeof(utils::md::wire::RecordHeader);
  const auto *right_payload =
      reinterpret_cast<const std::byte *>(&right) +
      sizeof(utils::md::wire::RecordHeader);
  return std::memcmp(
             left_payload, right_payload,
             sizeof(utils::md::wire::AggOrderBookRecord) -
                 sizeof(utils::md::wire::RecordHeader)) == 0;
}

}  // namespace

AggregationEngine::AggregationEngine(EngineConfig config) noexcept
    : config_(config) {}

bool AggregationEngine::add_member(const MemberConfig &config) noexcept {
  if (member_count_ >= kMaxMembers || config.venue == Venue::Unknown ||
      config.ttl_us == 0 || config.price_scale > config_.price_scale ||
      config.quantity_scale > config_.quantity_scale ||
      (config.convert_usdc_to_usdt &&
       config.venue != Venue::Hyperliquid)) {
    return false;
  }
  auto &member = members_[member_count_++];
  member.config = config;
  member.raw_liveness_us = derive_raw_liveness_us(config.ttl_us);
  member.configured = true;
  ++generation_;
  return true;
}

bool AggregationEngine::reconfigure_member(
    std::size_t slot, const MemberConfig &config) noexcept {
  if (slot >= member_count_ || config.venue == Venue::Unknown ||
      config.venue != members_[slot].config.venue ||
      config.ttl_us == 0 || config.price_scale > config_.price_scale ||
      config.quantity_scale > config_.quantity_scale ||
      (config.convert_usdc_to_usdt &&
       config.venue != Venue::Hyperliquid)) {
    exclude_member(slot);
    return false;
  }
  auto &member = members_[slot];
  member.config = config;
  member.raw_liveness_us = derive_raw_liveness_us(config.ttl_us);
  member.configured = true;
  member.bbo_valid = false;
  member.book_valid = false;
  ++generation_;
  return true;
}

void AggregationEngine::invalidate_member(std::size_t slot) noexcept {
  if (slot >= member_count_) {
    return;
  }
  auto &member = members_[slot];
  member.bbo_valid = false;
  member.book_valid = false;
  ++generation_;
}

void AggregationEngine::exclude_member(std::size_t slot) noexcept {
  if (slot >= member_count_) {
    return;
  }
  auto &member = members_[slot];
  member.configured = false;
  member.bbo_valid = false;
  member.book_valid = false;
  ++generation_;
}

void AggregationEngine::invalidate_fx() noexcept {
  fx_.valid = false;
  ++generation_;
}

bool AggregationEngine::update_bbo(
    std::size_t slot, const utils::md::wire::BboRecord &record,
    std::uint64_t ingress_mono_ns) noexcept {
  if (slot >= member_count_ ||
      !members_[slot].configured ||
      record.header.state !=
          static_cast<std::uint8_t>(utils::md::BookState::Live) ||
      record.bid_price <= 0 || record.ask_price <= 0 ||
      record.bid_quantity < 0 || record.ask_quantity < 0) {
    return false;
  }
  auto &member = members_[slot];
  const bool rejoined = !member.bbo_valid;
  member.bbo = record;
  member.bbo_ingress_ns = ingress_mono_ns;
  member.bbo_valid = true;
  if (rejoined) {
    ++generation_;
  }
  return true;
}

bool AggregationEngine::update_book(std::size_t slot,
                                    const BookInput &input) noexcept {
  if (slot >= member_count_ || !members_[slot].configured ||
      input.bids.empty() || input.asks.empty()) {
    return false;
  }
  const auto valid_ladder = [](std::span<const utils::md::Level> levels,
                               Side side) noexcept {
    for (std::size_t index = 0; index < levels.size(); ++index) {
      if (levels[index].price <= 0 || levels[index].quantity <= 0) {
        return false;
      }
      if (index != 0 &&
          (side == Side::Bid
               ? levels[index - 1].price <= levels[index].price
               : levels[index - 1].price >= levels[index].price)) {
        return false;
      }
    }
    return true;
  };
  if (!valid_ladder(input.bids, Side::Bid) ||
      !valid_ladder(input.asks, Side::Ask)) {
    return false;
  }
  auto &member = members_[slot];
  const auto bid_count = std::min(input.bids.size(), kLevelsPerMember);
  const auto ask_count = std::min(input.asks.size(), kLevelsPerMember);
  std::copy_n(input.bids.begin(), bid_count, member.bids.begin());
  std::copy_n(input.asks.begin(), ask_count, member.asks.begin());
  member.bid_count = bid_count;
  member.ask_count = ask_count;
  member.book_ingress_ns = input.ingress_mono_ns;
  member.book_exchange_ts_ns = input.exchange_ts_ns;
  const bool rejoined =
      !member.book_valid || member.generation != input.generation;
  member.generation = input.generation;
  member.book_valid = true;
  if (rejoined) {
    ++generation_;
  }
  return true;
}

void AggregationEngine::update_fx(
    const utils::md::wire::BboRecord &record, std::uint8_t price_scale,
    std::uint64_t ingress_mono_ns, Venue venue) noexcept {
  fx_.bbo = record;
  fx_.price_scale = price_scale;
  fx_.ingress_ns = ingress_mono_ns;
  fx_.venue = venue;
  fx_.valid = venue == Venue::Binance &&
      record.header.state ==
          static_cast<std::uint8_t>(utils::md::BookState::Live) &&
      record.bid_price > 0 && record.ask_price > 0;
}

BboBuild AggregationEngine::build_bbo(std::uint64_t now_mono_ns) noexcept {
  BboBuild result{};
  auto &output = result.record;
  output.header.instrument_id = config_.instrument_id;
  output.header.book_generation = generation_;
  output.header.source_seq = ++source_sequence_;
  output.header.state = static_cast<std::uint8_t>(utils::md::BookState::Live);
  output.base_asset = config_.base_asset;
  output.quote_asset = config_.quote_asset;
  output.price_scale = config_.price_scale;
  output.quantity_scale = config_.quantity_scale;
  output.member_count = static_cast<std::uint8_t>(member_count_);
  output.cross_skew_threshold_us = config_.cross_skew_threshold_us;

  std::uint32_t raw_mask = 0;
  std::uint32_t gated_mask = 0;
  bool needs_fx = false;
  for (std::size_t slot = 0; slot < member_count_; ++slot) {
    const auto &member = members_[slot];
    const auto bit = std::uint32_t{1} << slot;
    output.venue_slot_ids[slot] = venue_id(member.config.venue);
    if (!member.configured) {
      continue;
    }
    output.member_mask |= bit;
    if (!member.bbo_valid) {
      continue;
    }
    const auto age = age_us(now_mono_ns, member.bbo_ingress_ns);
    const bool fx_required = member.config.convert_usdc_to_usdt;
    bool fx_usable = true;
    if (fx_required) {
      needs_fx = true;
      const auto fx_age = age_us(now_mono_ns, fx_.ingress_ns);
      output.fx_age_us = clamp_u32(fx_age);
      output.fx_venue = venue_id(fx_.venue);
      std::int64_t one{};
      const bool one_valid = rescale(1, 0, fx_.price_scale, one);
      const Int128 midpoint =
          (static_cast<Int128>(fx_.bbo.bid_price) + fx_.bbo.ask_price) / 2;
      const Int128 deviation = midpoint > one ? midpoint - one : one - midpoint;
      fx_usable = fx_.valid && one_valid && fx_age <= config_.fx_ttl_us &&
                  deviation * 10'000 <=
                      static_cast<Int128>(one) * config_.fx_max_depeg_bps;
    }
    if (!fx_usable) {
      continue;
    }
    if (age <= member.raw_liveness_us) {
      raw_mask |= bit;
    }
    if (age <= member.config.ttl_us) {
      gated_mask |= bit;
    }
  }
  if (!needs_fx) {
    output.fx_age_us = 0;
    output.fx_venue = 0;
  }

  const auto normalized = [&](std::size_t slot, Side side,
                              std::int64_t &price,
                              std::int64_t &quantity) noexcept {
    const auto &member = members_[slot];
    price = side == Side::Bid ? member.bbo.bid_price : member.bbo.ask_price;
    quantity =
        side == Side::Bid ? member.bbo.bid_quantity : member.bbo.ask_quantity;
    if (member.config.convert_usdc_to_usdt) {
      const auto fx_price =
          side == Side::Bid ? fx_.bbo.bid_price : fx_.bbo.ask_price;
      if (!multiply_scaled(price, member.config.price_scale, fx_price,
                           fx_.price_scale, config_.price_scale,
                           side == Side::Ask, price)) {
        return false;
      }
    } else if (!rescale(price, member.config.price_scale, config_.price_scale,
                        price)) {
      return false;
    }
    return rescale(quantity, member.config.quantity_scale,
                   config_.quantity_scale, quantity);
  };

  const auto raw_side = [&](Side side, std::uint32_t mask) noexcept {
    AggBboRawSide value{};
    bool found = false;
    for (std::size_t slot = 0; slot < member_count_; ++slot) {
      if ((mask & (std::uint32_t{1} << slot)) == 0) {
        continue;
      }
      std::int64_t price{};
      std::int64_t quantity{};
      if (!normalized(slot, side, price, quantity)) {
        continue;
      }
      const bool better =
          !found || (side == Side::Bid ? price > value.price
                                       : price < value.price);
      if (better) {
        value = {};
        value.price = price;
        value.quantity = quantity;
        value.best_venue = static_cast<std::uint8_t>(slot);
        value.venue_mask = std::uint32_t{1} << slot;
        found = true;
      } else if (price == value.price) {
        value.quantity = saturating_add(value.quantity, quantity);
        value.venue_mask |= std::uint32_t{1} << slot;
      }
    }
    return value;
  };

  const auto gated_side = [&](Side side, std::uint32_t mask) noexcept {
    AggBboSide value{};
    bool found = false;
    for (std::size_t slot = 0; slot < member_count_; ++slot) {
      if ((mask & (std::uint32_t{1} << slot)) == 0) {
        continue;
      }
      std::int64_t price{};
      std::int64_t quantity{};
      if (!normalized(slot, side, price, quantity)) {
        continue;
      }
      const bool better =
          !found || (side == Side::Bid ? price > value.price
                                       : price < value.price);
      if (better) {
        value = {};
        value.price = price;
        value.best_venue = static_cast<std::uint8_t>(slot);
        value.timestamp_venue = static_cast<std::uint8_t>(slot);
        found = true;
      }
      if (price != value.price) {
        continue;
      }
      value.quantity = saturating_add(value.quantity, quantity);
      value.venue_quantity[slot] = quantity;
      value.venue_mask |= std::uint32_t{1} << slot;
      ++value.contributor_count;
      value.worst_ingress_age_us = std::max(
          value.worst_ingress_age_us,
          clamp_u32(age_us(now_mono_ns, members_[slot].bbo_ingress_ns)));
      const auto timestamp = members_[slot].bbo.header.exchange_ts_ns;
      if (timestamp != 0 &&
          (value.exchange_ts_ns == 0 || timestamp < value.exchange_ts_ns)) {
        value.exchange_ts_ns = timestamp;
        value.timestamp_venue = static_cast<std::uint8_t>(slot);
      }
    }
    return value;
  };

  output.raw_bid = raw_side(Side::Bid, raw_mask);
  output.raw_ask = raw_side(Side::Ask, raw_mask);
  output.raw_cross_bps = cross_bps(output.raw_bid.price, output.raw_ask.price);

  std::uint32_t enforced_mask = gated_mask;
  output.gated_bid = gated_side(Side::Bid, enforced_mask);
  output.gated_ask = gated_side(Side::Ask, enforced_mask);
  if (output.gated_bid.price > output.gated_ask.price &&
      output.gated_bid.timestamp_venue ==
          output.gated_ask.timestamp_venue) {
    result.member_data_error = true;
  }
  for (std::size_t iteration = 0;
       !config_.cross_skew_observe_only && iteration + 1 < member_count_ &&
       output.gated_bid.price > output.gated_ask.price;
       ++iteration) {
    const auto bid_slot = output.gated_bid.timestamp_venue;
    const auto ask_slot = output.gated_ask.timestamp_venue;
    if (bid_slot == ask_slot || output.gated_bid.exchange_ts_ns == 0 ||
        output.gated_ask.exchange_ts_ns == 0) {
      result.member_data_error = bid_slot == ask_slot;
      break;
    }
    const auto difference =
        output.gated_bid.exchange_ts_ns > output.gated_ask.exchange_ts_ns
            ? output.gated_bid.exchange_ts_ns -
                  output.gated_ask.exchange_ts_ns
            : output.gated_ask.exchange_ts_ns -
                  output.gated_bid.exchange_ts_ns;
    output.skew_us = clamp_u32(difference / 1'000U);
    if (output.skew_us <= config_.cross_skew_threshold_us) {
      break;
    }
    const auto older_slot =
        output.gated_bid.exchange_ts_ns < output.gated_ask.exchange_ts_ns
            ? bid_slot
            : ask_slot;
    if (older_slot >= member_count_ ||
        (enforced_mask & (std::uint32_t{1} << older_slot)) == 0) {
      result.member_data_error = true;
      break;
    }
    enforced_mask &= ~(std::uint32_t{1} << older_slot);
    result.flags |= utils::md::wire::kAggSkewEnforced;
    output.gated_bid = gated_side(Side::Bid, enforced_mask);
    output.gated_ask = gated_side(Side::Ask, enforced_mask);
  }
  if (output.gated_bid.exchange_ts_ns != 0 &&
      output.gated_ask.exchange_ts_ns != 0) {
    const auto difference =
        output.gated_bid.exchange_ts_ns > output.gated_ask.exchange_ts_ns
            ? output.gated_bid.exchange_ts_ns -
                  output.gated_ask.exchange_ts_ns
            : output.gated_ask.exchange_ts_ns -
                  output.gated_bid.exchange_ts_ns;
    output.skew_us = clamp_u32(difference / 1'000U);
  }
  output.live_mask = enforced_mask;
  output.cross_bid_venue = output.gated_bid.timestamp_venue;
  output.cross_ask_venue = output.gated_ask.timestamp_venue;
  output.gated_cross_bps =
      cross_bps(output.gated_bid.price, output.gated_ask.price);
  if (result.member_data_error) {
    result.flags |= utils::md::wire::kAggMemberDataError;
  }

  result.changed =
      !have_previous_bbo_ ||
      !semantic_equal(output, previous_bbo_, result.flags, previous_flags_);
  if (result.changed) {
    previous_bbo_ = output;
    previous_flags_ = result.flags;
    have_previous_bbo_ = true;
  }
  return result;
}

BookBuild
AggregationEngine::build_orderbook(std::uint64_t now_mono_ns) noexcept {
  BookBuild result{};
  auto &output = result.record;
  output.header.instrument_id = config_.instrument_id;
  output.header.book_generation = generation_;
  output.header.source_seq = ++source_sequence_;
  output.header.state = static_cast<std::uint8_t>(utils::md::BookState::Live);
  output.base_asset = config_.base_asset;
  output.quote_asset = config_.quote_asset;
  output.price_scale = config_.price_scale;
  output.quantity_scale = config_.quantity_scale;
  output.member_count = static_cast<std::uint8_t>(member_count_);

  for (std::size_t slot = 0; slot < member_count_; ++slot) {
    const auto &member = members_[slot];
    const auto bit = std::uint32_t{1} << slot;
    output.venue_slot_ids[slot] = venue_id(member.config.venue);
    if (!member.configured) {
      continue;
    }
    output.member_mask |= bit;
    if (!member.book_valid ||
        age_us(now_mono_ns, member.book_ingress_ns) > member.config.ttl_us) {
      continue;
    }
    bool fx_usable = true;
    if (member.config.convert_usdc_to_usdt) {
      const auto fx_age = age_us(now_mono_ns, fx_.ingress_ns);
      std::int64_t one{};
      const bool one_valid = rescale(1, 0, fx_.price_scale, one);
      const Int128 midpoint =
          (static_cast<Int128>(fx_.bbo.bid_price) + fx_.bbo.ask_price) / 2;
      const Int128 deviation =
          midpoint > one ? midpoint - one : one - midpoint;
      fx_usable = fx_.valid && one_valid && fx_age <= config_.fx_ttl_us &&
                  deviation * 10'000 <=
                      static_cast<Int128>(one) *
                          config_.fx_max_depeg_bps;
    }
    if (!fx_usable) {
      continue;
    }
    output.active_mask |= bit;
  }

  const auto normalize_level = [&](std::size_t slot, Side side,
                                   std::size_t index,
                                   AggLevel &level) noexcept {
    const auto &member = members_[slot];
    const auto &source = side == Side::Bid ? member.bids : member.asks;
    level = {};
    level.price = source[index].price;
    level.quantity = source[index].quantity;
    bool valid = true;
    if (member.config.convert_usdc_to_usdt) {
      const auto fx_price =
          side == Side::Bid ? fx_.bbo.bid_price : fx_.bbo.ask_price;
      valid = multiply_scaled(
          level.price, member.config.price_scale, fx_price, fx_.price_scale,
          config_.price_scale, side == Side::Ask, level.price);
    } else {
      valid = rescale(level.price, member.config.price_scale,
                      config_.price_scale, level.price);
    }
    return valid &&
           rescale(level.quantity, member.config.quantity_scale,
                   config_.quantity_scale, level.quantity) &&
           level.quantity > 0;
  };

  const auto merge_side = [&](Side side, auto &destination,
                              std::uint16_t &destination_count) noexcept {
    std::array<std::size_t, kMaxMembers> cursors{};
    while (destination_count < destination.size()) {
      std::int64_t best_price{};
      bool found = false;
      for (std::size_t slot = 0; slot < member_count_; ++slot) {
        if ((output.active_mask & (std::uint32_t{1} << slot)) == 0) {
          continue;
        }
        const auto count = side == Side::Bid ? members_[slot].bid_count
                                             : members_[slot].ask_count;
        while (cursors[slot] < count) {
          AggLevel candidate{};
          if (!normalize_level(slot, side, cursors[slot], candidate)) {
            ++cursors[slot];
            continue;
          }
          if (!found ||
              (side == Side::Bid ? candidate.price > best_price
                                 : candidate.price < best_price)) {
            best_price = candidate.price;
            found = true;
          }
          break;
        }
      }
      if (!found) {
        break;
      }

      AggLevel aggregate{};
      aggregate.price = best_price;
      for (std::size_t slot = 0; slot < member_count_; ++slot) {
        if ((output.active_mask & (std::uint32_t{1} << slot)) == 0) {
          continue;
        }
        const auto count = side == Side::Bid ? members_[slot].bid_count
                                             : members_[slot].ask_count;
        bool contributed = false;
        while (cursors[slot] < count) {
          AggLevel candidate{};
          if (!normalize_level(slot, side, cursors[slot], candidate)) {
            ++cursors[slot];
            continue;
          }
          if (candidate.price != best_price) {
            break;
          }
          aggregate.quantity =
              saturating_add(aggregate.quantity, candidate.quantity);
          aggregate.venue_quantity[slot] = saturating_add(
              aggregate.venue_quantity[slot], candidate.quantity);
          contributed = true;
          ++cursors[slot];
        }
        if (contributed) {
          aggregate.venue_mask |= std::uint32_t{1} << slot;
          ++aggregate.contributor_count;
        }
      }
      destination[destination_count++] = aggregate;
    }
  };

  merge_side(Side::Bid, output.bids, output.bid_count);
  merge_side(Side::Ask, output.asks, output.ask_count);
  result.changed =
      !have_previous_book_ || !semantic_equal(output, previous_book_);
  if (result.changed) {
    previous_book_ = output;
    have_previous_book_ = true;
  }
  return result;
}

}  // namespace mds::agg
