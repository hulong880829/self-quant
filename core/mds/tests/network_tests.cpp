#include "net/http_client.h"
#include "net/epoll_loop.h"
#include "net/tcp_connector.h"
#include "net/websocket_client.h"
#include "net/websocket_codec.h"

#include <algorithm>
#include <array>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <iostream>
#include <netdb.h>
#include <netinet/in.h>
#include <stdexcept>
#include <string_view>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <unistd.h>

namespace {

std::array<sockaddr_in, 2> controlled_socket_addresses{};
std::array<addrinfo, 2> controlled_resolved_addresses{};

int resolve_controlled_addresses(const char *, const char *, const addrinfo *,
                                 addrinfo **result) noexcept {
  *result = controlled_resolved_addresses.data();
  return 0;
}

void free_controlled_addresses(addrinfo *) noexcept {}

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::span<const std::byte> bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte *>(value.data()), value.size()};
}

void test_upgrade_accept() {
  using namespace net;
  constexpr std::string_view key = "dGhlIHNhbXBsZSBub25jZQ==";
  std::array<char, 29> accept{};
  require(websocket_accept_value(key, accept), "accept computation failed");
  require(std::string_view(accept.data()) == "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=",
          "RFC 6455 accept value mismatch");

  constexpr std::string_view response =
      "HTTP/1.1 101 Switching Protocols\r\n"
      "Upgrade: websocket\r\n"
      "Connection: keep-alive, Upgrade\r\n"
      "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n";
  WebSocketUpgradeParser parser(512);
  std::string_view error;
  std::size_t consumed = 0;
  require(parser.feed(bytes(response.substr(0, 17)), key, consumed, error) &&
              !parser.complete(),
          "partial upgrade parsing failed");
  require(parser.feed(bytes(response.substr(17)), key, consumed, error) &&
              parser.complete(),
          "valid upgrade response rejected");

  constexpr std::string_view extension_response =
      "HTTP/1.1 101 Switching Protocols\r\n"
      "Upgrade: websocket\r\nConnection: Upgrade\r\n"
      "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n"
      "Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n";
  parser.reset();
  require(!parser.feed(bytes(extension_response), key, consumed, error),
          "WebSocket extension was accepted");
}

void test_masked_frame_and_partial_write() {
  using namespace net;
  WebSocketFrameWriter writer(64);
  constexpr std::string_view text = "hello";
  std::string_view error;
  require(writer.prepare_with_mask(WsOpcode::Text, bytes(text), true,
                                   0x04030201U, error),
          "masked frame construction failed");
  const auto frame = writer.pending();
  require(frame.size() == 11 && frame[0] == std::byte{0x81} &&
              frame[1] == std::byte{0x85},
          "masked frame header invalid");
  for (std::size_t i = 0; i < text.size(); ++i) {
    const auto decoded = frame[6 + i] ^ frame[2 + (i & 3U)];
    require(decoded == static_cast<std::byte>(text[i]),
            "masked frame payload invalid");
  }
  writer.consume(3);
  require(writer.pending().size() == 8, "partial write offset was not kept");
  writer.consume(8);
  require(writer.empty(), "completed frame remained pending");
}

void test_http_incremental_parsing() {
  using namespace net;
  HttpResponseParser fixed(256, 64);
  constexpr std::string_view response =
      "HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\nhello world";
  std::string_view error;
  for (char value : response) {
    const std::byte byte = static_cast<std::byte>(value);
    require(fixed.feed({&byte, 1}, error), "partial Content-Length rejected");
  }
  require(fixed.complete() && fixed.status_code() == 200 &&
              fixed.body().size() == 11 &&
              std::memcmp(fixed.body().data(), "hello world", 11) == 0,
          "Content-Length response decoded incorrectly");

  HttpResponseParser chunked(256, 64);
  constexpr std::string_view chunks =
      "HTTP/1.1 429 Too Many Requests\r\n"
      "Transfer-Encoding: chunked\r\n\r\n"
      "5\r\nhello\r\n6;name=value\r\n world\r\n0\r\nX-Test: yes\r\n\r\n";
  for (std::size_t position = 0; position < chunks.size(); position += 3) {
    const std::size_t length = std::min<std::size_t>(3, chunks.size() - position);
    require(chunked.feed(bytes(chunks.substr(position, length)), error),
            "partial chunked response rejected");
  }
  require(chunked.complete() &&
              chunked.status_class() == HttpStatusClass::RateLimited &&
              chunked.body().size() == 11 &&
              std::memcmp(chunked.body().data(), "hello world", 11) == 0,
          "chunked response decoded incorrectly");
  require(classify_http_status(418) == HttpStatusClass::RateLimited &&
              classify_http_status(503) == HttpStatusClass::ServerError,
          "HTTP status classification incorrect");
}

