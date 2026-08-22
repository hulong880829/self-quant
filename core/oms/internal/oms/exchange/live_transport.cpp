#include "oms/exchange/live_transport.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cstring>
#include <limits>
#include <stdexcept>
#include <utility>
#include <vector>

#include <openssl/crypto.h>

namespace oms::exchange::live {
namespace {

Failure HttpFailure(net::HttpClientError error) noexcept {
  switch (error) {
    case net::HttpClientError::None:
      return Failure::None;
    case net::HttpClientError::InvalidRequest:
      return Failure::InvalidRequest;
    case net::HttpClientError::RequestTooLarge:
      return Failure::RequestTooLarge;
    case net::HttpClientError::Connect:
      return Failure::Connect;
    case net::HttpClientError::Tls:
      return Failure::Tls;
    case net::HttpClientError::Transport:
      return Failure::Transport;
    case net::HttpClientError::InvalidResponse:
      return Failure::InvalidResponse;
    case net::HttpClientError::ResponseTooLarge:
      return Failure::ResponseTooLarge;
    case net::HttpClientError::Timeout:
      return Failure::Timeout;
  }
  return Failure::Transport;
}

bool HttpTerminal(net::HttpClientState state) noexcept {
  return state == net::HttpClientState::Complete ||
         state == net::HttpClientState::TimedOut ||
         state == net::HttpClientState::Failed;
}

bool WebSocketTerminal(net::WebSocketClientState state) noexcept {
  return state == net::WebSocketClientState::Closed ||
         state == net::WebSocketClientState::TimedOut ||
         state == net::WebSocketClientState::Failed;
}

net::HttpMethod BinanceMethod(binance::HttpMethod method) noexcept {
  switch (method) {
    case binance::HttpMethod::Get:
      return net::HttpMethod::Get;
    case binance::HttpMethod::Post:
      return net::HttpMethod::Post;
    case binance::HttpMethod::Put:
      return net::HttpMethod::Put;
    case binance::HttpMethod::Delete:
      return net::HttpMethod::Delete;
  }
  return net::HttpMethod::Get;
}

bool ParseMethod(std::string_view method, net::HttpMethod& output) noexcept {
  if (method == "GET") {
    output = net::HttpMethod::Get;
  } else if (method == "POST") {
    output = net::HttpMethod::Post;
  } else if (method == "PUT") {
    output = net::HttpMethod::Put;
  } else if (method == "DELETE") {
    output = net::HttpMethod::Delete;
  } else {
    return false;
  }
  return true;
}

LiveTransport::Clock::time_point FromMonotonicNs(
    std::uint64_t value) noexcept {
  return LiveTransport::Clock::time_point(
      std::chrono::nanoseconds(value));
}

std::uint64_t ToMonotonicNs(
    LiveTransport::Clock::time_point value) noexcept {
  if (value == LiveTransport::Clock::time_point{}) return 0;
  const auto count =
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          value.time_since_epoch())
          .count();
  return count <= 0 ? 0 : static_cast<std::uint64_t>(count);
}

bool ParseUnsigned(std::string_view text, std::uint64_t& output) noexcept {
  if (text.empty()) return false;
  const auto parsed =
      std::from_chars(text.data(), text.data() + text.size(), output);
  return parsed.ec == std::errc{} &&
         parsed.ptr == text.data() + text.size();
}

}  // namespace

class LiveTransport::Impl {
 public:
  struct RequestSlot {
    RequestSlot(net::SharedSslContext context, const Config& config)
        : client(std::move(context), config.header_capacity,
                 config.response_capacity, config.request_capacity,
                 config.rest.host.size(), config.tls_receive_capacity) {
      client.set_socket_options(config.socket_options);
    }

    net::HttpClient client;
    std::uint64_t request_id{};
    Clock::time_point deadline{};
    bool active{};
  };

  struct EventSlot {
    explicit EventSlot(std::size_t capacity) : payload(capacity) {}

    Event event{};
    std::vector<std::byte> payload;
    std::size_t payload_size{};
    bool used{};
  };

