#pragma once

#include "fairprice/catalog.h"
#include "fairprice/config.h"
#include "fairprice/store.h"

#include "utils/md/wire.h"

#include <cstdint>
#include <functional>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

namespace fairprice {

struct GatewayWireFrame {
  Kind kind{Kind::Unknown};
  std::uint8_t format{};
  std::uint16_t topic_id{};
  std::uint16_t flags{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  std::optional<utils::md::wire::AggBboRecord> bbo;
  std::optional<utils::md::wire::AggOrderBookRecord> book;
};

struct GatewayLearnedSnapshot {
  std::string segment;
  Kind kind{Kind::Unknown};
  std::uint16_t topic_id{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  std::optional<utils::md::wire::AggBboRecord> bbo;
  std::optional<utils::md::wire::AggOrderBookRecord> book;
};

class TopicIdLearner {
 public:
  explicit TopicIdLearner(std::vector<std::string> segments);

  void begin_learning(std::string segment);
  bool apply(const GatewayWireFrame &frame);
  [[nodiscard]] bool segment_mapped(std::string_view segment) const;
  [[nodiscard]] const std::string &learning_segment() const noexcept {
    return learning_segment_;
  }
  [[nodiscard]] std::optional<std::string> segment_for_topic(
      std::uint16_t topic_id) const;
  [[nodiscard]] std::vector<GatewayLearnedSnapshot> snapshots() const;
  [[nodiscard]] const std::vector<std::string> &missing() const noexcept {
    return missing_;
  }
  void record_missing(std::string_view segment);

 private:
  struct Slot {
    std::string segment;
    Kind kind{Kind::Unknown};
    std::string profile;
    std::string symbol;
    GatewayLearnedSnapshot snapshot;
    bool mapped{};
  };

  Slot *identify(const GatewayWireFrame &frame);
  Slot *find_segment(std::string_view segment);

  std::vector<Slot> slots_;
  std::unordered_map<std::uint16_t, std::size_t> topic_index_;
  std::string learning_segment_;
  std::vector<std::string> missing_;
};

using GatewayFrameSource = std::function<std::vector<GatewayWireFrame>(
    std::string_view learning_segment, std::uint64_t wait_ms)>;

class GatewayBootstrapClient {
 public:
  explicit GatewayBootstrapClient(GatewayBootstrapConfig config,
                                  GatewayFrameSource source = {});

  std::vector<GatewayLearnedSnapshot> run(
      const std::vector<std::string> &segments);

 private:
  GatewayBootstrapConfig config_;
  GatewayFrameSource source_;
};

bool decode_gateway_frame(std::span<const std::byte> bytes,
                          GatewayWireFrame &frame, std::string &error);
std::string encode_gateway_subscribe(std::string_view segment);

}  // namespace fairprice