void test_http_request_encoding_and_limits() {
  using namespace net;
  std::array<std::byte, 1024> output{};
  std::size_t output_size = 0;
  constexpr std::array<HttpHeader, 2> headers{{
      {"X-Request-Id", "42"},
      {"Authorization", "Bearer private-value"},
  }};
  constexpr std::string_view payload = R"({"price":"1"})";

  for (const auto method :
       {HttpMethod::Get, HttpMethod::Post, HttpMethod::Put,
        HttpMethod::Delete}) {
    const bool has_body =
        method == HttpMethod::Post || method == HttpMethod::Put;
    const HttpRequest request{
        method,
        "/orders?symbol=BTCUSDT",
        has_body ? "application/json" : std::string_view{},
        has_body ? bytes(payload) : std::span<const std::byte>{},
        headers,
    };
    require(encode_http_request("api.example.test", request, output,
                                output_size),
            "valid HTTP request was not encoded");
    const std::string_view encoded(
        reinterpret_cast<const char *>(output.data()), output_size);
    require(encoded.starts_with(
                method == HttpMethod::Get      ? "GET "
                : method == HttpMethod::Post   ? "POST "
                : method == HttpMethod::Put    ? "PUT "
                                                : "DELETE ") &&
                encoded.find("X-Request-Id: 42\r\n") !=
                    std::string_view::npos &&
                encoded.find("Authorization: Bearer private-value\r\n") !=
                    std::string_view::npos,
            "HTTP method or custom headers encoded incorrectly");
    if (has_body) {
      require(encoded.ends_with(payload),
              "HTTP request body encoded incorrectly");
    }
  }

  require(!encode_http_request(
              "api.example.test",
              {HttpMethod::Get, "/ok", {}, {}, headers}, output, output_size,
              1),
          "custom header count limit was not enforced");
  constexpr std::array<HttpHeader, 1> injected{{
      {"Authorization", "secret\r\nX-Injected: yes"},
  }};
  require(!encode_http_request(
              "api.example.test",
              {HttpMethod::Delete, "/orders/1", {}, {}, injected}, output,
              output_size),
          "header injection was accepted");
  constexpr std::array<HttpHeader, 1> reserved{{
      {"Content-Length", "100"},
  }};
  require(!encode_http_request(
              "api.example.test",
              {HttpMethod::Delete, "/orders/1", {}, {}, reserved}, output,
              output_size),
          "framing header override was accepted");

  std::array<std::byte, 16> too_small{};
  require(!encode_http_request(
              "api.example.test", {HttpMethod::Get, "/ok"}, too_small,
              output_size) &&
              output_size == 0,
          "request capacity limit was not enforced");

  HttpResponseParser body_limited(128, 2);
  std::string_view error;
  require(!body_limited.feed(
              bytes("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc"),
              error) &&
              body_limited.error_code() == HttpParseError::BodyTooLarge,
          "response body limit was not classified");
  HttpResponseParser header_limited(8, 8);
  require(!header_limited.feed(bytes("HTTP/1.1 200 OK\r\n"), error) &&
              header_limited.error_code() == HttpParseError::HeaderTooLarge,
          "response header limit was not classified");
}

void test_websocket_upgrade_header_validation() {
  using namespace net;
  WebSocketClient client(nullptr);
  require(client.set_upgrade_headers({}),
          "empty WebSocket upgrade headers were rejected");

  constexpr std::array<WebSocketHeader, 1> valid{{
      {"X-Gate-Size-Decimal", "1"},
  }};
  require(client.set_upgrade_headers(valid),
          "valid WebSocket upgrade header was rejected");
  client.reset();

  constexpr std::array<WebSocketHeader, 1> injected_name{{
      {"X-Gate\r\nInjected", "1"},
  }};
  require(!client.set_upgrade_headers(injected_name),
          "WebSocket upgrade header name injection was accepted");

  constexpr std::array<WebSocketHeader, 1> injected_value{{
      {"X-Gate-Size-Decimal", "1\r\nX-Injected: yes"},
  }};
  require(!client.set_upgrade_headers(injected_value),
          "WebSocket upgrade header value injection was accepted");

  constexpr std::array<WebSocketHeader, 1> reserved{{
      {"Sec-WebSocket-Key", "override"},
  }};
  require(!client.set_upgrade_headers(reserved),
          "reserved WebSocket upgrade header was accepted");
}