  struct OutboundSlot {
    explicit OutboundSlot(std::size_t capacity) : payload(capacity) {}

    net::WsOpcode opcode{net::WsOpcode::Text};
    std::vector<std::byte> payload;
    std::size_t size{};
  };

  Impl(net::SharedSslContext context, Config transport_config)
      : config(std::move(transport_config)),
        websocket(context, config.websocket_message_capacity,
                  config.header_capacity, config.request_capacity,
                  config.websocket.host.size(), config.tls_receive_capacity),
        event_queue(config.event_slots) {
    if (!context || config.rest.host.empty() || config.rest.service.empty() ||
        config.request_slots == 0 || config.event_slots == 0 ||
        config.response_capacity == 0 ||
        config.websocket_message_capacity == 0 ||
        config.request_capacity == 0 || config.header_capacity == 0 ||
        config.tls_receive_capacity == 0) {
      throw std::invalid_argument("invalid live transport configuration");
    }
    websocket.set_socket_options(config.socket_options);
    requests.reserve(config.request_slots);
    for (std::size_t index = 0; index < config.request_slots; ++index) {
      requests.push_back(std::make_unique<RequestSlot>(context, config));
    }
    const std::size_t event_payload_capacity =
        std::max(config.response_capacity, config.websocket_message_capacity);
    event_slots.reserve(config.event_slots);
    for (std::size_t index = 0; index < config.event_slots; ++index) {
      event_slots.push_back(
          std::make_unique<EventSlot>(event_payload_capacity));
    }
    outbound_slots.reserve(config.websocket_outbound_slots);
    for (std::size_t index = 0;
         index < config.websocket_outbound_slots; ++index) {
      outbound_slots.push_back(std::make_unique<OutboundSlot>(
          config.websocket_message_capacity));
    }
    websocket.set_frame_callback(
        [this](const net::WsFrameView& frame) noexcept {
          if (frame.opcode != net::WsOpcode::Text &&
              frame.opcode != net::WsOpcode::Binary) {
            return true;
          }
          Event event{};
          event.kind = EventKind::WebSocketMessage;
          event.opcode = frame.opcode;
          return enqueue(event, frame.payload);
        });
  }

  bool enqueue(Event event, std::span<const std::byte> payload = {}) noexcept {
    if (event_count == event_queue.size()) return false;
    EventSlot* target = nullptr;
    std::size_t index = 0;
    for (; index < event_slots.size(); ++index) {
      if (!event_slots[index]->used) {
        target = event_slots[index].get();
        break;
      }
    }
    if (target == nullptr || payload.size() > target->payload.size()) {
      return false;
    }
    if (!payload.empty()) {
      std::copy(payload.begin(), payload.end(), target->payload.begin());
    }
    target->payload_size = payload.size();
    target->event = event;
    target->event.payload = {
        reinterpret_cast<const char*>(target->payload.data()),
        target->payload_size};
    target->used = true;
    const std::size_t tail = (event_head + event_count) % event_queue.size();
    event_queue[tail] = index;
    ++event_count;
    return true;
  }

  bool finish_http(RequestSlot& slot) noexcept {
    Event event{};
    event.request_id = slot.request_id;
    std::span<const std::byte> payload;
    if (slot.client.state() == net::HttpClientState::Complete) {
      event.kind = EventKind::HttpResponse;
      event.status_code = slot.client.response().status_code();
      event.status_class = slot.client.response().status_class();
      std::uint64_t retry_after_seconds = 0;
      if (ParseUnsigned(
              slot.client.response().header_value("Retry-After"),
              retry_after_seconds)) {
        event.has_retry_after = true;
        event.retry_after_ms =
            retry_after_seconds >
                    std::numeric_limits<std::uint64_t>::max() / 1000U
                ? std::numeric_limits<std::uint64_t>::max()
                : retry_after_seconds * 1000U;
      }
      event.has_used_weight = ParseUnsigned(
          slot.client.response().header_value("X-MBX-USED-WEIGHT-1M"),
          event.used_weight_1m);
      payload = slot.client.response().body();
    } else {
      event.kind = EventKind::HttpFailure;
      event.failure = HttpFailure(slot.client.error_code());
    }
    if (!enqueue(event, payload)) return false;
    slot.client.reset();
    slot.request_id = 0;
    slot.deadline = {};
    slot.active = false;
    return true;
  }

