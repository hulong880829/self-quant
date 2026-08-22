#include "mds/gateway/gateway.h"

#include "net/epoll_loop.h"
#include "net/tcp_acceptor.h"
#include "net/websocket_codec.h"

#include <algorithm>
#include <array>
#include <cerrno>
#include <chrono>
#include <cstring>
#include <mutex>
#include <span>
#include <string_view>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <thread>
#include <type_traits>
#include <unordered_map>
#include <unordered_set>
#include <unistd.h>
#include <utility>

namespace mds::gateway {
namespace {

constexpr std::uint32_t kGatewayMagic = 0x57475153U;  // SQGW
constexpr std::uint16_t kGatewayMajor = 1;
constexpr std::uint16_t kGatewayMinor = 0;
constexpr std::string_view kGatewayPath = "/v1/market-data";
constexpr std::uint16_t kFrameReset = 1U << 0U;

std::uint64_t mono_now_ns() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

template <typename T>
void append_le(std::vector<std::byte> &output, T value) {
  using U = std::make_unsigned_t<T>;
  const auto bits = static_cast<U>(value);
  for (std::size_t index = 0; index < sizeof(T); ++index) {
    output.push_back(
        static_cast<std::byte>((bits >> (index * 8U)) & U{0xff}));
  }
}

template <typename T, std::size_t Size>
void append_array(std::vector<std::byte> &output,
                  const std::array<T, Size> &values) {
  for (const auto value : values) {
    append_le(output, value);
  }
}

void append_bytes(std::vector<std::byte> &output,
                  std::span<const std::byte> bytes) {
  output.insert(output.end(), bytes.begin(), bytes.end());
}

void append_level(std::vector<std::byte> &output,
                  const utils::md::wire::AggLevel &level) {
  append_le(output, level.price);
  append_le(output, level.quantity);
  append_array(output, level.venue_quantity);
  append_le(output, level.venue_mask);
  append_le(output, level.contributor_count);
}

std::string compact_json(std::span<const std::byte> payload) {
  std::string result;
  result.reserve(payload.size());
  for (const auto byte : payload) {
    const char value = static_cast<char>(byte);
    if (value != ' ' && value != '\t' && value != '\r' && value != '\n') {
      result.push_back(value);
    }
  }
  return result;
}

bool json_string(std::string_view json, std::string_view name,
                 std::string_view &value) noexcept {
  const std::string needle = "\"" + std::string(name) + "\":\"";
  const auto begin = json.find(needle);
  if (begin == std::string_view::npos) {
    return false;
  }
  const auto value_begin = begin + needle.size();
  const auto end = json.find('"', value_begin);
  if (end == std::string_view::npos ||
      json.find('\\', value_begin) < end) {
    return false;
  }
  value = json.substr(value_begin, end - value_begin);
  return true;
}

}  // namespace

class Gateway::Impl {
 public:
  Impl(GatewayOptions value, std::vector<Topic> value_topics)
      : options(std::move(value)), topics(std::move(value_topics)) {}

  struct Connection {
    enum class State : std::uint8_t {
      ReadUpgrade,
      WriteUpgrade,
      Open,
      Closed
    };

    Connection(int socket, std::size_t control_capacity,
               std::size_t topic_count)
        : fd(socket), upgrade(control_capacity), parser(control_capacity),
          writer(64U << 10U), last_generation(topic_count),
          last_reset_generation(topic_count) {
      payload.reserve(64U << 10U);
      subscriptions.reserve(topic_count);
      last_io_ns = mono_now_ns();
    }
    ~Connection() {
      if (fd >= 0) {
        (void)::close(fd);
      }
    }

