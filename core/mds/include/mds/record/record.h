#pragma once

#include "utils/md/wire.h"

#include <cstdint>
#include <string>
#include <vector>

namespace mds::record {

inline constexpr std::uint16_t kFormatMajor = 2;
inline constexpr std::uint16_t kFormatMinor = 0;
inline constexpr std::size_t kMaximumDepth = 50;

enum class Kind : std::uint8_t { AggBbo = 1, AggOrderBook = 2 };

enum RecordFlags : std::uint16_t {
  kGap = 1U << 0U,
  kReset = 1U << 1U,
};

struct Metadata {
  std::uint64_t wall_ns{};
  std::uint64_t mono_ns{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint16_t flags{};
  Kind kind{Kind::AggBbo};
};

struct CrossBpsWindow {
  std::uint64_t start_mono_ns{};
  std::uint64_t end_mono_ns{};
  std::int32_t raw_min{};
  std::int32_t raw_max{};
  std::int32_t gated_min{};
  std::int32_t gated_max{};
  std::uint64_t samples{};
};

struct Record {
  Metadata metadata;
  utils::md::wire::AggBboRecord bbo{};
  utils::md::wire::AggOrderBookRecord order_book{};
  CrossBpsWindow cross_window{};
};

struct ShardInfo {
  std::string path;
  std::uint64_t start_wall_ns{};
  std::uint64_t end_wall_ns{};
  std::uint64_t records{};
  Kind kind{Kind::AggBbo};
};

}  // namespace mds::record
