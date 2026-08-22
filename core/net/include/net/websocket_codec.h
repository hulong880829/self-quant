#pragma once

#include "net/tls_websocket.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string_view>
#include <vector>

namespace net {

bool websocket_accept_value(std::string_view client_key,
                            std::span<char, 29> output) noexcept;
bool validate_websocket_upgrade(std::string_view response_headers,
                                std::string_view client_key,
                                std::string_view &error) noexcept;
bool prepare_websocket_upgrade_response(
    std::string_view client_key, std::span<std::byte> output,
    std::size_t &size, std::string_view &error) noexcept;

class WebSocketUpgradeParser {
public:
  explicit WebSocketUpgradeParser(std::size_t header_capacity = 16U << 10U);

  bool feed(std::span<const std::byte> bytes, std::string_view client_key,
            std::size_t &consumed, std::string_view &error) noexcept;
  void reset() noexcept;
  [[nodiscard]] bool complete() const noexcept { return complete_; }
  [[nodiscard]] std::string_view headers() const noexcept;

private:
  std::vector<std::byte> buffer_;
  std::size_t used_{};
  bool complete_{};
};

class WebSocketUpgradeRequestParser {
public:
  explicit WebSocketUpgradeRequestParser(
      std::size_t header_capacity = 16U << 10U);

  bool feed(std::span<const std::byte> bytes, std::string_view expected_path,
            std::string_view expected_bearer_token, std::size_t &consumed,
            std::string_view &error) noexcept;
  void reset() noexcept;
  [[nodiscard]] bool complete() const noexcept { return complete_; }
  [[nodiscard]] std::string_view client_key() const noexcept {
    return client_key_;
  }
  [[nodiscard]] std::string_view request_target() const noexcept {
    return request_target_;
  }

private:
  bool validate(std::string_view expected_path,
                std::string_view expected_bearer_token,
                std::string_view &error) noexcept;

  std::vector<std::byte> buffer_;
  std::size_t used_{};
  std::string_view client_key_{};
  std::string_view request_target_{};
  bool complete_{};
};

class WebSocketFrameWriter {
public:
  explicit WebSocketFrameWriter(std::size_t payload_capacity = 1U << 20U);

  bool prepare(WsOpcode opcode, std::span<const std::byte> payload,
               bool final, std::string_view &error) noexcept;
  bool prepare_with_mask(WsOpcode opcode, std::span<const std::byte> payload,
                         bool final, std::uint32_t mask,
                         std::string_view &error) noexcept;
  bool prepare_unmasked(WsOpcode opcode, std::span<const std::byte> payload,
                        bool final, std::string_view &error) noexcept;
  void consume(std::size_t bytes) noexcept;
  void reset() noexcept;

  [[nodiscard]] std::span<const std::byte> pending() const noexcept;
  [[nodiscard]] bool empty() const noexcept { return offset_ == size_; }
  [[nodiscard]] std::size_t payload_capacity() const noexcept {
    return buffer_.size() >= 14 ? buffer_.size() - 14 : 0;
  }

private:
  std::vector<std::byte> buffer_;
  std::size_t offset_{};
  std::size_t size_{};
};

class WebSocketServerParser {
public:
  using FrameCallback = WebSocketParser::FrameCallback;

  explicit WebSocketServerParser(std::size_t capacity = 1U << 20U);
  bool feed(std::span<const std::byte> bytes, const FrameCallback &callback,
            std::string &error);
  void reset() noexcept;
  [[nodiscard]] std::size_t capacity() const noexcept {
    return message_buffer_.size();
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

} // namespace net
