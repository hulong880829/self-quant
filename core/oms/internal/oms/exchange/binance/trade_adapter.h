#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <string_view>

#include "oms/exchange/binance/protocol.h"
#include "oms/exchange/trade_adapter.h"

namespace oms::exchange::binance {

struct ResolvedCancel {
  std::array<char, kMaxSymbolBytes> symbol{};
  std::uint8_t symbol_length{};
  api::ClientOrderId client_order_id{};
  api::VenueOrderId venue_order_id{};
};

struct TransportRequest {
  std::uint32_t id{};
  HttpRequest wire{};
  TradingRequest trading{};
  bool use_trading_websocket{};
};

enum class TransportEventKind : std::uint8_t {
  Response = 1,
  Failure = 2,
  SessionReady = 3,
  SessionLost = 4,
  UserMessage = 5,
  TradingSessionReady = 6,
  TradingSessionLost = 7,
};

struct TransportEvent {
  TransportEventKind kind{TransportEventKind::Failure};
  std::uint32_t request_id{};
  AdapterResult result{AdapterResult::Failed};
  RateLimitMetadata metadata{};
  std::string_view payload{};
};

class Transport {
 public:
  virtual ~Transport() = default;
  // WouldBlock means the request was not accepted and may be submitted again.
  // Any other result consumes the submission attempt.
  [[nodiscard]] virtual AdapterResult submit(
      const TransportRequest& request) noexcept = 0;
  // Response payload views remain valid only until the next poll call.
  [[nodiscard]] virtual AdapterResult poll(TransportEvent& event) noexcept = 0;
  [[nodiscard]] virtual AdapterResult start_user_stream(
      std::string_view listen_key) noexcept {
    (void)listen_key;
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult start_trading_stream() noexcept {
    return AdapterResult::Unsupported;
  }
  virtual void close() noexcept = 0;
};

struct AdapterCallbacks {
  void* context{};
  bool (*resolve_symbol)(void*, api::InstrumentId, char*, std::size_t,
                         std::uint8_t&) noexcept{};
  bool (*resolve_cancel)(void*, api::OrderHandle, ResolvedCancel&) noexcept{};
  bool (*resolve_stream)(void*, std::string_view, ParseContext&) noexcept{};
};

struct BinanceAdapterConfig {
  Product product{Product::Spot};
  CredentialsView credentials{};
  AdapterCallbacks callbacks{};
  Transport* transport{};
  std::uint64_t request_timeout_ns{5'000'000'000ULL};
  std::uint32_t recv_window_ms{5000};
  std::uint8_t price_scale{8};
  std::uint8_t quantity_scale{8};
};

class BinanceTradeAdapter final : public TradeAdapter {
 public:
  static constexpr std::size_t kSendSlotCount = 8;
  static constexpr std::size_t kReceiveSlotCount = 16;
  static constexpr std::size_t kOrderBindingCount = 4096;
  static constexpr std::uint64_t kReconnectInitialBackoffNs =
      1'000'000'000ULL;
  static constexpr std::uint64_t kReconnectMaximumBackoffNs =
      32'000'000'000ULL;
  static constexpr std::uint8_t kMaximumReconnectAttempts = 6;

  explicit BinanceTradeAdapter(BinanceAdapterConfig config) noexcept;

  [[nodiscard]] AdapterIdentity identity() const noexcept override;
  [[nodiscard]] AdapterStatus status() const noexcept override {
    return status_;
  }
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

  // User-stream transport is owned by the caller. Correlation is supplied
  // after looking up the client/venue order id in the owning order table.
  [[nodiscard]] AdapterResult ingest_user_stream(
      std::string_view json, const ParseContext& context) noexcept;
  [[nodiscard]] ListenKeyState listen_key_state() const noexcept {
    return listen_key_.state();
  }
  [[nodiscard]] std::size_t pending_commands() const noexcept;
  [[nodiscard]] std::size_t pending_events() const noexcept {
    return receive_count_;
  }

 private:
  enum class SlotState : std::uint8_t {
    Free,
    Reserved,
    Queued,
    InFlight,
  };
  struct SendSlot {
    AdapterCommand command{};
    TransportRequest request{};
    ParseContext parse_context{};
    std::uint64_t generation{};
    std::uint64_t deadline_ns{};
    SlotState state{SlotState::Free};
    AdapterCommandKind reserved_kind{AdapterCommandKind::Place};
    bool request_built{};
  };

