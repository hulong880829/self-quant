#include "mds/network/websocket_codec.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <limits>
#include <openssl/evp.h>
#include <openssl/rand.h>
#include <openssl/sha.h>

namespace mds::network {
namespace {

constexpr std::string_view websocket_guid =
    "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

bool ascii_iequals(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t i = 0; i < left.size(); ++i) {
    unsigned char a = static_cast<unsigned char>(left[i]);
    unsigned char b = static_cast<unsigned char>(right[i]);
    if (a >= 'A' && a <= 'Z') {
      a = static_cast<unsigned char>(a + ('a' - 'A'));
    }
    if (b >= 'A' && b <= 'Z') {
      b = static_cast<unsigned char>(b + ('a' - 'A'));
    }
    if (a != b) {
      return false;
    }
  }
  return true;
}

std::string_view trim(std::string_view value) noexcept {
  while (!value.empty() && (value.front() == ' ' || value.front() == '\t')) {
    value.remove_prefix(1);
  }
  while (!value.empty() && (value.back() == ' ' || value.back() == '\t')) {
    value.remove_suffix(1);
  }
  return value;
}

} // namespace

bool websocket_accept_value(std::string_view client_key,
                            std::span<char, 29> output) noexcept {
  if (client_key.size() > 128) {
    return false;
  }
  std::array<unsigned char, 128 + websocket_guid.size()> input{};
  std::memcpy(input.data(), client_key.data(), client_key.size());
  std::memcpy(input.data() + client_key.size(), websocket_guid.data(),
              websocket_guid.size());
  std::array<unsigned char, SHA_DIGEST_LENGTH> digest{};
  if (!SHA1(input.data(), client_key.size() + websocket_guid.size(),
            digest.data())) {
    return false;
  }
  const int encoded =
      EVP_EncodeBlock(reinterpret_cast<unsigned char *>(output.data()),
                      digest.data(), static_cast<int>(digest.size()));
  if (encoded != 28) {
    return false;
  }
  output[28] = '\0';
  return true;
}

bool validate_websocket_upgrade(std::string_view headers,
                                std::string_view client_key,
                                std::string_view &error) noexcept {
  const auto first_end = headers.find("\r\n");
  const auto status_line = headers.substr(0, first_end);
  const auto first_space = status_line.find(' ');
  if (first_space == std::string_view::npos ||
      status_line.substr(0, first_space) != "HTTP/1.1") {
    error = "invalid WebSocket HTTP status line";
    return false;
  }
  std::size_t code_begin = first_space + 1;
  while (code_begin < status_line.size() && status_line[code_begin] == ' ') {
    ++code_begin;
  }
  if (code_begin + 3 > status_line.size() ||
      status_line.substr(code_begin, 3) != "101" ||
      (code_begin + 3 < status_line.size() &&
       status_line[code_begin + 3] != ' ')) {
    error = "WebSocket upgrade did not return 101";
    return false;
  }

  std::array<char, 29> expected{};
  if (!websocket_accept_value(client_key, expected)) {
    error = "failed to compute Sec-WebSocket-Accept";
    return false;
  }
  bool found_accept = false;
  bool seen_accept = false;
  bool found_upgrade = false;
  bool found_connection = false;
  std::size_t position =
      first_end == std::string_view::npos ? headers.size() : first_end + 2;
  while (position < headers.size()) {
    const auto end = headers.find("\r\n", position);
    const auto line =
        headers.substr(position, end == std::string_view::npos
                                      ? headers.size() - position
                                      : end - position);
    if (line.empty()) {
      break;
    }
    const auto colon = line.find(':');
    if (colon != std::string_view::npos) {
      const auto name = trim(line.substr(0, colon));
      const auto value = trim(line.substr(colon + 1));
      if (ascii_iequals(name, "Sec-WebSocket-Accept")) {
        if (seen_accept) {
          error = "duplicate Sec-WebSocket-Accept";
          return false;
        }
        seen_accept = true;
        found_accept =
            value == std::string_view(expected.data(), expected.size() - 1);
      } else if (ascii_iequals(name, "Sec-WebSocket-Extensions")) {
        if (!value.empty()) {
          error = "server negotiated a forbidden WebSocket extension";
          return false;
        }
      } else if (ascii_iequals(name, "Upgrade")) {
        found_upgrade = ascii_iequals(value, "websocket");
      } else if (ascii_iequals(name, "Connection")) {
        std::size_t token_position = 0;
        while (token_position < value.size()) {
          const auto comma = value.find(',', token_position);
          const auto token = trim(value.substr(
              token_position, comma == std::string_view::npos
                                  ? value.size() - token_position
                                  : comma - token_position));
          found_connection =
              found_connection || ascii_iequals(token, "upgrade");
          if (comma == std::string_view::npos) {
            break;
          }
          token_position = comma + 1;
        }
      }
    }
    if (end == std::string_view::npos) {
      break;
    }
    position = end + 2;
  }
  if (!found_accept) {
    error = "invalid or missing Sec-WebSocket-Accept";
    return false;
  }
  if (!found_upgrade || !found_connection) {
    error = "missing WebSocket upgrade headers";
    return false;
  }
  error = {};
  return true;
}

