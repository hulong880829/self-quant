#pragma once

#include <array>
#include <cstddef>
#include <string>
#include <string_view>
#include <vector>

#include "oms/exchange/hyperliquid/protocol.h"
#include "oms/exchange/hyperliquid/session.h"
#include "oms/exchange/trade_adapter.h"

namespace oms::exchange::live {
class LiveTransport;
}

namespace oms::exchange::hyperliquid {

struct WireConfig {
  std::string_view private_key{};
  std::string_view account_address{};
  std::string_view vault_address{};
  std::string_view strategy_instance{};
  bool mainnet{true};
  live::LiveTransport* transport{};
};

class HyperliquidTradeAdapter final : public TradeAdapter {
 public:
  explicit HyperliquidTradeAdapter(std::string_view private_key);
  explicit HyperliquidTradeAdapter(const WireConfig& config);

  [[nodiscard]] AdapterIdentity identity() const noexcept override;
  [[nodiscard]] AdapterStatus status() const noexcept override;
  [[nodiscard]] AdapterCapabilities capabilities() const noexcept override;
  [[nodiscard]] AdapterResult reserve_command(
      AdapterCommandKind kind, AdapterReservation& reservation) noexcept override;
  void cancel_reservation(AdapterReservation reservation) noexcept override;
  [[nodiscard]] AdapterResult commit_place(
      AdapterReservation reservation,
      const AdapterPlaceCommand& command) noexcept override;
  [[nodiscard]] AdapterResult commit_cancel(
      AdapterReservation reservation,
      const AdapterCancelCommand& command) noexcept override;
  [[nodiscard]] AdapterResult commit_cancel_venue(
      AdapterReservation reservation,
      const AdapterCancelVenueCommand& command) noexcept override;
  [[nodiscard]] AdapterResult query_open_orders(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult query_positions(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult register_instrument(
      api::InstrumentId instrument_id,
      const api::ResolvedInstrument& routing) noexcept override;
  [[nodiscard]] bool instrument_ready(
      const api::ResolvedInstrument& routing) const noexcept override;
  [[nodiscard]] bool tracks_instrument_readiness() const noexcept override {
    return true;
  }
  [[nodiscard]] AdapterServiceResult service_io(
      std::uint64_t now_ns, std::uint32_t event_budget,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult on_deadline(
      const AdapterDeadline& deadline, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult begin_reconcile(
      std::uint64_t generation, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult shutdown(
      std::uint64_t now_ns, const AdapterEventSink& sink) noexcept override;

  [[nodiscard]] Session& session() noexcept { return session_; }
  [[nodiscard]] bool rest_healthy() const noexcept { return rest_healthy_; }
  [[nodiscard]] bool websocket_healthy() const noexcept {
    return websocket_healthy_;
  }
  [[nodiscard]] bool metadata_healthy() const noexcept {
    return metadata_healthy_;
  }
  [[nodiscard]] std::uint64_t connection_generation() const noexcept {
    return connection_generation_;
  }

 private:
  [[nodiscard]] bool queue_reject(const AdapterPlaceCommand& command,
                                  std::string_view reason) noexcept;
  [[nodiscard]] AdapterResult emit(const AdapterEventSink& sink) noexcept;
  [[nodiscard]] std::string_view state_address() const noexcept {
    return vault_address_.empty() ? std::string_view(account_address_)
                                  : std::string_view(vault_address_);
  }

  Session session_;
  live::LiveTransport* transport_{};
  std::string account_address_;
  std::string vault_address_;
  std::uint32_t strategy_instance_hash_{};
  bool mainnet_{true};
  bool reserved_{};
  bool rest_healthy_{};
  bool websocket_healthy_{};
  bool metadata_healthy_{};
  std::uint64_t generation_{1};
  std::uint64_t connection_generation_{1};
  std::uint64_t next_request_id_{1};
  std::array<AdapterEvent, 8> pending_{};
  std::size_t pending_count_{};
  struct Inflight {
    std::uint64_t request_id{};
    api::OrderHandle handle{};
    api::RequestToken token{};
    api::ClientOrderId client_order_id{};
    std::string cloid;
    bool cancel{};
  };

  enum class MetadataPhase : std::uint8_t {
    PerpDexs,
    DexMetadata,
    Complete,
  };

  std::string body_;
  std::array<Inflight, 8> inflight_{};
  std::size_t inflight_count_{};
  struct Binding {
    bool used{};
    std::string cloid;
    std::string oid;
    std::string coin;
    api::InstrumentId instrument_id{};
    api::OrderHandle handle{};
    api::RequestToken token{};
    api::ClientOrderId client_order_id{};
  };
  std::array<Binding, 64> bindings_{};
  struct RequiredInstrument {
    api::InstrumentId instrument_id{};
    api::VenueSymbol symbol{};
  };
  std::array<RequiredInstrument, 16> required_instruments_{};
  std::size_t required_instrument_count_{};
  struct QueryState {
    bool active{};
    std::uint64_t request_id{};
    api::QueryKind kind{api::QueryKind::OpenOrders};
    AdapterQueryRequest request{};
  };
  QueryState query_{};
  std::uint64_t metadata_request_id_{};
  MetadataPhase metadata_phase_{MetadataPhase::PerpDexs};
  std::vector<PerpDex> perp_dexs_;
  std::size_t metadata_dex_cursor_{};
  bool ws_started_{};
  bool ws_subscribed_{};
  bool orders_queried_{};
  bool positions_queried_{};
  bool orders_mapped_{};
  bool positions_mapped_{};
  std::uint64_t fill_catchup_request_id_{};
  std::uint64_t process_start_ms_{};
  std::uint64_t last_fill_time_ms_{};
  bool fill_catchup_complete_{};
};

}  // namespace oms::exchange::hyperliquid
