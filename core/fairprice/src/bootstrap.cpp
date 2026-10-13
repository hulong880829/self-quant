#include "fairprice/bootstrap.h"

#include "net/websocket_client.h"

#include <algorithm>
#include <array>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <memory>
#include <poll.h>
#include <span>
#include <string>
#include <sys/epoll.h>
#include <vector>

namespace fairprice {
namespace {

constexpr std::uint32_t kGatewayMagic = 0x57475153U;
constexpr std::size_t kGatewayHeaderSize = 52;
constexpr std::size_t kCompactBookMeta = 69;
constexpr std::size_t kCompactLevelBytes = 85;

template <typename T>
T read_le(std::span<const std::byte> bytes, std::size_t offset) {
  T value{};
  std::memcpy(&value, bytes.data() + offset, sizeof(T));
  return value;
}

std::string_view fixed_text(std::span<const std::byte> bytes) {
  const auto *data = reinterpret_cast<const char *>(bytes.data());
  std::size_t size = 0;
  while (size < bytes.size() && data[size] != '\0') {
    ++size;
  }
  return {data, size};
}

std::string fixed_asset(const std::array<char, 16> &value) {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.data(), static_cast<std::size_t>(end - value.begin())};
}

utils::md::wire::AggLevel decode_compact_level(
    std::span<const std::byte> data) {
  utils::md::wire::AggLevel level{};
  level.price = read_le<std::int64_t>(data, 0);
  level.quantity = read_le<std::int64_t>(data, 8);
  for (std::size_t i = 0; i < 8; ++i) {
    level.venue_quantity[i] = read_le<std::int64_t>(data, 16 + i * 8);
  }
  level.venue_mask = read_le<std::uint32_t>(data, 80);
  level.contributor_count = static_cast<std::uint8_t>(data[84]);
  return level;
}

bool decode_compact_book(std::span<const std::byte> payload,
                         utils::md::wire::AggOrderBookRecord &record,
                         std::string &error) {
  if (payload.size() < kCompactBookMeta) {
    error = "compact AggOrderBook is truncated";
    return false;
  }
  record = {};
  record.header.exchange_ts_ns = read_le<std::uint64_t>(payload, 0);
  record.header.book_generation = read_le<std::uint32_t>(payload, 8);
  record.header.flags = read_le<std::uint16_t>(payload, 12);
  const auto base = fixed_text(payload.subspan(14, 16));
  const auto quote = fixed_text(payload.subspan(30, 16));
  std::memcpy(record.base_asset.data(), base.data(),
              std::min(base.size(), record.base_asset.size()));
  std::memcpy(record.quote_asset.data(), quote.data(),
              std::min(quote.size(), record.quote_asset.size()));
  std::memcpy(record.venue_slot_ids.data(), payload.data() + 46, 8);
  record.price_scale = static_cast<std::uint8_t>(payload[54]);
  record.quantity_scale = static_cast<std::uint8_t>(payload[55]);
  record.member_count = static_cast<std::uint8_t>(payload[56]);
  record.member_mask = read_le<std::uint32_t>(payload, 57);
  record.active_mask = read_le<std::uint32_t>(payload, 61);
  const auto bids = read_le<std::uint16_t>(payload, 65);
  const auto asks = read_le<std::uint16_t>(payload, 67);
  if (bids > utils::md::wire::kAggMaxLevelsPerSide ||
      asks > utils::md::wire::kAggMaxLevelsPerSide ||
      payload.size() !=
          kCompactBookMeta + kCompactLevelBytes *
                                 (static_cast<std::size_t>(bids) + asks)) {
    error = "invalid compact AggOrderBook metadata";
    return false;
  }
  record.bid_count = bids;
  record.ask_count = asks;
  std::size_t offset = kCompactBookMeta;
  for (std::uint16_t i = 0; i < bids; ++i) {
    record.bids[i] = decode_compact_level(payload.subspan(offset, kCompactLevelBytes));
    offset += kCompactLevelBytes;
  }
  for (std::uint16_t i = 0; i < asks; ++i) {
    record.asks[i] = decode_compact_level(payload.subspan(offset, kCompactLevelBytes));
    offset += kCompactLevelBytes;
  }
  return true;
}

struct ParsedUrl {
  bool secure{};
  std::string host;
  std::string service;
  std::string target;
};

bool parse_ws_url(std::string_view url, ParsedUrl &out, std::string &error) {
  out = {};
  std::string_view rest = url;
  if (rest.rfind("wss://", 0) == 0) {
    out.secure = true;
    rest.remove_prefix(6);
  } else if (rest.rfind("ws://", 0) == 0) {
    out.secure = false;
    rest.remove_prefix(5);
  } else {
    error = "gateway URL must be ws:// or wss://";
    return false;
  }
  const auto slash = rest.find('/');
  const auto hostport = slash == std::string_view::npos ? rest : rest.substr(0, slash);
  out.target = slash == std::string_view::npos ? "/" : std::string(rest.substr(slash));
  const auto colon = hostport.rfind(':');
  if (colon != std::string_view::npos && colon != 0 &&
      hostport.find(']') == std::string_view::npos) {
    out.host = std::string(hostport.substr(0, colon));
    out.service = std::string(hostport.substr(colon + 1));
  } else {
    out.host = std::string(hostport);
    out.service = out.secure ? "443" : "80";
  }
  if (out.host.empty() || out.service.empty()) {
    error = "gateway URL host is invalid";
    return false;
  }
  return true;
}

std::uint64_t now_mono_ms() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::milliseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

void pump_events(net::WebSocketClient &client, int timeout_ms) {
  pollfd descriptor{client.fd(), 0, 0};
  const auto wanted = client.wanted_events();
  if ((wanted & EPOLLIN) != 0) {
    descriptor.events |= POLLIN;
  }
  if ((wanted & EPOLLOUT) != 0) {
    descriptor.events |= POLLOUT;
  }
  const int ready = ::poll(&descriptor, 1, timeout_ms);
  std::uint32_t events{};
  if (ready > 0) {
    if ((descriptor.revents & POLLIN) != 0) {
      events |= EPOLLIN;
    }
    if ((descriptor.revents & POLLOUT) != 0) {
      events |= EPOLLOUT;
    }
    if ((descriptor.revents & (POLLERR | POLLHUP | POLLNVAL)) != 0) {
      events |= EPOLLERR | EPOLLHUP;
    }
  }
  (void)client.on_event(events);
  (void)client.check_timeout();
}

bool pump_websocket(net::WebSocketClient &client, std::uint64_t wait_ms,
                    std::string &error) {
  const auto deadline = net::WebSocketClient::Clock::now() +
                        std::chrono::milliseconds(wait_ms);
  while (client.state() != net::WebSocketClientState::Open &&
         client.state() != net::WebSocketClientState::Failed &&
         client.state() != net::WebSocketClientState::TimedOut &&
         client.state() != net::WebSocketClientState::Closed) {
    if (net::WebSocketClient::Clock::now() >= deadline) {
      error = "gateway websocket timed out";
      return false;
    }
    pump_events(client, 20);
  }
  if (client.state() != net::WebSocketClientState::Open) {
    error = std::string(client.error_message());
    if (error.empty()) {
      error = "gateway websocket failed";
    }
    return false;
  }
  return true;
}

}  // namespace

