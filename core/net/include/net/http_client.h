#pragma once

#include "net/tcp_connector.h"
#include "net/tls_websocket.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string_view>
#include <vector>

namespace net {

enum class HttpStatusClass : std::uint8_t {
  Informational,
  Success,
  Redirect,
  ClientError,
  RateLimited,
  ServerError,
  Invalid
};

HttpStatusClass classify_http_status(unsigned status) noexcept;

enum class HttpParseError : std::uint8_t {
  None,
  InvalidResponse,
  HeaderTooLarge,
  BodyTooLarge
};

class HttpResponseParser {
public:
  HttpResponseParser(std::size_t header_capacity = 16U << 10U,
                     std::size_t body_capacity = 8U << 20U);

  bool feed(std::span<const std::byte> bytes,
            std::string_view &error) noexcept;
  bool finish(std::string_view &error) noexcept;
  void reset() noexcept;

  [[nodiscard]] bool headers_complete() const noexcept {
    return headers_complete_;
  }
  [[nodiscard]] bool complete() const noexcept { return complete_; }
  [[nodiscard]] unsigned status_code() const noexcept { return status_code_; }
  [[nodiscard]] HttpStatusClass status_class() const noexcept {
    return classify_http_status(status_code_);
  }
  [[nodiscard]] HttpParseError error_code() const noexcept {
    return parse_error_;
  }
  [[nodiscard]] std::span<const std::byte> body() const noexcept {
    return {body_.data(), body_used_};
  }
  [[nodiscard]] std::string_view header_value(
      std::string_view name) const noexcept;

private:
  enum class BodyMode : std::uint8_t {
    None,
    ContentLength,
    Chunked,
    UntilClose
  };
  enum class ChunkState : std::uint8_t { Size, Data, DataCrlf, Trailers };

  bool parse_headers(std::string_view &error) noexcept;
  bool parse_trailers(std::string_view &error) noexcept;
  bool feed_body(std::span<const std::byte> bytes,
                 std::string_view &error) noexcept;
  bool append_body(std::span<const std::byte> bytes,
                   std::string_view &error) noexcept;
  bool reject(HttpParseError code, std::string_view message,
              std::string_view &error) noexcept;

  std::vector<std::byte> headers_;
  std::vector<std::byte> body_;
  std::size_t header_used_{};
  std::size_t body_used_{};
  std::size_t content_remaining_{};
  std::size_t chunk_remaining_{};
  std::size_t chunk_line_used_{};
  unsigned status_code_{};
  BodyMode body_mode_{BodyMode::None};
  ChunkState chunk_state_{ChunkState::Size};
  HttpParseError parse_error_{HttpParseError::None};
  bool headers_complete_{};
  bool complete_{};
  char chunk_line_[32]{};
};

enum class HttpClientState : std::uint8_t {
  Idle,
  TcpConnecting,
  TlsHandshaking,
  SendingRequest,
  ReadingResponse,
  Complete,
  TimedOut,
  Failed
};

enum class HttpMethod : std::uint8_t { Get, Post, Put, Delete };

struct HttpHeader {
  std::string_view name;
  std::string_view value;
};

struct HttpRequest {
  HttpMethod method{HttpMethod::Get};
  std::string_view target;
  std::string_view content_type;
  std::span<const std::byte> body;
  std::span<const HttpHeader> headers;

  constexpr HttpRequest(
      HttpMethod request_method = HttpMethod::Get,
      std::string_view request_target = {},
      std::string_view request_content_type = {},
      std::span<const std::byte> request_body = {},
      std::span<const HttpHeader> request_headers = {}) noexcept
      : method(request_method), target(request_target),
        content_type(request_content_type), body(request_body),
        headers(request_headers) {}
};

enum class HttpClientError : std::uint8_t {
  None,
  InvalidRequest,
  RequestTooLarge,
  Connect,
  Tls,
  Transport,
  InvalidResponse,
  ResponseTooLarge,
  Timeout
};

bool encode_http_request(std::string_view host, const HttpRequest &request,
                         std::span<std::byte> output,
                         std::size_t &output_size,
                         std::size_t max_custom_headers = 32) noexcept;

class HttpClient {
public:
  using Clock = std::chrono::steady_clock;

  HttpClient(SharedSslContext context,
             std::size_t header_capacity = 16U << 10U,
             std::size_t body_capacity = 8U << 20U,
             std::size_t request_capacity = 8U << 10U,
             std::size_t host_capacity = 255,
             std::size_t tls_receive_capacity = 1U << 20U);

  bool start_get(std::string_view host, std::string_view service,
                 std::string_view target, Clock::time_point deadline) noexcept;
  bool start_post(std::string_view host, std::string_view service,
                  std::string_view target, std::string_view content_type,
                  std::span<const std::byte> body,
                  Clock::time_point deadline) noexcept;
  bool start_put(std::string_view host, std::string_view service,
                 std::string_view target, std::string_view content_type,
                 std::span<const std::byte> body,
                 Clock::time_point deadline) noexcept;
  bool start_delete(std::string_view host, std::string_view service,
                    std::string_view target,
                    Clock::time_point deadline) noexcept;
  bool start_request(std::string_view host, std::string_view service,
                     const HttpRequest &request,
                     Clock::time_point deadline) noexcept;
  HttpClientState on_event(std::uint32_t events,
                           Clock::time_point now = Clock::now()) noexcept;
  HttpClientState check_timeout(Clock::time_point now = Clock::now()) noexcept;
  void reset() noexcept;
  void set_socket_options(SocketOptions options) noexcept {
    connector_.set_socket_options(options);
  }

  [[nodiscard]] int fd() const noexcept { return connector_.fd(); }
  [[nodiscard]] std::uint64_t socket_generation() const noexcept {
    return connector_.socket_generation();
  }
  [[nodiscard]] std::uint32_t wanted_events() const noexcept;
  [[nodiscard]] HttpClientState state() const noexcept { return state_; }
  [[nodiscard]] const HttpResponseParser &response() const noexcept {
    return parser_;
  }
  [[nodiscard]] std::string_view error_message() const noexcept {
    return error_;
  }
  [[nodiscard]] HttpClientError error_code() const noexcept {
    return error_code_;
  }

private:
  bool begin_tls() noexcept;
  void drive_tls() noexcept;
  void drive_write() noexcept;
  void drive_read() noexcept;
  void fail(HttpClientError code, std::string_view error) noexcept;

  SharedSslContext context_;
  TcpConnector connector_;
  std::optional<TlsSession> tls_;
  HttpResponseParser parser_;
  std::vector<std::byte> request_;
  std::vector<std::byte> tls_receive_;
  std::vector<char> host_;
  std::size_t request_size_{};
  std::size_t request_offset_{};
  Clock::time_point deadline_{};
  HttpClientState state_{HttpClientState::Idle};
  std::uint32_t wanted_events_{};
  HttpClientError error_code_{HttpClientError::None};
  std::string_view error_{};
};

} // namespace net
