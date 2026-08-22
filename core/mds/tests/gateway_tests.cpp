#include "mds/consume/aggregate_dispatch.h"
#include "mds/gateway/gateway.h"
#include "net/tls_websocket.h"
#include "net/websocket_codec.h"

#include <array>
#include <chrono>
#include <cerrno>
#include <cstdint>
#include <netinet/in.h>
#include <stdexcept>
#include <string>
#include <string_view>
#include <sys/socket.h>
#include <unistd.h>

namespace {

void require(bool condition) {
  if (!condition) {
    throw std::runtime_error("gateway test requirement failed");
  }
}

std::span<const std::byte> bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte *>(value.data()), value.size()};
}

int connect_client(std::uint16_t port) {
  const int fd = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  require(fd >= 0);
  timeval timeout{.tv_sec = 1, .tv_usec = 0};
  require(::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout,
                       sizeof(timeout)) == 0);
  sockaddr_in address{};
  address.sin_family = AF_INET;
  address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  address.sin_port = htons(port);
  require(::connect(fd, reinterpret_cast<sockaddr *>(&address),
                    sizeof(address)) == 0);
  return fd;
}

void send_all(int fd, std::span<const std::byte> data) {
  while (!data.empty()) {
    const auto sent = ::send(fd, data.data(), data.size(), MSG_NOSIGNAL);
    require(sent > 0);
    data = data.subspan(static_cast<std::size_t>(sent));
  }
}

void upgrade(int fd, std::string_view token) {
  const std::string request =
      "GET /v1/market-data HTTP/1.1\r\n"
      "Host: localhost\r\n"
      "Upgrade: websocket\r\n"
      "Connection: Upgrade\r\n"
      "Sec-WebSocket-Version: 13\r\n"
      "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
      "Authorization: Bearer " +
      std::string(token) + "\r\n\r\n";
  send_all(fd, bytes(request));
  std::array<char, 1024> response{};
  std::string headers;
  while (headers.find("\r\n\r\n") == std::string::npos) {
    const auto count = ::recv(fd, response.data(), response.size(), 0);
    require(count > 0);
    headers.append(response.data(), static_cast<std::size_t>(count));
  }
  require(headers.starts_with("HTTP/1.1 101"));
}

void send_text(int fd, std::string_view text, std::uint32_t mask) {
  net::WebSocketFrameWriter writer(4096);
  std::string_view error;
  require(writer.prepare_with_mask(net::WsOpcode::Text, bytes(text),
                                   true, mask, error));
  send_all(fd, writer.pending());
}

}  // namespace