bool decode_gateway_frame(std::span<const std::byte> bytes,
                          GatewayWireFrame &frame, std::string &error) {
  frame = {};
  if (bytes.size() < kGatewayHeaderSize) {
    error = "SQGW frame is truncated";
    return false;
  }
  if (read_le<std::uint32_t>(bytes, 0) != kGatewayMagic) {
    error = "invalid SQGW magic";
    return false;
  }
  if (read_le<std::uint16_t>(bytes, 4) != 1 ||
      read_le<std::uint16_t>(bytes, 6) != 0) {
    error = "unsupported SQGW version";
    return false;
  }
  frame.kind = static_cast<Kind>(static_cast<std::uint8_t>(bytes[8]));
  frame.format = static_cast<std::uint8_t>(bytes[9]);
  frame.topic_id = read_le<std::uint16_t>(bytes, 10);
  frame.flags = read_le<std::uint16_t>(bytes, 12);
  if (read_le<std::uint16_t>(bytes, 14) != kGatewayHeaderSize) {
    error = "invalid SQGW header length";
    return false;
  }
  const auto payload_length = read_le<std::uint32_t>(bytes, 16);
  if (payload_length != bytes.size() - kGatewayHeaderSize) {
    error = "SQGW payload length mismatch";
    return false;
  }
  frame.ring_epoch = read_le<std::uint64_t>(bytes, 20);
  frame.ring_sequence = read_le<std::uint64_t>(bytes, 28);
  frame.generation = read_le<std::uint64_t>(bytes, 36);
  frame.wall_ns = read_le<std::uint64_t>(bytes, 44);
  const auto payload = bytes.subspan(kGatewayHeaderSize);
  if (frame.kind == Kind::Reset) {
    if (frame.format != 0 || frame.flags != 1 || !payload.empty()) {
      error = "invalid SQGW reset frame";
      return false;
    }
    return true;
  }
  if (frame.kind == Kind::Bbo) {
    if (frame.format != 1 || frame.flags != 0 ||
        payload.size() != sizeof(utils::md::wire::AggBboRecord)) {
      error = "invalid SQGW AggBbo frame";
      return false;
    }
    utils::md::wire::AggBboRecord record{};
    std::memcpy(&record, payload.data(), sizeof(record));
    frame.bbo = record;
    return true;
  }
  if (frame.kind == Kind::Book) {
    if (frame.format != 2 || frame.flags != 0) {
      error = "invalid SQGW AggOrderBook frame";
      return false;
    }
    utils::md::wire::AggOrderBookRecord record{};
    if (!decode_compact_book(payload, record, error)) {
      return false;
    }
    frame.book = record;
    return true;
  }
  error = "unsupported SQGW frame kind";
  return false;
}

