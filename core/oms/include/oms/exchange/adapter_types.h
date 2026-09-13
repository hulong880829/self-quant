#pragma once

#include <cstddef>
#include <cstdint>
#include <type_traits>

#include "oms/api/order_types.h"
#include "utils/md/types.h"

namespace oms::exchange {

inline constexpr std::size_t kMaxTradeAdapters = 4;

enum class AdapterKind : std::uint8_t {
  BinanceSpot = 1,
  BinanceUsdm = 2,
  Polymarket = 3,
  Fake = 4,
};

enum class AdapterStatus : std::uint8_t {
  Stopped = 0,
  Connecting = 1,
  Authenticating = 2,
  Ready = 3,
  Backpressured = 4,
  Reconnecting = 5,
  Reconciling = 6,
  Failed = 7,
};

enum class AdapterResult : std::uint8_t {
  Ok = 0,
  WouldBlock = 1,
  NotReady = 2,
  Unsupported = 3,
  InvalidArgument = 4,
  StaleReservation = 5,
  Failed = 6,
};

enum class AdapterCommandKind : std::uint8_t {
  Place = 1,
  Cancel = 2,
};

enum class AdapterEventKind : std::uint8_t {
  Venue = 1,
  CommandResult = 2,
  Status = 3,
  ReconcileComplete = 4,
  OpenOrderSnapshot = 5,
  PositionSnapshot = 6,
  QueryComplete = 7,
};

enum class AdapterDeadlineKind : std::uint8_t {
  Request = 1,
  Keepalive = 2,
  Heartbeat = 3,
  Reconnect = 4,
  Reconcile = 5,
};

enum class AdapterCapability : std::uint64_t {
  Limit = 1ULL << 0U,
  Market = 1ULL << 1U,
  TifGtc = 1ULL << 2U,
  TifGtd = 1ULL << 3U,
  TifIoc = 1ULL << 4U,
  TifFok = 1ULL << 5U,
  TifFak = 1ULL << 6U,
  PostOnly = 1ULL << 7U,
  ReduceOnly = 1ULL << 8U,
  ClosePosition = 1ULL << 9U,
  QuoteQuantity = 1ULL << 10U,
  BatchPlace = 1ULL << 11U,
  BatchCancel = 1ULL << 12U,
  ReconcileOpenOrders = 1ULL << 13U,
  UserOrderStream = 1ULL << 14U,
  UserFillStream = 1ULL << 15U,
};

struct AdapterCapabilities {
  std::uint64_t bits{};