  void finish_terminals() noexcept {
    for (const auto& owned : requests) {
      RequestSlot& slot = *owned;
      if (slot.active && HttpTerminal(slot.client.state())) {
        (void)finish_http(slot);
      }
    }
    if (websocket_terminal_pending) {
      Event event{};
      event.kind = EventKind::WebSocketClosed;
      event.failure =
          websocket.state() == net::WebSocketClientState::TimedOut
              ? Failure::Timeout
              : (websocket.state() == net::WebSocketClientState::Closed
                     ? Failure::Disconnected
                     : Failure::Transport);
      if (enqueue(event)) websocket_terminal_pending = false;
    }
  }

  void observe_websocket_state(net::WebSocketClientState before) noexcept {
    const net::WebSocketClientState after = websocket.state();
    if (after == net::WebSocketClientState::Open && !websocket_open_reported) {
      Event event{};
      event.kind = EventKind::WebSocketOpen;
      if (enqueue(event)) websocket_open_reported = true;
      websocket_deadline = {};
    }
    if (!WebSocketTerminal(before) && WebSocketTerminal(after) &&
        !websocket_terminal_reported) {
      websocket_terminal_reported = true;
      websocket_terminal_pending = true;
    }
  }

  void pump_outbound() noexcept {
    while (outbound_count != 0 && websocket.can_send_data()) {
      OutboundSlot& slot = *outbound_slots[outbound_head];
      std::string_view ignored;
      if (!websocket.send(slot.opcode,
                          {slot.payload.data(), slot.size}, ignored)) {
        break;
      }
      slot.size = 0;
      outbound_head = (outbound_head + 1) % outbound_slots.size();
      --outbound_count;
    }
  }

  Config config;
  std::vector<std::unique_ptr<RequestSlot>> requests;
  net::WebSocketClient websocket;
  std::vector<std::unique_ptr<EventSlot>> event_slots;
  std::vector<std::size_t> event_queue;
  std::size_t event_head{};
  std::size_t event_count{};
  std::size_t delivered_slot{std::numeric_limits<std::size_t>::max()};
  std::vector<std::unique_ptr<OutboundSlot>> outbound_slots;
  std::size_t outbound_head{};
  std::size_t outbound_count{};
  bool websocket_open_reported{};
  bool websocket_terminal_reported{};
  bool websocket_terminal_pending{};
  Clock::time_point websocket_deadline{};
};

LiveTransport::LiveTransport(net::SharedSslContext ssl_context, Config config)
    : impl_(std::make_unique<Impl>(std::move(ssl_context),
                                  std::move(config))) {}

LiveTransport::~LiveTransport() = default;
LiveTransport::LiveTransport(LiveTransport&&) noexcept = default;
LiveTransport& LiveTransport::operator=(LiveTransport&&) noexcept = default;

SubmitResult LiveTransport::submit(const Request& request) noexcept {
  Impl::RequestSlot* available = nullptr;
  for (const auto& owned : impl_->requests) {
    if (!owned->active) {
      available = owned.get();
      break;
    }
  }
  if (available == nullptr) return SubmitResult::WouldBlock;
  if (request.id == 0 || request.target.empty() ||
      request.deadline == Clock::time_point{}) {
    return SubmitResult::InvalidArgument;
  }
  available->active = true;
  available->request_id = request.id;
  available->deadline = request.deadline;
  const net::HttpRequest wire{request.method, request.target,
                              request.content_type, request.body,
                              request.headers};
  const bool started = available->client.start_request(
      impl_->config.rest.host, impl_->config.rest.service, wire,
      request.deadline);
  if (!started &&
      (available->client.error_code() == net::HttpClientError::InvalidRequest ||
       available->client.error_code() ==
           net::HttpClientError::RequestTooLarge)) {
    available->client.reset();
    available->request_id = 0;
    available->deadline = {};
    available->active = false;
    return SubmitResult::InvalidArgument;
  }
  if (HttpTerminal(available->client.state())) {
    (void)impl_->finish_http(*available);
  }
  return SubmitResult::Accepted;
}

