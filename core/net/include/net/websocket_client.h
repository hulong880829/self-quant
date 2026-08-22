#pragma once

#include "net/tcp_connector.h"
#include "net/tls_websocket.h"
#include "net/websocket_codec.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace net {

enum class WebSocketClientState : std::uint8_t {
  Idle,
  TcpConnecting,
  TlsHandshaking,
  SendingUpgrade,
  ReadingUpgrade,
  Open,
  Closing,
  Closed,
  TimedOut,
  Failed
};

class WebSocketClient {
public:
  using Clock = std::chrono::steady_clock;
  using FrameCallback = WebSocketParser::FrameCallback;

  WebSocketClient(SharedSslContext context,
                  std::size_t message_capacity = 1U << 20U,
                  std::size_t header_capacity = 16U << 10U,
                  std::size_t request_capacity = 8U << 10U,
                  std::size_t host_capacity = 255,
                  std::size_t tls_receive_capacity = 1U << 20U);

  bool start(std::string_view host, std::string_view service,
             std::string_view target, Clock::time_point deadline) noexcept;
  WebSocketClientState
  on_event(std::uint32_t events,
           Clock::time_point now = Clock::now()) noexcept;
  WebSocketClientState
  check_timeout(Clock::time_point now = Clock::now()) noexcept;
  void reset() noexcept;
  void set_origin(std::string origin) { origin_ = std::move(origin); }
  void set_secure(bool secure) noexcept { secure_ = secure; }
  void set_socket_options(SocketOptions options) noexcept {
    connector_.set_socket_options(options);
  }

  bool send(WsOpcode opcode, std::span<const std::byte> payload,
            std::string_view &error) noexcept;
  bool ping(std::span<const std::byte> payload,
            std::string_view &error) noexcept;
  bool close(std::uint16_t code, std::string_view reason,
             std::string_view &error) noexcept;
  void set_frame_callback(FrameCallback callback);

  [[nodiscard]] int fd() const noexcept { return connector_.fd(); }
  [[nodiscard]] std::uint64_t socket_generation() const noexcept {
    return connector_.socket_generation();
  }
  [[nodiscard]] std::uint32_t wanted_events() const noexcept;
  [[nodiscard]] WebSocketClientState state() const noexcept { return state_; }
  [[nodiscard]] bool can_send_data() const noexcept {
    return state_ == WebSocketClientState::Open && writer_.empty();
  }
  [[nodiscard]] std::string_view error_message() const noexcept {
    return error_;
  }
  [[nodiscard]] std::optional<std::uint16_t> close_code() const noexcept {
    return close_code_;
  }
  [[nodiscard]] std::string_view close_reason() const noexcept {
    return close_reason_;
  }

private:
  bool begin_tls() noexcept;
  void drive_tls() noexcept;
  void drive_upgrade_write() noexcept;
  void drive_read() noexcept;
  void drive_frame_writes() noexcept;
  bool process_frame(const WsFrameView &frame) noexcept;
  bool queue_control(WsOpcode opcode,
                     std::span<const std::byte> payload) noexcept;
  void update_open_events() noexcept;
  void fail(std::string_view error) noexcept;
  void fail_tls(std::string_view error) noexcept;
  void fail_socket(std::string_view error, int error_number) noexcept;
  void set_close_detail(std::span<const std::byte> payload);

  SharedSslContext context_;
  TcpConnector connector_;
  std::optional<TlsSession> tls_;
  WebSocketUpgradeParser upgrade_parser_;
  WebSocketParser parser_;
  WebSocketFrameWriter writer_;
  WebSocketFrameWriter control_writer_;
  std::vector<std::byte> request_;
  std::vector<std::byte> tls_receive_;
  std::vector<char> host_;
  std::string origin_;
  std::vector<std::byte> close_payload_;
  FrameCallback callback_;
  std::size_t request_size_{};
  std::size_t request_offset_{};
  char client_key_[25]{};
  Clock::time_point deadline_{};
  WebSocketClientState state_{WebSocketClientState::Idle};
  std::uint32_t wanted_events_{};
  std::string_view error_{};
  std::string parser_error_;
  std::string error_storage_;
  std::string close_reason_;
  std::optional<std::uint16_t> close_code_;
  bool close_sent_{};
  bool close_received_{};
  bool secure_{true};
};

} // namespace net