  [[nodiscard]] constexpr bool supports(
      AdapterCapability capability) const noexcept {
    return (bits & static_cast<std::uint64_t>(capability)) != 0;
  }
};

[[nodiscard]] constexpr AdapterResult preflight_place(
    AdapterCapabilities capabilities,
    const api::NewOrderRequest& request) noexcept {
  const auto supported = [&capabilities](AdapterCapability capability) {
    return capabilities.supports(capability);
  };
  constexpr std::uint16_t known_flags =
      api::PostOnly | api::ReduceOnly | api::ClosePosition |
      api::QuoteQuantity;
  if ((request.type != api::OrderType::Limit &&
       request.type != api::OrderType::Market) ||
      (request.flags & static_cast<std::uint16_t>(~known_flags)) != 0U) {
    return AdapterResult::InvalidArgument;
  }
  if ((request.type == api::OrderType::Limit &&
       !supported(AdapterCapability::Limit)) ||
      (request.type == api::OrderType::Market &&
       !supported(AdapterCapability::Market))) {
    return AdapterResult::Unsupported;
  }
  AdapterCapability tif = AdapterCapability::TifGtc;
  switch (request.time_in_force) {
    case api::TimeInForce::GTC:
      tif = AdapterCapability::TifGtc;
      break;
    case api::TimeInForce::GTD:
      tif = AdapterCapability::TifGtd;
      break;
    case api::TimeInForce::IOC:
      tif = AdapterCapability::TifIoc;
      break;
    case api::TimeInForce::FOK:
      tif = AdapterCapability::TifFok;
      break;
    case api::TimeInForce::FAK:
      tif = AdapterCapability::TifFak;
      break;
    default:
      return AdapterResult::InvalidArgument;
  }
  // Market orders have no venue TIF parameter. GTC is the OMS convention;
  // accepting another value would silently discard requested semantics.
  if ((request.type == api::OrderType::Market &&
       request.time_in_force != api::TimeInForce::GTC) ||
      !supported(tif)) {
    return AdapterResult::Unsupported;
  }
  const struct {
    std::uint16_t flag;
    AdapterCapability capability;
  } flags[] = {
      {api::PostOnly, AdapterCapability::PostOnly},
      {api::ReduceOnly, AdapterCapability::ReduceOnly},
      {api::ClosePosition, AdapterCapability::ClosePosition},
      {api::QuoteQuantity, AdapterCapability::QuoteQuantity},
  };
  for (const auto& flag : flags) {
    if ((request.flags & flag.flag) != 0U && !supported(flag.capability))
      return AdapterResult::Unsupported;
  }
  const bool post_only = (request.flags & api::PostOnly) != 0U;
  const bool quote_quantity = (request.flags & api::QuoteQuantity) != 0U;
  // These combinations cannot be represented without discarding or changing
  // caller semantics on the supported venues.
  if ((post_only &&
       (request.type != api::OrderType::Limit ||
        request.time_in_force != api::TimeInForce::GTC)) ||
      (quote_quantity && request.type != api::OrderType::Market)) {
    return AdapterResult::Unsupported;
  }
  return AdapterResult::Ok;
}

struct AdapterIdentity {
  AdapterKind kind{AdapterKind::Fake};
  std::uint8_t reserved0{};
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product_type{utils::md::ProductType::Unknown};
  std::uint8_t reserved1[3]{};
};

struct AdapterReservation {
  AdapterKind adapter{AdapterKind::Fake};
  std::uint8_t reserved[3]{};
  std::uint32_t slot{};
  std::uint64_t generation{};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return generation != 0;
  }
};

struct AdapterPlaceCommand {
  std::uint64_t command_id{};
  api::OrderHandle handle{};
  api::NewOrderRequest request{};
  api::ResolvedInstrument routing{};
};

struct AdapterCancelCommand {
  std::uint64_t command_id{};
  api::CancelOrderRequest request{};
};

struct AdapterCommand {
  AdapterCommandKind kind{AdapterCommandKind::Place};
  std::uint8_t reserved[7]{};
  AdapterPlaceCommand place{};
  AdapterCancelCommand cancel{};
};

struct AdapterCommandResult {
  std::uint64_t command_id{};
  AdapterCommandKind kind{AdapterCommandKind::Place};
  AdapterResult result{AdapterResult::Ok};
  std::uint8_t reserved[2]{};
  std::int32_t venue_code{};
  api::RequestToken request_token{};
};

struct AdapterStatusEvent {
  AdapterIdentity identity{};
  AdapterStatus status{AdapterStatus::Stopped};
  AdapterResult reason{AdapterResult::Ok};
  std::uint8_t reserved[6]{};
  std::uint64_t event_time_ns{};
};

struct AdapterReconcileEvent {
  std::uint64_t generation{};
  AdapterResult result{AdapterResult::Ok};
  std::uint8_t reserved[7]{};
};

struct AdapterQueryRequest {
  api::QueryToken token{};
  api::QueryRequest request{};
};

struct AdapterEvent {
  AdapterEventKind kind{AdapterEventKind::Venue};
  std::uint8_t reserved[7]{};
  AdapterIdentity source{};
  api::VenueEvent venue{};
  AdapterCommandResult command_result{};
  AdapterStatusEvent status{};
  AdapterReconcileEvent reconcile{};
  api::OpenOrderSnapshotItem open_order{};
  api::PositionSnapshotItem position{};
  api::QueryComplete query_complete{};
};

struct AdapterEventSink {
  void* context{};
  AdapterResult (*on_event)(void*, const AdapterEvent&) noexcept{};
};