    int fd{-1};
    std::array<std::byte, 64U << 10U> receive{};
    net::WebSocketUpgradeRequestParser upgrade;
    net::WebSocketServerParser parser;
    net::WebSocketFrameWriter writer;
    State state{State::ReadUpgrade};
    std::uint32_t wanted{EPOLLIN};
    std::array<std::byte, 512> upgrade_response{};
    std::size_t upgrade_size{};
    std::size_t upgrade_offset{};
    std::vector<std::size_t> subscriptions;
    std::vector<std::uint64_t> last_generation;
    std::vector<std::uint64_t> last_reset_generation;
    std::vector<std::byte> payload;
    std::size_t round_robin{};
    std::uint64_t last_io_ns{};
    std::uint64_t ping_sent_ns{};
    std::uint64_t write_started_ns{};
    bool waiting_pong{};
  };

  bool start() {
    if (topics.empty() || topics.size() > 65'535 ||
        options.publish_interval_ms < 10 ||
        options.depth == 0 || options.depth > 50 || options.max_clients == 0 ||
        options.bearer_token.empty()) {
      return fail("invalid gateway options or no aggregate topics");
    }
    std::unordered_set<std::string_view> topic_names;
    topic_names.reserve(topics.size());
    for (const auto &topic : topics) {
      if (topic.segment.empty() || topic.latest == nullptr ||
          topic.kind == consume::AggregateTopic::Unsupported) {
        return fail("gateway topic is invalid");
      }
      if (!topic_names.emplace(topic.segment).second) {
        return fail("gateway topic names must be unique");
      }
    }
    if (!acceptor.open(options.listen_address, options.port,
                       static_cast<int>(
                           std::min<std::size_t>(options.max_clients, 4096)),
                       options.reuse_port)) {
      return fail(std::string(acceptor.error_message()));
    }
    if (!loop.add(acceptor.fd(), EPOLLIN | EPOLLERR | EPOLLHUP,
                  [this](std::uint32_t events) { accept_ready(events); })) {
      return fail("failed to register gateway listener");
    }
    running.store(true, std::memory_order_release);
    worker = std::thread([this] { run(); });
    return true;
  }

  void stop() noexcept {
    running.store(false, std::memory_order_release);
    loop.stop();
    if (worker.joinable()) {
      worker.join();
    }
    connections.clear();
    pending_accepts.clear();
    acceptor.reset();
  }

  bool fail(std::string message) {
    {
      std::lock_guard lock(error_mutex);
      error_text = std::move(message);
    }
    failed.store(true, std::memory_order_release);
    return false;
  }

  void accept_ready(std::uint32_t events) {
    if ((events & (EPOLLERR | EPOLLHUP)) != 0) {
      (void)fail("gateway listener failed");
      running.store(false, std::memory_order_release);
      return;
    }
    for (;;) {
      const int fd = acceptor.accept_one();
      if (fd >= 0) {
        if (connections.size() + pending_accepts.size() >=
            options.max_clients) {
          (void)::close(fd);
        } else {
          pending_accepts.push_back(fd);
        }
        continue;
      }
      if (acceptor.last_error() != EAGAIN &&
          acceptor.last_error() != EWOULDBLOCK) {
        (void)fail("gateway accept failed");
        running.store(false, std::memory_order_release);
      }
      break;
    }
  }

  void register_pending() {
    for (const int fd : pending_accepts) {
      auto connection = std::make_unique<Connection>(
          fd, options.max_control_frame_bytes, topics.size());
      auto *raw = connection.get();
      if (!loop.add(fd, EPOLLIN | EPOLLERR | EPOLLHUP | EPOLLRDHUP,
                    [this, fd](std::uint32_t events) {
                      on_connection(fd, events);
                    })) {
        continue;
      }
      connections.emplace(fd, std::move(connection));
      raw->wanted = EPOLLIN | EPOLLERR | EPOLLHUP | EPOLLRDHUP;
    }
    pending_accepts.clear();
  }

  void close_connection(Connection &connection) noexcept {
    connection.state = Connection::State::Closed;
  }

