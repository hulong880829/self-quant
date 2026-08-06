#pragma once

#include "mds/network/tcp_connector.h"
#include "mds/network/tls_websocket.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string_view>
#include <vector>

namespace mds::network {

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

class HttpResponseParser {
public:
  HttpResponseParser(std::size_t header_capacity = 16U << 10U,
                     std::size_t body_capacity = 8U << 20U);

  bool feed(std::span<const std::byte> bytes,
            std::string_view &error) noexcept;
  void reset() noexcept;

  [[nodiscard]] bool headers_complete() const noexcept {
    return headers_complete_;
  }
  [[nodiscard]] bool complete() const noexcept { return complete_; }
  [[nodiscard]] unsigned status_code() const noexcept { return status_code_; }
  [[nodiscard]] HttpStatusClass status_class() const noexcept {
    return classify_http_status(status_code_);
  }
  [[nodiscard]] std::span<const std::byte> body() const noexcept {
    return {body_.data(), body_used_};
  }

private:
  enum class BodyMode : std::uint8_t { None, ContentLength, Chunked };
  enum class ChunkState : std::uint8_t { Size, Data, DataCrlf, Trailers };

  bool parse_headers(std::string_view &error) noexcept;
  bool feed_body(std::span<const std::byte> bytes,
                 std::string_view &error) noexcept;
  bool append_body(std::span<const std::byte> bytes,
                   std::string_view &error) noexcept;

  std::vector<std::byte> headers_;
  std::vector<std::byte> body_;
  std::size_t header_used_{};
  std::size_t body_used_{};
  std::size_t content_remaining_{};
  std::size_t chunk_remaining_{};
  std::size_t chunk_line_used_{};
  std::size_t trailer_match_{};
  unsigned status_code_{};
  BodyMode body_mode_{BodyMode::None};
  ChunkState chunk_state_{ChunkState::Size};
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
  HttpClientState on_event(std::uint32_t events,
                           Clock::time_point now = Clock::now()) noexcept;
  HttpClientState check_timeout(Clock::time_point now = Clock::now()) noexcept;
  void reset() noexcept;

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

private:
  bool begin_tls() noexcept;
  void drive_tls() noexcept;
  void drive_write() noexcept;
  void drive_read() noexcept;
  void fail(std::string_view error) noexcept;

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
  std::string_view error_{};
};

} // namespace mds::network
