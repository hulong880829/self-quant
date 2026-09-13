#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <span>
#include <string_view>

#include "oms/api/oms_api.h"

namespace oms::api {

struct EndpointOverride {
  // Empty fields select the venue default. Hosts do not include a URI scheme
  // or path; service is a decimal TCP port.
  std::string_view host{};
  std::string_view service{};
};

struct VenueEndpointOverrides {
  EndpointOverride rest{};
  EndpointOverride websocket{};
};

struct TransportSocketConfig {
  bool tcp_nodelay{true};
  std::int32_t receive_buffer_bytes{};
  std::int32_t send_buffer_bytes{};
  std::int32_t busy_poll_us{};
};

struct BinanceCredentialView {
  std::string_view api_key{};
  std::string_view secret_key{};
};

using BinanceCredentialProvider = bool (*)(
    void* context, BinanceCredentialView& output) noexcept;

struct BinanceExecutionConfig {
  AccountId account_id{};
  bool enabled{};
  VenueEndpointOverrides endpoints{};
  // Dedicated order-entry WebSocket API. This is distinct from the user-data
  // stream in endpoints.websocket and from endpoints.rest.
  EndpointOverride trading_websocket{};
  BinanceCredentialView credentials{};
  BinanceCredentialProvider credential_provider{};
  void* credential_context{};
};

struct PolymarketCredentialView {
  std::string_view signer_address{};
  std::string_view funder_address{};
  std::string_view private_key{};
  std::string_view api_key{};
  std::string_view api_secret{};
  std::string_view passphrase{};
};

using PolymarketCredentialProvider = bool (*)(
    void* context, PolymarketCredentialView& output) noexcept;

struct PolymarketExecutionConfig {
  AccountId account_id{};
  bool enabled{};
  VenueEndpointOverrides endpoints{};
  EndpointOverride data_api{};
  PolymarketCredentialView credentials{};
  PolymarketCredentialProvider credential_provider{};
  void* credential_context{};
};

struct ExecutionChannelConfig {
  RuntimeConfig runtime{};
  TransportSocketConfig socket{};
  BinanceExecutionConfig binance_spot{};
  BinanceExecutionConfig binance_usdm{};
  PolymarketExecutionConfig polymarket{};
  std::uint32_t event_budget{64};
};

// Owning StrategyFrame-facing façade. Network transports and venue adapters
// are intentionally absent from this interface; live construction is supplied
// by the library implementation while callers retain only this owner.
class ExecutionChannel {
 public:
  // Fake/offline construction retained for deterministic replay and existing
  // StrategyFrame integrations.
  static Result<std::unique_ptr<ExecutionChannel>> Create(
      const RuntimeConfig& config, std::span<const InstrumentInit> instruments,
      std::span<const ReplayStep> replay = {});
  // Live construction. Every enabled venue gets a dedicated transport,
  // transport wrapper, and trade adapter. Credential providers are invoked
  // synchronously and all returned views are copied before Create returns.
  static Result<std::unique_ptr<ExecutionChannel>> Create(
      const ExecutionChannelConfig& config,
      std::span<const InstrumentInit> instruments);

  ~ExecutionChannel();
  ExecutionChannel(const ExecutionChannel&) = delete;
  ExecutionChannel& operator=(const ExecutionChannel&) = delete;

  Result<void> initialize_lane(std::uint32_t lane_id,
                               std::uint32_t session_epoch) noexcept;
  Result<RequestToken> place_order(std::uint32_t lane_id,
                                   SubmitOrderRequest request) noexcept;
  Result<RequestToken> cancel_order(std::uint32_t lane_id,
                                    RequestToken target,
                                    OrderHandle handle = {}) noexcept;
  Result<RequestToken> register_instrument(
      std::uint32_t lane_id, RegisterInstrumentRequest request) noexcept;
  Result<RequestToken> retire_instrument(std::uint32_t lane_id,
                                         InstrumentId instrument_id) noexcept;
  Result<QueryToken> query_open_orders(std::uint32_t lane_id,
                                      AccountId account_id) noexcept;
  Result<QueryToken> query_open_orders(std::uint32_t lane_id,
                                      QueryRequest request) noexcept;
  Result<QueryToken> query_positions(std::uint32_t lane_id,
                                    AccountId account_id) noexcept;
  Result<QueryToken> query_positions(std::uint32_t lane_id,
                                     QueryRequest request) noexcept;
  Error service_io(int timeout_ms) noexcept;
  std::size_t drain_updates(std::uint32_t lane_id, UpdateCallback callback,
                            void* context,
                            std::size_t maximum = SIZE_MAX) noexcept;
  [[nodiscard]] int notification_fd(std::uint32_t lane_id) const noexcept;
  [[nodiscard]] RuntimeMetrics metrics() const noexcept;
  [[nodiscard]] Result<AdapterStatusSnapshot> venue_status(
      exchange::AdapterKind kind) const noexcept;
  Error reconcile(exchange::AdapterKind kind) noexcept;
  Error shutdown() noexcept;

 private:
  struct Impl;
  explicit ExecutionChannel(std::unique_ptr<Impl> impl) noexcept;
  std::unique_ptr<Impl> impl_;
};

}  // namespace oms::api