void test_http_protocol_hardening() {
  using namespace net;
  std::string_view error;

  HttpResponseParser interim(256, 64);
  constexpr std::string_view continued =
      "HTTP/1.1 100 Continue\r\n\r\n"
      "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok";
  require(interim.feed(bytes(continued), error) && interim.complete() &&
              interim.status_code() == 200 && interim.body().size() == 2,
          "interim HTTP response was treated as final");

  HttpResponseParser close_delimited(256, 64);
  require(close_delimited.feed(
              bytes("HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nbody"),
              error) &&
              !close_delimited.complete() &&
              close_delimited.finish(error) && close_delimited.complete() &&
              close_delimited.body().size() == 4,
          "close-delimited HTTP response was rejected");

  HttpResponseParser truncated(256, 64);
  require(truncated.feed(
              bytes("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nab"),
              error) &&
              !truncated.finish(error) &&
              truncated.error_code() == HttpParseError::InvalidResponse,
          "truncated Content-Length response was accepted");

  const auto rejects = [&error](std::string_view response) {
    HttpResponseParser parser(256, 64);
    return !parser.feed(bytes(response), error) &&
           parser.error_code() == HttpParseError::InvalidResponse;
  };
  require(rejects("HTTP/1.1 2000 Nope\r\nContent-Length: 0\r\n\r\n"),
          "four-digit HTTP status code was accepted");
  require(rejects(
              "HTTP/1.1 200 OK\r\n Content-Length: 0\r\n\r\n"),
          "whitespace-prefixed HTTP header was accepted");
  require(rejects(
              "HTTP/1.1 200 OK\r\n"
              "Transfer-Encoding: gzip, chunked\r\n\r\n0\r\n\r\n"),
          "unsupported transfer coding was accepted");
  require(rejects(
              "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n"
              "Transfer-Encoding: chunked\r\n\r\n"),
          "ambiguous HTTP response framing was accepted");
  require(rejects(
              "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
              "0\r\nContent-Length: 1\r\n\r\n"),
          "forbidden framing trailer was accepted");
  require(rejects(
              "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
              "0\r\nnot-a-header\r\n\r\n"),
          "malformed HTTP trailer was accepted");

  HttpResponseParser close_limited(256, 2);
  require(!close_limited.feed(
              bytes("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nabc"),
              error) &&
              close_limited.error_code() == HttpParseError::BodyTooLarge &&
              !close_limited.finish(error),
          "close-delimited body capacity was not enforced");
  close_limited.reset();
  require(close_limited.feed(
              bytes("HTTP/1.1 204 No Content\r\n\r\n"), error) &&
              close_limited.complete() &&
              close_limited.error_code() == HttpParseError::None,
          "HTTP parser reset did not clear failed state");

  std::array<std::byte, 512> output{};
  std::size_t output_size = 0;
  require(!encode_http_request(
              "api.example.test\tbad", {HttpMethod::Get, "/ok"}, output,
              output_size),
          "whitespace in HTTP host was accepted");
  require(!encode_http_request(
              "api.example.test", {HttpMethod::Get, "/bad\tpath"}, output,
              output_size),
          "whitespace in HTTP target was accepted");
}

void test_connector_states() {
  using namespace net;
  const int listener = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  require(listener >= 0, "loopback listener creation failed");
  sockaddr_in address{};
  address.sin_family = AF_INET;
  address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  address.sin_port = 0;
  require(::bind(listener, reinterpret_cast<sockaddr *>(&address),
                 sizeof(address)) == 0 &&
              ::listen(listener, 1) == 0,
          "loopback listener setup failed");
  socklen_t address_length = sizeof(address);
  require(::getsockname(listener, reinterpret_cast<sockaddr *>(&address),
                        &address_length) == 0,
          "loopback listener address failed");
  char service[6]{};
  const int service_length =
      std::snprintf(service, sizeof(service), "%u", ntohs(address.sin_port));
  require(service_length > 0, "loopback service conversion failed");
  TcpConnector connected;
  require(connected.start("127.0.0.1", service,
                          TcpConnector::Clock::now() +
                              std::chrono::seconds(1)),
          "loopback connection start failed");
  if (connected.state() == ConnectState::Connecting) {
    connected.on_event(EPOLLOUT);
  }
  require(connected.state() == ConnectState::Connected,
          "connector connected state missing");
  ::close(listener);

  TcpConnector invalid;
  require(!invalid.start("", "443", TcpConnector::Clock::now()),
          "invalid connector endpoint accepted");
  require(invalid.state() == ConnectState::Failed,
          "connector failure state missing");

  TcpConnector timeout;
  const auto expired = TcpConnector::Clock::now() - std::chrono::seconds(1);
  require(!timeout.start("192.0.2.1", "9", expired) &&
              timeout.state() == ConnectState::TimedOut &&
              timeout.last_error() == ETIMEDOUT,
          "connector timeout state missing");
}

