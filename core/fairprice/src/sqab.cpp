#include "fairprice/sqab.h"

#include "fairprice/store.h"

#include <algorithm>
#include <cstring>

namespace fairprice {
namespace {

void append_u16(std::vector<std::byte> &out, std::uint16_t value) {
  const auto n = out.size();
  out.resize(n + 2);
  std::memcpy(out.data() + n, &value, 2);
}

void append_u32(std::vector<std::byte> &out, std::uint32_t value) {
  const auto n = out.size();
  out.resize(n + 4);
  std::memcpy(out.data() + n, &value, 4);
}

void append_u64(std::vector<std::byte> &out, std::uint64_t value) {
  const auto n = out.size();
  out.resize(n + 8);
  std::memcpy(out.data() + n, &value, 8);
}

void append_compact_side(std::vector<std::byte> &out,
                         const utils::md::wire::AggBboSide &side) {
  append_u64(out, static_cast<std::uint64_t>(side.price));
  append_u64(out, static_cast<std::uint64_t>(side.quantity));
  append_u32(out, side.venue_mask);
  out.push_back(static_cast<std::byte>(side.contributor_count));
  for (int i = 0; i < kMaxVenues; ++i) {
    if ((side.venue_mask & (1U << i)) == 0) {
      continue;
    }
    out.push_back(static_cast<std::byte>(i));
    append_u64(out, static_cast<std::uint64_t>(
                        side.venue_quantity[static_cast<std::size_t>(i)]));
  }
}

void append_raw_side(std::vector<std::byte> &out,
                     const utils::md::wire::AggBboRawSide &side) {
  append_u64(out, static_cast<std::uint64_t>(side.price));
  append_u64(out, static_cast<std::uint64_t>(side.quantity));
  append_u32(out, side.venue_mask);
  out.push_back(static_cast<std::byte>(side.best_venue));
}

void append_compact_level(std::vector<std::byte> &out,
                          const ResidualLevel &level) {
  append_u64(out, static_cast<std::uint64_t>(level.price));
  append_u64(out, static_cast<std::uint64_t>(level.quantity));
  append_u32(out, level.venue_mask);
  out.push_back(static_cast<std::byte>(level.contributor_count));
  for (int i = 0; i < kMaxVenues; ++i) {
    if ((level.venue_mask & (1U << i)) == 0) {
      continue;
    }
    out.push_back(static_cast<std::byte>(i));
    append_u64(out, static_cast<std::uint64_t>(
                        level.venue_quantity[static_cast<std::size_t>(i)]));
  }
}

std::shared_ptr<const std::vector<std::byte>>
finish_frame(std::uint8_t kind, std::string_view symbol, std::uint8_t price_scale,
             std::uint8_t quantity_scale, std::uint8_t member_count,
             const std::array<std::uint8_t, 8> &venue_ids, std::uint64_t sequence,
             std::uint64_t generation, std::uint64_t wall_ns,
             const std::vector<std::byte> &body) {
  if (symbol.empty() || symbol.size() > 255 || member_count > kMaxVenues) {
    return {};
  }
  constexpr std::uint16_t kHeaderBytes = 42;
  const auto payload_size = symbol.size() + static_cast<std::size_t>(member_count) +
                            body.size();
  auto frame = std::make_shared<std::vector<std::byte>>();
  frame->reserve(kHeaderBytes + payload_size);
  append_u32(*frame, kSqabMagic);
  frame->push_back(std::byte{1});
  frame->push_back(std::byte{0});
  frame->push_back(static_cast<std::byte>(kind));
  frame->push_back(std::byte{0});
  append_u16(*frame, kHeaderBytes);
  append_u32(*frame, static_cast<std::uint32_t>(payload_size));
  frame->push_back(static_cast<std::byte>(symbol.size()));
  frame->push_back(static_cast<std::byte>(price_scale));
  frame->push_back(static_cast<std::byte>(quantity_scale));
  frame->push_back(static_cast<std::byte>(member_count));
  append_u64(*frame, sequence);
  append_u64(*frame, generation);
  append_u64(*frame, wall_ns);
  frame->insert(frame->end(),
                reinterpret_cast<const std::byte *>(symbol.data()),
                reinterpret_cast<const std::byte *>(symbol.data()) +
                    symbol.size());
  frame->insert(frame->end(),
                reinterpret_cast<const std::byte *>(venue_ids.data()),
                reinterpret_cast<const std::byte *>(venue_ids.data()) +
                    member_count);
  frame->insert(frame->end(), body.begin(), body.end());
  return frame;
}

}  // namespace

std::shared_ptr<const std::vector<std::byte>>
encode_sqab_bbo(std::string_view symbol, const BboView &bbo) {
  if (!bbo.ready) {
    return {};
  }
  const auto &value = bbo.record;
  std::vector<std::byte> body;
  body.reserve(256);
  append_compact_side(body, value.gated_bid);
  append_compact_side(body, value.gated_ask);
  append_raw_side(body, value.raw_bid);
  append_raw_side(body, value.raw_ask);
  const auto raw_spread =
      signed_spread_bps(value.raw_bid.price, value.raw_ask.price);
  const auto gated_spread =
      signed_spread_bps(value.gated_bid.price, value.gated_ask.price);
  std::uint64_t raw_bits = 0;
  std::uint64_t gated_bits = 0;
  std::memcpy(&raw_bits, &raw_spread, sizeof(raw_bits));
  std::memcpy(&gated_bits, &gated_spread, sizeof(gated_bits));
  append_u64(body, raw_bits);
  append_u64(body, gated_bits);
  return finish_frame(kSqabKindBbo, symbol, value.price_scale,
                      value.quantity_scale, value.member_count,
                      value.venue_slot_ids, bbo.ring_sequence, bbo.generation,
                      bbo.wall_ns, body);
}

std::shared_ptr<const std::vector<std::byte>>
encode_sqab_book(std::string_view symbol, const BookSnapshot &book, int depth) {
  if (!book.ready || depth <= 0 || depth > kMaxDepth) {
    return {};
  }
  const int bids = std::min(depth, book.bid_count);
  const int asks = std::min(depth, book.ask_count);
  std::vector<std::byte> body;
  body.reserve(static_cast<std::size_t>((bids + asks) * 32 + 4));
  append_u16(body, static_cast<std::uint16_t>(bids));
  append_u16(body, static_cast<std::uint16_t>(asks));
  for (int i = 0; i < bids; ++i) {
    append_compact_level(body, book.bids[static_cast<std::size_t>(i)]);
  }
  for (int i = 0; i < asks; ++i) {
    append_compact_level(body, book.asks[static_cast<std::size_t>(i)]);
  }
  return finish_frame(kSqabKindBook, symbol, book.price_scale,
                      book.quantity_scale, book.member_count,
                      book.venue_slot_ids, book.ring_sequence, book.generation,
                      book.wall_ns, body);
}

}  // namespace fairprice
