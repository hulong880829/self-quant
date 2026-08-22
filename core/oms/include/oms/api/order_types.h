#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <type_traits>

#include "oms/api/error.h"

namespace oms::api {

using InstrumentId = std::uint64_t;
using AccountId = std::uint32_t;

template <std::size_t Capacity, typename Tag>
struct alignas(8) FixedId {
  std::array<char, Capacity> value{};
  std::uint16_t length{};
  std::array<std::uint8_t, 6> reserved{};

  friend constexpr bool operator==(const FixedId& lhs,
                                   const FixedId& rhs) noexcept {
    if (lhs.length != rhs.length || lhs.length > Capacity) return false;
    for (std::size_t i = 0; i < lhs.length; ++i) {
      if (lhs.value[i] != rhs.value[i]) return false;
    }
    return true;
  }
};

struct ClientOrderIdTag;
struct VenueOrderIdTag;
struct TradeIdTag;
using ClientOrderId = FixedId<64, ClientOrderIdTag>;
using VenueOrderId = FixedId<96, VenueOrderIdTag>;
using TradeId = FixedId<96, TradeIdTag>;

struct RequestToken {
  std::uint32_t lane{};
  std::uint32_t session_epoch{};
  std::uint64_t sequence{};

  friend constexpr bool operator==(const RequestToken&, const RequestToken&) =
      default;
};

using QueryToken = RequestToken;

enum class QueryKind : std::uint8_t {
  OpenOrders = 1,
  Positions = 2,
};

struct OrderHandle {
  std::uint32_t slot{};
  std::uint32_t reserved{};
  std::uint64_t generation{};

  friend constexpr bool operator==(const OrderHandle&, const OrderHandle&) =
      default;
};

struct FixedPoint {
  std::int64_t value{};
  std::uint8_t scale{};
  std::array<std::uint8_t, 7> reserved{};