std::string encode_gateway_subscribe(std::string_view segment) {
  return std::string("{\"op\":\"subscribe\",\"topic\":\"") + std::string(segment) +
         "\"}";
}

TopicIdLearner::TopicIdLearner(std::vector<std::string> segments) {
  std::sort(segments.begin(), segments.end());
  slots_.reserve(segments.size());
  for (auto &segment : segments) {
    Slot slot;
    slot.segment = segment;
    std::string error;
    (void)parse_segment(segment, slot.profile, slot.symbol, slot.kind, error);
    slot.snapshot.segment = segment;
    slot.snapshot.kind = slot.kind;
    slots_.push_back(std::move(slot));
  }
}

TopicIdLearner::Slot *TopicIdLearner::find_segment(std::string_view segment) {
  for (auto &slot : slots_) {
    if (slot.segment == segment) {
      return &slot;
    }
  }
  return nullptr;
}

void TopicIdLearner::begin_learning(std::string segment) {
  learning_segment_ = std::move(segment);
}

bool TopicIdLearner::segment_mapped(std::string_view segment) const {
  for (const auto &slot : slots_) {
    if (slot.segment == segment) {
      return slot.mapped;
    }
  }
  return false;
}

std::optional<std::string> TopicIdLearner::segment_for_topic(
    std::uint16_t topic_id) const {
  auto it = topic_index_.find(topic_id);
  if (it == topic_index_.end()) {
    return std::nullopt;
  }
  return slots_[it->second].segment;
}

void TopicIdLearner::record_missing(std::string_view segment) {
  missing_.emplace_back(segment);
}

TopicIdLearner::Slot *TopicIdLearner::identify(const GatewayWireFrame &frame) {
  if (learning_segment_.empty()) {
    return nullptr;
  }
  auto *slot = find_segment(learning_segment_);
  if (slot == nullptr || slot->kind != frame.kind) {
    return nullptr;
  }
  if (frame.kind == Kind::Bbo && frame.bbo) {
    const auto symbol = canonical_symbol(fixed_asset(frame.bbo->base_asset) +
                                         fixed_asset(frame.bbo->quote_asset));
    if (symbol != slot->symbol) {
      return nullptr;
    }
  } else if (frame.kind == Kind::Book && frame.book) {
    const auto symbol = canonical_symbol(fixed_asset(frame.book->base_asset) +
                                         fixed_asset(frame.book->quote_asset));
    if (symbol != slot->symbol) {
      return nullptr;
    }
  }
  return slot;
}

bool TopicIdLearner::apply(const GatewayWireFrame &frame) {
  if (frame.kind == Kind::Reset || frame.kind == Kind::Unknown) {
    return false;
  }
  auto found = topic_index_.find(frame.topic_id);
  Slot *slot = nullptr;
  if (found != topic_index_.end()) {
    slot = &slots_[found->second];
  } else {
    slot = identify(frame);
    if (slot == nullptr) {
      return false;
    }
    topic_index_[frame.topic_id] = static_cast<std::size_t>(slot - slots_.data());
    slot->mapped = true;
    slot->snapshot.topic_id = frame.topic_id;
    if (learning_segment_ == slot->segment) {
      learning_segment_.clear();
    }
  }
  if (frame.kind != slot->kind && frame.kind != Kind::Unknown) {
    return false;
  }
  slot->snapshot.ring_epoch = frame.ring_epoch;
  slot->snapshot.ring_sequence = frame.ring_sequence;
  slot->snapshot.generation = frame.generation;
  slot->snapshot.wall_ns = frame.wall_ns;
  if (frame.bbo) {
    slot->snapshot.bbo = frame.bbo;
  }
  if (frame.book) {
    slot->snapshot.book = frame.book;
  }
  return true;
}

