#include "fairprice/json.h"
#include "fairprice/store.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstring>

namespace fairprice {
namespace {

void append_fixed(std::string &out, std::int64_t mantissa, std::uint8_t scale) {
  out += "{\"mantissa\":\"";
  append_i64(out, mantissa);
  out += "\",\"scale\":";
  append_u64(out, scale);
  out += '}';
}

void append_contributions(std::string &out,
                          const std::array<std::int64_t, 8> &quantities,
                          std::uint32_t mask,
                          const std::array<std::uint8_t, 8> &slots,
                          std::uint8_t scale) {
  out += '[';
  bool first = true;
  for (int i = 0; i < 8; ++i) {
    if ((mask & (1U << i)) == 0) {
      continue;
    }
    if (!first) {
      out += ',';
    }
    first = false;
    out += "{\"venue\":\"";
    out += venue_name(slots[static_cast<std::size_t>(i)]);
    out += "\",\"quantity\":";
    append_fixed(out, quantities[static_cast<std::size_t>(i)], scale);
    out += '}';
  }
  out += ']';
}

}  // namespace

std::string_view venue_name(std::uint8_t id) {
  switch (id) {
    case 1:
      return "binance";
    case 2:
      return "okx";
    case 3:
      return "bybit";
    case 4:
      return "gate";
    case 5:
      return "bitget";
    case 6:
      return "polymarket";
    case 7:
      return "sse";
    case 8:
      return "hyperliquid";
    case 9:
      return "aster";
    case 10:
      return "lighter";
    default: {
      thread_local char buf[32];
      std::snprintf(buf, sizeof(buf), "venue-%u", static_cast<unsigned>(id));
      return buf;
    }
  }
}

std::string json_escape(std::string_view value) {
  std::string out;
  out.reserve(value.size());
  for (char ch : value) {
    const unsigned char c = static_cast<unsigned char>(ch);
    switch (c) {
      case '"':
        out += "\\\"";
        break;
      case '\\':
        out += "\\\\";
        break;
      case '\n':
        out += "\\n";
        break;
      case '\r':
        out += "\\r";
        break;
      case '\t':
        out += "\\t";
        break;
      default:
        out.push_back(static_cast<char>(c));
        break;
    }
  }
  return out;
}

void append_u64(std::string &out, std::uint64_t value) {
  char buf[32]{};
  std::snprintf(buf, sizeof(buf), "%llu",
                static_cast<unsigned long long>(value));
  out += buf;
}

void append_i64(std::string &out, std::int64_t value) {
  char buf[32]{};
  std::snprintf(buf, sizeof(buf), "%lld", static_cast<long long>(value));
  out += buf;
}

void append_f64(std::string &out, double value) {
  if (!std::isfinite(value)) {
    out += "0";
    return;
  }
  char buf[64]{};
  std::snprintf(buf, sizeof(buf), "%.17g", value);
  out += buf;
}

std::string encode_status_json(std::string_view status) {
  return std::string("{\"status\":\"") + json_escape(status) + "\"}\n";
}

std::string encode_error_json(std::string_view error) {
  return std::string("{\"error\":\"") + json_escape(error) + "\"}\n";
}

std::string encode_reset_json(std::string_view profile, std::string_view symbol,
                              std::string_view channel,
                              std::string_view reason) {
  std::string out = "{\"op\":\"reset\",\"profile\":\"";
  out += json_escape(profile);
  out += "\",\"symbol\":\"";
  out += json_escape(symbol);
  out += "\",\"channel\":\"";
  out += json_escape(channel);
  out += "\",\"reason\":\"";
  out += json_escape(reason);
  out += "\"}";
  return out;
}

std::string encode_ack_json(bool ok, std::string_view op,
                            std::string_view profile, std::string_view symbol,
                            std::string_view channel, std::string_view error) {
  std::string out = "{\"ok\":";
  out += ok ? "true" : "false";
  out += ",\"op\":\"";
  out += json_escape(op);
  out += '"';
  if (!ok) {
    out += ",\"error\":\"";
    out += json_escape(error);
    out += '"';
  } else if (!profile.empty()) {
    out += ",\"profile\":\"";
    out += json_escape(profile);
    out += "\",\"symbol\":\"";
    out += json_escape(symbol);
    out += "\",\"channel\":\"";
    out += json_escape(channel);
    out += '"';
  }
  out += '}';
  return out;
}

std::string encode_markets_json(const std::vector<MarketJson> &markets) {
  std::string out = "{\"markets\":[";
  bool first = true;
  for (const auto &market : markets) {
    if (!first) {
      out += ',';
    }
    first = false;
    out += "{\"profile\":\"";
    out += json_escape(market.profile);
    out += "\",\"symbol\":\"";
    out += json_escape(market.symbol);
    out += "\",\"base\":\"";
    out += json_escape(market.base);
    out += "\",\"quote\":\"";
    out += json_escape(market.quote);
    out += "\",\"venues\":[";
    for (std::size_t i = 0; i < market.venues.size(); ++i) {
      if (i) {
        out += ',';
      }
      out += '"';
      out += json_escape(market.venues[i]);
      out += '"';
    }
    out += "],\"price_scale\":";
    append_u64(out, market.price_scale);
    out += ",\"quantity_scale\":";
    append_u64(out, market.quantity_scale);
    out += ",\"has_bbo\":";
    out += market.has_bbo ? "true" : "false";
    out += ",\"has_order_book\":";
    out += market.has_order_book ? "true" : "false";
    out += ",\"recording\":";
    out += market.recording ? "true" : "false";
    out += ",\"live\":";
    out += market.live ? "true" : "false";
    out += ",\"updated_ns\":";
    append_u64(out, market.updated_ns);
    out += '}';
  }
  out += "]}\n";
  return out;
}

std::string encode_fair_price_json(std::string_view profile,
                                   std::string_view symbol,
                                   const FairPricePod &pod) {
  std::string out;
  out.reserve(1024);
  out += "{\"type\":\"data\",\"channel\":\"fairprice\",\"profile\":\"";
  out += json_escape(profile);
  out += "\",\"symbol\":\"";
  out += json_escape(symbol);
  out += "\",\"model_id\":\"";
  out += json_escape(cstr_view(pod.model_id));
  out += "\",\"ring_epoch\":\"";
  append_u64(out, pod.ring_epoch);
  out += "\",\"seq\":\"";
  append_u64(out, pod.ring_sequence);
  out += "\",\"generation\":\"";
  append_u64(out, pod.generation);
  out += "\",\"wall_ns\":\"";
  append_u64(out, pod.wall_ns);
  out += "\",\"exchange_ts_ns\":\"";
  append_u64(out, pod.exchange_ts_ns);
  out += "\",\"price\":";
  append_fixed(out, pod.price, pod.output_scale);
  out += ",\"price_raw\":";
  append_fixed(out, pod.price_raw, pod.output_scale);
  out += ",\"mid\":";
  append_fixed(out, pod.mid, pod.output_scale);
  out += ",\"microprice\":";
  append_fixed(out, pod.microprice, pod.output_scale);
  out += ",\"best_bid\":";
  append_fixed(out, pod.best_bid, pod.output_scale);
  out += ",\"best_ask\":";
  append_fixed(out, pod.best_ask, pod.output_scale);
  out += ",\"effective_bid\":";
  append_fixed(out, pod.effective_bid, pod.output_scale);
  out += ",\"effective_ask\":";
  append_fixed(out, pod.effective_ask, pod.output_scale);
  out += ",\"impact_bid\":";
  if (pod.has_impact_bid) {
    append_fixed(out, pod.impact_bid, pod.output_scale);
  } else {
    out += "null";
  }
  out += ",\"impact_ask\":";
  if (pod.has_impact_ask) {
    append_fixed(out, pod.impact_ask, pod.output_scale);
  } else {
    out += "null";
  }
  out += ",\"crossed_quantity\":";
  if (pod.has_crossed_quantity) {
    append_fixed(out, pod.crossed_quantity, pod.quantity_scale);
  } else {
    out += "null";
  }
  out += ",\"imbalance\":";
  append_f64(out, pod.imbalance);
  out += ",\"spread_bps\":";
  append_f64(out, pod.spread_bps);
  out += ",\"cross_bps\":";
  if (pod.has_cross_bps) {
    append_f64(out, pod.cross_bps);
  } else {
    out += "null";
  }
  out += ",\"weighted_bid_depth\":";
  append_f64(out, pod.weighted_bid_depth);
  out += ",\"weighted_ask_depth\":";
  append_f64(out, pod.weighted_ask_depth);
  out += ",\"depth_bid_notional\":";
  append_f64(out, pod.depth_bid_notional);
  out += ",\"depth_ask_notional\":";
  append_f64(out, pod.depth_ask_notional);
  out += ",\"active_venue_mask\":";
  append_u64(out, pod.active_venue_mask);
  out += ",\"crossed\":";
  out += pod.crossed ? "true" : "false";
  out += ",\"degraded\":";
  out += pod.degraded ? "true" : "false";
  out += ",\"degraded_reasons\":";
  if (pod.degraded_mask == 0) {
    out += "null";
  } else {
    out += '[';
    append_degraded_reasons(pod.degraded_mask, out);
    out += ']';
  }
  out += "}";
  return out;
}

std::string encode_snapshot_json(std::string_view profile,
                                 std::string_view symbol, const BboView *bbo,
                                 const BookSnapshot *book, int depth) {
  const bool bbo_ready = bbo && bbo->ready;
  const bool book_ready = book && book->ready;
  if (!bbo_ready && !book_ready) {
    return {};
  }
  std::string out = "{\"symbol\":\"";
  out += json_escape(symbol);
  out += "\",\"profile\":\"";
  out += json_escape(profile);
  out += "\",\"ready\":true";
  if (bbo_ready) {
    const auto &value = bbo->record;
    out += ",\"bbo\":{\"sequence\":\"";
    append_u64(out, bbo->ring_sequence);
    out += "\",\"generation\":\"";
    append_u64(out, bbo->generation);
    out += "\",\"wall_ns\":\"";
    append_u64(out, bbo->wall_ns);
    out += "\",\"price_scale\":";
    append_u64(out, value.price_scale);
    out += ",\"quantity_scale\":";
    append_u64(out, value.quantity_scale);
    auto side = [&](const utils::md::wire::AggBboSide &side) {
      out += "{\"price\":";
      append_fixed(out, side.price, value.price_scale);
      out += ",\"quantity\":";
      append_fixed(out, side.quantity, value.quantity_scale);
      out += ",\"venue_mask\":";
      append_u64(out, side.venue_mask);
      out += ",\"contributions\":";
      append_contributions(out, side.venue_quantity, side.venue_mask,
                           value.venue_slot_ids, value.quantity_scale);
      out += '}';
    };
    out += ",\"gated_bid\":";
    side(value.gated_bid);
    out += ",\"gated_ask\":";
    side(value.gated_ask);
    out += ",\"raw_bid\":{\"price\":";
    append_fixed(out, value.raw_bid.price, value.price_scale);
    out += ",\"quantity\":";
    append_fixed(out, value.raw_bid.quantity, value.quantity_scale);
    out += ",\"venue_mask\":";
    append_u64(out, value.raw_bid.venue_mask);
    out += ",\"best_venue\":";
    append_u64(out, value.raw_bid.best_venue);
    out += "},\"raw_ask\":{\"price\":";
    append_fixed(out, value.raw_ask.price, value.price_scale);
    out += ",\"quantity\":";
    append_fixed(out, value.raw_ask.quantity, value.quantity_scale);
    out += ",\"venue_mask\":";
    append_u64(out, value.raw_ask.venue_mask);
    out += ",\"best_venue\":";
    append_u64(out, value.raw_ask.best_venue);
    out += "},\"raw_spread_bps\":";
    append_f64(out, signed_spread_bps(value.raw_bid.price, value.raw_ask.price));
    out += ",\"gated_spread_bps\":";
    append_f64(out,
               signed_spread_bps(value.gated_bid.price, value.gated_ask.price));
    out += ",\"raw_cross_bps\":";
    append_i64(out, value.raw_cross_bps);
    out += ",\"gated_cross_bps\":";
    append_i64(out, value.gated_cross_bps);
    out += "}";
  }
  if (book_ready) {
    out += ",\"orderbook\":{\"sequence\":\"";
    append_u64(out, book->ring_sequence);
    out += "\",\"generation\":\"";
    append_u64(out, book->generation);
    out += "\",\"wall_ns\":\"";
    append_u64(out, book->wall_ns);
    out += "\",\"price_scale\":";
    append_u64(out, book->price_scale);
    out += ",\"quantity_scale\":";
    append_u64(out, book->quantity_scale);
    auto levels = [&](const std::array<ResidualLevel, kMaxDepth> &src, int count) {
      out += '[';
      const int n = std::min(depth, count);
      for (int i = 0; i < n; ++i) {
        if (i) {
          out += ',';
        }
        const auto &level = src[static_cast<std::size_t>(i)];
        out += "{\"price\":";
        append_fixed(out, level.price, book->price_scale);
        out += ",\"quantity\":";
        append_fixed(out, level.quantity, book->quantity_scale);
        out += ",\"venue_mask\":";
        append_u64(out, level.venue_mask);
        out += ",\"contributions\":";
        append_contributions(out, level.venue_quantity, level.venue_mask,
                             book->venue_slot_ids, book->quantity_scale);
        out += '}';
      }
      out += ']';
    };
    out += ",\"bids\":";
    levels(book->bids, book->bid_count);
    out += ",\"asks\":";
    levels(book->asks, book->ask_count);
    out += "}";
  }
  out += "}\n";
  return out;
}

}  // namespace fairprice
