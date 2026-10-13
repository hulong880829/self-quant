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

namespace oms::exchange::polymarket {

void SecureWipe(void* data, std::size_t size) noexcept;

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

  void secure_clear() noexcept {
    SecureWipe(value.data(), value.size());
    size = 0;
  }

  [[nodiscard]] bool secure_assign(std::string_view source) noexcept {
    secure_clear();
    if (source.size() > value.size()) return false;
    std::copy(source.begin(), source.end(), value.begin());
    size = static_cast<std::uint16_t>(source.size());
    return true;
  }
};

struct Credentials {
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

struct ClobAuthRequest {
  std::uint32_t id{};
  WireRequest wire{};
  FixedText<42> poly_address{};
  FixedText<132> poly_signature{};
  FixedText<24> poly_timestamp{};
  FixedText<8> poly_nonce{};
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
  bool auth_rejected{};
};

class Transport {
 public:
  virtual ~Transport() = default;
  [[nodiscard]] virtual AdapterResult submit(
      const TransportRequest& request) noexcept = 0;
  [[nodiscard]] virtual AdapterResult poll(TransportEvent& event) noexcept = 0;
  [[nodiscard]] virtual AdapterResult submit_clob_auth(
      const ClobAuthRequest&) noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult set_user_subscription(
      std::string_view) noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult request_reconnect() noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] virtual AdapterResult send_heartbeat() noexcept {
    return AdapterResult::Unsupported;
  }
  virtual void close() noexcept = 0;
};

struct AdapterConfig {
  void* instrument_context{};
  api::InstrumentId (*resolve_polymarket_token)(
      void*, const std::array<std::uint8_t, 32>&) noexcept{};
  Transport* transport{};
  Transport* data_transport{};
  Credentials credentials{};
  api::PolymarketSignatureType signature_type{
      api::PolymarketSignatureType::Unspecified};
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
  ~PolymarketTradeAdapter() noexcept override;
  [[nodiscard]] AdapterResult start_initial_session() noexcept;

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
  enum class CredentialPhase : std::uint8_t {
    None = 0,
    Deriving,
    Creating,
    FinalDerivingAfterCreate,
  };
  enum class PostCreateProbeReason : std::uint8_t {
    None = 0,
    CreateRejected,
    CreateAmbiguous,
  };

  void ClearDerivedCredentials() noexcept;
  void WipeUserSubscription() noexcept;
  [[nodiscard]] AdapterResult SubmitCredentialRequest(bool create) noexcept;
  [[nodiscard]] AdapterResult BeginCredentialDerive() noexcept;
  [[nodiscard]] AdapterResult BeginCredentialCreate() noexcept;
  [[nodiscard]] AdapterResult RetryCurrentCredential() noexcept;
  [[nodiscard]] AdapterResult FailCredential(
      const char* reason, std::uint16_t http_status,
      std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult ApplyCredentialSuccess(
      std::string_view payload, std::uint64_t now_ns) noexcept;
  void ScheduleCredentialRetry(std::uint64_t now_ns) noexcept;
  [[nodiscard]] AdapterResult StartInternalReconcile(
      std::uint64_t now_ns) noexcept;

  AdapterConfig config_{};
  OrderSigningContext signing_context_;
  FixedText<42> derived_signer_text_{};
  FixedText<42> effective_funder_text_{};
  Address effective_funder_binary_{};
  AdapterStatus status_{AdapterStatus::Authenticating};
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
  AdapterStatus resume_status_{AdapterStatus::Authenticating};
  bool initial_connect_started_{};
  bool session_ever_ready_{};
  bool reconnect_reconcile_required_{};
  std::uint64_t next_reconnect_generation_{
      std::numeric_limits<std::uint64_t>::max()};
  std::uint64_t reconnect_due_ns_{};
  std::uint64_t reconnect_delay_ns_{};
  std::uint64_t heartbeat_due_ns_{};
  std::uint64_t last_session_activity_ns_{};
  bool auth_refresh_attempted_{};
  CredentialPhase credential_phase_{CredentialPhase::None};
  PostCreateProbeReason post_create_probe_reason_{
      PostCreateProbeReason::None};
  bool credential_inflight_{};
  std::uint32_t credential_request_id_{};
  std::uint64_t credential_deadline_ns_{};
  std::uint64_t credential_retry_due_ns_{};
  std::uint64_t credential_retry_delay_ns_{};
};

}  // namespace oms::exchange::polymarket