bool LiveTransport::start_websocket(std::string_view target,
                                    Clock::time_point deadline) noexcept {
  if (impl_->config.websocket.host.empty() ||
      impl_->config.websocket.service.empty()) {
    return false;
  }
  impl_->websocket_open_reported = false;
  impl_->websocket_terminal_reported = false;
  impl_->websocket_terminal_pending = false;
  // Outbound frames belong to the socket generation that accepted them.
  // Never replay an uncertain order/subscription frame after reconnect.
  for (const auto& slot : impl_->outbound_slots) slot->size = 0;
  impl_->outbound_head = 0;
  impl_->outbound_count = 0;
  impl_->websocket_deadline = deadline;
  const net::WebSocketClientState before = impl_->websocket.state();
  const bool started =
      impl_->websocket.start(impl_->config.websocket.host,
                             impl_->config.websocket.service, target, deadline);
  impl_->observe_websocket_state(before);
  if (!started || WebSocketTerminal(impl_->websocket.state())) {
    impl_->websocket_deadline = {};
  }
  return started;
}

SubmitResult LiveTransport::send_websocket(
    net::WsOpcode opcode, std::span<const std::byte> payload) noexcept {
  if ((opcode != net::WsOpcode::Text && opcode != net::WsOpcode::Binary) ||
      payload.size() > impl_->config.websocket_message_capacity) {
    return SubmitResult::InvalidArgument;
  }
  if (impl_->outbound_slots.empty() ||
      impl_->outbound_count == impl_->outbound_slots.size()) {
    return SubmitResult::WouldBlock;
  }
  const std::size_t tail =
      (impl_->outbound_head + impl_->outbound_count) %
      impl_->outbound_slots.size();
  Impl::OutboundSlot& slot = *impl_->outbound_slots[tail];
  std::copy(payload.begin(), payload.end(), slot.payload.begin());
  slot.size = payload.size();
  slot.opcode = opcode;
  ++impl_->outbound_count;
  impl_->pump_outbound();
  return SubmitResult::Accepted;
}

void LiveTransport::close_websocket(std::uint16_t code,
                                    std::string_view reason) noexcept {
  std::string_view ignored;
  (void)impl_->websocket.close(code, reason, ignored);
}

std::size_t LiveTransport::descriptors(
    std::span<Descriptor> output) const noexcept {
  std::size_t count = 0;
  for (std::size_t index = 0; index < impl_->requests.size(); ++index) {
    const Impl::RequestSlot& slot = *impl_->requests[index];
    if (!slot.active || slot.client.fd() < 0 ||
        slot.client.wanted_events() == 0) {
      continue;
    }
    if (count == output.size()) return count;
    output[count++] = {slot.client.fd(), slot.client.wanted_events(),
                       slot.client.socket_generation(), DescriptorKind::Http,
                       static_cast<std::uint32_t>(index)};
  }
  if (impl_->websocket.fd() >= 0 && impl_->websocket.wanted_events() != 0 &&
      count < output.size()) {
    output[count++] = {
        impl_->websocket.fd(), impl_->websocket.wanted_events(),
        impl_->websocket.socket_generation(), DescriptorKind::WebSocket, 0};
  }
  return count;
}

std::size_t LiveTransport::descriptor_capacity() const noexcept {
  return impl_->requests.size() + 1;
}

