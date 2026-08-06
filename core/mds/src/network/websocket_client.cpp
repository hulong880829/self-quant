#include "mds/network/websocket_client.h"

#include <algorithm>
#include <cstring>
#include <openssl/evp.h>
#include <openssl/rand.h>
#include <sys/epoll.h>

namespace mds::network {

WebSocketClient::WebSocketClient(SharedSslContext context,
                                 std::size_t message_capacity,
                                 std::size_t header_capacity,
                                 std::size_t request_capacity,
                                 std::size_t host_capacity,
                                 std::size_t tls_receive_capacity)
    : context_(std::move(context)), upgrade_parser_(header_capacity),
      parser_(message_capacity), writer_(message_capacity),
      control_writer_(125), request_(request_capacity),
      tls_receive_(tls_receive_capacity), host_(host_capacity + 1),
      close_payload_(125) {}

void WebSocketClient::reset() noexcept {
  tls_.reset();
  connector_.reset();
  upgrade_parser_.reset();
  parser_.reset();
  writer_.reset();
  control_writer_.reset();
  request_size_ = 0;
  request_offset_ = 0;
  client_key_[0] = '\0';
  state_ = WebSocketClientState::Idle;
  wanted_events_ = 0;
  error_ = {};
  parser_error_.clear();
  close_sent_ = false;
  close_received_ = false;
}

void WebSocketClient::set_frame_callback(FrameCallback callback) {
  callback_ = std::move(callback);
}

void WebSocketClient::fail(std::string_view error) noexcept {
  state_ = WebSocketClientState::Failed;
  wanted_events_ = 0;
  error_ = error;
}

bool WebSocketClient::start(std::string_view host, std::string_view service,
                            std::string_view target,
                            Clock::time_point deadline) noexcept {
  reset();
  if (!context_ || host.empty() || host.size() + 1 > host_.size() ||
      target.empty()) {
    fail("invalid WebSocket endpoint");
    return false;
  }
  std::memcpy(host_.data(), host.data(), host.size());
  host_[host.size()] = '\0';
  unsigned char nonce[16]{};
  if (RAND_bytes(nonce, sizeof(nonce)) != 1 ||
      EVP_EncodeBlock(reinterpret_cast<unsigned char *>(client_key_), nonce,
                      sizeof(nonce)) != 24) {
    fail("failed to generate WebSocket client key");
    return false;
  }
  client_key_[24] = '\0';
  const auto append = [this](std::string_view value) noexcept {
    if (value.size() > request_.size() - request_size_) {
      return false;
    }
    std::memcpy(request_.data() + request_size_, value.data(), value.size());
    request_size_ += value.size();
    return true;
  };
  if (!append("GET ") || !append(target) || !append(" HTTP/1.1\r\nHost: ") ||
      !append(host) ||
      !append("\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
              "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: ") ||
      !append(client_key_) ||
      !append("\r\nUser-Agent: self-quant-mds/1\r\n\r\n")) {
    fail("WebSocket upgrade request exceeds configured capacity");
    return false;
  }
  deadline_ = deadline;
  if (!connector_.start(host, service, deadline)) {
    fail(connector_.error_message());
    return false;
  }
  if (connector_.state() == ConnectState::Connected) {
    return begin_tls();
  }
  state_ = WebSocketClientState::TcpConnecting;
  wanted_events_ = connector_.wanted_events();
  return true;
}

bool WebSocketClient::begin_tls() noexcept {
  tls_.emplace(context_, connector_.fd(), std::string_view(host_.data()),
               std::span<std::byte>(tls_receive_));
  if (!tls_->native_handle()) {
    fail("failed to create WebSocket TLS session");
    return false;
  }
  state_ = WebSocketClientState::TlsHandshaking;
  drive_tls();
  return state_ != WebSocketClientState::Failed;
}

void WebSocketClient::drive_tls() noexcept {
  const int result = tls_->handshake();
  if (result == 1) {
    state_ = WebSocketClientState::SendingUpgrade;
    wanted_events_ = EPOLLOUT | EPOLLERR | EPOLLHUP;
    drive_upgrade_write();
  } else {
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail("WebSocket TLS handshake failed");
    }
  }
}

