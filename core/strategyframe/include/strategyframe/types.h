#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>
#include <type_traits>

#include "utils/md/types.h"

namespace strategyframe {

using InstrumentId = utils::md::InstrumentId;
using AccountId = std::uint32_t;

template <std::size_t Capacity, typename Tag>
struct FixedId {
  char value[Capacity]{};
  std::uint16_t length{};
  std::uint8_t reserved[6]{};
  friend constexpr bool operator==(const FixedId& lhs,
                                   const FixedId& rhs) noexcept {
    if (lhs.length != rhs.length || lhs.length > Capacity) return false;
    for (std::size_t index = 0; index < lhs.length; ++index) {
      if (lhs.value[index] != rhs.value[index]) return false;
    }
    return true;
  }
};
struct ClientOrderIdTag;
struct VenueOrderIdTag;
struct TradeIdTag;
struct InstrumentSymbolTag;
struct VenueInstrumentSymbolTag;
struct ConditionIdTag;
struct MarketSlugTag;
struct OutcomeTag;
using ClientOrderId = FixedId<64, ClientOrderIdTag>;
using VenueOrderId = FixedId<96, VenueOrderIdTag>;
using TradeId = FixedId<96, TradeIdTag>;
using InstrumentSymbol = FixedId<64, InstrumentSymbolTag>;
using VenueInstrumentSymbol = FixedId<80, VenueInstrumentSymbolTag>;
using ConditionId = FixedId<72, ConditionIdTag>;
using MarketSlug = FixedId<64, MarketSlugTag>;
using OutcomeName = FixedId<16, OutcomeTag>;

struct FixedPoint {
  std::int64_t value{};
  std::uint8_t scale{};
  std::uint8_t reserved[7]{};
  friend constexpr bool operator==(const FixedPoint&, const FixedPoint&) =
      default;
};

using Venue = utils::md::Venue;
using ProductType = utils::md::ProductType;

enum class Side : std::uint8_t { Buy = 1, Sell = 2 };
enum class OrderType : std::uint8_t { Limit = 1, Market = 2 };
enum class TimeInForce : std::uint8_t {
  GTC = 1,
  GTD = 2,
  IOC = 3,
  FOK = 4,
};
enum OrderFlag : std::uint16_t {
  PostOnly = 1U << 0U,
  ReduceOnly = 1U << 1U,
  ClosePosition = 1U << 2U,
  QuoteQuantity = 1U << 3U,
};
enum class CommandType : std::uint8_t {
  Place = 1,
  Cancel = 2,
  RegisterInstrument = 3,
  RetireInstrument = 4,
};
enum class OrderStatus : std::uint8_t {
  PendingSubmit = 1,
  Open = 2,
  PartiallyFilled = 3,
  Filled = 4,
  Canceled = 5,
  Rejected = 6,
  Expired = 7,
  Unknown = 8,
};
enum class PositionSide : std::uint8_t {
  Net = 0,
  Long = 1,
  Short = 2,
};

struct InstrumentInfo {
  InstrumentId instrument_id{};
  Venue venue{Venue::Unknown};
  ProductType product{ProductType::Unknown};
  InstrumentSymbol symbol{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
};

struct InstrumentCatalogInfo {
  InstrumentId instrument_id{};
  Venue venue{Venue::Unknown};
  ProductType product{ProductType::Unknown};
  InstrumentSymbol canonical_symbol{};
  MarketSlug market_slug{};
  VenueInstrumentSymbol venue_symbol{};
  ConditionId condition_id{};
  OutcomeName outcome{};
  std::uint64_t expiry_time_ns{};
  std::uint32_t generation{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t signature_type{};
  bool negative_risk{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
  std::int64_t contract_multiplier{};
};

struct InstrumentSelector {
  Venue venue{Venue::Unknown};
  ProductType product{ProductType::Unknown};
  std::string canonical_symbol{};
};

struct MarketLevel {
  FixedPoint price{};
  FixedPoint quantity{};
};

struct MarketUpdateHeader {
  InstrumentId instrument_id{};
  Venue venue{Venue::Unknown};
  ProductType product{ProductType::Unknown};
  std::uint8_t source_id{};
  std::uint8_t state{};
  std::uint16_t flags{};
  std::uint64_t bus_sequence{};
  std::uint64_t source_sequence{};
  std::uint64_t exchange_time_ns{};
  std::uint64_t receive_tsc{};
  std::uint64_t dispatch_tsc{};
  std::uint32_t book_generation{};
};

struct BboUpdate {
  MarketUpdateHeader header{};
  MarketLevel bid{};
  MarketLevel ask{};
};

struct OrderBookUpdate {
  MarketUpdateHeader header{};
  std::span<const MarketLevel> bids{};
  std::span<const MarketLevel> asks{};
};

inline constexpr std::size_t kMaxAggregateVenues = 8;

struct AggregateLevel {
  FixedPoint price{};
  FixedPoint quantity{};
  std::span<const std::int64_t> venue_quantities{};
  std::uint32_t venue_mask{};
  std::uint8_t contributor_count{};
};

struct AggBboSide {
  FixedPoint price{};
  FixedPoint quantity{};
  std::span<const std::int64_t> venue_quantities{};
  std::uint64_t exchange_time_ns{};
  std::uint32_t venue_mask{};
  std::uint32_t worst_ingress_age_us{};
  std::uint8_t best_venue{};
};

struct AggBboUpdate {
  MarketUpdateHeader header{};
  AggBboSide bid{};
  AggBboSide ask{};
  std::uint32_t member_mask{};
  std::uint32_t live_mask{};
  std::int32_t cross_bps{};
  std::uint32_t skew_us{};
};

struct AggOrderBookUpdate {
  MarketUpdateHeader header{};
  std::span<const AggregateLevel> bids{};
  std::span<const AggregateLevel> asks{};
  std::uint32_t member_mask{};
  std::uint32_t active_mask{};
};

struct OrderToken {
  std::uint32_t lane{};
  std::uint32_t session_epoch{};
  std::uint64_t sequence{};
  friend constexpr bool operator==(const OrderToken&, const OrderToken&) =
      default;
};

using QueryToken = OrderToken;

struct OrderRequest {
  AccountId account_id{};
  InstrumentId instrument_id{};
  Side side{Side::Buy};
  OrderType type{OrderType::Limit};
  TimeInForce time_in_force{TimeInForce::GTC};
  std::uint16_t flags{};
  FixedPoint quantity{};
  FixedPoint price{};
  std::uint64_t expire_time_ns{};
  std::string_view client_order_id{};
};

struct ExecutionUpdate {
  enum class Kind : std::uint8_t {
    Order = 1,
    Fill = 2,
    CommandResult = 3,
  };

  Kind kind{Kind::Order};
  OrderStatus status{OrderStatus::Unknown};
  std::uint8_t update_type{};
  std::uint8_t reserved{};
  AccountId account_id{};
  InstrumentId instrument_id{};
  OrderToken token{};
  ClientOrderId client_order_id{};
  VenueOrderId venue_order_id{};
  TradeId trade_id{};
  FixedPoint fill_quantity{};
  FixedPoint fill_price{};
  FixedPoint cumulative_quantity{};
  FixedPoint remaining_quantity{};
  std::int32_t error{};
  std::uint64_t event_time_ns{};
};

struct OmsStatusUpdate {
  enum class Kind : std::uint8_t {
    VenueStatus = 1,
    ReconcileComplete = 2,
  };

  Kind kind{Kind::VenueStatus};
  std::uint8_t adapter_kind{};
  std::uint8_t adapter_status{};
  std::uint8_t reserved{};
  std::int32_t error{};
  std::uint64_t generation{};
  std::uint64_t event_time_ns{};
};

struct TimerHandle {
  std::uint32_t slot{};
  std::uint32_t generation{};
  friend constexpr bool operator==(const TimerHandle&, const TimerHandle&) =
      default;
};

struct TimerEvent {
  TimerHandle handle{};
  std::uint64_t scheduled_time_ns{};
  std::uint64_t dispatch_time_ns{};
  std::uint64_t expiration_count{};
};

struct OrderView {
  AccountId account_id{};
  InstrumentId instrument_id{};
  OrderToken token{};
  OrderStatus status{OrderStatus::Unknown};
  Side side{Side::Buy};
  OrderType type{OrderType::Limit};
  TimeInForce time_in_force{TimeInForce::GTC};
  FixedPoint quantity{};
  FixedPoint price{};
  FixedPoint cumulative_quantity{};
  FixedPoint remaining_quantity{};
  std::string_view client_order_id{};
  std::string_view venue_order_id{};
};

struct PositionView {
  AccountId account_id{};
  InstrumentId instrument_id{};
  PositionSide side{PositionSide::Net};
  FixedPoint quantity{};
  std::uint64_t generation{};
};

struct QueriedOrderView {
  AccountId account_id{};
  InstrumentId instrument_id{};
  VenueOrderId venue_order_id{};
  OrderStatus status{OrderStatus::Unknown};
  Side side{Side::Buy};
  std::uint8_t reserved[6]{};
  FixedPoint quantity{};
  FixedPoint price{};
  FixedPoint matched_quantity{};
  FixedPoint remaining_quantity{};
};

struct QueriedPositionView {
  AccountId account_id{};
  InstrumentId instrument_id{};
  FixedPoint quantity{};
};

struct OpenOrdersSnapshot {
  QueryToken token{};
  AccountId account_id{};
  std::span<const QueriedOrderView> orders{};
};

struct PositionsSnapshot {
  QueryToken token{};
  AccountId account_id{};
  std::span<const QueriedPositionView> positions{};
};

struct QueryComplete {
  enum class Kind : std::uint8_t {
    OpenOrders = 1,
    Positions = 2,
  };
  QueryToken token{};
  AccountId account_id{};
  Kind kind{Kind::OpenOrders};
  std::uint8_t reserved{};
  std::int32_t error{};
};

static_assert(std::is_trivially_copyable_v<FixedPoint>);
static_assert(std::is_trivially_copyable_v<ClientOrderId>);
static_assert(std::is_trivially_copyable_v<MarketUpdateHeader>);
static_assert(std::is_trivially_copyable_v<BboUpdate>);
static_assert(std::is_trivially_copyable_v<OrderToken>);
static_assert(std::is_trivially_copyable_v<ExecutionUpdate>);
static_assert(std::is_trivially_copyable_v<OmsStatusUpdate>);
static_assert(sizeof(OrderToken) == 16);

}  // namespace strategyframe
