#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string_view>

#include "oms/api/order_types.h"
#include "oms/exchange/adapter_types.h"

namespace oms::exchange::binance {

inline constexpr std::size_t kMaxRequestBytes = 2048;
inline constexpr std::size_t kMaxResponseBytes = 16384;
inline constexpr std::size_t kMaxSymbolBytes = 32;
inline constexpr std::size_t kMaxListenKeyBytes = 256;

enum class Product : std::uint8_t { Spot = 1, Usdm = 2 };
enum class HttpMethod : std::uint8_t { Get = 1, Post = 2, Put = 3, Delete = 4 };
enum class ParseResult : std::uint8_t {
  Ok = 0,
  Malformed = 1,
  Oversize = 2,
  DuplicateField = 3,
  Unsupported = 4,
  MissingField = 5,
  InvalidValue = 6
};
enum class ErrorClass : std::uint8_t {
  None = 0,
  TimestampSkew = 1,
  RateLimited = 2,
  Banned = 3,
  VenueReject = 4,
  Transport = 5,
  Unknown = 6
};
enum class RetryDisposition : std::uint8_t {
  Never = 0,
  Safe = 1,
  ReconcileBeforeRetry = 2
};

struct Profile {
  Product product{Product::Spot};
  std::string_view rest_host{};
  std::string_view websocket_host{};
  std::string_view place_path{};
  std::string_view cancel_path{};
  std::string_view open_orders_path{};
  std::string_view listen_key_path{};
  std::string_view time_path{};
  AdapterCapabilities capabilities{};
};

[[nodiscard]] const Profile& profile(Product product) noexcept;

struct CredentialsView {
  std::string_view api_key{};
  std::string_view secret_key{};
};

struct HttpRequest {
  HttpMethod method{HttpMethod::Get};
  std::array<char, kMaxRequestBytes> target{};
  std::uint16_t target_length{};
  std::array<char, 64> api_key_header_name{};
  std::uint8_t api_key_header_name_length{};
  std::string_view api_key_value{};
  bool signed_request{};

  [[nodiscard]] std::string_view target_view() const noexcept {
    return {target.data(), target_length};
  }
  [[nodiscard]] std::string_view api_key_header() const noexcept {
    return {api_key_header_name.data(), api_key_header_name_length};
  }
};

struct TradingRequest {
  std::array<char, kMaxRequestBytes> payload{};
  std::uint16_t payload_length{};

  [[nodiscard]] std::string_view payload_view() const noexcept {
    return {payload.data(), payload_length};
  }
};

struct PlaceParameters {
  std::string_view symbol{};
  api::Side side{api::Side::Buy};
  api::OrderType type{api::OrderType::Limit};
  api::TimeInForce time_in_force{api::TimeInForce::GTC};
  std::uint16_t flags{};
  api::FixedPoint quantity{};
  api::FixedPoint price{};
  std::string_view client_order_id{};
  std::uint64_t expire_time_ns{};
};

struct CancelParameters {
  std::string_view symbol{};
  std::string_view client_order_id{};
  std::string_view venue_order_id{};
};

class RequestBuilder {
 public:
  explicit RequestBuilder(Product product) noexcept : product_(product) {}

  [[nodiscard]] bool place(const PlaceParameters& parameters,
                           std::uint64_t timestamp_ms,
                           std::uint32_t recv_window_ms,
                           CredentialsView credentials,
                           HttpRequest& output) const noexcept;
  [[nodiscard]] bool cancel(const CancelParameters& parameters,
                            std::uint64_t timestamp_ms,
                            std::uint32_t recv_window_ms,
                            CredentialsView credentials,
                            HttpRequest& output) const noexcept;
  [[nodiscard]] bool trading_place(const PlaceParameters& parameters,
                                   std::uint32_t request_id,
                                   std::uint64_t timestamp_ms,
                                   std::uint32_t recv_window_ms,
                                   CredentialsView credentials,
                                   TradingRequest& output) const noexcept;
  [[nodiscard]] bool trading_cancel(const CancelParameters& parameters,
                                    std::uint32_t request_id,
                                    std::uint64_t timestamp_ms,
                                    std::uint32_t recv_window_ms,
                                    CredentialsView credentials,
                                    TradingRequest& output) const noexcept;
  [[nodiscard]] bool open_orders(std::string_view symbol,
                                 std::uint64_t timestamp_ms,
                                 std::uint32_t recv_window_ms,
                                 CredentialsView credentials,
                                 HttpRequest& output) const noexcept;
  [[nodiscard]] bool query_order(const CancelParameters& parameters,
                                 std::uint64_t timestamp_ms,
                                 std::uint32_t recv_window_ms,
                                 CredentialsView credentials,
                                 HttpRequest& output) const noexcept;
  [[nodiscard]] bool create_listen_key(CredentialsView credentials,
                                       HttpRequest& output) const noexcept;
  [[nodiscard]] bool keepalive_listen_key(CredentialsView credentials,
                                          std::string_view listen_key,
                                          HttpRequest& output) const noexcept;
  [[nodiscard]] bool close_listen_key(CredentialsView credentials,
                                      std::string_view listen_key,
                                      HttpRequest& output) const noexcept;
  [[nodiscard]] bool server_time(HttpRequest& output) const noexcept;

