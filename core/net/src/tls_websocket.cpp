#include "net/tls_websocket.h"

#include <algorithm>
#include <cerrno>
#include <cctype>
#include <cstring>
#include <limits>
#include <openssl/err.h>
#include <sys/epoll.h>

namespace net {

void SslCtxDeleter::operator()(SSL_CTX *context) const noexcept {
  SSL_CTX_free(context);
}

SharedSslContext make_client_ssl_context(std::string &error) {
  SSL_CTX *raw = SSL_CTX_new(TLS_client_method());
  if (!raw) {
    error = "SSL_CTX_new failed";
    return {};
  }
  SharedSslContext context(raw, SslCtxDeleter{});
  SSL_CTX_set_min_proto_version(raw, TLS1_2_VERSION);
  SSL_CTX_set_verify(raw, SSL_VERIFY_PEER, nullptr);
  SSL_CTX_set_session_cache_mode(raw, SSL_SESS_CACHE_CLIENT);
  if (SSL_CTX_set_default_verify_paths(raw) != 1) {
    error = "system trust store is unavailable";
    return {};
  }
  return context;
}

bool validate_websocket_extensions(std::string_view headers,
                                   std::string &error) {
  const auto name_matches = [](std::string_view name,
                               std::string_view expected) noexcept {
    if (name.size() != expected.size()) {
      return false;
    }
    for (std::size_t index = 0; index < name.size(); ++index) {
      unsigned char value = static_cast<unsigned char>(name[index]);
      if (value >= 'A' && value <= 'Z') {
        value = static_cast<unsigned char>(value + ('a' - 'A'));
      }
      if (value != static_cast<unsigned char>(expected[index])) {
        return false;
      }
    }
    return true;
  };
  std::size_t position = 0;
  while (position < headers.size()) {
    const auto end = headers.find("\r\n", position);
    const auto line = headers.substr(
        position, end == std::string_view::npos ? headers.size() - position
                                                : end - position);
    const auto colon = line.find(':');
    if (colon != std::string_view::npos) {
      const auto name = line.substr(0, colon);
      if (name_matches(name, "sec-websocket-extensions")) {
        const auto value = line.substr(colon + 1);
        if (value.find("permessage-deflate") != std::string_view::npos) {
          error = "server negotiated forbidden permessage-deflate";
          return false;
        }
        if (value.find_first_not_of(" \t") != std::string_view::npos) {
          error = "server negotiated an unsupported WebSocket extension";
          return false;
        }
      }
    }
    if (end == std::string_view::npos) {
      break;
    }
    position = end + 2;
  }
  return true;
}

WebSocketParser::WebSocketParser(std::size_t capacity)
    : buffer_(capacity), message_buffer_(capacity) {}

void WebSocketParser::reset() noexcept {
  used_ = 0;
  message_used_ = 0;
  fragmented_opcode_ = WsOpcode::Continuation;
  fragmented_ = false;
  close_received_ = false;
}

bool WebSocketParser::feed(std::span<const std::byte> bytes,
                           const FrameCallback &callback, std::string &error) {
  if (close_received_ && !bytes.empty()) {
    error = "WebSocket data received after close";
    return false;
  }
  if (bytes.size() > buffer_.size() - used_) {
    error = "WebSocket receive buffer exhausted";
    return false;
  }
  std::memcpy(buffer_.data() + used_, bytes.data(), bytes.size());
  used_ += bytes.size();

  std::size_t consumed = 0;
  while (used_ - consumed >= 2) {
    const auto *p = reinterpret_cast<const std::uint8_t *>(buffer_.data()) +
                    consumed;
    const bool final = (p[0] & 0x80U) != 0;
    if ((p[0] & 0x70U) != 0) {
      error = "RSV bits require an unsupported extension";
      return false;
    }
    const auto opcode = static_cast<WsOpcode>(p[0] & 0x0fU);
    const auto opcode_value = static_cast<std::uint8_t>(opcode);
    if (opcode_value != 0x0U && opcode_value != 0x1U &&
        opcode_value != 0x2U && opcode_value != 0x8U &&
        opcode_value != 0x9U && opcode_value != 0xAU) {
      error = "reserved or unknown WebSocket opcode";
      return false;
    }
    const bool control = (p[0] & 0x08U) != 0;
    const bool masked = (p[1] & 0x80U) != 0;
    std::uint64_t payload_length = p[1] & 0x7fU;
    std::size_t header_length = 2;
    if (payload_length == 126) {
      if (used_ - consumed < 4) {
        break;
      }
      payload_length = (static_cast<std::uint64_t>(p[2]) << 8U) | p[3];
      header_length = 4;
    } else if (payload_length == 127) {
      if (used_ - consumed < 10) {
        break;
      }
      if ((p[2] & 0x80U) != 0) {
        error = "invalid WebSocket 64-bit payload length";
        return false;
      }
      payload_length = 0;
      for (int i = 2; i < 10; ++i) {
        payload_length = (payload_length << 8U) | p[i];
      }
      header_length = 10;
    }
    if (control && (!final || payload_length > 125)) {
      error = "invalid fragmented or oversized control frame";
      return false;
    }
    if (masked) {
      error = "masked server frame rejected";
      return false;
    }
    if (payload_length > buffer_.size() ||
        payload_length >
            std::numeric_limits<std::size_t>::max() - header_length) {
      error = "WebSocket frame exceeds configured capacity";
      return false;
    }
    const auto frame_length =
        header_length + static_cast<std::size_t>(payload_length);
    if (used_ - consumed < frame_length) {
      break;
    }
    const std::span<const std::byte> payload{
        buffer_.data() + consumed + header_length,
        static_cast<std::size_t>(payload_length)};

    if (control) {
      if (opcode == WsOpcode::Close && payload.size() == 1) {
        error = "invalid WebSocket close payload";
        return false;
      }
      if (!callback({true, opcode, payload})) {
        error = "frame callback rejected frame";
        return false;
      }
      if (opcode == WsOpcode::Close) {
        close_received_ = true;
      }
    } else if (opcode == WsOpcode::Continuation) {
      if (!fragmented_) {
        error = "unexpected WebSocket continuation frame";
        return false;
      }
      if (payload.size() > message_buffer_.size() - message_used_) {
        error = "fragmented WebSocket message exceeds configured capacity";
        return false;
      }
      std::memcpy(message_buffer_.data() + message_used_, payload.data(),
                  payload.size());
      message_used_ += payload.size();
      if (final) {
        const WsFrameView message{
            true, fragmented_opcode_,
            {message_buffer_.data(), message_used_}};
        if (!callback(message)) {
          error = "frame callback rejected message";
          return false;
        }
        fragmented_ = false;
        fragmented_opcode_ = WsOpcode::Continuation;
        message_used_ = 0;
      }
    } else {
      if (fragmented_) {
        error = "new WebSocket data frame during fragmented message";
        return false;
      }
      if (final) {
        if (!callback({true, opcode, payload})) {
          error = "frame callback rejected frame";
          return false;
        }
      } else {
        if (payload.size() > message_buffer_.size()) {
          error = "fragmented WebSocket message exceeds configured capacity";
          return false;
        }
        std::memcpy(message_buffer_.data(), payload.data(), payload.size());
        message_used_ = payload.size();
        fragmented_opcode_ = opcode;
        fragmented_ = true;
      }
    }
    consumed += frame_length;
    if (close_received_ && used_ - consumed != 0) {
      error = "WebSocket frame received after close";
      return false;
    }
  }
  if (consumed != 0) {
    std::memmove(buffer_.data(), buffer_.data() + consumed, used_ - consumed);
    used_ -= consumed;
  }
  return true;
}

TlsSession::TlsSession(SharedSslContext context, int socket_fd,
                       std::string_view host, std::size_t receive_capacity)
    : context_(std::move(context)), owned_receive_buffer_(receive_capacity),
      receive_buffer_(owned_receive_buffer_) {
  initialize(socket_fd, host);
}

TlsSession::TlsSession(SharedSslContext context, int socket_fd,
                       std::string_view host,
                       std::span<std::byte> receive_buffer)
    : context_(std::move(context)), receive_buffer_(receive_buffer) {
  initialize(socket_fd, host);
}

void TlsSession::initialize(int socket_fd, std::string_view host) noexcept {
  if (!context_ || host.empty() || host.size() >= 256 ||
      receive_buffer_.empty()) {
    return;
  }
  char host_name[256]{};
  std::memcpy(host_name, host.data(), host.size());
  ssl_ = SSL_new(context_.get());
  if (!ssl_) {
    return;
  }
  SSL_set_fd(ssl_, socket_fd);
  SSL_set_connect_state(ssl_);
  SSL_set_mode(ssl_, SSL_MODE_ENABLE_PARTIAL_WRITE |
                         SSL_MODE_ACCEPT_MOVING_WRITE_BUFFER);
  SSL_set_tlsext_host_name(ssl_, host_name);
  SSL_set1_host(ssl_, host_name);
}

TlsSession::~TlsSession() {
  if (ssl_) {
    SSL_free(ssl_);
  }
}

int TlsSession::capture_error(int result) noexcept {
  last_ssl_result_ = result;
  last_system_error_ = errno;
  last_ssl_error_ = ssl_ ? SSL_get_error(ssl_, result) : SSL_ERROR_SSL;
  last_openssl_error_ = ERR_peek_last_error();
  return last_ssl_error_ == SSL_ERROR_ZERO_RETURN ? 0 : -last_ssl_error_;
}

int TlsSession::handshake() noexcept {
  const int result = ssl_ ? SSL_connect(ssl_) : -1;
  if (result == 1) {
    last_ssl_result_ = result;
    last_ssl_error_ = SSL_ERROR_NONE;
    last_system_error_ = 0;
    last_openssl_error_ = 0;
    return result;
  }
  (void)capture_error(result);
  return result;
}

int TlsSession::read_some(std::span<const std::byte> &data) noexcept {
  data = {};
  if (!ssl_) {
    return -SSL_ERROR_SSL;
  }
  const auto length = static_cast<int>(
      std::min(receive_buffer_.size(),
               static_cast<std::size_t>(std::numeric_limits<int>::max())));
  const int result = SSL_read(ssl_, receive_buffer_.data(), length);
  if (result > 0) {
    last_ssl_result_ = result;
    last_ssl_error_ = SSL_ERROR_NONE;
    last_system_error_ = 0;
    last_openssl_error_ = 0;
    data = {receive_buffer_.data(),
            static_cast<std::size_t>(result)};
    return result;
  }
  return capture_error(result);
}

int TlsSession::write(std::span<const std::byte> data) noexcept {
  if (!ssl_) {
    return -SSL_ERROR_SSL;
  }
  if (data.empty()) {
    return 0;
  }
  const auto length = static_cast<int>(
      std::min(data.size(),
               static_cast<std::size_t>(std::numeric_limits<int>::max())));
  const int result = SSL_write(ssl_, data.data(), length);
  if (result > 0) {
    last_ssl_result_ = result;
    last_ssl_error_ = SSL_ERROR_NONE;
    last_system_error_ = 0;
    last_openssl_error_ = 0;
    return result;
  }
  return capture_error(result);
}

int TlsSession::wanted_events(int ssl_result) const noexcept {
  if (!ssl_) {
    return EPOLLERR;
  }
  // read_some/write return a stable negative SSL_ERROR_* value to callers,
  // while OpenSSL requires the exact SSL_* return value from the last call.
  (void)ssl_result;
  if (last_ssl_error_ == SSL_ERROR_WANT_READ) {
    return EPOLLIN;
  }
  if (last_ssl_error_ == SSL_ERROR_WANT_WRITE) {
    return EPOLLOUT;
  }
  return EPOLLERR;
}

std::string TlsSession::last_error_detail() const {
  std::string result = "ssl_error=" + std::to_string(last_ssl_error_);
  if (last_openssl_error_ != 0) {
    char message[256]{};
    ERR_error_string_n(last_openssl_error_, message, sizeof(message));
    result.append(" openssl=");
    result.append(message);
  }
  if (last_ssl_error_ == SSL_ERROR_SYSCALL && last_system_error_ != 0) {
    result.append(" errno=");
    result.append(std::to_string(last_system_error_));
    result.push_back('(');
    result.append(std::strerror(last_system_error_));
    result.push_back(')');
  }
  return result;
}

} // namespace net
