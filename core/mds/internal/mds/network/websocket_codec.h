#pragma once

#include "mds/network/tls_websocket.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string_view>
#include <vector>

namespace mds::network {

bool websocket_accept_value(std::string_view client_key,
                            std::span<char, 29> output) noexcept;
bool validate_websocket_upgrade(std::string_view response_headers,
                                std::string_view client_key,
                                std::string_view &error) noexcept;

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

class WebSocketFrameWriter {
public:
  explicit WebSocketFrameWriter(std::size_t payload_capacity = 1U << 20U);

  bool prepare(WsOpcode opcode, std::span<const std::byte> payload,
               bool final, std::string_view &error) noexcept;
  bool prepare_with_mask(WsOpcode opcode, std::span<const std::byte> payload,
                         bool final, std::uint32_t mask,
                         std::string_view &error) noexcept;
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

} // namespace mds::network
