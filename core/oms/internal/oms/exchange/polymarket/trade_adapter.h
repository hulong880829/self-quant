#pragma once

#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <string_view>

#include "oms/exchange/polymarket/crypto.h"
#include "oms/exchange/polymarket/protocol.h"
#include "oms/exchange/trade_adapter.h"
#include "oms/instrument_registry.h"

namespace oms::exchange::polymarket {

template <std::size_t Capacity>
struct FixedText {
  std::array<char, Capacity> value{};
  std::uint16_t size{};

  [[nodiscard]] std::string_view view() const noexcept {
    return {value.data(), size};
  }

  [[nodiscard]] bool assign(std::string_view source) noexcept {
    if (source.size() > value.size()) return false;
    std::copy(source.begin(), source.end(), value.begin());
    size = static_cast<std::uint16_t>(source.size());
    return true;
  }
};

struct Credentials {
  FixedText<42> signer_address{};
  FixedText<42> funder_address{};
  FixedText<66> private_key{};
  FixedText<128> api_key{};
  FixedText<256> api_secret{};
  FixedText<128> passphrase{};
};

struct TransportRequest {
  std::uint32_t id{};
  WireRequest wire{};
  FixedText<42> poly_address{};
  FixedText<45> poly_signature{};
  FixedText<24> poly_timestamp{};
  FixedText<128> poly_api_key{};
  FixedText<128> poly_passphrase{};
};

enum class TransportEventKind : std::uint8_t {
  SessionReady = 1,
  SessionLost = 2,
  HttpResponse = 3,
  UserMessage = 4,
};

struct TransportEvent {
  TransportEventKind kind{TransportEventKind::SessionLost};
  std::uint32_t request_id{};
  std::uint16_t status_code{};
  std::string_view payload{};
};

class Transport {
 public:
  virtual ~Transport() = default;
  [[nodiscard]] virtual AdapterResult submit(
      const TransportRequest& request) noexcept = 0;
  [[nodiscard]] virtual AdapterResult poll(TransportEvent& event) noexcept = 0;
  [[nodiscard]] virtual AdapterResult request_reconnect() noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult send_heartbeat() noexcept {
    return AdapterResult::Unsupported;
  }
  virtual void close() noexcept = 0;
};

struct AdapterConfig {
  InstrumentRegistry* instruments{};
  Transport* transport{};
  Transport* data_transport{};
  Credentials credentials{};
  std::uint64_t (*now_ms)(void*) noexcept{};
  void* clock_context{};
  std::uint64_t request_timeout_ns{5'000'000'000ULL};
  std::uint64_t reconnect_initial_ns{250'000'000ULL};
  std::uint64_t reconnect_max_ns{8'000'000'000ULL};
  std::uint64_t heartbeat_interval_ns{10'000'000'000ULL};
  std::uint64_t liveness_timeout_ns{25'000'000'000ULL};
};

class PolymarketTradeAdapter final : public TradeAdapter {
 public:
  static constexpr std::size_t kCommandCapacity = 16;
  static constexpr std::size_t kOrderCapacity = 64;
  static constexpr std::size_t kPendingCapacity = kMaximumOpenOrders + 8;

  explicit PolymarketTradeAdapter(const AdapterConfig& config) noexcept;

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
  [[nodiscard]] AdapterResult validate_rebind(
      const api::RebindPolymarketInstrumentRequest& request) const
      noexcept override;
  [[nodiscard]] AdapterResult apply_rebind(
      const api::RebindPolymarketInstrumentRequest& request) noexcept override;
  [[nodiscard]] AdapterResult query_open_orders(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult query_positions(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
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

 private:
  struct CommandSlot {
    std::uint64_t generation{};
    bool reserved{};
    bool inflight{};
    AdapterCommandKind kind{AdapterCommandKind::Place};
    std::uint32_t request_id{};
    std::uint64_t command_id{};
    std::uint64_t deadline_ns{};
    api::OrderHandle handle{};
    api::RequestToken token{};
    api::VenueOrderId venue_order_id{};
    api::InstrumentId instrument_id{};
  };

  struct OrderBinding {
    bool used{};
    bool reconcile_seen{};
    api::OrderHandle handle{};
    api::VenueOrderId venue_order_id{};
    api::InstrumentId instrument_id{};
  };

  [[nodiscard]] bool valid(AdapterReservation reservation,
                           AdapterCommandKind kind) const noexcept;
  void release(std::size_t slot) noexcept;
  void refresh_backpressure() noexcept;
  [[nodiscard]] AdapterResult authorize(TransportRequest& request) noexcept;
  [[nodiscard]] AdapterResult submit_reconcile_page() noexcept;
  [[nodiscard]] AdapterResult submit_query_page() noexcept;
  [[nodiscard]] AdapterResult offer(const AdapterEventSink& sink,
                                    const AdapterEvent& event) noexcept;
  [[nodiscard]] AdapterResult queue(const AdapterEvent& event) noexcept;
  [[nodiscard]] CommandSlot* find_request(std::uint32_t request_id) noexcept;
  [[nodiscard]] const api::VenueOrderId* find_order(
      api::OrderHandle handle) const noexcept;
  void bind_order(api::OrderHandle handle,
                  const api::VenueOrderId& venue_order_id,
                  api::InstrumentId instrument_id) noexcept;

  AdapterConfig config_{};
  OrderSigningContext signing_context_;
  AdapterStatus status_{AdapterStatus::Connecting};
  std::array<CommandSlot, kCommandCapacity> commands_{};
  std::array<OrderBinding, kOrderCapacity> orders_{};
  std::uint32_t next_request_id_{1};
  Pagination pagination_{};
  std::uint64_t reconcile_generation_{};
  std::uint32_t reconcile_request_id_{};
  std::uint64_t reconcile_deadline_ns_{};
  bool reconcile_uncertain_{};
  struct QueryState {
    bool active{};
    api::QueryKind kind{api::QueryKind::OpenOrders};
    AdapterQueryRequest request{};
    Pagination pagination{};
    std::uint32_t request_id{};
    std::uint64_t deadline_ns{};
  } query_{};
  std::array<AdapterEvent, kPendingCapacity> pending_events_{};
  std::uint16_t pending_begin_{};
  std::uint16_t pending_size_{};
  AdapterStatus resume_status_{AdapterStatus::Connecting};
  bool reconnect_reconcile_required_{};
  std::uint64_t next_reconnect_generation_{
      std::numeric_limits<std::uint64_t>::max()};
  std::uint64_t reconnect_due_ns_{};
  std::uint64_t reconnect_delay_ns_{};
  std::uint64_t heartbeat_due_ns_{};
  std::uint64_t last_session_activity_ns_{};
};

}  // namespace oms::exchange::polymarket
