#pragma once

#include <openssl/ssl.h>

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace net {

struct SslCtxDeleter {
  void operator()(SSL_CTX *context) const noexcept;
};
using SharedSslContext = std::shared_ptr<SSL_CTX>;

SharedSslContext make_client_ssl_context(std::string &error);
bool validate_websocket_extensions(std::string_view response_headers,
                                   std::string &error);

enum class WsOpcode : std::uint8_t {
  Continuation = 0x0,
  Text = 0x1,
  Binary = 0x2,
  Close = 0x8,
  Ping = 0x9,
  Pong = 0xA
};

struct WsFrameView {
  bool final{};
  WsOpcode opcode{};
  std::span<const std::byte> payload{};
};

class WebSocketParser {
public:
  using FrameCallback = std::function<bool(const WsFrameView &)>;

  explicit WebSocketParser(std::size_t capacity = 1U << 20U);
  bool feed(std::span<const std::byte> bytes, const FrameCallback &callback,
            std::string &error);
  void reset() noexcept;
  [[nodiscard]] std::size_t capacity() const noexcept {
    return buffer_.size();
  }
  [[nodiscard]] bool close_received() const noexcept { return close_received_; }

private:
  std::vector<std::byte> buffer_;
  std::vector<std::byte> message_buffer_;
  std::size_t used_{};
  std::size_t message_used_{};
  WsOpcode fragmented_opcode_{WsOpcode::Continuation};
  bool fragmented_{};
  bool close_received_{};
};

class TlsSession {
public:
  TlsSession(SharedSslContext context, int socket_fd, std::string_view host,
             std::size_t receive_capacity = 1U << 20U);
  TlsSession(SharedSslContext context, int socket_fd, std::string_view host,
             std::span<std::byte> receive_buffer);
  ~TlsSession();
  TlsSession(const TlsSession &) = delete;
  TlsSession &operator=(const TlsSession &) = delete;

  int handshake() noexcept;
  int read_some(std::span<const std::byte> &data) noexcept;
  int write(std::span<const std::byte> data) noexcept;
  [[nodiscard]] int wanted_events(int ssl_result) const noexcept;
  [[nodiscard]] std::string last_error_detail() const;
  [[nodiscard]] SSL *native_handle() noexcept { return ssl_; }

private:
  void initialize(int socket_fd, std::string_view host) noexcept;
  int capture_error(int result) noexcept;

  SharedSslContext context_;
  SSL *ssl_{nullptr};
  std::vector<std::byte> owned_receive_buffer_;
  std::span<std::byte> receive_buffer_;
  int last_ssl_result_{};
  int last_ssl_error_{};
  int last_system_error_{};
  unsigned long last_openssl_error_{};
};

} // namespace net
