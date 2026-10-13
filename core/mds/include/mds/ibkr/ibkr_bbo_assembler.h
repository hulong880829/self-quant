#pragma once

#include <atomic>
#include <cstdint>
#include <string_view>

namespace mds::ibkr {

enum class MarketDataKind : std::uint8_t {
  Unset = 0,
  Live = 1,
  Frozen = 2,
  Delayed = 3,
  DelayedFrozen = 4,
};

struct FixedQuote {
  std::int64_t bid_price{};
  std::int64_t ask_price{};
  std::int64_t bid_size{};
  std::int64_t ask_size{};
};

struct AssemblerUpdate {
  bool changed{};
  bool publishable{};
};

inline constexpr std::uint8_t kMaxDerivedPriceScale = 8;

// Converts a finite positive double through a stack buffer and decimal_to_fixed.
// Extra fractional digits are rounded by std::to_chars fixed formatting.
[[nodiscard]] bool double_price_to_fixed(double value, std::uint8_t scale,
                                         std::int64_t &out) noexcept;

// Smallest scale that exactly represents the shortest decimal form of min_tick.
// Does not compare binary doubles for equality. Fails above kMaxDerivedPriceScale.
[[nodiscard]] bool price_scale_for_tick(double min_tick,
                                        std::uint8_t &scale) noexcept;

// True when min_tick's shortest decimal form fits in scale without rounding.
[[nodiscard]] bool min_tick_fits_scale(double min_tick,
                                       std::uint8_t scale) noexcept;

// Converts an IBKR Decimal string without passing through double.
// non_positive is set when the text parses to <= 0.
[[nodiscard]] bool decimal_size_to_fixed(std::string_view text,
                                         std::uint8_t scale,
                                         std::int64_t &out,
                                         bool &non_positive) noexcept;

class BboAssembler {
 public:
  explicit BboAssembler(std::uint8_t price_scale,
                        std::uint8_t quantity_scale) noexcept;

  void reset(std::uint64_t generation) noexcept;
  [[nodiscard]] std::uint64_t generation() const noexcept { return generation_; }
  [[nodiscard]] bool publishable() const noexcept { return publishable_; }
  [[nodiscard]] bool unhealthy() const noexcept { return unhealthy_; }
  [[nodiscard]] bool has_bid_price() const noexcept { return bid_price_valid_; }
  [[nodiscard]] bool has_ask_price() const noexcept { return ask_price_valid_; }
  [[nodiscard]] bool has_bid_size() const noexcept { return bid_size_valid_; }
  [[nodiscard]] bool has_ask_size() const noexcept { return ask_size_valid_; }
  [[nodiscard]] FixedQuote quote() const noexcept;
  [[nodiscard]] std::uint64_t invalid_ticks() const noexcept {
    return invalid_ticks_.load(std::memory_order_relaxed);
  }
  [[nodiscard]] std::uint64_t non_live_ticks() const noexcept {
    return non_live_ticks_.load(std::memory_order_relaxed);
  }
  [[nodiscard]] MarketDataKind market_data_kind() const noexcept {
    return data_kind_;
  }

  [[nodiscard]] AssemblerUpdate on_price(bool bid, double price,
                                         std::uint64_t generation) noexcept;
  [[nodiscard]] AssemblerUpdate on_size(bool bid, std::string_view size,
                                        std::uint64_t generation) noexcept;
  [[nodiscard]] AssemblerUpdate on_market_data_type(
      MarketDataKind kind, std::uint64_t generation) noexcept;

 private:
  [[nodiscard]] AssemblerUpdate refresh(bool changed) noexcept;
  void clear_price(bool bid) noexcept;
  void clear_size(bool bid) noexcept;

  std::uint8_t price_scale_{};
  std::uint8_t quantity_scale_{};
  std::uint64_t generation_{1};
  std::int64_t bid_price_{};
  std::int64_t ask_price_{};
  std::int64_t bid_size_{};
  std::int64_t ask_size_{};
  bool bid_price_valid_{};
  bool ask_price_valid_{};
  bool bid_size_valid_{};
  bool ask_size_valid_{};
  bool publishable_{};
  bool unhealthy_{};
  MarketDataKind data_kind_{MarketDataKind::Unset};
  std::atomic<std::uint64_t> invalid_ticks_{0};
  std::atomic<std::uint64_t> non_live_ticks_{0};
};

}  // namespace mds::ibkr
