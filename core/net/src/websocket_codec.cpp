#include "net/websocket_codec.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <limits>
#include <openssl/crypto.h>
#include <openssl/evp.h>
#include <openssl/rand.h>
#include <openssl/sha.h>

namespace net {
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

bool header_has_token(std::string_view value,
                      std::string_view expected) noexcept {
  std::size_t position = 0;
  while (position < value.size()) {
    const auto comma = value.find(',', position);
    const auto token =
        trim(value.substr(position, comma == std::string_view::npos
                                        ? value.size() - position
                                        : comma - position));
    if (ascii_iequals(token, expected)) {
      return true;
    }
    if (comma == std::string_view::npos) {
      break;
    }
    position = comma + 1;
  }
  return false;
}

bool valid_websocket_key(std::string_view value) noexcept {
  if (value.size() != 24 || value[value.size() - 1] != '=' ||
      value[value.size() - 2] != '=') {
    return false;
  }
  std::array<unsigned char, 18> decoded{};
  const int length = EVP_DecodeBlock(
      decoded.data(), reinterpret_cast<const unsigned char *>(value.data()),
      static_cast<int>(value.size()));
  return length == 18;
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

bool prepare_websocket_upgrade_response(
    std::string_view client_key, std::span<std::byte> output,
    std::size_t &size, std::string_view &error) noexcept {
  size = 0;
  std::array<char, 29> accept{};
  if (!valid_websocket_key(client_key) ||
      !websocket_accept_value(client_key, accept)) {
    error = "invalid Sec-WebSocket-Key";
    return false;
  }
  constexpr std::string_view prefix =
      "HTTP/1.1 101 Switching Protocols\r\n"
      "Upgrade: websocket\r\n"
      "Connection: Upgrade\r\n"
      "Sec-WebSocket-Accept: ";
  constexpr std::string_view suffix = "\r\n\r\n";
  const std::size_t required = prefix.size() + 28 + suffix.size();
  if (output.size() < required) {
    error = "WebSocket upgrade response exceeds configured capacity";
    return false;
  }
  std::memcpy(output.data(), prefix.data(), prefix.size());
  std::memcpy(output.data() + prefix.size(), accept.data(), 28);
  std::memcpy(output.data() + prefix.size() + 28, suffix.data(),
              suffix.size());
  size = required;
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

WebSocketUpgradeRequestParser::WebSocketUpgradeRequestParser(
    std::size_t header_capacity)
    : buffer_(header_capacity) {}

void WebSocketUpgradeRequestParser::reset() noexcept {
  used_ = 0;
  client_key_ = {};
  request_target_ = {};
  complete_ = false;
}

bool WebSocketUpgradeRequestParser::feed(
    std::span<const std::byte> bytes, std::string_view expected_path,
    std::string_view expected_bearer_token, std::size_t &consumed,
    std::string_view &error) noexcept {
  consumed = 0;
  if (complete_) {
    return true;
  }
  for (const std::byte byte : bytes) {
    if (used_ == buffer_.size()) {
      error = "WebSocket upgrade request exceeds configured capacity";
      return false;
    }
    buffer_[used_++] = byte;
    ++consumed;
    if (used_ >= 4 && buffer_[used_ - 4] == std::byte{'\r'} &&
        buffer_[used_ - 3] == std::byte{'\n'} &&
        buffer_[used_ - 2] == std::byte{'\r'} &&
        buffer_[used_ - 1] == std::byte{'\n'}) {
      complete_ = validate(expected_path, expected_bearer_token, error);
      return complete_;
    }
  }
  return true;
}

bool WebSocketUpgradeRequestParser::validate(
    std::string_view expected_path, std::string_view expected_bearer_token,
    std::string_view &error) noexcept {
  const std::string_view request{
      reinterpret_cast<const char *>(buffer_.data()), used_};
  const auto first_end = request.find("\r\n");
  if (first_end == std::string_view::npos) {
    error = "invalid WebSocket request line";
    return false;
  }
  const auto request_line = request.substr(0, first_end);
  const auto first_space = request_line.find(' ');
  const auto second_space =
      first_space == std::string_view::npos
          ? std::string_view::npos
          : request_line.find(' ', first_space + 1);
  if (first_space == std::string_view::npos ||
      second_space == std::string_view::npos ||
      request_line.substr(0, first_space) != "GET" ||
      request_line.substr(second_space + 1) != "HTTP/1.1") {
    error = "invalid WebSocket GET request line";
    return false;
  }
  request_target_ =
      request_line.substr(first_space + 1, second_space - first_space - 1);
  if (request_target_.empty() ||
      (!expected_path.empty() && request_target_ != expected_path)) {
    error = "WebSocket request path rejected";
    return false;
  }

  bool found_upgrade = false;
  bool found_connection = false;
  bool found_version = false;
  bool found_key = false;
  bool found_authorization = false;
  bool seen_upgrade = false;
  bool seen_version = false;
  std::string_view authorization;
  std::size_t position = first_end + 2;
  while (position < request.size()) {
    const auto end = request.find("\r\n", position);
    if (end == std::string_view::npos) {
      error = "invalid WebSocket request headers";
      return false;
    }
    const auto line = request.substr(position, end - position);
    if (line.empty()) {
      break;
    }
    if (line.front() == ' ' || line.front() == '\t') {
      error = "folded WebSocket headers are unsupported";
      return false;
    }
    const auto colon = line.find(':');
    if (colon == std::string_view::npos) {
      error = "invalid WebSocket request header";
      return false;
    }
    const auto name = trim(line.substr(0, colon));
    const auto value = trim(line.substr(colon + 1));
    if (ascii_iequals(name, "Upgrade")) {
      if (seen_upgrade) {
        error = "duplicate Upgrade header";
        return false;
      }
      seen_upgrade = true;
      found_upgrade = ascii_iequals(value, "websocket");
    } else if (ascii_iequals(name, "Connection")) {
      found_connection =
          found_connection || header_has_token(value, "upgrade");
    } else if (ascii_iequals(name, "Sec-WebSocket-Version")) {
      if (seen_version) {
        error = "duplicate Sec-WebSocket-Version";
        return false;
      }
      seen_version = true;
      found_version = value == "13";
    } else if (ascii_iequals(name, "Sec-WebSocket-Key")) {
      if (found_key) {
        error = "duplicate Sec-WebSocket-Key";
        return false;
      }
      found_key = true;
      client_key_ = value;
    } else if (ascii_iequals(name, "Authorization")) {
      if (found_authorization) {
        error = "duplicate Authorization header";
        return false;
      }
      found_authorization = true;
      authorization = value;
    }
    position = end + 2;
  }

  if (!found_upgrade || !found_connection || !found_version ||
      !found_key || !valid_websocket_key(client_key_)) {
    error = "missing or invalid WebSocket upgrade headers";
    return false;
  }
  if (!expected_bearer_token.empty()) {
    constexpr std::string_view bearer = "Bearer ";
    if (!found_authorization || authorization.size() !=
                                    bearer.size() +
                                        expected_bearer_token.size() ||
        !authorization.starts_with(bearer) ||
        CRYPTO_memcmp(authorization.data() + bearer.size(),
                      expected_bearer_token.data(),
                      expected_bearer_token.size()) != 0) {
      error = "WebSocket bearer token rejected";
      return false;
    }
  }
  error = {};
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

bool WebSocketFrameWriter::prepare_unmasked(
    WsOpcode opcode, std::span<const std::byte> payload, bool final,
    std::string_view &error) noexcept {
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
    buffer_[1] = static_cast<std::byte>(payload.size());
  } else if (payload.size() <= std::numeric_limits<std::uint16_t>::max()) {
    buffer_[1] = std::byte{126};
    buffer_[2] = static_cast<std::byte>((payload.size() >> 8U) & 0xffU);
    buffer_[3] = static_cast<std::byte>(payload.size() & 0xffU);
    header = 4;
  } else {
    buffer_[1] = std::byte{127};
    const auto length = static_cast<std::uint64_t>(payload.size());
    for (std::size_t index = 0; index < 8; ++index) {
      buffer_[2 + index] =
          static_cast<std::byte>((length >> ((7U - index) * 8U)) & 0xffU);
    }
    header = 10;
  }
  std::memcpy(buffer_.data() + header, payload.data(), payload.size());
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

WebSocketServerParser::WebSocketServerParser(std::size_t capacity)
    : buffer_(capacity + 14), message_buffer_(capacity) {}

void WebSocketServerParser::reset() noexcept {
  used_ = 0;
  message_used_ = 0;
  fragmented_opcode_ = WsOpcode::Continuation;
  fragmented_ = false;
  close_received_ = false;
}

bool WebSocketServerParser::feed(std::span<const std::byte> bytes,
                                 const FrameCallback &callback,
                                 std::string &error) {
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
    auto *p = reinterpret_cast<std::uint8_t *>(buffer_.data()) + consumed;
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
      if (payload_length < 126) {
        error = "non-minimal WebSocket payload length";
        return false;
      }
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
      if (payload_length <= std::numeric_limits<std::uint16_t>::max()) {
        error = "non-minimal WebSocket payload length";
        return false;
      }
      header_length = 10;
    }
    if (control && (!final || payload_length > 125)) {
      error = "invalid fragmented or oversized control frame";
      return false;
    }
    if (!masked) {
      error = "unmasked client frame rejected";
      return false;
    }
    if (payload_length > message_buffer_.size() ||
        payload_length >
            std::numeric_limits<std::size_t>::max() - header_length - 4) {
      error = "WebSocket frame exceeds configured capacity";
      return false;
    }
    const std::size_t payload_size =
        static_cast<std::size_t>(payload_length);
    const std::size_t frame_length = header_length + 4 + payload_size;
    if (used_ - consumed < frame_length) {
      break;
    }
    const auto mask = p + header_length;
    auto *payload_data =
        reinterpret_cast<std::byte *>(p + header_length + 4);
    for (std::size_t index = 0; index < payload_size; ++index) {
      payload_data[index] ^=
          static_cast<std::byte>(mask[index & std::size_t{3}]);
    }
    const std::span<const std::byte> payload{payload_data, payload_size};

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

} // namespace net