 private:
  Product product_;
};

[[nodiscard]] bool hmac_sha256_hex(std::string_view key,
                                   std::string_view message,
                                   std::array<char, 64>& output) noexcept;

struct RateLimitMetadata {
  std::uint32_t http_status{};
  std::uint64_t retry_after_ms{};
  std::uint64_t used_weight_1m{};
  bool has_retry_after{};
  bool has_used_weight{};
};

struct VenueError {
  std::int32_t code{};
  std::array<char, 256> message{};
  std::uint16_t message_length{};
  ErrorClass classification{ErrorClass::None};
  RateLimitMetadata rate_limit{};
};

struct ParseContext {
  api::OrderHandle handle{};
  api::RequestToken token{};
  std::uint8_t price_scale{8};
  std::uint8_t quantity_scale{8};
};

struct OpenOrdersCursor {
  std::uint32_t offset{};
  std::uint16_t page_count{};
  std::uint16_t order_count{};
  bool complete{};
};

inline constexpr std::uint16_t kMaxOpenOrders = 256;
inline constexpr std::uint16_t kMaxOpenOrderPages = kMaxOpenOrders + 1;

[[nodiscard]] ParseResult parse_rest_place(
    std::string_view json, const ParseContext& context,
    api::VenueEvent& output) noexcept;
[[nodiscard]] ParseResult parse_rest_cancel(
    std::string_view json, const ParseContext& context,
    api::VenueEvent& output) noexcept;
[[nodiscard]] ParseResult parse_trading_response(
    std::string_view json, std::uint32_t& request_id,
    std::uint32_t& status, std::string_view& payload) noexcept;
[[nodiscard]] ParseResult parse_open_orders_page(
    std::string_view json, OpenOrdersCursor& cursor, api::VenueEvent* output,
    std::size_t capacity, std::size_t& output_count) noexcept;
[[nodiscard]] ParseResult parse_order_query(
    std::string_view json, api::VenueEvent& output) noexcept;
[[nodiscard]] ParseResult parse_error(std::string_view json,
                                      std::uint32_t http_status,
                                      RateLimitMetadata metadata,
                                      VenueError& output) noexcept;
[[nodiscard]] ParseResult parse_spot_execution_report(
    std::string_view json, const ParseContext& context,
    api::VenueEvent& output) noexcept;
[[nodiscard]] ParseResult parse_usdm_order_trade_update(
    std::string_view json, const ParseContext& context,
    api::VenueEvent& output) noexcept;
[[nodiscard]] ParseResult parse_listen_key(
    std::string_view json,
    std::array<char, kMaxListenKeyBytes>& output,
    std::uint16_t& output_length) noexcept;
[[nodiscard]] ParseResult parse_server_time(std::string_view json,
                                            std::uint64_t& output_ms) noexcept;

[[nodiscard]] constexpr ErrorClass classify_error(std::int32_t code,
                                                  std::uint32_t status) noexcept {
  if (code == -1021) return ErrorClass::TimestampSkew;
  if (status == 429) return ErrorClass::RateLimited;
  if (status == 418) return ErrorClass::Banned;
  if (code != 0) return ErrorClass::VenueReject;
  return status >= 400 ? ErrorClass::Unknown : ErrorClass::None;
}

[[nodiscard]] constexpr RetryDisposition retry_disposition(
    AdapterCommandKind kind, ErrorClass error) noexcept {
  if (kind == AdapterCommandKind::Place)
    return error == ErrorClass::TimestampSkew
               ? RetryDisposition::ReconcileBeforeRetry
               : RetryDisposition::Never;
  return error == ErrorClass::RateLimited || error == ErrorClass::TimestampSkew ||
                 error == ErrorClass::Transport
             ? RetryDisposition::Safe
             : RetryDisposition::Never;
}

class ServerClock {
 public:
  void observe(std::uint64_t local_send_ms, std::uint64_t local_receive_ms,
               std::uint64_t server_ms) noexcept;
  [[nodiscard]] std::uint64_t venue_time_ms(
      std::uint64_t local_ms) const noexcept;
  [[nodiscard]] std::int64_t offset_ms() const noexcept { return offset_ms_; }
  [[nodiscard]] bool synchronized() const noexcept { return synchronized_; }

 private:
  std::int64_t offset_ms_{};
  bool synchronized_{};
};

enum class ListenKeyState : std::uint8_t {
  Empty = 0,
  Creating = 1,
  Active = 2,
  KeepalivePending = 3,
  RecreateRequired = 4,
  Closing = 5
};
enum class ListenKeyAction : std::uint8_t {
  None = 0,
  Create = 1,
  Keepalive = 2,
  ReconnectStream = 3,
  Close = 4
};

class ListenKeySession {
 public:
  static constexpr std::uint64_t kKeepaliveIntervalNs =
      30ULL * 60ULL * 1000ULL * 1000ULL * 1000ULL;
  static constexpr std::uint64_t kExpiryNs =
      60ULL * 60ULL * 1000ULL * 1000ULL * 1000ULL;

  [[nodiscard]] ListenKeyAction start() noexcept;
  [[nodiscard]] bool activated(std::string_view key,
                               std::uint64_t now_ns) noexcept;
  [[nodiscard]] ListenKeyAction on_deadline(std::uint64_t now_ns) noexcept;
  void keepalive_succeeded(std::uint64_t now_ns) noexcept;
  void request_failed() noexcept;
  [[nodiscard]] ListenKeyAction shutdown() noexcept;
  [[nodiscard]] ListenKeyState state() const noexcept { return state_; }
  [[nodiscard]] std::uint64_t next_deadline_ns() const noexcept {
    return next_deadline_ns_;
  }
  [[nodiscard]] std::string_view key() const noexcept {
    return {key_.data(), key_length_};
  }

 private:
  ListenKeyState state_{ListenKeyState::Empty};
  std::array<char, kMaxListenKeyBytes> key_{};
  std::uint16_t key_length_{};
  std::uint64_t activated_ns_{};
  std::uint64_t next_deadline_ns_{};
};

}  // namespace oms::exchange::binance
