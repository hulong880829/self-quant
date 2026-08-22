#pragma once

#include <array>
#include <cstdint>

namespace polymm {

enum class Outcome : std::uint8_t { Up = 1, Down = 2 };

struct FairPriceEvent {
  double price_raw{};
  double microprice{};
  std::uint64_t wall_ns{};
  std::uint64_t received_ns{};
};

struct SidecarEvent {
  enum class Kind : std::uint8_t {
    FairPrice = 1,
    FairPriceDisconnected = 2,
  };

  Kind kind{Kind::FairPriceDisconnected};
  FairPriceEvent fair_price{};
};

struct Parameters {
  std::uint32_t account_id{};
  std::uint64_t up_instrument_id{};
  std::uint64_t down_instrument_id{};
  std::uint32_t vol_lookback_ms{30'000};
  std::uint32_t signal_horizon_ms{250};
  double vol_threshold{};
  double min_market_price{0.15};
  double max_market_price{0.85};
  double min_edge_ticks{1.0};
  double order_shares{1.0};
  double max_single_side_shares{10.0};
  double imbalance_tolerance_shares{0.1};
  std::int64_t profit_spread_ticks{1};
  std::int64_t close_improve_ticks{};
  std::uint32_t max_order_resting_ms{1'000};
  std::int64_t requote_deviation_ticks{1};
  std::uint32_t no_trade_before_expiry_ms{30'000};
  double stop_loss_ticks{5.0};
  std::uint32_t fairprice_timer_us{200};
};

}  // namespace polymm