std::size_t LiveTransport::snapshot_descriptors(
    std::span<AsyncIoDescriptor> output) const noexcept {
  std::size_t count = 0;
  for (const auto& owned : impl_->requests) {
    const Impl::RequestSlot& slot = *owned;
    if (!slot.active || slot.client.fd() < 0 ||
        slot.client.wanted_events() == 0) {
      continue;
    }
    if (count == output.size()) return count;
    output[count++] = {slot.client.fd(), slot.client.wanted_events(),
                       slot.client.socket_generation()};
  }
  if (impl_->websocket.fd() >= 0 &&
      impl_->websocket.wanted_events() != 0 && count < output.size()) {
    output[count++] = {impl_->websocket.fd(),
                       impl_->websocket.wanted_events(),
                       impl_->websocket.socket_generation()};
  }
  return count;
}

void LiveTransport::service(int fd, std::uint64_t generation,
                            std::uint32_t events, Clock::time_point now) noexcept {
  for (const auto& owned : impl_->requests) {
    Impl::RequestSlot& slot = *owned;
    if (!slot.active || slot.client.fd() != fd ||
        slot.client.socket_generation() != generation) {
      continue;
    }
    slot.client.on_event(events, now);
    if (HttpTerminal(slot.client.state())) (void)impl_->finish_http(slot);
    return;
  }
  if (impl_->websocket.fd() != fd ||
      impl_->websocket.socket_generation() != generation) {
    return;
  }
  const net::WebSocketClientState before = impl_->websocket.state();
  impl_->websocket.on_event(events, now);
  impl_->observe_websocket_state(before);
  impl_->pump_outbound();
  impl_->finish_terminals();
}

void LiveTransport::service_io(int fd, std::uint64_t generation,
                               std::uint32_t events,
                               std::uint64_t now_ns) noexcept {
  service(fd, generation, events, FromMonotonicNs(now_ns));
}

void LiveTransport::check_timeouts(Clock::time_point now) noexcept {
  for (const auto& owned : impl_->requests) {
    Impl::RequestSlot& slot = *owned;
    if (!slot.active) continue;
    slot.client.check_timeout(now);
  }
  const net::WebSocketClientState before = impl_->websocket.state();
  impl_->websocket.check_timeout(now);
  impl_->observe_websocket_state(before);
  if (WebSocketTerminal(impl_->websocket.state())) {
    impl_->websocket_deadline = {};
  }
  impl_->finish_terminals();
}

void LiveTransport::check_timeouts(std::uint64_t now_ns) noexcept {
  check_timeouts(FromMonotonicNs(now_ns));
}

std::uint64_t LiveTransport::next_deadline_ns() const noexcept {
  Clock::time_point earliest{};
  const auto consider = [&earliest](Clock::time_point candidate) noexcept {
    if (candidate != Clock::time_point{} &&
        (earliest == Clock::time_point{} || candidate < earliest)) {
      earliest = candidate;
    }
  };
  for (const auto& owned : impl_->requests) {
    if (owned->active) consider(owned->deadline);
  }
  if (!WebSocketTerminal(impl_->websocket.state())) {
    consider(impl_->websocket_deadline);
  }
  return ToMonotonicNs(earliest);
}

bool LiveTransport::poll(Event& event) noexcept {
  if (impl_->delivered_slot != std::numeric_limits<std::size_t>::max()) {
    impl_->event_slots[impl_->delivered_slot]->used = false;
    impl_->delivered_slot = std::numeric_limits<std::size_t>::max();
  }
  impl_->finish_terminals();
  if (impl_->event_count == 0) return false;
  const std::size_t index = impl_->event_queue[impl_->event_head];
  impl_->event_head = (impl_->event_head + 1) % impl_->event_queue.size();
  --impl_->event_count;
  impl_->delivered_slot = index;
  event = impl_->event_slots[index]->event;
  return true;
}

std::size_t LiveTransport::drain(void* context, EventCallback callback,
                                 std::size_t budget) noexcept {
  if (callback == nullptr) return 0;
  std::size_t delivered = 0;
  Event event{};
  while (delivered < budget && poll(event)) {
    if (!callback(context, event)) break;
    ++delivered;
  }
  return delivered;
}

