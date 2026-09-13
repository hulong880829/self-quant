#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string_view>

#include "oms/api/order_types.h"
#include "oms/exchange/polymarket/crypto.h"

namespace oms::exchange::polymarket {

inline constexpr std::size_t kMaximumWireBytes = 16 * 1024;
inline constexpr std::size_t kMaximumCursorBytes = 256;
inline constexpr std::size_t kMaximumOpenOrders = 500;
inline constexpr std::size_t kMaximumPages = 20;
inline constexpr std::size_t kMaximumTradeEvents = 16;

enum class ProtocolResult : std::uint8_t {
  Ok = 0,
  InvalidArgument = 1,
  Malformed = 2,
  BoundsExceeded = 3,
  Unsupported = 4,
};

struct WireRequest {
  std::array<char, 8> method{};
  std::uint8_t method_size{};
  std::array<char, 512> path{};
  std::uint16_t path_size{};
  std::array<char, kMaximumWireBytes> body{};
  std::uint16_t body_size{};
};

struct PlaceOrderInput {
  Order order{};
  Signature signature{};
  std::string_view owner{};
  api::TimeInForce time_in_force{api::TimeInForce::GTC};
  bool post_only{};
  std::uint64_t expiration_seconds{};
};

struct Pagination {
  std::array<char, kMaximumCursorBytes> cursor{};
  std::uint16_t cursor_size{};
  std::array<std::array<char, kMaximumCursorBytes>, kMaximumPages> seen{};
  std::array<std::uint16_t, kMaximumPages> seen_sizes{};
  std::uint16_t page_count{};
  std::uint16_t item_count{};
  bool complete{};
};

struct OpenOrderSnapshot {
  api::VenueOrderId venue_order_id{};
  std::array<std::uint8_t, 32> token_id{};
  api::Side side{api::Side::Buy};
  api::OrderStatus status{api::OrderStatus::Unknown};
  api::FixedPoint quantity{};
  api::FixedPoint price{};
  api::FixedPoint matched_quantity{};
};

struct PositionSnapshot {
  std::array<std::uint8_t, 32> token_id{};
  api::FixedPoint quantity{};
};

[[nodiscard]] ProtocolResult ScaleToSix(api::FixedPoint value,
                                        std::uint64_t& output) noexcept;
[[nodiscard]] ProtocolResult OrderAmounts(api::Side side,
                                          api::FixedPoint quantity,
                                          api::FixedPoint price,
                                          std::uint64_t& maker,
                                          std::uint64_t& taker) noexcept;

[[nodiscard]] ProtocolResult BuildPlaceOrder(const PlaceOrderInput& input,
                                             WireRequest& request) noexcept;
[[nodiscard]] ProtocolResult BuildCancelOrder(
    std::string_view venue_order_id, WireRequest& request) noexcept;
[[nodiscard]] ProtocolResult BuildOpenOrdersPage(
    const Pagination& pagination, WireRequest& request,
    std::string_view asset_id = {}) noexcept;
[[nodiscard]] ProtocolResult BuildPositions(std::string_view funder,
                                            WireRequest& request,
                                            std::string_view market = {}) noexcept;

// Accepts both the V2 envelope and legacy top-level array. next_cursor is
// bounded, repeated cursors terminate pagination, and LTE= is terminal.
[[nodiscard]] ProtocolResult ParseOpenOrdersPage(
    std::string_view json, Pagination& pagination, api::VenueEvent* events,
    std::size_t event_capacity, std::size_t& event_count) noexcept;
[[nodiscard]] ProtocolResult ParseOpenOrderSnapshots(
    std::string_view json, Pagination& pagination, OpenOrderSnapshot* items,
    std::size_t item_capacity, std::size_t& item_count) noexcept;
[[nodiscard]] ProtocolResult ParsePositions(
    std::string_view json, PositionSnapshot* items, std::size_t item_capacity,
    std::size_t& item_count) noexcept;

[[nodiscard]] ProtocolResult ParsePlaceResponse(
    std::string_view json, const api::OrderHandle& handle,
    const api::RequestToken& token, api::VenueEvent& event) noexcept;
[[nodiscard]] ProtocolResult ParseCancelResponse(
    std::string_view json, std::string_view expected_order_id,
    const api::OrderHandle& handle, const api::RequestToken& token,
    api::VenueEvent& event) noexcept;
[[nodiscard]] ProtocolResult ParseUserMessage(
    std::string_view json, api::VenueEvent& event) noexcept;
[[nodiscard]] ProtocolResult ParseUserMessageEvents(
    std::string_view json, api::VenueEvent* events, std::size_t event_capacity,
    std::size_t& event_count) noexcept;

}  // namespace oms::exchange::polymarket
