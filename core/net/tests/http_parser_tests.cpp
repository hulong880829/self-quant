#include "net/http_parser.h"
#include "net/tcp_acceptor.h"
#include "net/websocket_codec.h"

#include <arpa/inet.h>
#include <array>
#include <cassert>
#include <chrono>
#include <cstring>
#include <stdexcept>
#include <string>
#include <thread>
#include <unistd.h>
#include <netinet/in.h>
#include <sys/socket.h>

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::span<const std::byte> bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte *>(value.data()), value.size()};
}

void test_percent_and_query() {
  std::string decoded;
  require(net::percent_decode("/v1/markets/BTC%2FUSDT", decoded, false) &&
              decoded == "/v1/markets/BTC/USDT",
          "path percent-decode failed");
  require(net::query_value("depth=20&profile=agg+perp", "depth") == "20",
          "query depth decode failed");
  require(net::query_value("depth=20&profile=agg+perp", "profile") ==
              "agg perp",
          "query plus-as-space failed");
}

void test_get_and_options() {
  net::HttpRequestParser parser;
  std::string_view error;
  std::size_t consumed = 0;
  constexpr std::string_view request =
      "GET /v1/markets/BTCUSDT/snapshot?depth=20 HTTP/1.1\r\n"
      "Host: 127.0.0.1:9093\r\n"
      "Origin: http://localhost:3000\r\n\r\ntrailing";
  require(parser.feed(bytes(request), consumed, error) ==
              net::HttpParseStatus::Complete,
          "GET parser failed");
  require(parser.request().method == "GET" &&
              parser.request().path ==
                  "/v1/markets/BTCUSDT/snapshot" &&
              parser.request().query == "depth=20" &&
              parser.request().keep_alive && consumed + 8 == request.size(),
          "GET fields were not parsed");

  parser.reset();
  constexpr std::string_view options =
      "OPTIONS /healthz HTTP/1.1\r\n"
      "Origin: http://localhost:3000\r\n"
      "Access-Control-Request-Method: GET\r\n"
      "Connection: close\r\n\r\n";
  require(parser.feed(bytes(options), consumed, error) ==
              net::HttpParseStatus::Complete,
          "OPTIONS parser failed");
  require(parser.request().method == "OPTIONS" && !parser.request().keep_alive,
          "OPTIONS keep-alive/close failed");
}

void test_malformed_and_limits() {
  net::HttpRequestParser parser({.max_header_bytes = 64, .max_body_bytes = 8});
  std::string_view error;
  std::size_t consumed = 0;
  constexpr std::string_view malformed = "GET /no-version\r\n\r\n";
  require(parser.feed(bytes(malformed), consumed, error) ==
              net::HttpParseStatus::Error,
          "malformed request accepted");
  parser.reset();
  constexpr std::string_view oversized =
      "GET /healthz HTTP/1.1\r\nX-Fill: 012345678901234567890123456789\r\n"
      "X-More: abcdefghijklmnopqrstuvwxyz\r\n\r\n";
  require(parser.feed(bytes(oversized), consumed, error) ==
              net::HttpParseStatus::Error,
          "oversized headers accepted");
}

void test_websocket_upgrade_flag() {
  net::HttpRequestParser parser;
  std::string_view error;
  std::size_t consumed = 0;
  constexpr std::string_view request =
      "GET /v1/stream HTTP/1.1\r\n"
      "Host: 127.0.0.1:9093\r\n"
      "Upgrade: websocket\r\n"
      "Connection: Upgrade\r\n"
      "Sec-WebSocket-Version: 13\r\n"
      "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n";
  require(parser.feed(bytes(request), consumed, error) ==
              net::HttpParseStatus::Complete &&
              parser.request().websocket_upgrade,
          "websocket upgrade was not detected");
  std::array<std::byte, 256> response{};
  std::size_t size = 0;
  require(net::prepare_websocket_upgrade_response(
              "dGhlIHNhbXBsZSBub25jZQ==", response, size, error),
          "upgrade response failed");
}

void test_response_writer() {
  net::HttpResponseWriter writer;
  writer.set_status(200, "OK");
  writer.set_header("Cache-Control", "no-store");
  writer.set_body("application/json", "{\"status\":\"ok\"}\n");
  const auto encoded = writer.serialize(true);
  require(encoded.find("HTTP/1.1 200 OK\r\n") == 0 &&
              encoded.find("Content-Type: application/json\r\n") !=
                  std::string::npos &&
              encoded.find("Connection: keep-alive\r\n") != std::string::npos &&
              encoded.find("{\"status\":\"ok\"}\n") != std::string::npos,
          "response writer output is wrong");
}

void test_idle_timeout() {
  net::TcpAcceptor acceptor;
  require(acceptor.open("127.0.0.1", 0), "idle-timeout acceptor failed");
  const auto client = ::socket(AF_INET, SOCK_STREAM, 0);
  require(client >= 0, "idle-timeout client socket failed");
  sockaddr_in address{};
  address.sin_family = AF_INET;
  address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  address.sin_port = htons(acceptor.port());
  require(::connect(client, reinterpret_cast<sockaddr *>(&address),
                    sizeof(address)) == 0,
          "idle-timeout connect failed");
  const auto server = acceptor.accept_one();
  require(server >= 0, "idle-timeout accept failed");
  std::array<char, 1> byte{};
  const auto started = std::chrono::steady_clock::now();
  timeval timeout{};
  timeout.tv_sec = 0;
  timeout.tv_usec = 50 * 1000;
  require(::setsockopt(server, SOL_SOCKET, SO_RCVTIMEO, &timeout,
                       sizeof(timeout)) == 0,
          "idle-timeout setsockopt failed");
  const auto n = ::recv(server, byte.data(), 1, 0);
  require(n < 0, "slow connection produced data");
  require(std::chrono::steady_clock::now() - started <
              std::chrono::milliseconds(500),
          "idle timeout did not fire promptly");
  ::close(client);
  ::close(server);
}

}  // namespace

int main() {
  test_percent_and_query();
  test_get_and_options();
  test_malformed_and_limits();
  test_websocket_upgrade_flag();
  test_response_writer();
  test_idle_timeout();
  return 0;
}
