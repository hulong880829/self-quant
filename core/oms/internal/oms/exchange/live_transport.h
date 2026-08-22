#pragma once

#include "net/http_client.h"
#include "net/websocket_client.h"
#include "oms/exchange/binance/trade_adapter.h"
#include "oms/exchange/polymarket/trade_adapter.h"
#include "oms/exchange/trade_adapter.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <span>
#include <string>
#include <string_view>

namespace oms::exchange::live {

struct Endpoint {
  std::string host;
  std::string service{"443"};
};

struct Config {
  Endpoint rest;
  Endpoint websocket;
  net::SocketOptions socket_options{};
  std::size_t request_slots{8};
  std::size_t event_slots{32};
  std::size_t websocket_outbound_slots{16};
  std::size_t response_capacity{16U << 10U};
  std::size_t websocket_message_capacity{1U << 20U};
  std::size_t request_capacity{32U << 10U};
  std::size_t header_capacity{16U << 10U};
  std::size_t tls_receive_capacity{1U << 20U};
};

enum class SubmitResult : std::uint8_t {
  Accepted,
  WouldBlock,
  InvalidArgument,
};

enum class EventKind : std::uint8_t {
  HttpResponse,
  HttpFailure,
  WebSocketOpen,
  WebSocketMessage,
  WebSocketClosed,
};

enum class Failure : std::uint8_t {
  None,
  InvalidRequest,
  RequestTooLarge,
  Connect,
  Tls,
  Transport,
  InvalidResponse,
  ResponseTooLarge,
  Timeout,
  Disconnected,
  Backpressure,
};

struct Request {
  std::uint64_t id{};
  net::HttpMethod method{net::HttpMethod::Get};
  std::string_view target;
  std::string_view content_type;
  std::span<const std::byte> body;
  std::span<const net::HttpHeader> headers;
  std::chrono::steady_clock::time_point deadline;
};

struct Event {
  EventKind kind{EventKind::HttpFailure};
  Failure failure{Failure::None};
  std::uint64_t request_id{};
  unsigned status_code{};
  net::HttpStatusClass status_class{net::HttpStatusClass::Invalid};
  net::WsOpcode opcode{net::WsOpcode::Text};
  std::uint64_t retry_after_ms{};
  std::uint64_t used_weight_1m{};
  bool has_retry_after{};
  bool has_used_weight{};
  std::string_view payload;
};

enum class DescriptorKind : std::uint8_t { Http, WebSocket };

struct Descriptor {
  int fd{-1};
  std::uint32_t events{};
  std::uint64_t generation{};
  DescriptorKind kind{DescriptorKind::Http};
  std::uint32_t slot{};
};

// LiveTransport never owns an epoll instance. The owning event loop snapshots
// descriptors(), applies add/mod/del, and returns readiness through service().
// Socket generation rejects stale readiness after reconnect or slot reuse.
class LiveTransport : public AsyncIoDriver {
 public:
  using Clock = std::chrono::steady_clock;
  using EventCallback = bool (*)(void*, const Event&) noexcept;

  LiveTransport(net::SharedSslContext ssl_context, Config config);
  ~LiveTransport();
  LiveTransport(const LiveTransport&) = delete;
  LiveTransport& operator=(const LiveTransport&) = delete;
  LiveTransport(LiveTransport&&) noexcept;
  LiveTransport& operator=(LiveTransport&&) noexcept;

  [[nodiscard]] SubmitResult submit(const Request& request) noexcept;
  [[nodiscard]] bool start_websocket(
      std::string_view target, Clock::time_point deadline) noexcept;
  [[nodiscard]] SubmitResult send_websocket(
      net::WsOpcode opcode, std::span<const std::byte> payload) noexcept;
  void close_websocket(std::uint16_t code = 1000,
                       std::string_view reason = {}) noexcept;

  [[nodiscard]] std::size_t descriptors(
      std::span<Descriptor> output) const noexcept;
  void service(int fd, std::uint64_t generation, std::uint32_t events,
               Clock::time_point now = Clock::now()) noexcept;
  void check_timeouts(Clock::time_point now = Clock::now()) noexcept;

  [[nodiscard]] std::size_t descriptor_capacity() const noexcept override;
  [[nodiscard]] std::size_t snapshot_descriptors(
      std::span<AsyncIoDescriptor> output) const noexcept override;
  void service_io(int fd, std::uint64_t generation, std::uint32_t events,
                  std::uint64_t now_ns) noexcept override;
  void check_timeouts(std::uint64_t now_ns) noexcept override;
  [[nodiscard]] std::uint64_t next_deadline_ns() const noexcept override;

  // payload remains valid until the next poll() call.
  [[nodiscard]] bool poll(Event& event) noexcept;
  [[nodiscard]] std::size_t drain(void* context, EventCallback callback,
                                  std::size_t budget) noexcept;
  [[nodiscard]] std::size_t pending_events() const noexcept;
  [[nodiscard]] std::size_t active_requests() const noexcept;

  // Convenience conversion for the existing asynchronous Polymarket
  // Transport contract. Dedicate one LiveTransport instance to this wrapper.
  class PolymarketAdapter;
  class BinanceAdapter;

  // Converts Binance wire requests into asynchronous live requests.
  [[nodiscard]] SubmitResult submit_binance(
      std::uint64_t request_id, const binance::HttpRequest& request,
      Clock::time_point deadline) noexcept;
  static void binance_metadata(const Event& event,
                               binance::RateLimitMetadata& output) noexcept;

 private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

class LiveTransport::PolymarketAdapter final
    : public polymarket::Transport {
 public:
  explicit PolymarketAdapter(
      LiveTransport& transport,
      std::chrono::milliseconds request_timeout = std::chrono::seconds(5),
      std::string websocket_target = "/ws/user",
      std::string subscription_message = {});
  ~PolymarketAdapter() override;

  [[nodiscard]] AdapterResult submit(
      const polymarket::TransportRequest& request) noexcept override;
  [[nodiscard]] AdapterResult poll(
      polymarket::TransportEvent& event) noexcept override;
  [[nodiscard]] AdapterResult request_reconnect() noexcept override;
  [[nodiscard]] AdapterResult send_heartbeat() noexcept override;
  void close() noexcept override;

 private:
  LiveTransport* transport_{};
  std::chrono::milliseconds request_timeout_{};
  std::string websocket_target_;
  std::string subscription_message_;
};

class LiveTransport::BinanceAdapter final : public binance::Transport {
 public:
  explicit BinanceAdapter(
      LiveTransport& transport, LiveTransport& trading_transport,
      binance::Product product,
      std::chrono::milliseconds request_timeout = std::chrono::seconds(5))
      noexcept;

  [[nodiscard]] AdapterResult submit(
      const binance::TransportRequest& request) noexcept override;
  [[nodiscard]] AdapterResult poll(
      binance::TransportEvent& event) noexcept override;
  [[nodiscard]] AdapterResult start_user_stream(
      std::string_view listen_key) noexcept override;
  [[nodiscard]] AdapterResult start_trading_stream() noexcept override;
  void close() noexcept override;

 private:
  LiveTransport* transport_{};
  LiveTransport* trading_transport_{};
  binance::Product product_{binance::Product::Spot};
  std::chrono::milliseconds request_timeout_{};
  bool trading_ready_{};
  bool poll_trading_first_{};
  bool trading_loss_reported_{};
};

}  // namespace oms::exchange::live
