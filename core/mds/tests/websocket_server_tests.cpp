#include "net/tls_websocket.h"
#include "net/websocket_codec.h"

#include <array>
#include <iostream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::span<const std::byte> bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte *>(value.data()), value.size()};
}

void test_upgrade_request_parser() {
  using namespace net;
  constexpr std::string_view request =
      "GET /stream HTTP/1.1\r\n"
      "Host: localhost\r\n"
      "Upgrade: websocket\r\n"
      "Connection: keep-alive, Upgrade\r\n"
      "Sec-WebSocket-Version: 13\r\n"
      "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
      "Authorization: Bearer secret\r\n\r\n"
      "extra";
  WebSocketUpgradeRequestParser parser(512);
  std::string_view error;
  std::size_t consumed = 0;
  for (std::size_t position = 0; position < request.size(); position += 3) {
    const auto part =
        request.substr(position, std::min<std::size_t>(3, request.size() -
                                                             position));
    require(parser.feed(bytes(part), "/stream", "secret", consumed, error),
            "valid incremental upgrade request rejected");
    if (parser.complete()) {
      require(position + consumed + 1 < request.size(),
              "upgrade parser consumed bytes after headers");
      break;
    }
  }
  require(parser.complete() && parser.request_target() == "/stream" &&
              parser.client_key() == "dGhlIHNhbXBsZSBub25jZQ==",
          "upgrade request fields were not retained");

  std::array<std::byte, 256> response{};
  std::size_t response_size = 0;
  require(prepare_websocket_upgrade_response(
              parser.client_key(), response, response_size, error),
          "upgrade response construction failed");
  require(std::string_view(reinterpret_cast<const char *>(response.data()),
                           response_size)
              .find("Sec-WebSocket-Accept: "
                    "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=") !=
              std::string_view::npos,
          "upgrade response accept value is wrong");

  parser.reset();
  constexpr std::string_view wrong_version =
      "GET /stream HTTP/1.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
      "Sec-WebSocket-Version: 12\r\n"
      "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n";
  require(!parser.feed(bytes(wrong_version), "/stream", {}, consumed, error),
          "unsupported WebSocket version accepted");

  parser.reset();
  require(!parser.feed(bytes(request), "/stream", "wrong", consumed, error),
          "invalid WebSocket bearer token accepted");

  WebSocketUpgradeRequestParser bounded(32);
  constexpr std::string_view oversized =
      "GET /stream HTTP/1.1\r\nX-Fill: 01234567890123456789";
  require(!bounded.feed(bytes(oversized), "/stream", {}, consumed, error),
          "oversized upgrade request accepted");
}

void test_server_frame_codec() {
  using namespace net;
  WebSocketServerParser parser(16);
  WebSocketFrameWriter first(16);
  WebSocketFrameWriter second(16);
  WebSocketFrameWriter ping(125);
  WebSocketFrameWriter close(125);
  constexpr std::array close_payload{std::byte{0x03}, std::byte{0xe8}};
  std::string_view writer_error;
  require(first.prepare_with_mask(WsOpcode::Text, bytes("hel"), false,
                                  0x04030201U, writer_error) &&
              second.prepare_with_mask(WsOpcode::Continuation, bytes("lo"),
                                       true, 0x08070605U, writer_error) &&
              ping.prepare_with_mask(WsOpcode::Ping, bytes("p"), true,
                                     0x0c0b0a09U, writer_error) &&
              close.prepare_with_mask(
                  WsOpcode::Close, close_payload, true,
                  0x100f0e0dU, writer_error),
          "masked client frame construction failed");

  std::vector<WsOpcode> opcodes;
  std::vector<std::string> payloads;
  std::string parser_error;
  const auto callback = [&](const WsFrameView &frame) {
    opcodes.push_back(frame.opcode);
    payloads.emplace_back(reinterpret_cast<const char *>(frame.payload.data()),
                          frame.payload.size());
    return true;
  };
  require(parser.feed(first.pending(), callback, parser_error) &&
              parser.feed(ping.pending(), callback, parser_error) &&
              parser.feed(second.pending(), callback, parser_error),
          "masked fragmented client frames rejected");
  require(opcodes.size() == 2 && opcodes[0] == WsOpcode::Ping &&
              payloads[0] == "p" && opcodes[1] == WsOpcode::Text &&
              payloads[1] == "hello",
          "control interleave or fragmented message parsing failed");
  require(parser.feed(close.pending(), callback, parser_error) &&
              parser.close_received() && opcodes.back() == WsOpcode::Close &&
              !parser.feed(first.pending(), callback, parser_error),
          "close frame state handling failed");

  WebSocketFrameWriter server_writer(16);
  require(server_writer.prepare_unmasked(WsOpcode::Pong, bytes("p"), true,
                                         writer_error),
          "unmasked server frame construction failed");
  require(server_writer.pending().size() == 3 &&
              server_writer.pending()[0] == std::byte{0x8a} &&
              server_writer.pending()[1] == std::byte{0x01},
          "unmasked server frame header is invalid");

  WebSocketServerParser rejects_unmasked(16);
  require(!rejects_unmasked.feed(server_writer.pending(), callback,
                                 parser_error),
          "unmasked client frame accepted");

  WebSocketServerParser bounded(4);
  WebSocketFrameWriter oversized(8);
  require(oversized.prepare_with_mask(WsOpcode::Binary, bytes("12345"), true,
                                      0x01020304U, writer_error) &&
              !bounded.feed(oversized.pending(), callback, parser_error),
          "frame above configured message capacity accepted");

  std::array<std::byte, 7> fragmented_ping{
      std::byte{0x09}, std::byte{0x81}, std::byte{1}, std::byte{2},
      std::byte{3},    std::byte{4},    std::byte{'x' ^ 1}};
  WebSocketServerParser control_rules(16);
  require(!control_rules.feed(fragmented_ping, callback, parser_error),
          "fragmented control frame accepted");
}

} // namespace

int main() {
  try {
    test_upgrade_request_parser();
    test_server_frame_codec();
    std::cout << "all WebSocket server tests passed\n";
    return 0;
  } catch (const std::exception &error) {
    std::cerr << "WebSocket server test failure: " << error.what() << '\n';
    return 1;
  }
}