  void prepare_text(Connection &connection, std::string_view message) {
    if (!connection.writer.empty()) {
      return;
    }
    std::string_view error;
    (void)connection.writer.prepare_unmasked(
        net::WsOpcode::Text,
        {reinterpret_cast<const std::byte *>(message.data()), message.size()},
        true, error);
    connection.write_started_ns = mono_now_ns();
  }

  bool control(Connection &connection, const net::WsFrameView &frame) {
    if (frame.opcode == net::WsOpcode::Ping) {
      if (connection.writer.empty()) {
        std::string_view error;
        (void)connection.writer.prepare_unmasked(net::WsOpcode::Pong,
                                                 frame.payload, true, error);
        connection.write_started_ns = mono_now_ns();
      }
      return true;
    }
    if (frame.opcode == net::WsOpcode::Pong) {
      connection.waiting_pong = false;
      return true;
    }
    if (frame.opcode == net::WsOpcode::Close) {
      close_connection(connection);
      return true;
    }
    if (frame.opcode != net::WsOpcode::Text) {
      return false;
    }
    const auto json = compact_json(frame.payload);
    std::string_view operation;
    std::string_view topic_name;
    if (!json_string(json, "op", operation) ||
        !json_string(json, "topic", topic_name)) {
      prepare_text(connection,
                   R"({"ok":false,"error":"invalid-control"})");
      return true;
    }
    const auto found = std::find_if(
        topics.begin(), topics.end(),
        [&](const Topic &topic) { return topic.segment == topic_name; });
    if (found == topics.end()) {
      prepare_text(connection,
                   R"({"ok":false,"error":"unknown-topic"})");
      return true;
    }
    const auto index =
        static_cast<std::size_t>(std::distance(topics.begin(), found));
    auto subscribed = std::find(connection.subscriptions.begin(),
                                connection.subscriptions.end(), index);
    if (operation == "subscribe") {
      if (subscribed == connection.subscriptions.end()) {
        if (connection.subscriptions.size() >=
            options.max_subscriptions_per_client) {
          prepare_text(connection,
                       R"({"ok":false,"error":"subscription-limit"})");
          return true;
        }
        connection.subscriptions.push_back(index);
        connection.last_generation[index] = 0;
        connection.last_reset_generation[index] = 0;
      }
      prepare_text(connection, R"({"ok":true,"op":"subscribe"})");
      return true;
    }
    if (operation == "unsubscribe") {
      if (subscribed != connection.subscriptions.end()) {
        connection.subscriptions.erase(subscribed);
      }
      prepare_text(connection, R"({"ok":true,"op":"unsubscribe"})");
      return true;
    }
    prepare_text(connection,
                 R"({"ok":false,"error":"unknown-operation"})");
    return true;
  }

  void drive_read(Connection &connection) {
    for (;;) {
      const auto result =
          ::recv(connection.fd, connection.receive.data(),
                 connection.receive.size(), 0);
      if (result < 0) {
        if (errno == EINTR) {
          continue;
        }
        if (errno != EAGAIN && errno != EWOULDBLOCK) {
          close_connection(connection);
        }
        break;
      }
      if (result == 0) {
        close_connection(connection);
        break;
      }
      const auto data = std::span<const std::byte>(
          connection.receive.data(), static_cast<std::size_t>(result));
      connection.last_io_ns = mono_now_ns();
      if (connection.state == Connection::State::ReadUpgrade) {
        std::size_t consumed = 0;
        std::string_view error;
        if (!connection.upgrade.feed(data, kGatewayPath,
                                     options.bearer_token, consumed, error)) {
          close_connection(connection);
          return;
        }
        if (connection.upgrade.complete()) {
          if (!net::prepare_websocket_upgrade_response(
                  connection.upgrade.client_key(),
                  connection.upgrade_response, connection.upgrade_size,
                  error)) {
            close_connection(connection);
            return;
          }
          connection.state = Connection::State::WriteUpgrade;
          connection.upgrade_offset = 0;
          return;
        }
      } else if (connection.state == Connection::State::Open) {
        std::string parser_error;
        if (!connection.parser.feed(
                data,
                [&](const net::WsFrameView &frame) {
                  return control(connection, frame);
                },
                parser_error)) {
          close_connection(connection);
          return;
        }
      }
    }
  }