int main() {
  using namespace std::chrono_literals;
  mds::consume::AggregateLatestState latest;
  mds::consume::AggregateLatestState book_latest;
  const std::string bbo_segment =
      "/selfquant.mds.agg_perp_usdt_a-b.btcusdt.aggbbo.2";
  const std::string book_segment =
      "/selfquant.mds.agg_perp_usdt_a-b.btcusdt.aggorderbook.2";
  mds::gateway::Gateway unauthenticated(
      {.listen_address = "127.0.0.1", .port = 0},
      {{.segment = bbo_segment,
        .kind = mds::consume::AggregateTopic::AggBbo,
        .latest = &latest}});
  require(!unauthenticated.start());

  mds::gateway::Gateway gateway(
      {.listen_address = "127.0.0.1",
       .port = 0,
       .bearer_token = "secret",
       .max_clients = 4,
       .max_subscriptions_per_client = 2,
       .publish_interval_ms = 10,
       .ping_interval_ms = 5'000,
       .pong_timeout_ms = 1'000,
       .slow_client_timeout_ms = 1'000,
       .depth = 2},
      {{.segment = bbo_segment,
        .kind = mds::consume::AggregateTopic::AggBbo,
        .latest = &latest},
       {.segment = book_segment,
        .kind = mds::consume::AggregateTopic::AggOrderBook,
        .latest = &book_latest}});
  require(gateway.start());
  require(gateway.port() != 0);

  utils::md::wire::AggBboRecord record{};
  record.header = utils::md::wire::MakeHeader(
      utils::md::MessageType::AggBbo, sizeof(record));
  latest.publish(record, {.ring_epoch = 7,
                          .ring_sequence = 11,
                          .receive_mono_ns = 12,
                          .receive_wall_ns = 13});
  utils::md::wire::AggOrderBookRecord book{};
  book.header = utils::md::wire::MakeHeader(
      utils::md::MessageType::AggOrderBook, sizeof(book));
  book.bid_count = 3;
  book.ask_count = 3;
  for (std::size_t index = 0; index < 3; ++index) {
    book.bids[index].price = 100 - static_cast<std::int64_t>(index);
    book.bids[index].quantity = 1;
    book.asks[index].price = 101 + static_cast<std::int64_t>(index);
    book.asks[index].quantity = 1;
  }
  book_latest.publish(book, {.ring_epoch = 8,
                             .ring_sequence = 12,
                             .receive_mono_ns = 13,
                             .receive_wall_ns = 14});

  {
    const int unauthorized = connect_client(gateway.port());
    const std::string request =
        "GET /v1/market-data HTTP/1.1\r\n"
        "Host: localhost\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n";
    send_all(unauthorized, bytes(request));
    std::array<std::byte, 64> rejected{};
    require(::recv(unauthorized, rejected.data(), rejected.size(), 0) <= 0);
    ::close(unauthorized);
  }

  const int client = connect_client(gateway.port());
  upgrade(client, "secret");
  std::size_t acknowledgements = 0;
  bool bbo_binary = false;
  bool book_binary = false;
  bool reset_binary = false;
  net::WebSocketParser parser(64U << 10U);
  const auto callback = [&](const net::WsFrameView &frame) {
    if (frame.opcode == net::WsOpcode::Text) {
      const std::string_view text(
          reinterpret_cast<const char *>(frame.payload.data()),
          frame.payload.size());
      acknowledgements +=
          text.find("\"ok\":true") != std::string_view::npos ? 1 : 0;
    } else if (frame.opcode == net::WsOpcode::Binary &&
               frame.payload.size() >= 52) {
      const auto *data =
          reinterpret_cast<const std::uint8_t *>(frame.payload.data());
      require(data[0] == 'S' && data[1] == 'Q' && data[2] == 'G' &&
              data[3] == 'W');
      if (data[8] == 1) {
        bbo_binary = data[9] == 1;
      } else if (data[8] == 2) {
        require(data[9] == 2 && frame.payload.size() >= 121);
        const auto bids =
            static_cast<std::uint16_t>(data[117]) |
            (static_cast<std::uint16_t>(data[118]) << 8U);
        const auto asks =
            static_cast<std::uint16_t>(data[119]) |
            (static_cast<std::uint16_t>(data[120]) << 8U);
        book_binary = bids == 2 && asks == 2;
      } else if (data[8] == 3) {
        reset_binary = true;
      }
    }
    return true;
  };
  const auto deadline = std::chrono::steady_clock::now() + 5s;

  std::size_t subscribed = 0;
  bool reset_requested = false;
  std::array<std::byte, 64U << 10U> receive{};
  while (std::chrono::steady_clock::now() < deadline && !book_binary) {
    const bool send_bbo = subscribed == 0;
    const bool send_book =
        subscribed == 1 && acknowledgements >= 1 && reset_binary;
    if (send_bbo || send_book) {
      const auto &segment = send_bbo ? bbo_segment : book_segment;
      const std::string request =
          "{\"op\":\"subscribe\",\"topic\":\"" + segment + "\"}";
      send_text(client, request,
                send_bbo ? 0x04030201U : 0x08070605U);
      ++subscribed;
    }
    if (bbo_binary && !reset_requested) {
      latest.reset(mds::consume::AggregateTopic::AggBbo,
                   {.ring_epoch = 9, .ring_sequence = 0});
      reset_requested = true;
    }
    const auto count = ::recv(client, receive.data(), receive.size(), 0);
    if (count > 0) {
      std::string error;
      require(parser.feed(
          std::span<const std::byte>(receive.data(),
                                     static_cast<std::size_t>(count)),
          callback, error));
    } else {
      require(errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR);
    }
  }
  require(subscribed == 2 && acknowledgements == 2 && bbo_binary &&
          reset_binary && book_binary);
  ::close(client);
  gateway.stop();
  require(!gateway.failed());
  return 0;
}
