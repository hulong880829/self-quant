#include "aggregate_dashboard.h"

#include "utils/md/types.h"

#include <algorithm>
#include <bit>
#include <cmath>
#include <iomanip>
#include <limits>
#include <sstream>
#include <string>

namespace mds::examples {
namespace {

namespace md = utils::md;
namespace wire = utils::md::wire;

constexpr std::string_view kReset = "\033[0m";
constexpr std::string_view kRed = "\033[31;1m";
constexpr std::string_view kGreen = "\033[32;1m";
constexpr std::string_view kDim = "\033[2m";

template <std::size_t Size>
std::string fixed_text(const std::array<char, Size> &value) {
  const auto end =
      std::find(value.begin(), value.end(), static_cast<char>('\0'));
  return {value.begin(), end};
}

std::string decimal_text(std::int64_t mantissa, std::uint8_t scale) {
  const bool negative = mantissa < 0;
  const auto magnitude =
      negative ? std::uint64_t{0} - static_cast<std::uint64_t>(mantissa)
               : static_cast<std::uint64_t>(mantissa);
  std::string digits = std::to_string(magnitude);
  const auto places = static_cast<std::size_t>(scale);
  if (places != 0) {
    if (digits.size() <= places) {
      digits.insert(0, places + 1 - digits.size(), '0');
    }
    digits.insert(digits.size() - places, 1, '.');
  }
  if (negative) {
    digits.insert(0, 1, '-');
  }
  return digits;
}

std::string_view venue_name(std::uint8_t venue, bool compact) noexcept {
  switch (static_cast<md::Venue>(venue)) {
    case md::Venue::Binance:
      return compact ? "BN" : "Binance";
    case md::Venue::Okx:
      return "OKX";
    case md::Venue::Bybit:
      return compact ? "BY" : "Bybit";
    case md::Venue::Gate:
      return "Gate";
    case md::Venue::Bitget:
      return compact ? "BG" : "Bitget";
    case md::Venue::Hyperliquid:
      return compact ? "HL" : "Hyperliquid";
    case md::Venue::Aster:
      return compact ? "AS" : "Aster";
    case md::Venue::Lighter:
      return compact ? "LT" : "Lighter";
    case md::Venue::Polymarket:
      return compact ? "PM" : "Polymarket";
    case md::Venue::Sse:
      return "SSE";
    case md::Venue::Unknown:
      return "Unknown";
  }
  return "Unknown";
}

std::string product_text(std::string_view segment) {
  if (segment.find(".agg_perp_") != std::string_view::npos) {
    return "PERP";
  }
  if (segment.find(".agg_spot_") != std::string_view::npos) {
    return "SPOT";
  }
  return "AGG";
}

std::string spread_text(std::int64_t bid, std::int64_t ask) {
  if (bid <= 0 || ask <= 0 || bid + ask == 0) {
    return "n/a";
  }
  const auto midpoint =
      (static_cast<long double>(bid) + static_cast<long double>(ask)) / 2.0L;
  const auto bps =
      (static_cast<long double>(ask) - static_cast<long double>(bid)) *
      10'000.0L / midpoint;
  std::ostringstream output;
  output << std::fixed << std::setprecision(3) << std::fabs(bps);
  return output.str();
}

template <typename Side>
std::string bbo_pair(const Side &bid, const Side &ask,
                     std::uint8_t price_scale,
                     std::uint8_t quantity_scale) {
  return decimal_text(bid.price, price_scale) + '@' +
         decimal_text(bid.quantity, quantity_scale) + " / " +
         decimal_text(ask.price, price_scale) + '@' +
         decimal_text(ask.quantity, quantity_scale);
}

std::string venue_contributions(
    const wire::AggLevel &level,
    const std::array<std::uint8_t, wire::kAggVenueSlots> &slots,
    std::uint8_t member_count, std::uint8_t quantity_scale, bool compact,
    std::size_t maximum_width) {
  std::string output;
  const auto count =
      std::min<std::size_t>(member_count, wire::kAggVenueSlots);
  for (std::size_t slot = 0; slot < count; ++slot) {
    const auto bit = std::uint32_t{1} << static_cast<std::uint32_t>(slot);
    if ((level.venue_mask & bit) == 0 ||
        level.venue_quantity[slot] == 0) {
      continue;
    }
    std::string item;
    if (!output.empty()) {
      item = " | ";
    }
    item += venue_name(slots[slot], compact);
    item += ' ';
    item += decimal_text(level.venue_quantity[slot], quantity_scale);
    if (output.size() + item.size() > maximum_width) {
      if (output.size() + 4 <= maximum_width) {
        output += " ...";
      }
      break;
    }
    output += item;
  }
  return output.empty() ? "-" : output;
}

std::string bbo_contributions(
    const wire::AggBboSide &side,
    const std::array<std::uint8_t, wire::kAggVenueSlots> &slots,
    std::uint8_t member_count, std::uint8_t quantity_scale, bool compact) {
  std::string output;
  const auto count =
      std::min<std::size_t>(member_count, wire::kAggVenueSlots);
  for (std::size_t slot = 0; slot < count; ++slot) {
    const auto bit = std::uint32_t{1} << static_cast<std::uint32_t>(slot);
    if ((side.venue_mask & bit) == 0 || side.venue_quantity[slot] == 0) {
      continue;
    }
    if (!output.empty()) {
      output += " | ";
    }
    output += venue_name(slots[slot], compact);
    output += ' ';
    output += decimal_text(side.venue_quantity[slot], quantity_scale);
  }
  return output.empty() ? "-" : output;
}

std::string depth_bar(std::int64_t quantity, std::uint64_t maximum,
                      std::size_t width) {
  if (quantity <= 0 || maximum == 0 || width == 0) {
    return {};
  }
  const auto ratio = static_cast<long double>(quantity) /
                     static_cast<long double>(maximum);
  auto filled = static_cast<std::size_t>(
      std::ceil(ratio * static_cast<long double>(width)));
  filled = std::clamp<std::size_t>(filled, 1, width);
  return std::string(filled, '#') + std::string(width - filled, ' ');
}

std::string status_line(const wire::AggBboRecord *bbo,
                        const wire::AggOrderBookRecord *book,
                        bool compact) {
  const auto *slots =
      bbo != nullptr ? &bbo->venue_slot_ids : &book->venue_slot_ids;
  const auto member_count =
      bbo != nullptr ? bbo->member_count : book->member_count;
  const auto configured =
      bbo != nullptr ? bbo->member_mask : book->member_mask;
  const auto live = bbo != nullptr ? bbo->live_mask : book->active_mask;
  std::string output;
  const auto count =
      std::min<std::size_t>(member_count, wire::kAggVenueSlots);
  for (std::size_t slot = 0; slot < count; ++slot) {
    if (!output.empty()) {
      output += " | ";
    }
    const auto bit = std::uint32_t{1} << static_cast<std::uint32_t>(slot);
    output += venue_name((*slots)[slot], compact);
    output += (configured & bit) == 0 ? " INVALID"
              : (live & bit) != 0     ? " LIVE"
                                      : " STALE";
  }
  return output;
}

void append_level(
    std::ostringstream &output, const wire::AggLevel &level,
    const std::array<std::uint8_t, wire::kAggVenueSlots> &slots,
    std::uint8_t member_count, std::uint8_t price_scale,
    std::uint8_t quantity_scale, std::uint64_t maximum_quantity,
    std::size_t bar_width, std::size_t terminal_width, bool compact) {
  const auto price = decimal_text(level.price, price_scale);
  const auto quantity = decimal_text(level.quantity, quantity_scale);
  const auto contributions_width =
      terminal_width > 48 ? terminal_width - 48 : std::size_t{12};
  output << std::setw(14) << price << "  [" << depth_bar(
      level.quantity, maximum_quantity, bar_width) << "] "
         << std::setw(12) << quantity << "  "
         << venue_contributions(level, slots, member_count, quantity_scale,
                                compact, contributions_width)
         << '\n';
}

}  // namespace

std::string render_aggregate_dashboard(const AggregateDashboardView &view) {
  const auto *bbo = view.bbo;
  const auto *book = view.book;
  std::ostringstream output;
  if (bbo == nullptr && book == nullptr) {
    return "Waiting for aggregate records...\n";
  }

  const auto base =
      bbo != nullptr ? fixed_text(bbo->base_asset) : fixed_text(book->base_asset);
  const auto quote = bbo != nullptr ? fixed_text(bbo->quote_asset)
                                    : fixed_text(book->quote_asset);
  const auto generation = bbo != nullptr ? bbo->header.book_generation
                                         : book->header.book_generation;
  const auto &segment =
      !view.bbo_segment.empty() ? view.bbo_segment : view.book_segment;
  output << base << '/' << quote << ' ' << product_text(segment);
  if (bbo != nullptr) {
    output << "    BBO LIVE "
           << std::popcount(bbo->live_mask & bbo->member_mask) << '/'
           << static_cast<unsigned>(bbo->member_count);
  }
  if (book != nullptr) {
    output << "    BOOK ACTIVE "
           << std::popcount(book->active_mask & book->member_mask) << '/'
           << static_cast<unsigned>(book->member_count);
  }
  output << "    Generation " << generation << '\n';
  output << "BBO ring_seq=" << view.bbo_ring_sequence
         << " bus_seq=" << (bbo != nullptr ? bbo->header.bus_seq : 0)
         << "    BOOK ring_seq=" << view.book_ring_sequence
         << " bus_seq=" << (book != nullptr ? book->header.bus_seq : 0)
         << "\n\n";

  if (bbo != nullptr) {
    const bool raw_crossed = bbo->raw_bid.price > bbo->raw_ask.price;
    const bool gated_crossed = bbo->gated_bid.price > bbo->gated_ask.price;
    output << "RAW BBO       "
           << bbo_pair(bbo->raw_bid, bbo->raw_ask, bbo->price_scale,
                       bbo->quantity_scale)
           << "    ";
    if (view.color && raw_crossed) {
      output << kRed;
    }
    output << (raw_crossed ? "CROSSED " : "spread ")
           << spread_text(bbo->raw_bid.price, bbo->raw_ask.price) << " bps";
    if (view.color && raw_crossed) {
      output << kReset;
    }
    output << '\n';
    output << "GATED BBO     "
           << bbo_pair(bbo->gated_bid, bbo->gated_ask, bbo->price_scale,
                       bbo->quantity_scale)
           << "    ";
    if (view.color && gated_crossed) {
      output << kRed;
    }
    output << (gated_crossed ? "CROSSED " : "spread ")
           << spread_text(bbo->gated_bid.price, bbo->gated_ask.price) << " bps";
    if (view.color && gated_crossed) {
      output << kReset;
    }
    output << "\nSkew " << bbo->skew_us / 1'000U << "ms / threshold "
           << bbo->cross_skew_threshold_us / 1'000U << "ms"
           << (((bbo->header.flags & wire::kAggSkewEnforced) != 0)
                   ? " / skew_enforced=yes"
                   : " / skew_enforced=no")
           << "    ingress_age bid=" << bbo->gated_bid.worst_ingress_age_us
           << "us ask=" << bbo->gated_ask.worst_ingress_age_us << "us\n"
           << "GATED ROUTES  bid={"
           << bbo_contributions(bbo->gated_bid, bbo->venue_slot_ids,
                                bbo->member_count, bbo->quantity_scale,
                                view.terminal_width < 100)
           << "}  ask={"
           << bbo_contributions(bbo->gated_ask, bbo->venue_slot_ids,
                                bbo->member_count, bbo->quantity_scale,
                                view.terminal_width < 100)
           << "}\n\n";
  } else if (view.expect_bbo) {
    output << "Waiting for AggBbo record...\n\n";
  }

  if (book != nullptr && view.depth != 0) {
    const auto bid_depth =
        std::min<std::size_t>(view.depth, book->bid_count);
    const auto ask_depth =
        std::min<std::size_t>(view.depth, book->ask_count);
    std::uint64_t maximum_quantity = 0;
    for (std::size_t index = 0; index < bid_depth; ++index) {
      maximum_quantity =
          std::max(maximum_quantity,
                   static_cast<std::uint64_t>(book->bids[index].quantity));
    }
    for (std::size_t index = 0; index < ask_depth; ++index) {
      maximum_quantity =
          std::max(maximum_quantity,
                   static_cast<std::uint64_t>(book->asks[index].quantity));
    }
    const bool compact = view.terminal_width < 100;
    const auto bar_width =
        std::clamp<std::size_t>(view.terminal_width / 8, 6, 20);
    if (view.color) {
      output << kRed;
    }
    output << "ASK - SELL";
    if (view.color) {
      output << kReset;
    }
    output << '\n';
    for (std::size_t index = ask_depth; index > 0; --index) {
      append_level(output, book->asks[index - 1], book->venue_slot_ids,
                   book->member_count, book->price_scale,
                   book->quantity_scale, maximum_quantity, bar_width,
                   view.terminal_width, compact);
    }

    if (bbo != nullptr) {
      const bool crossed = bbo->gated_bid.price > bbo->gated_ask.price;
      if (view.color && crossed) {
        output << kRed;
      } else if (view.color) {
        output << kDim;
      }
      output << "---------------- "
             << (crossed ? "CROSSED " : "SPREAD ")
             << spread_text(bbo->gated_bid.price, bbo->gated_ask.price)
             << " BPS ----------------";
      if (view.color) {
        output << kReset;
      }
      output << '\n';
    } else {
      output << "---------------- SPREAD unavailable ----------------\n";
    }

    if (view.color) {
      output << kGreen;
    }
    output << "BID - BUY";
    if (view.color) {
      output << kReset;
    }
    output << '\n';
    for (std::size_t index = 0; index < bid_depth; ++index) {
      append_level(output, book->bids[index], book->venue_slot_ids,
                   book->member_count, book->price_scale,
                   book->quantity_scale, maximum_quantity, bar_width,
                   view.terminal_width, compact);
    }
  } else if (book == nullptr && view.depth != 0 && view.expect_book) {
    output << "Waiting for AggOrderBook record...\n";
  }

  output << "\n" << status_line(bbo, book, view.terminal_width < 100) << '\n';
  output << "AggBbo and AggOrderBook are independent snapshots.\n";
  return output.str();
}

}  // namespace mds::examples