  void drive_write(Connection &connection) {
    if (connection.state == Connection::State::WriteUpgrade) {
      const auto pending = std::span<const std::byte>(
          connection.upgrade_response.data() + connection.upgrade_offset,
          connection.upgrade_size - connection.upgrade_offset);
      const auto result =
          ::send(connection.fd, pending.data(), pending.size(), MSG_NOSIGNAL);
      if (result > 0) {
        connection.upgrade_offset += static_cast<std::size_t>(result);
        connection.last_io_ns = mono_now_ns();
        if (connection.upgrade_offset == connection.upgrade_size) {
          connection.state = Connection::State::Open;
        }
      } else if (result < 0 && errno != EAGAIN && errno != EWOULDBLOCK &&
                 errno != EINTR) {
        close_connection(connection);
      }
      return;
    }
    if (connection.state != Connection::State::Open ||
        connection.writer.empty()) {
      return;
    }
    const auto pending = connection.writer.pending();
    const auto result =
        ::send(connection.fd, pending.data(), pending.size(), MSG_NOSIGNAL);
    if (result > 0) {
      connection.writer.consume(static_cast<std::size_t>(result));
      connection.last_io_ns = mono_now_ns();
      if (connection.writer.empty()) {
        connection.write_started_ns = 0;
      }
    } else if (result < 0 && errno != EAGAIN && errno != EWOULDBLOCK &&
               errno != EINTR) {
      close_connection(connection);
    }
  }

  void on_connection(int fd, std::uint32_t events) {
    const auto found = connections.find(fd);
    if (found == connections.end()) {
      return;
    }
    auto &connection = *found->second;
    if ((events & (EPOLLERR | EPOLLHUP | EPOLLRDHUP)) != 0) {
      close_connection(connection);
      return;
    }
    if ((events & EPOLLIN) != 0) {
      drive_read(connection);
    }
    if ((events & EPOLLOUT) != 0) {
      drive_write(connection);
    }
    if (connection.state != Connection::State::Closed) {
      std::uint32_t wanted = EPOLLIN | EPOLLERR | EPOLLHUP | EPOLLRDHUP;
      if (connection.state == Connection::State::WriteUpgrade ||
          !connection.writer.empty()) {
        wanted |= EPOLLOUT;
      }
      connection.wanted = wanted;
      (void)loop.modify(fd, wanted);
    }
  }

  void append_gateway_header(Connection &connection, std::uint8_t kind,
                             std::uint8_t payload_format,
                             std::uint16_t topic_id, std::uint16_t flags,
                             const consume::TopicStatus &status,
                             std::uint32_t payload_bytes) {
    auto &output = connection.payload;
    output.clear();
    append_le(output, kGatewayMagic);
    append_le(output, kGatewayMajor);
    append_le(output, kGatewayMinor);
    append_le(output, kind);
    append_le(output, payload_format);
    append_le(output, topic_id);
    append_le(output, flags);
    append_le(output, std::uint16_t{52});
    append_le(output, payload_bytes);
    append_le(output, status.receive.ring_epoch);
    append_le(output, status.receive.ring_sequence);
    append_le(output, status.generation);
    append_le(output, status.receive.receive_wall_ns);
  }

