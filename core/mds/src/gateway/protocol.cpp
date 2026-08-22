#include "mds/gateway/protocol.h"

#include <algorithm>
#include <array>
#include <type_traits>

namespace mds::gateway {
namespace {

class Encoder {
public:
  template <typename T> void put(T value) {
    using U = std::make_unsigned_t<T>;
    const auto bits = static_cast<U>(value);
    for (std::size_t index = 0; index < sizeof(T); ++index) {
      bytes_.push_back(static_cast<std::byte>(bits >> (index * 8U)));
    }
  }

  template <typename T, std::size_t Size>
  void put_array(const std::array<T, Size> &values) {
    for (const auto value : values) {
      put(value);
    }
  }

  [[nodiscard]] std::vector<std::byte> take() { return std::move(bytes_); }

private:
  std::vector<std::byte> bytes_;
};

void put_header(Encoder &out, const FrameMetadata &metadata,
                std::uint32_t payload_length) {
  out.put(kFrameMagic);
  out.put(kProtocolMajor);
  out.put(kProtocolMinor);
  out.put(static_cast<std::uint8_t>(metadata.kind));
  out.put(static_cast<std::uint8_t>(metadata.topic));
  out.put(std::uint16_t{0});
  out.put(metadata.topic_id);
  out.put(metadata.ring_epoch);
  out.put(metadata.ring_sequence);
  out.put(metadata.generation);
  out.put(metadata.wall_ns);
  out.put(payload_length);
}

void put_side(Encoder &out, const utils::md::wire::AggBboSide &side) {
  out.put(side.price);
  out.put(side.quantity);
  out.put_array(side.venue_quantity);
  out.put(side.exchange_ts_ns);
  out.put(side.venue_mask);
  out.put(side.worst_ingress_age_us);
  out.put(side.best_venue);
  out.put(side.timestamp_venue);
  out.put(side.contributor_count);
}

void put_raw_side(Encoder &out, const utils::md::wire::AggBboRawSide &side) {
  out.put(side.price);
  out.put(side.quantity);
  out.put(side.venue_mask);
  out.put(side.best_venue);
}

void put_level(Encoder &out, const utils::md::wire::AggLevel &level) {
  out.put(level.price);
  out.put(level.quantity);
  out.put_array(level.venue_quantity);
  out.put(level.venue_mask);
  out.put(level.contributor_count);
}

std::vector<std::byte> finish(const FrameMetadata &metadata, Encoder body) {
  auto payload = body.take();
  Encoder frame;
  put_header(frame, metadata, static_cast<std::uint32_t>(payload.size()));
  auto header = frame.take();
  header.insert(header.end(), payload.begin(), payload.end());
  return header;
}

} // namespace

std::vector<std::byte> encode_reset(const FrameMetadata &metadata) {
  return finish(metadata, {});
}

std::vector<std::byte>
encode_bbo(const FrameMetadata &metadata,
           const utils::md::wire::AggBboRecord &record) {
  Encoder body;
  body.put_array(record.base_asset);
  body.put_array(record.quote_asset);
  body.put_array(record.venue_slot_ids);
  body.put(record.price_scale);
  body.put(record.quantity_scale);
  body.put(record.member_count);
  body.put(record.member_mask);
  body.put(record.live_mask);
  body.put(record.header.flags);
  put_side(body, record.gated_bid);
  put_side(body, record.gated_ask);
  put_raw_side(body, record.raw_bid);
  put_raw_side(body, record.raw_ask);
  body.put(record.raw_cross_bps);
  body.put(record.gated_cross_bps);
  body.put(record.skew_us);
  body.put(record.cross_skew_threshold_us);
  body.put(record.fx_age_us);
  body.put(record.cross_bid_venue);
  body.put(record.cross_ask_venue);
  body.put(record.fx_venue);
  return finish(metadata, std::move(body));
}

std::vector<std::byte>
encode_order_book(const FrameMetadata &metadata,
                  const utils::md::wire::AggOrderBookRecord &record,
                  std::size_t depth) {
  Encoder body;
  const auto bounded_depth = std::min(depth, kMaximumDepth);
  const auto bids = std::min<std::size_t>(bounded_depth, record.bid_count);
  const auto asks = std::min<std::size_t>(bounded_depth, record.ask_count);
  body.put_array(record.base_asset);
  body.put_array(record.quote_asset);
  body.put_array(record.venue_slot_ids);
  body.put(record.price_scale);
  body.put(record.quantity_scale);
  body.put(record.member_count);
  body.put(record.member_mask);
  body.put(record.active_mask);
  body.put(static_cast<std::uint16_t>(bids));
  body.put(static_cast<std::uint16_t>(asks));
  for (std::size_t index = 0; index < bids; ++index) {
    put_level(body, record.bids[index]);
  }
  for (std::size_t index = 0; index < asks; ++index) {
    put_level(body, record.asks[index]);
  }
  return finish(metadata, std::move(body));
}

} // namespace mds::gateway