void WebSocketClient::drive_upgrade_write() noexcept {
  while (request_offset_ < request_size_) {
    const int result = tls_->write(
        {request_.data() + request_offset_, request_size_ - request_offset_});
    if (result > 0) {
      request_offset_ += static_cast<std::size_t>(result);
      continue;
    }
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail("WebSocket upgrade write failed");
    }
    return;
  }
  state_ = WebSocketClientState::ReadingUpgrade;
  wanted_events_ = EPOLLIN | EPOLLERR | EPOLLHUP;
}

bool WebSocketClient::queue_control(
    WsOpcode opcode, std::span<const std::byte> payload) noexcept {
  std::string_view control_error;
  if (!control_writer_.prepare(opcode, payload, true, control_error)) {
    fail(control_error);
    return false;
  }
  return true;
}

bool WebSocketClient::process_frame(const WsFrameView &frame) noexcept {
  if (frame.opcode == WsOpcode::Ping) {
    if (!queue_control(WsOpcode::Pong, frame.payload)) {
      return false;
    }
  } else if (frame.opcode == WsOpcode::Close) {
    close_received_ = true;
    if (!close_sent_) {
      if (!queue_control(WsOpcode::Close, frame.payload)) {
        return false;
      }
      close_sent_ = true;
    }
    state_ = WebSocketClientState::Closing;
  }
  return !callback_ || callback_(frame);
}

void WebSocketClient::drive_read() noexcept {
  for (;;) {
    std::span<const std::byte> bytes;
    const int result = tls_->read_some(bytes);
    if (result > 0) {
      if (state_ == WebSocketClientState::ReadingUpgrade) {
        std::size_t consumed = 0;
        if (!upgrade_parser_.feed(bytes, client_key_, consumed, error_)) {
          fail(error_);
          return;
        }
        if (!upgrade_parser_.complete()) {
          continue;
        }
        state_ = WebSocketClientState::Open;
        if (consumed < bytes.size() &&
            !parser_.feed(bytes.subspan(consumed),
                          [this](const WsFrameView &frame) {
                            return process_frame(frame);
                          },
                          parser_error_)) {
          fail(parser_error_);
          return;
        }
      } else if (!parser_.feed(
                     bytes,
                     [this](const WsFrameView &frame) {
                       return process_frame(frame);
                     },
                     parser_error_)) {
        fail(parser_error_);
        return;
      }
      continue;
    }
    if (result == 0) {
      if (close_received_) {
        state_ = WebSocketClientState::Closed;
        wanted_events_ = 0;
      } else {
        fail("WebSocket TLS connection closed unexpectedly");
      }
      return;
    }
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail("WebSocket read failed");
      return;
    }
    update_open_events();
    return;
  }
}

void WebSocketClient::drive_frame_writes() noexcept {
  WebSocketFrameWriter *active =
      !writer_.empty() ? &writer_
                       : (!control_writer_.empty() ? &control_writer_ : nullptr);
  while (active) {
    const int result = tls_->write(active->pending());
    if (result > 0) {
      active->consume(static_cast<std::size_t>(result));
      active = !writer_.empty()
                   ? &writer_
                   : (!control_writer_.empty() ? &control_writer_ : nullptr);
      continue;
    }
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail("WebSocket frame write failed");
    }
    return;
  }
  if (state_ == WebSocketClientState::Closing && close_received_ &&
      close_sent_) {
    state_ = WebSocketClientState::Closed;
    wanted_events_ = 0;
  } else {
    update_open_events();
  }
}