  bool prepare_topic(Connection &connection, std::size_t index) {
    const auto &topic = topics[index];
    if (topic.kind == consume::AggregateTopic::AggBbo) {
      consume::AggBboSnapshot snapshot{};
      if (!topic.latest->snapshot(snapshot)) {
        return false;
      }
      if (!snapshot.status.ready) {
        if (snapshot.status.reset_generation ==
            connection.last_reset_generation[index]) {
          return false;
        }
        append_gateway_header(connection, 3, 0,
                              static_cast<std::uint16_t>(index), kFrameReset,
                              snapshot.status, 0);
        connection.last_reset_generation[index] =
            snapshot.status.reset_generation;
      } else {
        if (snapshot.status.generation ==
            connection.last_generation[index]) {
          return false;
        }
        append_gateway_header(
            connection, 1, 1, static_cast<std::uint16_t>(index), 0,
            snapshot.status, sizeof(snapshot.record));
        append_bytes(connection.payload, std::as_bytes(
            std::span<const utils::md::wire::AggBboRecord>(
                &snapshot.record, 1)));
        connection.last_generation[index] = snapshot.status.generation;
      }
    } else {
      consume::AggOrderBookSnapshot snapshot{};
      if (!topic.latest->snapshot(snapshot)) {
        return false;
      }
      if (!snapshot.status.ready) {
        if (snapshot.status.reset_generation ==
            connection.last_reset_generation[index]) {
          return false;
        }
        append_gateway_header(connection, 3, 0,
                              static_cast<std::uint16_t>(index), kFrameReset,
                              snapshot.status, 0);
        connection.last_reset_generation[index] =
            snapshot.status.reset_generation;
      } else {
        if (snapshot.status.generation ==
            connection.last_generation[index]) {
          return false;
        }
        const auto &book = snapshot.record;
        const auto bids = static_cast<std::uint16_t>(
            std::min<std::size_t>({options.depth, book.bid_count, 50}));
        const auto asks = static_cast<std::uint16_t>(
            std::min<std::size_t>({options.depth, book.ask_count, 50}));
        constexpr std::uint32_t metadata_bytes =
            8 + 4 + 2 + 16 + 16 + 8 + 1 + 1 + 1 + 4 + 4 + 2 + 2;
        constexpr std::uint32_t level_bytes = 8 + 8 + 8 * 8 + 4 + 1;
        const auto payload_bytes =
            metadata_bytes +
            level_bytes * (static_cast<std::uint32_t>(bids) + asks);
        append_gateway_header(
            connection, 2, 2, static_cast<std::uint16_t>(index), 0,
            snapshot.status, payload_bytes);
        auto &compact = connection.payload;
        append_le(compact, book.header.exchange_ts_ns);
        append_le(compact, book.header.book_generation);
        append_le(compact, book.header.flags);
        append_array(compact, book.base_asset);
        append_array(compact, book.quote_asset);
        append_array(compact, book.venue_slot_ids);
        append_le(compact, book.price_scale);
        append_le(compact, book.quantity_scale);
        append_le(compact, book.member_count);
        append_le(compact, book.member_mask);
        append_le(compact, book.active_mask);
        append_le(compact, bids);
        append_le(compact, asks);
        for (std::size_t level = 0; level < bids; ++level) {
          append_level(compact, book.bids[level]);
        }
        for (std::size_t level = 0; level < asks; ++level) {
          append_level(compact, book.asks[level]);
        }
        connection.last_generation[index] = snapshot.status.generation;
      }
    }
    std::string_view error;
    if (!connection.writer.prepare_unmasked(
            net::WsOpcode::Binary, connection.payload, true, error)) {
      close_connection(connection);
      return false;
    }
    connection.write_started_ns = mono_now_ns();
    (void)loop.modify(connection.fd,
                      EPOLLIN | EPOLLOUT | EPOLLERR | EPOLLHUP |
                          EPOLLRDHUP);
    return true;
  }