std::size_t LiveTransport::pending_events() const noexcept {
  return impl_->event_count;
}

std::size_t LiveTransport::active_requests() const noexcept {
  return static_cast<std::size_t>(std::count_if(
      impl_->requests.begin(), impl_->requests.end(),
      [](const auto& slot) noexcept { return slot->active; }));
}

SubmitResult LiveTransport::submit_binance(
    std::uint64_t request_id, const binance::HttpRequest& request,
    Clock::time_point deadline) noexcept {
  net::HttpHeader header{};
  std::span<const net::HttpHeader> headers;
  if (!request.api_key_header().empty()) {
    header = {request.api_key_header(), request.api_key_value};
    headers = {&header, 1};
  }
  return submit({request_id, BinanceMethod(request.method),
                 request.target_view(), {}, {}, headers, deadline});
}

void LiveTransport::binance_metadata(
    const Event& event, binance::RateLimitMetadata& output) noexcept {
  output = {};
  if (event.kind == EventKind::HttpResponse) {
    output.http_status = event.status_code;
    output.retry_after_ms = event.retry_after_ms;
    output.used_weight_1m = event.used_weight_1m;
    output.has_retry_after = event.has_retry_after;
    output.has_used_weight = event.has_used_weight;
  }
}

LiveTransport::PolymarketAdapter::PolymarketAdapter(
    LiveTransport& transport,
    std::chrono::milliseconds request_timeout, std::string websocket_target,
    std::string subscription_message)
    : transport_(&transport),
      request_timeout_(request_timeout),
      websocket_target_(std::move(websocket_target)),
      subscription_message_(std::move(subscription_message)) {}

LiveTransport::PolymarketAdapter::~PolymarketAdapter() {
  if (!subscription_message_.empty()) {
    OPENSSL_cleanse(subscription_message_.data(),
                    subscription_message_.size());
  }
}

AdapterResult LiveTransport::PolymarketAdapter::submit(
    const polymarket::TransportRequest& request) noexcept {
  net::HttpMethod method{};
  if (!ParseMethod({request.wire.method.data(), request.wire.method_size},
                   method)) {
    return AdapterResult::InvalidArgument;
  }
  const std::array<net::HttpHeader, 5> headers{{
      {"POLY_ADDRESS", request.poly_address.view()},
      {"POLY_SIGNATURE", request.poly_signature.view()},
      {"POLY_TIMESTAMP", request.poly_timestamp.view()},
      {"POLY_API_KEY", request.poly_api_key.view()},
      {"POLY_PASSPHRASE", request.poly_passphrase.view()},
  }};
  const auto body = std::span<const std::byte>(
      reinterpret_cast<const std::byte*>(request.wire.body.data()),
      request.wire.body_size);
  const SubmitResult result = transport_->submit(
      {request.id, method,
       {request.wire.path.data(), request.wire.path_size},
       body.empty() ? std::string_view{} : std::string_view{"application/json"},
       body, headers, Clock::now() + request_timeout_});
  switch (result) {
    case SubmitResult::Accepted:
      return AdapterResult::Ok;
    case SubmitResult::WouldBlock:
      return AdapterResult::WouldBlock;
    case SubmitResult::InvalidArgument:
      return AdapterResult::InvalidArgument;
  }
  return AdapterResult::Failed;
}