  enum class ControlKind : std::uint8_t {
    None,
    ListenKey,
    Reconcile,
    TimeSync,
    QueryOrder,
  };
  struct ControlSlot {
    TransportRequest request{};
    std::uint64_t deadline_ns{};
    std::uint64_t reconcile_generation{};
    std::uint64_t local_send_ms{};
    std::uint32_t binding_index{
        std::numeric_limits<std::uint32_t>::max()};
    ListenKeyAction listen_key_action{ListenKeyAction::None};
    ControlKind kind{ControlKind::None};
  };
  struct OrderBinding {
    bool used{};
    api::OrderHandle handle{};
    api::RequestToken token{};
    std::array<char, kMaxSymbolBytes> symbol{};
    std::uint8_t symbol_length{};
    api::ClientOrderId client_order_id{};
    api::VenueOrderId venue_order_id{};
    bool uncertain{};
  };

  [[nodiscard]] bool valid_reservation(
      AdapterReservation reservation, AdapterCommandKind kind) const noexcept;
  void release(std::size_t index) noexcept;
  void refresh_backpressure() noexcept;
  [[nodiscard]] AdapterResult queue_event(const AdapterEvent& event) noexcept;
  [[nodiscard]] AdapterResult process_one(std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult submit_listen_key(ListenKeyAction action,
                                                std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult submit_reconcile(std::uint64_t generation,
                                               std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult submit_time_sync(std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult submit_query_order(
      std::size_t binding_index, std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult process_transport_event(
      const TransportEvent& event, std::uint64_t now_ns) noexcept;
  [[nodiscard]] SendSlot* find_request(std::uint32_t request_id) noexcept;
  [[nodiscard]] OrderBinding* find_binding(api::OrderHandle handle) noexcept;
  [[nodiscard]] OrderBinding* find_binding(
      const api::ClientOrderId& client_order_id,
      const api::VenueOrderId& venue_order_id) noexcept;
  [[nodiscard]] std::size_t next_uncertain_binding() const noexcept;
  [[nodiscard]] bool bind_place(
      api::OrderHandle handle, std::string_view symbol,
      const api::ClientOrderId& client_order_id,
      api::RequestToken token) noexcept;
  [[nodiscard]] bool resolve_stream_context(
      std::string_view json, ParseContext& context) noexcept;
  [[nodiscard]] std::uint32_t next_request_id() noexcept;
  [[nodiscard]] std::uint64_t request_deadline(
      std::uint64_t now_ns) const noexcept;
  [[nodiscard]] AdapterResult fail_command(std::size_t index,
                                           AdapterResult reason,
                                           bool uncertain_place = false) noexcept;
  [[nodiscard]] AdapterResult fail_control(AdapterResult reason,
                                           std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult continue_reconcile(
      const AdapterEventSink& sink, std::uint32_t event_budget,
      std::uint32_t& events_processed) noexcept;
  [[nodiscard]] AdapterResult finish_reconcile(
      AdapterResult result, const AdapterEventSink& sink,
      std::uint32_t& events_processed) noexcept;
  void schedule_reconnect(std::uint64_t now_ns) noexcept;
  void reset_reconnect() noexcept;
  void start_reconcile_generation() noexcept;
  [[nodiscard]] AdapterResult emit_immediate(
      const AdapterEvent& event, const AdapterEventSink& sink) noexcept;

  BinanceAdapterConfig config_;
  RequestBuilder builder_;
  ServerClock clock_;
  ListenKeySession listen_key_;
  std::array<SendSlot, kSendSlotCount> send_{};
  ControlSlot control_{};
  std::array<AdapterEvent, kReceiveSlotCount> receive_{};
  std::array<OrderBinding, kOrderBindingCount> bindings_{};
  std::array<char, kMaxResponseBytes> response_buffer_{};
  std::size_t receive_head_{};
  std::size_t receive_count_{};
  std::size_t next_send_{};
  std::uint32_t next_transport_request_id_{1};
  std::uint64_t next_generation_{1};
  std::uint64_t reconcile_generation_{};
  OpenOrdersCursor reconcile_cursor_{};
  std::uint32_t reconcile_response_size_{};
  bool reconcile_snapshot_complete_{};
  std::uint64_t next_reconnect_generation_{
      std::numeric_limits<std::uint64_t>::max()};
  std::uint64_t reconnect_deadline_ns_{};
  std::uint64_t rate_limit_until_ns_{};
  std::uint8_t reconnect_attempts_{};
  ListenKeyAction lifecycle_action_{ListenKeyAction::None};
  bool stream_reconcile_required_{};
  bool user_session_ready_{};
  bool trading_session_ready_{};
  bool trading_start_required_{};
  bool time_sync_required_{true};
  AdapterStatus status_{AdapterStatus::Stopped};
};

using SpotTradeAdapter = BinanceTradeAdapter;
using UsdmTradeAdapter = BinanceTradeAdapter;

}  // namespace oms::exchange::binance