  void publish(std::uint64_t now) {
    for (auto &[fd, pointer] : connections) {
      (void)fd;
      auto &connection = *pointer;
      if (connection.state != Connection::State::Open) {
        if (now - connection.last_io_ns >
            options.slow_client_timeout_ms * 1'000'000ULL) {
          close_connection(connection);
        }
        continue;
      }
      if (connection.waiting_pong &&
          now - connection.ping_sent_ns >
              options.pong_timeout_ms * 1'000'000ULL) {
        close_connection(connection);
        continue;
      }
      if (!connection.writer.empty()) {
        if (connection.write_started_ns != 0 &&
            now - connection.write_started_ns >
                options.slow_client_timeout_ms * 1'000'000ULL) {
          close_connection(connection);
        }
        continue;
      }
      if (!connection.waiting_pong &&
          now - connection.last_io_ns >
              options.ping_interval_ms * 1'000'000ULL) {
        std::string_view error;
        if (connection.writer.prepare_unmasked(net::WsOpcode::Ping, {},
                                                true, error)) {
          connection.waiting_pong = true;
          connection.ping_sent_ns = now;
          connection.write_started_ns = now;
          (void)loop.modify(connection.fd,
                            EPOLLIN | EPOLLOUT | EPOLLERR | EPOLLHUP |
                                EPOLLRDHUP);
        }
        continue;
      }
      if (connection.subscriptions.empty()) {
        continue;
      }
      for (std::size_t attempt = 0;
           attempt < connection.subscriptions.size(); ++attempt) {
        const auto offset =
            (connection.round_robin + attempt) %
            connection.subscriptions.size();
        const auto topic = connection.subscriptions[offset];
        if (prepare_topic(connection, topic)) {
          connection.round_robin =
              (offset + 1) % connection.subscriptions.size();
          break;
        }
      }
    }
  }

  void cleanup_closed() {
    for (auto iterator = connections.begin(); iterator != connections.end();) {
      if (iterator->second->state == Connection::State::Closed) {
        (void)loop.remove(iterator->first);
        iterator = connections.erase(iterator);
      } else {
        ++iterator;
      }
    }
  }

  void run() {
    const auto interval_ns = options.publish_interval_ms * 1'000'000ULL;
    std::uint64_t next_publish = mono_now_ns();
    while (running.load(std::memory_order_acquire)) {
      const auto now = mono_now_ns();
      if (now >= next_publish) {
        publish(now);
        next_publish = now + interval_ns;
      }
      const auto before_wait = mono_now_ns();
      const auto remaining_ns =
          next_publish > before_wait ? next_publish - before_wait : 0;
      const auto wait_ms = static_cast<int>(
          std::min<std::uint64_t>(20, (remaining_ns + 999'999ULL) / 1'000'000ULL));
      if (loop.run_once(wait_ms) < 0 && errno != EINTR) {
        (void)fail("gateway epoll wait failed");
        break;
      }
      register_pending();
      cleanup_closed();
    }
    running.store(false, std::memory_order_release);
  }

  GatewayOptions options;
  std::vector<Topic> topics;
  net::EpollLoop loop;
  net::TcpAcceptor acceptor;
  std::unordered_map<int, std::unique_ptr<Connection>> connections;
  std::vector<int> pending_accepts;
  std::thread worker;
  std::atomic<bool> running{};
  std::atomic<bool> failed{};
  mutable std::mutex error_mutex;
  std::string error_text;
};

Gateway::Gateway(GatewayOptions options, std::vector<Topic> topics)
    : impl_(std::make_unique<Impl>(std::move(options), std::move(topics))) {}

Gateway::~Gateway() { stop(); }

bool Gateway::start() { return impl_ != nullptr && impl_->start(); }

void Gateway::stop() noexcept {
  if (impl_ != nullptr) {
    impl_->stop();
  }
}

bool Gateway::failed() const noexcept {
  return impl_ == nullptr || impl_->failed.load(std::memory_order_acquire);
}

std::string Gateway::error() const {
  if (impl_ == nullptr) {
    return "gateway is unavailable";
  }
  std::lock_guard lock(impl_->error_mutex);
  return impl_->error_text;
}

std::uint16_t Gateway::port() const noexcept {
  return impl_ == nullptr ? 0 : impl_->acceptor.port();
}

}  // namespace mds::gateway