struct AdapterDeadline {
  AdapterDeadlineKind kind{AdapterDeadlineKind::Request};
  std::uint8_t reserved[3]{};
  std::uint32_t id{};
  std::uint64_t generation{};
  std::uint64_t due_time_ns{};
};

struct AdapterServiceResult {
  AdapterResult result{AdapterResult::Ok};
  std::uint8_t reserved[3]{};
  std::uint32_t events_processed{};
  std::uint64_t next_deadline_ns{};
};

static_assert(std::is_trivially_copyable_v<AdapterCapabilities>);
static_assert(std::is_trivially_copyable_v<AdapterIdentity>);
static_assert(std::is_trivially_copyable_v<AdapterReservation>);
static_assert(std::is_trivially_copyable_v<AdapterPlaceCommand>);
static_assert(std::is_trivially_copyable_v<AdapterCommand>);
static_assert(std::is_trivially_copyable_v<AdapterQueryRequest>);
static_assert(std::is_trivially_copyable_v<AdapterEvent>);
static_assert(std::is_trivially_copyable_v<AdapterEventSink>);
static_assert(std::is_trivially_copyable_v<AdapterDeadline>);
static_assert(std::is_trivially_copyable_v<AdapterServiceResult>);
static_assert(std::is_standard_layout_v<AdapterCapabilities>);
static_assert(std::is_standard_layout_v<AdapterIdentity>);
static_assert(std::is_standard_layout_v<AdapterReservation>);
static_assert(std::is_standard_layout_v<AdapterPlaceCommand>);
static_assert(std::is_standard_layout_v<AdapterCommand>);
static_assert(std::is_standard_layout_v<AdapterQueryRequest>);
static_assert(std::is_standard_layout_v<AdapterEvent>);
static_assert(std::is_standard_layout_v<AdapterEventSink>);
static_assert(std::is_standard_layout_v<AdapterDeadline>);
static_assert(std::is_standard_layout_v<AdapterServiceResult>);

static_assert(sizeof(AdapterKind) == 1);
static_assert(sizeof(AdapterStatus) == 1);
static_assert(sizeof(AdapterResult) == 1);
static_assert(sizeof(AdapterCommandKind) == 1);
static_assert(sizeof(AdapterEventKind) == 1);
static_assert(sizeof(AdapterDeadlineKind) == 1);
static_assert(sizeof(AdapterCapability) == 8);
static_assert(sizeof(AdapterCapabilities) == 8);
static_assert(sizeof(AdapterIdentity) == 8);
static_assert(sizeof(AdapterReservation) == 16);
static_assert(sizeof(AdapterPlaceCommand) == 280);
static_assert(alignof(AdapterPlaceCommand) == 8);
static_assert(sizeof(AdapterCancelCommand) == 56);
static_assert(sizeof(AdapterCommand) == 344);
static_assert(sizeof(AdapterCommandResult) == 32);
static_assert(sizeof(AdapterQueryRequest) == 96);
static_assert(sizeof(AdapterStatusEvent) == 24);
static_assert(sizeof(AdapterReconcileEvent) == 16);
static_assert(sizeof(AdapterEventSink) == 16);
static_assert(sizeof(AdapterDeadline) == 24);
static_assert(sizeof(AdapterServiceResult) == 16);

static_assert(static_cast<std::uint8_t>(AdapterKind::BinanceSpot) == 1);
static_assert(static_cast<std::uint8_t>(AdapterKind::BinanceUsdm) == 2);
static_assert(static_cast<std::uint8_t>(AdapterKind::Polymarket) == 3);
static_assert(static_cast<std::uint8_t>(AdapterKind::Fake) == 4);
static_assert(static_cast<std::uint8_t>(AdapterResult::WouldBlock) == 1);
static_assert(static_cast<std::uint8_t>(AdapterStatus::Ready) == 3);
static_assert(static_cast<std::uint8_t>(AdapterStatus::Reconciling) == 6);
static_assert(static_cast<std::uint8_t>(AdapterEventKind::Venue) == 1);
static_assert(static_cast<std::uint64_t>(
                  AdapterCapability::ReconcileOpenOrders) ==
              (1ULL << 13U));

}  // namespace oms::exchange