AdapterResult LiveTransport::PolymarketAdapter::poll(
    polymarket::TransportEvent& event) noexcept {
  Event source{};
  if (!transport_->poll(source)) return AdapterResult::WouldBlock;
  switch (source.kind) {
    case EventKind::WebSocketOpen:
      if (!subscription_message_.empty()) {
        const auto payload = std::span<const std::byte>(
            reinterpret_cast<const std::byte*>(subscription_message_.data()),
            subscription_message_.size());
        if (transport_->send_websocket(net::WsOpcode::Text, payload) !=
            SubmitResult::Accepted) {
          transport_->close_websocket(1011, "subscription backpressure");
          return AdapterResult::WouldBlock;
        }
      }
      event = {polymarket::TransportEventKind::SessionReady, 0, 0, {}};
      break;
    case EventKind::WebSocketClosed:
      event = {polymarket::TransportEventKind::SessionLost, 0, 0, {}};
      break;
    case EventKind::WebSocketMessage:
      event = {polymarket::TransportEventKind::UserMessage, 0, 0,
               source.payload};
      break;
    case EventKind::HttpResponse:
      event = {polymarket::TransportEventKind::HttpResponse,
               static_cast<std::uint32_t>(source.request_id),
               static_cast<std::uint16_t>(source.status_code), source.payload};
      break;
    case EventKind::HttpFailure:
      event = {polymarket::TransportEventKind::HttpResponse,
               static_cast<std::uint32_t>(source.request_id), 0, {}};
      break;
  }
  return AdapterResult::Ok;
}

AdapterResult LiveTransport::PolymarketAdapter::request_reconnect() noexcept {
  if (websocket_target_.empty()) return AdapterResult::InvalidArgument;
  return transport_->start_websocket(
             websocket_target_, Clock::now() + request_timeout_)
             ? AdapterResult::Ok
             : AdapterResult::Failed;
}

AdapterResult LiveTransport::PolymarketAdapter::send_heartbeat() noexcept {
  constexpr std::string_view heartbeat = "PING";
  const auto payload = std::span<const std::byte>(
      reinterpret_cast<const std::byte*>(heartbeat.data()), heartbeat.size());
  switch (transport_->send_websocket(net::WsOpcode::Text, payload)) {
    case SubmitResult::Accepted:
      return AdapterResult::Ok;
    case SubmitResult::WouldBlock:
      return AdapterResult::WouldBlock;
    case SubmitResult::InvalidArgument:
      return AdapterResult::InvalidArgument;
  }
  return AdapterResult::Failed;
}

void LiveTransport::PolymarketAdapter::close() noexcept {
  transport_->close_websocket();
}

LiveTransport::BinanceAdapter::BinanceAdapter(
    LiveTransport& transport, LiveTransport& trading_transport,
    binance::Product product,
    std::chrono::milliseconds request_timeout) noexcept
    : transport_(&transport),
      trading_transport_(&trading_transport),
      product_(product),
      request_timeout_(request_timeout) {}

AdapterResult LiveTransport::BinanceAdapter::submit(
    const binance::TransportRequest& request) noexcept {
  if (request.use_trading_websocket) {
    if (!trading_ready_) return AdapterResult::NotReady;
    const std::string_view payload = request.trading.payload_view();
    if (payload.empty()) return AdapterResult::InvalidArgument;
    const auto bytes = std::span<const std::byte>(
        reinterpret_cast<const std::byte*>(payload.data()), payload.size());
    switch (trading_transport_->send_websocket(net::WsOpcode::Text, bytes)) {
      case SubmitResult::Accepted:
        return AdapterResult::Ok;
      case SubmitResult::WouldBlock:
        return AdapterResult::WouldBlock;
      case SubmitResult::InvalidArgument:
        return AdapterResult::InvalidArgument;
    }
    return AdapterResult::Failed;
  }
  const SubmitResult result = transport_->submit_binance(
      request.id, request.wire, Clock::now() + request_timeout_);
  switch (result) {
    case SubmitResult::Accepted:
      return AdapterResult::Ok;
    case SubmitResult::WouldBlock:
      return AdapterResult::WouldBlock;
    case SubmitResult::InvalidArgument:
      return AdapterResult::InvalidArgument;
  }
  return AdapterResult::Failed;
}

