#pragma once

#include <array>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "oms/api/order_types.h"
#include "oms/exchange/polymarket/crypto.h"

namespace oms::exchange::hyperliquid {

struct LimitOrder {
  std::int64_t asset{};
  bool buy{};
  bool reduce_only{};
  std::string price{};
  std::string size{};
  std::string tif{"Gtc"};
  std::string cloid{};
};

inline constexpr std::uint16_t kCloidMagic = 0x5351;
inline constexpr std::uint8_t kCloidVersion = 1;
inline constexpr std::uint64_t kMaximumCloidSequence =
    (1ULL << 48U) - 1U;

struct CloidFields {
  std::uint32_t strategy_instance_hash{};
  std::uint32_t process_epoch{};
  std::uint64_t sequence{};
};

[[nodiscard]] std::uint32_t stable_strategy_hash(
    std::string_view strategy_instance) noexcept;
[[nodiscard]] std::uint32_t make_process_epoch() noexcept;
[[nodiscard]] bool encode_cloid(const CloidFields& fields,
                                std::string& output) noexcept;
[[nodiscard]] bool decode_cloid(std::string_view text,
                                CloidFields& output) noexcept;
[[nodiscard]] bool cloid_owned_by(std::string_view text,
                                  std::uint32_t strategy_instance_hash) noexcept;

// Msgpack follows the Hyperliquid Go/Python SDK field order, not sorted keys.
[[nodiscard]] std::vector<std::uint8_t> encode_order_action(
    const LimitOrder& order);
[[nodiscard]] std::vector<std::uint8_t> encode_cancel_by_cloid(
    std::int64_t asset, std::string_view cloid);

[[nodiscard]] polymarket::Bytes32 action_hash(
    const std::uint8_t* action, std::size_t size, std::uint64_t nonce,
    const polymarket::Address* vault) noexcept;

[[nodiscard]] polymarket::Bytes32 agent_digest(
    const polymarket::Bytes32& connection_id, bool mainnet) noexcept;

enum class JsonParseResult : std::uint8_t {
  Ok = 0,
  Malformed = 1,
  MissingField = 2,
  InvalidValue = 3,
  Oversize = 4,
};

struct PerpDex {
  std::string name;
  std::int64_t index{};
};

struct PerpAsset {
  std::string symbol;
  std::int64_t dex_index{};
  std::int64_t asset_index{};
  std::uint8_t size_decimals{};
};

enum class UserEventKind : std::uint8_t { Order, Fill };

struct UserEvent {
  UserEventKind kind{UserEventKind::Order};
  std::string coin;
  std::string cloid;
  std::string status;
  std::string oid;
  std::string tid;
  api::FixedPoint size{};
  api::FixedPoint price{};
  std::uint64_t time_ms{};
};

struct ExchangeResponse {
  bool accepted{};
  bool rejected{};
  std::string oid;
  std::string cloid;
};

struct OpenOrder {
  std::string coin;
  std::string cloid;
  std::string oid;
  api::Side side{api::Side::Buy};
  api::FixedPoint size{};
  api::FixedPoint price{};
  std::uint64_t time_ms{};
};

struct Position {
  std::string coin;
  api::FixedPoint size{};
};

[[nodiscard]] JsonParseResult parse_perp_dexs(
    std::string_view json, std::vector<PerpDex>& output) noexcept;
[[nodiscard]] JsonParseResult parse_perp_meta(
    std::string_view json, std::int64_t dex_index, std::string_view dex_name,
    std::vector<PerpAsset>& output) noexcept;
[[nodiscard]] JsonParseResult parse_user_events(
    std::string_view json, std::vector<UserEvent>& output) noexcept;
[[nodiscard]] JsonParseResult parse_exchange_response(
    std::string_view json, ExchangeResponse& output) noexcept;
[[nodiscard]] JsonParseResult parse_open_orders(
    std::string_view json, std::vector<OpenOrder>& output) noexcept;
[[nodiscard]] JsonParseResult parse_positions(
    std::string_view json, std::vector<Position>& output) noexcept;

}  // namespace oms::exchange::hyperliquid
