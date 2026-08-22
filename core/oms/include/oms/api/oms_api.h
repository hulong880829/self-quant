#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <span>

#include "oms/api/runtime_types.h"
#include "oms/exchange/trade_adapter.h"
#include "utils/md/symbol.h"

namespace oms::api {

struct InstrumentInit {
  utils::md::Instrument instrument{};
  std::array<std::uint8_t, 32> polymarket_condition_id{};
  std::array<std::uint8_t, 32> polymarket_token_id{};
  PolymarketOutcome polymarket_outcome{PolymarketOutcome::Unknown};
  bool polymarket_negative_risk{};
  std::uint8_t polymarket_signature_type{};
  std::uint8_t reserved{};
  std::int64_t minimum_order_size{1};
  std::uint32_t polymarket_taker_delay_ms{};
};

enum class ReplayControl : std::uint8_t {
  Event = 0,
  Disconnect = 1,
  Reconnect = 2,
};

struct ReplayStep {
  std::uint64_t delay_ns{};
  ReplayControl control{ReplayControl::Event};
  VenueEvent event{};
};

using UpdateCallback = void (*)(void*, const RuntimeUpdate&) noexcept;

struct AdapterRuntimeConfig {
  // Adapters are externally owned and must outlive OmsApi. Construction is the
  // initialization control path for injecting credentials and transports.
  std::span<exchange::TradeAdapter* const> adapters{};
  bool enable_fake_fallback{true};
  std::uint32_t event_budget{64};
  // Drivers are externally owned and must outlive OmsApi. The OMS owner
  // serializes descriptor snapshots, readiness, and timeout callbacks with
  // adapter callbacks in both execution modes.
  std::span<exchange::AsyncIoDriver* const> io_drivers{};
  struct AccountRoute {
    AccountId account_id{};
    exchange::AdapterKind adapter{exchange::AdapterKind::Fake};
  };
  std::span<const AccountRoute> account_routes{};
};

struct AdapterStatusSnapshot {
  exchange::AdapterIdentity identity{};
  exchange::AdapterStatus status{exchange::AdapterStatus::Stopped};
  exchange::AdapterCapabilities capabilities{};
};

class OmsApi {
 public:
  static Result<std::unique_ptr<OmsApi>> Create(
      const RuntimeConfig& config, std::span<const InstrumentInit> instruments,
      std::span<const ReplayStep> replay = {});
  static Result<std::unique_ptr<OmsApi>> Create(
      const RuntimeConfig& config, std::span<const InstrumentInit> instruments,
      std::span<const ReplayStep> replay,
      const AdapterRuntimeConfig& adapters);

  ~OmsApi();
  OmsApi(const OmsApi&) = delete;
  OmsApi& operator=(const OmsApi&) = delete;

  Result<void> initialize_lane(std::uint32_t lane_id,
                               std::uint32_t session_epoch) noexcept;
  // Legacy registry-routed entry point. New integrations should prepare and
  // freeze routing from the MDS catalog before submission.
  Result<RequestToken> submit_order(std::uint32_t lane_id,
                                    NewOrderRequest request) noexcept;
  Result<RequestToken> submit_prepared_order(
      std::uint32_t lane_id, PreparedOrderRequest request) noexcept;
  Result<RequestToken> cancel_order(std::uint32_t lane_id,
                                    RequestToken target,
                                    OrderHandle handle = {}) noexcept;
  Result<RequestToken> rebind_polymarket_instrument(
      std::uint32_t lane_id,
      RebindPolymarketInstrumentRequest request) noexcept;
  Result<QueryToken> query_open_orders(std::uint32_t lane_id,
                                      AccountId account_id) noexcept;
  Result<QueryToken> query_positions(std::uint32_t lane_id,
                                    AccountId account_id) noexcept;

  // Inline only: timeout 0 polls, timeout >0 waits at most that many
  // milliseconds, and -1 waits indefinitely.
  Error service_io(int timeout_ms) noexcept;
  std::size_t drain_updates(std::uint32_t lane_id, UpdateCallback callback,
                            void* context,
                            std::size_t maximum = SIZE_MAX) noexcept;
  [[nodiscard]] int update_fd(std::uint32_t lane_id) const noexcept;
  [[nodiscard]] RuntimeMetrics metrics() const noexcept;
  [[nodiscard]] Result<AdapterStatusSnapshot> adapter_status(
      exchange::AdapterKind kind) const noexcept;
  Error reconcile(exchange::AdapterKind kind) noexcept;
  Error shutdown() noexcept;

  // Deterministic fake transport control path. Events are normalized OMS
  // VenueEvent values; no exchange wire protocol is accepted here.
  Error inject(const ReplayStep& step) noexcept;

 private:
  struct Impl;
  explicit OmsApi(std::unique_ptr<Impl> impl) noexcept;
  std::unique_ptr<Impl> impl_;
};

}  // namespace oms::api