void WebSocketClient::update_open_events() noexcept {
  if (state_ != WebSocketClientState::Open &&
      state_ != WebSocketClientState::Closing) {
    return;
  }
  wanted_events_ = EPOLLIN | EPOLLERR | EPOLLHUP;
  if (!writer_.empty() || !control_writer_.empty()) {
    wanted_events_ |= EPOLLOUT;
  }
}

bool WebSocketClient::send(WsOpcode opcode,
                           std::span<const std::byte> payload,
                           std::string_view &error) noexcept {
  if (state_ != WebSocketClientState::Open ||
      (opcode != WsOpcode::Text && opcode != WsOpcode::Binary)) {
    error = "WebSocket is not open for data frames";
    return false;
  }
  if (!writer_.prepare(opcode, payload, true, error)) {
    return false;
  }
  update_open_events();
  return true;
}

bool WebSocketClient::ping(std::span<const std::byte> payload,
                           std::string_view &error) noexcept {
  if (state_ != WebSocketClientState::Open) {
    error = "WebSocket is not open";
    return false;
  }
  if (!control_writer_.prepare(WsOpcode::Ping, payload, true, error)) {
    return false;
  }
  update_open_events();
  return true;
}

bool WebSocketClient::close(std::uint16_t code, std::string_view reason,
                            std::string_view &error) noexcept {
  if (state_ != WebSocketClientState::Open || reason.size() > 123) {
    error = "invalid WebSocket close request";
    return false;
  }
  close_payload_[0] = static_cast<std::byte>((code >> 8U) & 0xffU);
  close_payload_[1] = static_cast<std::byte>(code & 0xffU);
  std::memcpy(close_payload_.data() + 2, reason.data(), reason.size());
  if (!control_writer_.prepare(
          WsOpcode::Close,
          {close_payload_.data(), reason.size() + 2}, true, error)) {
    return false;
  }
  close_sent_ = true;
  state_ = WebSocketClientState::Closing;
  update_open_events();
  return true;
}

WebSocketClientState
WebSocketClient::check_timeout(Clock::time_point now) noexcept {
  if (state_ != WebSocketClientState::Idle &&
      state_ != WebSocketClientState::Open &&
      state_ != WebSocketClientState::Closing &&
      state_ != WebSocketClientState::Closed &&
      state_ != WebSocketClientState::Failed && now >= deadline_) {
    state_ = WebSocketClientState::TimedOut;
    wanted_events_ = 0;
    error_ = "WebSocket operation timed out";
  }
  return state_;
}

WebSocketClientState WebSocketClient::on_event(std::uint32_t events,
                                               Clock::time_point now) noexcept {
  if (check_timeout(now) == WebSocketClientState::TimedOut) {
    return state_;
  }
  if (state_ == WebSocketClientState::TcpConnecting) {
    const ConnectState connected = connector_.on_event(events, now);
    if (connected == ConnectState::Connected) {
      begin_tls();
    } else if (connected == ConnectState::Failed ||
               connected == ConnectState::TimedOut) {
      fail(connector_.error_message());
    } else {
      wanted_events_ = connector_.wanted_events();
    }
  } else if (state_ == WebSocketClientState::TlsHandshaking) {
    drive_tls();
  } else if (state_ == WebSocketClientState::SendingUpgrade) {
    drive_upgrade_write();
  } else if (state_ == WebSocketClientState::ReadingUpgrade) {
    drive_read();
  } else if (state_ == WebSocketClientState::Open ||
             state_ == WebSocketClientState::Closing) {
    if ((events & (EPOLLIN | EPOLLERR | EPOLLHUP)) != 0) {
      drive_read();
    }
    if ((state_ == WebSocketClientState::Open ||
         state_ == WebSocketClientState::Closing) &&
        (!writer_.empty() || !control_writer_.empty()) &&
        (events & (EPOLLOUT | EPOLLIN)) != 0) {
      drive_frame_writes();
    }
  }
  return state_;
}

std::uint32_t WebSocketClient::wanted_events() const noexcept {
  return wanted_events_;
}

} // namespace mds::network