void test_connector_fallback_reused_fd_registration() {
  using namespace net;

  const int listener = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  const int refused_socket = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  require(listener >= 0 && refused_socket >= 0,
          "controlled loopback sockets creation failed");

  sockaddr_in listener_address{};
  listener_address.sin_family = AF_INET;
  listener_address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  listener_address.sin_port = 0;
  require(::bind(listener, reinterpret_cast<sockaddr *>(&listener_address),
                 sizeof(listener_address)) == 0 &&
              ::listen(listener, 1) == 0,
          "controlled listener setup failed");
  socklen_t address_length = sizeof(listener_address);
  require(::getsockname(listener,
                        reinterpret_cast<sockaddr *>(&listener_address),
                        &address_length) == 0,
          "controlled listener address failed");

  sockaddr_in refused_address{};
  refused_address.sin_family = AF_INET;
  refused_address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  refused_address.sin_port = 0;
  require(::bind(refused_socket, reinterpret_cast<sockaddr *>(&refused_address),
                 sizeof(refused_address)) == 0,
          "controlled refused endpoint setup failed");
  address_length = sizeof(refused_address);
  require(::getsockname(refused_socket,
                        reinterpret_cast<sockaddr *>(&refused_address),
                        &address_length) == 0,
          "controlled refused endpoint address failed");

  controlled_socket_addresses = {refused_address, listener_address};
  controlled_resolved_addresses = {};
  for (std::size_t index = 0; index < controlled_resolved_addresses.size();
       ++index) {
    auto &address = controlled_resolved_addresses[index];
    address.ai_family = AF_INET;
    address.ai_socktype = SOCK_STREAM;
    address.ai_protocol = IPPROTO_TCP;
    address.ai_addrlen = sizeof(sockaddr_in);
    address.ai_addr = reinterpret_cast<sockaddr *>(
        &controlled_socket_addresses[index]);
    address.ai_next =
        index + 1 < controlled_resolved_addresses.size()
            ? &controlled_resolved_addresses[index + 1]
            : nullptr;
  }

  const TcpConnector::ResolverHooks resolver{
      resolve_controlled_addresses, free_controlled_addresses};
  EpollLoop loop;
  TcpConnector connector(&resolver);
  require(connector.start("controlled.test", "443",
                          TcpConnector::Clock::now() +
                              std::chrono::seconds(1)) &&
              connector.state() == ConnectState::Connecting,
          "controlled first address did not enter connecting state");

  const int first_fd = connector.fd();
  const std::uint64_t first_generation = connector.socket_generation();
  require(loop.add(first_fd, connector.wanted_events(),
                   [](std::uint32_t) {}),
          "first socket epoll registration failed");

  const ConnectState fallback_state = connector.on_event(EPOLLOUT);
  require(fallback_state == ConnectState::Connecting ||
              fallback_state == ConnectState::Connected,
          "connector did not fall back to second address");
  require(connector.fd() == first_fd,
          "test did not reproduce same-number fd reuse");
  require(connector.socket_generation() > first_generation,
          "socket generation did not change across fallback");

  require(loop.remove(first_fd),
          "stale epoll registration removal did not tolerate ENOENT");
  require(loop.add(connector.fd(), connector.wanted_events(),
                   [](std::uint32_t) {}),
          "reused fd was not added as a new epoll registration");

  const std::uint64_t connected_generation = connector.socket_generation();
  connector.reset();
  require(connector.socket_generation() > connected_generation,
          "socket generation did not change on close");
  ::close(refused_socket);
  ::close(listener);
}

} // namespace

int main() {
  try {
    test_upgrade_accept();
    test_masked_frame_and_partial_write();
    test_http_incremental_parsing();
    test_http_request_encoding_and_limits();
    test_websocket_upgrade_header_validation();
    test_http_protocol_hardening();
    test_connector_states();
    test_connector_fallback_reused_fd_registration();
    std::cout << "all network tests passed\n";
    return 0;
  } catch (const std::exception &error) {
    std::cerr << "network test failure: " << error.what() << '\n';
    return 1;
  }
}