std::vector<GatewayLearnedSnapshot> TopicIdLearner::snapshots() const {
  std::vector<GatewayLearnedSnapshot> out;
  for (const auto &slot : slots_) {
    if (slot.mapped) {
      out.push_back(slot.snapshot);
    }
  }
  return out;
}

GatewayBootstrapClient::GatewayBootstrapClient(GatewayBootstrapConfig config,
                                               GatewayFrameSource source)
    : config_(std::move(config)), source_(std::move(source)) {}

std::vector<GatewayLearnedSnapshot> GatewayBootstrapClient::run(
    const std::vector<std::string> &segments) {
  auto ordered = segments;
  std::sort(ordered.begin(), ordered.end());
  TopicIdLearner learner(ordered);
  const auto total_ms = config_.timeout_ms == 0 ? 1 : config_.timeout_ms;
  const auto start_ms = now_mono_ms();
  const auto deadline_ms = start_ms + total_ms;

  std::unique_ptr<net::WebSocketClient> client;
  if (!source_) {
    const auto &url = config_.url;
    const auto &token = config_.token;
    if (!url.empty() && !token.empty()) {
      ParsedUrl parsed;
      std::string error;
      if (parse_ws_url(url, parsed, error)) {
        auto tls_error = std::string{};
        auto tls = net::make_client_ssl_context(tls_error);
        if (tls) {
          client = std::make_unique<net::WebSocketClient>(std::move(tls));
          client->set_secure(parsed.secure);
          const auto auth = std::string("Bearer ") + token;
          const net::WebSocketHeader headers[] = {{"Authorization", auth}};
          (void)client->set_upgrade_headers(headers);
          const auto handshake_ms =
              std::max<std::uint64_t>(1, deadline_ms - now_mono_ms());
          const auto connect_deadline =
              net::WebSocketClient::Clock::now() +
              std::chrono::milliseconds(handshake_ms);
          if (!client->start(parsed.host, parsed.service, parsed.target,
                             connect_deadline) ||
              !pump_websocket(*client, handshake_ms, error)) {
            const auto message = error.empty()
                                     ? std::string(client->error_message())
                                     : error;
            std::fprintf(stderr, "fairprice: gateway unavailable: %s\n",
                         message.c_str());
            client.reset();
          } else {
            client->set_frame_callback([&](const net::WsFrameView &view) {
              if (view.opcode != net::WsOpcode::Binary) {
                return true;
              }
              GatewayWireFrame frame;
              std::string decode_error;
              if (decode_gateway_frame(view.payload, frame, decode_error)) {
                (void)learner.apply(frame);
              }
              return true;
            });
          }
        } else {
          std::fprintf(stderr, "fairprice: gateway tls: %s\n", tls_error.c_str());
        }
      } else {
        std::fprintf(stderr, "fairprice: %s\n", error.c_str());
      }
    }
  }

  for (std::size_t index = 0; index < ordered.size(); ++index) {
    const auto now = now_mono_ms();
    if (now >= deadline_ms) {
      for (std::size_t rest = index; rest < ordered.size(); ++rest) {
        learner.record_missing(ordered[rest]);
      }
      break;
    }
    const auto remaining_ms = deadline_ms - now;
    const auto remaining_count = ordered.size() - index;
    const auto slice_ms =
        std::max<std::uint64_t>(1, remaining_ms / remaining_count);
    const auto &segment = ordered[index];
    learner.begin_learning(segment);
    if (source_) {
      for (const auto &frame : source_(segment, slice_ms)) {
        (void)learner.apply(frame);
      }
    } else if (client && client->state() == net::WebSocketClientState::Open) {
      const auto subscribe = encode_gateway_subscribe(segment);
      std::string_view send_error;
      if (!client->send(
              net::WsOpcode::Text,
              std::as_bytes(std::span(subscribe.data(), subscribe.size())),
              send_error)) {
        std::fprintf(stderr, "fairprice: gateway subscribe failed: %s\n",
                     std::string(send_error).c_str());
      } else {
        const auto read_deadline = net::WebSocketClient::Clock::now() +
                                   std::chrono::milliseconds(slice_ms);
        while (!learner.segment_mapped(segment) &&
               net::WebSocketClient::Clock::now() < read_deadline &&
               client->state() == net::WebSocketClientState::Open) {
          pump_events(*client, 20);
        }
      }
    }
    if (!learner.segment_mapped(segment)) {
      std::fprintf(stderr, "fairprice: gateway topic timeout segment=%s\n",
                   segment.c_str());
      learner.record_missing(segment);
    }
  }
  if (client) {
    std::string_view close_error;
    (void)client->close(1000, "bootstrap complete", close_error);
  }
  return learner.snapshots();
}

}  // namespace fairprice
