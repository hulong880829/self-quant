#include "mds/network/http_client.h"
#include "mds/network/epoll_loop.h"
#include "mds/network/tcp_connector.h"
#include "mds/network/websocket_codec.h"

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
  using namespace mds::network;
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
  using namespace mds::network;
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
  using namespace mds::network;
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

void test_connector_states() {
  using namespace mds::network;
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
  using namespace mds::network;

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
    test_connector_states();
    test_connector_fallback_reused_fd_registration();
    std::cout << "all network tests passed\n";
    return 0;
  } catch (const std::exception &error) {
    std::cerr << "network test failure: " << error.what() << '\n';
    return 1;
  }
}
