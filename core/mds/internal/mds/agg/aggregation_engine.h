#pragma once

#include "utils/md/types.h"
#include "utils/md/wire.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>

namespace mds::agg {

inline constexpr std::size_t kMaxMembers = utils::md::wire::kAggVenueSlots;
inline constexpr std::size_t kLevelsPerMember = 10;

struct MemberConfig {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  std::uint64_t ttl_us{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  bool convert_usdc_to_usdt{};
};

struct EngineConfig {
  utils::md::InstrumentId instrument_id{};
  std::array<char, 16> base_asset{};
  std::array<char, 16> quote_asset{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  bool cross_skew_observe_only{true};
  std::uint32_t cross_skew_threshold_us{50'000};
  std::uint64_t fx_ttl_us{100'000};
  std::uint32_t fx_max_depeg_bps{200};
};

struct BookInput {
  std::span<const utils::md::Level> bids{};
  std::span<const utils::md::Level> asks{};
  std::uint64_t exchange_ts_ns{};
  std::uint64_t ingress_mono_ns{};
  std::uint32_t generation{};
};

struct BboBuild {
  utils::md::wire::AggBboRecord record{};
  std::uint16_t flags{};
  bool changed{};
  bool member_data_error{};
};

struct BookBuild {
  utils::md::wire::AggOrderBookRecord record{};
  bool changed{};
};

[[nodiscard]] constexpr std::uint64_t
derive_raw_liveness_us(std::uint64_t ttl_us) noexcept {
  constexpr std::uint64_t multiplier = 10;
  constexpr std::uint64_t cap = 5'000'000;
  const auto scaled =
      ttl_us > UINT64_MAX / multiplier ? UINT64_MAX : ttl_us * multiplier;
  return ttl_us > cap ? ttl_us : (scaled < cap ? scaled : cap);
}

class AggregationEngine {
public:
  explicit AggregationEngine(EngineConfig config) noexcept;

  [[nodiscard]] bool add_member(const MemberConfig &config) noexcept;
  [[nodiscard]] bool
  reconfigure_member(std::size_t slot,
                     const MemberConfig &config) noexcept;
  void invalidate_member(std::size_t slot) noexcept;
  void exclude_member(std::size_t slot) noexcept;
  void invalidate_fx() noexcept;
  [[nodiscard]] bool update_bbo(std::size_t slot,
                                const utils::md::wire::BboRecord &record,
                                std::uint64_t ingress_mono_ns) noexcept;
  [[nodiscard]] bool update_book(std::size_t slot,
                                 const BookInput &input) noexcept;
  void update_fx(const utils::md::wire::BboRecord &record,
                 std::uint8_t price_scale,
                 std::uint64_t ingress_mono_ns,
                 utils::md::Venue venue) noexcept;

  [[nodiscard]] BboBuild build_bbo(std::uint64_t now_mono_ns) noexcept;
  [[nodiscard]] BookBuild build_orderbook(std::uint64_t now_mono_ns) noexcept;
  [[nodiscard]] std::size_t member_count() const noexcept {
    return member_count_;
  }

private:
  struct MemberState {
    MemberConfig config{};
    std::uint64_t raw_liveness_us{};
    utils::md::wire::BboRecord bbo{};
    std::array<utils::md::Level, kLevelsPerMember> bids{};
    std::array<utils::md::Level, kLevelsPerMember> asks{};
    std::size_t bid_count{};
    std::size_t ask_count{};
    std::uint64_t bbo_ingress_ns{};
    std::uint64_t book_ingress_ns{};
    std::uint64_t book_exchange_ts_ns{};
    std::uint32_t generation{};
    bool configured{};
    bool bbo_valid{};
    bool book_valid{};
  };

  struct FxState {
    utils::md::wire::BboRecord bbo{};
    std::uint64_t ingress_ns{};
    std::uint8_t price_scale{};
    utils::md::Venue venue{utils::md::Venue::Unknown};
    bool valid{};
  };

  EngineConfig config_{};
  std::array<MemberState, kMaxMembers> members_{};
  FxState fx_{};
  std::size_t member_count_{};
  std::uint32_t generation_{1};
  std::uint64_t source_sequence_{};
  utils::md::wire::AggBboRecord previous_bbo_{};
  utils::md::wire::AggOrderBookRecord previous_book_{};
  std::uint16_t previous_flags_{};
  bool have_previous_bbo_{};
  bool have_previous_book_{};
};

}  // namespace mds::agg
