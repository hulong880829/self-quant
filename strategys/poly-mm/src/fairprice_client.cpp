#include "polymm/fairprice_client.h"

#include <array>
#include <charconv>
#include <chrono>
#include <cmath>
#include <cstring>
#include <poll.h>
#include <string_view>
#include <sys/epoll.h>
#include <vector>

#include <simdjson.h>

#include "net/tls_websocket.h"
#include "net/websocket_client.h"

namespace polymm {
namespace {

using Clock = std::chrono::steady_clock;

std::uint64_t WallNowNs() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

std::uint32_t EpollEvents(short events) noexcept {
  std::uint32_t result{};
  if ((events & POLLIN) != 0) result |= EPOLLIN;
  if ((events & POLLOUT) != 0) result |= EPOLLOUT;
  if ((events & POLLERR) != 0) result |= EPOLLERR;
  if ((events & POLLHUP) != 0) result |= EPOLLHUP;
  return result;
}

bool Terminal(net::WebSocketClientState state) noexcept {
  return state == net::WebSocketClientState::Closed ||
         state == net::WebSocketClientState::TimedOut ||
         state == net::WebSocketClientState::Failed;
}

bool Uint64Text(simdjson::dom::element element,
                std::uint64_t& output) noexcept {
  auto number = element.get_uint64();
  if (!number.error()) {
    output = number.value();
    return true;
  }
  auto text = element.get_string();
  if (text.error()) return false;
  const auto value = std::string_view(text.value());
  const auto parsed =
      std::from_chars(value.data(), value.data() + value.size(), output);
  return parsed.ec == std::errc{} &&
         parsed.ptr == value.data() + value.size();
}

bool Fixed(simdjson::dom::element element, double& output) noexcept {
  auto object = element.get_object();
  if (object.error()) return false;
  auto mantissa = object.value()["mantissa"].get_string();
  auto scale = object.value()["scale"].get_uint64();
  if (mantissa.error() || scale.error() || scale.value() > 18) return false;
  const auto text = std::string_view(mantissa.value());
  std::int64_t value{};
  const auto parsed =
      std::from_chars(text.data(), text.data() + text.size(), value);
  if (parsed.ec != std::errc{} ||
      parsed.ptr != text.data() + text.size()) {
    return false;
  }
  double divisor = 1.0;
  for (std::uint64_t index = 0; index < scale.value(); ++index)
    divisor *= 10.0;
  output = static_cast<double>(value) / divisor;
  return std::isfinite(output);
}

}  // namespace

FairPriceClient::FairPriceClient(SidecarConfig config)
    : config_(std::move(config)) {}

FairPriceClient::~FairPriceClient() { stop(); }

bool FairPriceClient::start() {
  bool expected = false;
  if (!started_.compare_exchange_strong(expected, true)) return false;
  thread_ = std::jthread([this](std::stop_token stop) { run(stop); });
  return true;
}

void FairPriceClient::stop() noexcept {
  if (thread_.joinable()) {
    thread_.request_stop();
    thread_.join();
  }
  started_.store(false, std::memory_order_release);
}

bool FairPriceClient::publish(const SidecarEvent& event) noexcept {
  auto lease = queue_.try_reserve();
  if (!lease) {
    queue_drops_.fetch_add(1, std::memory_order_relaxed);
    return false;
  }
  lease->emplace(event);
  return lease->commit();
}

bool FairPriceClient::try_pop(SidecarEvent& event) noexcept {
  auto lease = queue_.try_peek();
  if (!lease) return false;
  event = **lease;
  lease->release();
  return true;
}

SidecarMetrics FairPriceClient::metrics() const noexcept {
  return {fairprice_messages_.load(std::memory_order_relaxed),
          fairprice_parse_errors_.load(std::memory_order_relaxed),
          fairprice_reconnects_.load(std::memory_order_relaxed),
          queue_drops_.load(std::memory_order_relaxed),
          last_fairprice_lag_ns_.load(std::memory_order_relaxed)};
}

void FairPriceClient::run(std::stop_token stop) {
  std::string ssl_error;
  auto ssl = net::make_client_ssl_context(ssl_error);
  if (!ssl) {
    publish({SidecarEvent::Kind::FairPriceDisconnected, {}});
    return;
  }
  net::WebSocketClient websocket(ssl);
  websocket.set_origin(config_.fairprice_origin);
  websocket.set_secure(config_.fairprice_secure);
  simdjson::dom::parser fair_parser;
  bool subscribed = false;
  bool ws_terminal_reported = false;
  auto next_ws = Clock::now();
  auto next_ping = Clock::now() + std::chrono::seconds(10);

  websocket.set_frame_callback([&](const net::WsFrameView& frame) {
    if (frame.opcode != net::WsOpcode::Text || !frame.final) return true;
    auto document = fair_parser.parse(
        reinterpret_cast<const char*>(frame.payload.data()),
        frame.payload.size());
    if (document.error()) {
      fairprice_parse_errors_.fetch_add(1, std::memory_order_relaxed);
      return true;
    }
    auto object = document.value().get_object();
    if (object.error()) return true;
    auto channel_element = object.value()["channel"];
    if (channel_element.error()) return true;
    auto channel = channel_element.value().get_string();
    if (channel.error() || std::string_view(channel.value()) != "fairprice")
      return true;
    auto price_raw = object.value()["price_raw"];
    auto microprice = object.value()["microprice"];
    auto wall_ns = object.value()["wall_ns"];
    if (price_raw.error() || microprice.error() || wall_ns.error())
      return true;
    FairPriceEvent fair;
    if (!Fixed(price_raw.value(), fair.price_raw) ||
        !Fixed(microprice.value(), fair.microprice) ||
        !Uint64Text(wall_ns.value(), fair.wall_ns)) {
      fairprice_parse_errors_.fetch_add(1, std::memory_order_relaxed);
      return true;
    }
    fair.received_ns = WallNowNs();
    last_fairprice_lag_ns_.store(
        fair.received_ns >= fair.wall_ns ? fair.received_ns - fair.wall_ns
                                        : 0,
        std::memory_order_relaxed);
    SidecarEvent event;
    event.kind = SidecarEvent::Kind::FairPrice;
    event.fair_price = fair;
    publish(event);
    fairprice_messages_.fetch_add(1, std::memory_order_relaxed);
    return true;
  });

  while (!stop.stop_requested()) {
    const auto now = Clock::now();
    if ((websocket.state() == net::WebSocketClientState::Idle ||
         Terminal(websocket.state())) &&
        now >= next_ws) {
      if (websocket.state() != net::WebSocketClientState::Idle) {
        websocket.reset();
        fairprice_reconnects_.fetch_add(1, std::memory_order_relaxed);
      }
      std::string target = config_.fairprice_target;
      if (!config_.fairprice_token.empty()) {
        target += target.find('?') == std::string::npos ? "?token=" : "&token=";
        target += config_.fairprice_token;
      }
      subscribed = false;
      ws_terminal_reported = false;
      if (!websocket.start(
              config_.fairprice_host, config_.fairprice_service, target,
              now + std::chrono::milliseconds(config_.connect_timeout_ms))) {
        next_ws = now + std::chrono::seconds(1);
      }
    }
    if (websocket.state() == net::WebSocketClientState::Open && !subscribed &&
        websocket.can_send_data()) {
      std::string control =
          "{\"op\":\"subscribe\",\"profile\":\"" +
          config_.fairprice_profile + "\",\"symbol\":\"" +
          config_.fairprice_symbol +
          "\",\"channel\":\"fairprice\",\"depth\":0}";
      const auto payload = std::span(
          reinterpret_cast<const std::byte*>(control.data()), control.size());
      std::string_view error;
      subscribed = websocket.send(net::WsOpcode::Text, payload, error);
    }
    if (websocket.state() == net::WebSocketClientState::Open &&
        now >= next_ping && websocket.can_send_data()) {
      std::string_view error;
      (void)websocket.ping({}, error);
      next_ping = now + std::chrono::seconds(10);
    }

    std::array<pollfd, 1> descriptors{};
    nfds_t count = 0;
    if (websocket.fd() >= 0) {
      descriptors[count++] = {
          websocket.fd(),
          static_cast<short>(
              ((websocket.wanted_events() & EPOLLIN) != 0 ? POLLIN : 0) |
              ((websocket.wanted_events() & EPOLLOUT) != 0 ? POLLOUT : 0)),
          0};
    }
    (void)::poll(descriptors.data(), count, 20);
    nfds_t index = 0;
    if (websocket.fd() >= 0 && index < count) {
      if (descriptors[index].revents != 0)
        websocket.on_event(EpollEvents(descriptors[index].revents));
      ++index;
    }
    websocket.check_timeout();
    if (Terminal(websocket.state()) && !ws_terminal_reported) {
      publish({SidecarEvent::Kind::FairPriceDisconnected, {}});
      next_ws = Clock::now() + std::chrono::seconds(1);
      ws_terminal_reported = true;
    }
  }
  websocket.reset();
}

}  // namespace polymm