WebSocketUpgradeParser::WebSocketUpgradeParser(std::size_t header_capacity)
    : buffer_(header_capacity) {}

void WebSocketUpgradeParser::reset() noexcept {
  used_ = 0;
  complete_ = false;
}

std::string_view WebSocketUpgradeParser::headers() const noexcept {
  return {reinterpret_cast<const char *>(buffer_.data()), used_};
}

bool WebSocketUpgradeParser::feed(std::span<const std::byte> bytes,
                                  std::string_view client_key,
                                  std::size_t &consumed,
                                  std::string_view &error) noexcept {
  consumed = 0;
  if (complete_) {
    return true;
  }
  for (const std::byte byte : bytes) {
    if (used_ == buffer_.size()) {
      error = "WebSocket upgrade headers exceed configured capacity";
      return false;
    }
    buffer_[used_++] = byte;
    ++consumed;
    if (used_ >= 4 && buffer_[used_ - 4] == std::byte{'\r'} &&
        buffer_[used_ - 3] == std::byte{'\n'} &&
        buffer_[used_ - 2] == std::byte{'\r'} &&
        buffer_[used_ - 1] == std::byte{'\n'}) {
      complete_ = validate_websocket_upgrade(headers(), client_key, error);
      return complete_;
    }
  }
  return true;
}

WebSocketFrameWriter::WebSocketFrameWriter(std::size_t payload_capacity)
    : buffer_(payload_capacity + 14) {}

void WebSocketFrameWriter::reset() noexcept {
  offset_ = 0;
  size_ = 0;
}

bool WebSocketFrameWriter::prepare(WsOpcode opcode,
                                   std::span<const std::byte> payload,
                                   bool final,
                                   std::string_view &error) noexcept {
  std::uint32_t mask{};
  if (RAND_bytes(reinterpret_cast<unsigned char *>(&mask), sizeof(mask)) != 1) {
    error = "failed to generate WebSocket mask";
    return false;
  }
  return prepare_with_mask(opcode, payload, final, mask, error);
}

bool WebSocketFrameWriter::prepare_with_mask(
    WsOpcode opcode, std::span<const std::byte> payload, bool final,
    std::uint32_t mask, std::string_view &error) noexcept {
  if (!empty()) {
    error = "WebSocket write already pending";
    return false;
  }
  const auto opcode_value = static_cast<std::uint8_t>(opcode);
  const bool control = (opcode_value & 0x08U) != 0;
  if (control && (!final || payload.size() > 125)) {
    error = "invalid WebSocket control frame";
    return false;
  }
  if (payload.size() > payload_capacity()) {
    error = "WebSocket payload exceeds configured capacity";
    return false;
  }
  std::size_t header = 2;
  buffer_[0] =
      static_cast<std::byte>((final ? 0x80U : 0U) | (opcode_value & 0x0fU));
  if (payload.size() <= 125) {
    buffer_[1] = static_cast<std::byte>(0x80U | payload.size());
  } else if (payload.size() <= std::numeric_limits<std::uint16_t>::max()) {
    buffer_[1] = std::byte{0xFE};
    buffer_[2] = static_cast<std::byte>((payload.size() >> 8U) & 0xffU);
    buffer_[3] = static_cast<std::byte>(payload.size() & 0xffU);
    header = 4;
  } else {
    buffer_[1] = std::byte{0xFF};
    const auto length = static_cast<std::uint64_t>(payload.size());
    for (std::size_t index = 0; index < 8; ++index) {
      buffer_[2 + index] =
          static_cast<std::byte>((length >> ((7U - index) * 8U)) & 0xffU);
    }
    header = 10;
  }
  std::array<std::byte, 4> mask_bytes{};
  std::memcpy(mask_bytes.data(), &mask, mask_bytes.size());
  std::memcpy(buffer_.data() + header, mask_bytes.data(), mask_bytes.size());
  header += mask_bytes.size();
  for (std::size_t i = 0; i < payload.size(); ++i) {
    buffer_[header + i] = payload[i] ^ mask_bytes[i & 3U];
  }
  offset_ = 0;
  size_ = header + payload.size();
  error = {};
  return true;
}

void WebSocketFrameWriter::consume(std::size_t bytes) noexcept {
  offset_ += std::min(bytes, size_ - offset_);
  if (offset_ == size_) {
    reset();
  }
}

std::span<const std::byte> WebSocketFrameWriter::pending() const noexcept {
  return {buffer_.data() + offset_, size_ - offset_};
}

} // namespace mds::network