  friend constexpr bool operator==(const FixedPoint&, const FixedPoint&) =
      default;
};

enum class Side : std::uint8_t { Buy = 1, Sell = 2 };
enum class OrderType : std::uint8_t { Limit = 1, Market = 2 };
enum class TimeInForce : std::uint8_t {
  GTC = 1,
  GTD = 2,
  IOC = 3,
  FOK = 4,
  FAK = 5
};
enum class OrderStatus : std::uint8_t {
  PendingSubmit = 1,
  Open = 2,
  PartiallyFilled = 3,
  Filled = 4,
  Canceled = 5,
  Rejected = 6,
  Expired = 7,
  Unknown = 8
};
enum class InflightAction : std::uint8_t {
  None = 0,
  Submit = 1,
  Cancel = 2,
  Reconcile = 3
};

struct OpenOrderSnapshotItem {
  QueryToken query_token{};
  AccountId account_id{};
  std::uint32_t reserved{};
  InstrumentId instrument_id{};
  VenueOrderId venue_order_id{};
  Side side{Side::Buy};
  OrderStatus status{OrderStatus::Unknown};
  std::uint8_t reserved1[6]{};
  FixedPoint quantity{};
  FixedPoint price{};
  FixedPoint matched_quantity{};
  FixedPoint remaining_quantity{};
};

struct PositionSnapshotItem {
  QueryToken query_token{};
  AccountId account_id{};
  std::uint32_t reserved{};
  InstrumentId instrument_id{};
  FixedPoint quantity{};
};

struct QueryComplete {
  QueryToken query_token{};
  AccountId account_id{};
  QueryKind kind{QueryKind::OpenOrders};
  std::uint8_t reserved[3]{};
  Error error{Error::Ok};
  std::uint8_t reserved1[7]{};
};

enum class PolymarketOutcome : std::uint8_t {
  Unknown = 0,
  Yes = 1,
  No = 2,
};

enum OrderFlag : std::uint16_t {
  PostOnly = 1U << 0U,
  ReduceOnly = 1U << 1U,
  ClosePosition = 1U << 2U,
  QuoteQuantity = 1U << 3U
};

struct NewOrderRequest {
  RequestToken token{};
  ClientOrderId client_order_id{};
  InstrumentId instrument_id{};
  Side side{Side::Buy};
  OrderType type{OrderType::Limit};
  TimeInForce time_in_force{TimeInForce::GTC};
  std::uint8_t reserved0{};
  std::uint16_t flags{};
  std::uint16_t reserved1{};
  FixedPoint quantity{};
  FixedPoint price{};
  std::uint64_t expire_time_ns{};
};

enum class ExecutionRouteKind : std::uint8_t {
  LegacyRegistry = 0,
  Generic = 1,
  Polymarket = 2,
};

// Immutable venue-routing data captured from the MDS catalog by the caller.
// Adapters must use this snapshot for prepared orders instead of consulting
// mutable instrument metadata.
struct ExecutionRoutingSnapshot {
  ExecutionRouteKind kind{ExecutionRouteKind::LegacyRegistry};
  std::uint8_t venue{};
  std::uint8_t product_type{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t signature_type{};
  bool negative_risk{};
  PolymarketOutcome outcome{PolymarketOutcome::Unknown};
  std::uint32_t catalog_generation{};
  std::uint32_t taker_delay_ms{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
  std::int64_t minimum_order_size{};
  std::uint64_t instrument_expiry_ns{};
  std::array<std::uint8_t, 32> condition_id{};
  std::array<std::uint8_t, 32> token_id{};
};

struct PreparedOrderRequest {
  NewOrderRequest order{};
  ExecutionRoutingSnapshot routing{};
};

struct CancelOrderRequest {
  RequestToken request_token{};
  RequestToken target_token{};
  OrderHandle handle{};
};

struct RebindPolymarketInstrumentRequest {
  RequestToken request_token{};
  std::array<std::uint8_t, 32> condition_id{};
  std::array<std::uint8_t, 32> token_id{};
  InstrumentId instrument_id{};
  PolymarketOutcome outcome{PolymarketOutcome::Unknown};
  bool negative_risk{};
  std::uint8_t signature_type{};
  std::uint8_t reserved{};
  std::int64_t minimum_order_size{1};
  std::uint32_t taker_delay_ms{};
  std::uint32_t reserved1{};
};

// Cancel updates keep using target_token as the order identity. This explicit
// correlation pair carries the independently generated cancel request token.
struct CancelCommandCorrelation {
  RequestToken target_token{};
  RequestToken request_token{};
};

struct RebindPolymarketInstrumentResult {
  RequestToken request_token{};
  InstrumentId instrument_id{};
};

enum class VenueEventType : std::uint8_t {
  NewAck = 1,
  NewReject = 2,
  CancelAck = 3,
  CancelReject = 4,
  Expire = 5,
  ReconcileOpen = 6,
  ReconcileTerminal = 7,
  Fill = 8
};

struct VenueEvent {
  VenueEventType type{VenueEventType::NewAck};
  std::array<std::uint8_t, 7> reserved{};
  OrderHandle handle{};
  RequestToken token{};
  ClientOrderId client_order_id{};
  VenueOrderId venue_order_id{};
  TradeId trade_id{};
  FixedPoint fill_quantity{};
  FixedPoint fill_price{};
  std::uint64_t event_time_ns{};
  OrderStatus reconciled_status{OrderStatus::Unknown};
  std::array<std::uint8_t, 7> reserved1{};
};

enum class UpdateType : std::uint8_t {
  Submitted = 1,
  Accepted = 2,
  Rejected = 3,
  CancelRequested = 4,
  Canceled = 5,
  CancelRejected = 6,
  Expired = 7,
  Reconciled = 8
};

struct OrderUpdate {
  UpdateType type{UpdateType::Submitted};
  std::array<std::uint8_t, 7> reserved{};
  OrderHandle handle{};
  RequestToken token{};
  ClientOrderId client_order_id{};
  VenueOrderId venue_order_id{};
  OrderStatus status{OrderStatus::PendingSubmit};
  InflightAction inflight{InflightAction::None};
  std::array<std::uint8_t, 6> reserved1{};

  friend constexpr bool operator==(const OrderUpdate&, const OrderUpdate&) =
      default;
};

struct FillUpdate {
  OrderHandle handle{};
  RequestToken token{};
  ClientOrderId client_order_id{};
  VenueOrderId venue_order_id{};
  TradeId trade_id{};
  FixedPoint quantity{};
  FixedPoint price{};
  FixedPoint cumulative_quantity{};
  FixedPoint remaining_quantity{};
  FixedPoint average_price{};

  friend constexpr bool operator==(const FillUpdate&, const FillUpdate&) =
      default;
};

struct UpdateSink {
  void* context{};
  void (*on_order)(void*, const OrderUpdate&) noexcept{};
  void (*on_fill)(void*, const FillUpdate&) noexcept{};
};

static_assert(std::is_trivially_copyable_v<ClientOrderId>);
static_assert(std::is_trivially_copyable_v<VenueOrderId>);
static_assert(std::is_trivially_copyable_v<TradeId>);
static_assert(std::is_trivially_copyable_v<RequestToken>);
static_assert(std::is_trivially_copyable_v<OrderHandle>);
static_assert(std::is_trivially_copyable_v<FixedPoint>);
static_assert(std::is_trivially_copyable_v<PolymarketOutcome>);
static_assert(
    std::is_trivially_copyable_v<RebindPolymarketInstrumentRequest>);
static_assert(std::is_trivially_copyable_v<RebindPolymarketInstrumentResult>);
static_assert(std::is_standard_layout_v<RebindPolymarketInstrumentRequest>);
static_assert(std::is_standard_layout_v<RebindPolymarketInstrumentResult>);
static_assert(sizeof(PolymarketOutcome) == 1);
static_assert(sizeof(RebindPolymarketInstrumentRequest) == 112);
static_assert(sizeof(RebindPolymarketInstrumentResult) == 24);
static_assert(std::is_trivially_copyable_v<NewOrderRequest>);
static_assert(std::is_trivially_copyable_v<ExecutionRoutingSnapshot>);
static_assert(std::is_trivially_copyable_v<PreparedOrderRequest>);
static_assert(std::is_trivially_copyable_v<CancelOrderRequest>);
static_assert(std::is_trivially_copyable_v<CancelCommandCorrelation>);
static_assert(std::is_trivially_copyable_v<VenueEvent>);
static_assert(std::is_trivially_copyable_v<OrderUpdate>);
static_assert(std::is_trivially_copyable_v<FillUpdate>);
static_assert(std::is_trivially_copyable_v<UpdateSink>);
static_assert(std::is_standard_layout_v<ClientOrderId>);
static_assert(std::is_standard_layout_v<VenueOrderId>);
static_assert(std::is_standard_layout_v<TradeId>);
static_assert(std::is_standard_layout_v<RequestToken>);
static_assert(std::is_standard_layout_v<OrderHandle>);
static_assert(std::is_standard_layout_v<FixedPoint>);
static_assert(std::is_standard_layout_v<NewOrderRequest>);
static_assert(std::is_standard_layout_v<ExecutionRoutingSnapshot>);
static_assert(std::is_standard_layout_v<PreparedOrderRequest>);
static_assert(std::is_standard_layout_v<CancelOrderRequest>);
static_assert(std::is_standard_layout_v<CancelCommandCorrelation>);
static_assert(std::is_standard_layout_v<VenueEvent>);
static_assert(std::is_standard_layout_v<OrderUpdate>);
static_assert(std::is_standard_layout_v<FillUpdate>);
static_assert(sizeof(ClientOrderId) == 72);
static_assert(sizeof(VenueOrderId) == 104);
static_assert(sizeof(TradeId) == 104);
static_assert(sizeof(RequestToken) == 16);
static_assert(sizeof(OrderHandle) == 16);
static_assert(sizeof(FixedPoint) == 16);
static_assert(sizeof(NewOrderRequest) == 144);
static_assert(sizeof(ExecutionRoutingSnapshot) == 112);
static_assert(sizeof(PreparedOrderRequest) == 256);
static_assert(sizeof(CancelOrderRequest) == 48);
static_assert(sizeof(CancelCommandCorrelation) == 32);
static_assert(sizeof(VenueEvent) == 368);
static_assert(sizeof(OrderUpdate) == 224);
static_assert(sizeof(FillUpdate) == 392);
static_assert(static_cast<std::uint8_t>(TimeInForce::GTC) == 1);
static_assert(static_cast<std::uint8_t>(TimeInForce::GTD) == 2);
static_assert(static_cast<std::uint8_t>(TimeInForce::IOC) == 3);
static_assert(static_cast<std::uint8_t>(TimeInForce::FOK) == 4);
static_assert(static_cast<std::uint8_t>(TimeInForce::FAK) == 5);
static_assert(static_cast<std::uint16_t>(OrderFlag::PostOnly) == 1);
static_assert(static_cast<std::uint16_t>(OrderFlag::QuoteQuantity) == 8);

}  // namespace oms::api