AdapterResult LiveTransport::BinanceAdapter::poll(
    binance::TransportEvent& event) noexcept {
  Event source{};
  bool trading = false;
  if (poll_trading_first_) {
    if (trading_transport_->poll(source)) {
      trading = true;
    } else if (!transport_->poll(source)) {
      return AdapterResult::WouldBlock;
    }
  } else if (!transport_->poll(source)) {
    if (!trading_transport_->poll(source)) return AdapterResult::WouldBlock;
    trading = true;
  }
  poll_trading_first_ = !trading;
  event = {};
  if (trading) {
    switch (source.kind) {
      case EventKind::WebSocketOpen:
        trading_ready_ = true;
        trading_loss_reported_ = false;
        event.kind = binance::TransportEventKind::TradingSessionReady;
        event.result = AdapterResult::Ok;
        return AdapterResult::Ok;
      case EventKind::WebSocketClosed:
        trading_ready_ = false;
        if (trading_loss_reported_) {
          trading_loss_reported_ = false;
          return AdapterResult::WouldBlock;
        }
        event.kind = binance::TransportEventKind::TradingSessionLost;
        event.result = AdapterResult::NotReady;
        return AdapterResult::Ok;
      case EventKind::WebSocketMessage: {
        std::uint32_t status = 0;
        if (binance::parse_trading_response(
                source.payload, event.request_id, status, event.payload) !=
            binance::ParseResult::Ok) {
          trading_ready_ = false;
          trading_loss_reported_ = true;
          trading_transport_->close_websocket(1002,
                                               "invalid trading response");
          event = {};
          event.kind = binance::TransportEventKind::TradingSessionLost;
          event.result = AdapterResult::Failed;
          return AdapterResult::Ok;
        }
        event.kind = binance::TransportEventKind::Response;
        event.result = AdapterResult::Ok;
        event.metadata.http_status = status;
        return AdapterResult::Ok;
      }
      case EventKind::HttpResponse:
      case EventKind::HttpFailure:
        return AdapterResult::InvalidArgument;
    }
  }
  switch (source.kind) {
    case EventKind::HttpResponse:
      event.kind = binance::TransportEventKind::Response;
      event.request_id = static_cast<std::uint32_t>(source.request_id);
      event.result = AdapterResult::Ok;
      event.payload = source.payload;
      LiveTransport::binance_metadata(source, event.metadata);
      break;
    case EventKind::HttpFailure:
      event.kind = binance::TransportEventKind::Failure;
      event.request_id = static_cast<std::uint32_t>(source.request_id);
      event.result = AdapterResult::Failed;
      LiveTransport::binance_metadata(source, event.metadata);
      break;
    case EventKind::WebSocketOpen:
      event.kind = binance::TransportEventKind::SessionReady;
      event.result = AdapterResult::Ok;
      break;
    case EventKind::WebSocketClosed:
      event.kind = binance::TransportEventKind::SessionLost;
      event.result = AdapterResult::NotReady;
      break;
    case EventKind::WebSocketMessage:
      event.kind = binance::TransportEventKind::UserMessage;
      event.result = AdapterResult::Ok;
      event.payload = source.payload;
      break;
  }
  return AdapterResult::Ok;
}

AdapterResult LiveTransport::BinanceAdapter::start_user_stream(
    std::string_view listen_key) noexcept {
  std::array<char, binance::kMaxListenKeyBytes + 4> target{};
  if (listen_key.empty() || listen_key.size() > target.size() - 4)
    return AdapterResult::InvalidArgument;
  std::memcpy(target.data(), "/ws/", 4);
  std::memcpy(target.data() + 4, listen_key.data(), listen_key.size());
  return transport_->start_websocket(
             {target.data(), listen_key.size() + 4},
             Clock::now() + request_timeout_)
             ? AdapterResult::Ok
             : AdapterResult::Failed;
}

AdapterResult LiveTransport::BinanceAdapter::start_trading_stream() noexcept {
  trading_ready_ = false;
  const std::string_view target =
      product_ == binance::Product::Spot ? "/ws-api/v3" : "/ws-fapi/v1";
  return trading_transport_->start_websocket(
             target, Clock::now() + request_timeout_)
             ? AdapterResult::Ok
             : AdapterResult::Failed;
}

void LiveTransport::BinanceAdapter::close() noexcept {
  transport_->close_websocket();
  trading_transport_->close_websocket();
  trading_ready_ = false;
}

}  // namespace oms::exchange::live
