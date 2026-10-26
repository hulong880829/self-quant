#include "polymm/fairprice_client.h"

#include <array>
#include <charconv>
#include <chrono>
#include <cmath>
#include <cstring>
#include <poll.h>
#include <string>
#include <string_view>
#include <sys/epoll.h>
#include <vector>

#include <simdjson.h>

#include "net/http_client.h"
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

std::uint64_t SteadyNs(Clock::time_point time) noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          time.time_since_epoch())
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

bool CopyItems(simdjson::dom::element element,
               std::array<std::string, 3>& items, std::size_t& count,
               simdjson::dom::parser& scratch) {
  count = 0;
  const auto append = [&](std::string item) {
    if (count >= items.size()) {
      count = items.size();
      return;
    }
    items[count++] = std::move(item);
  };
  const auto append_element = [&](simdjson::dom::element item) -> bool {
    if (auto text = item.get_string(); !text.error()) {
      append(std::string(text.value()));
      return true;
    }
    if (auto integer = item.get_int64(); !integer.error()) {
      if (integer.value() == 0) append("0");
      else if (integer.value() == 1) append("1");
      else append("other");
      return true;
    }
    return false;
  };
  if (auto text = element.get_string(); !text.error()) {
    const std::string copy(text.value());
    auto inner = scratch.parse(copy);
    if (inner.error()) return false;
    auto array = inner.get_array();
    if (array.error()) return false;
    for (auto item : array.value()) {
      if (!append_element(item)) return false;
    }
    return true;
  }
  if (auto array = element.get_array(); !array.error()) {
    for (auto item : array.value()) {
      if (!append_element(item)) return false;
    }
    return true;
  }
  return false;
}

}  // namespace

ParsedGammaResolution parse_gamma_resolution(
    std::string_view body,
    const strategyframe::ConditionId& expected_condition) noexcept {
  ParsedGammaResolution result;
  try {
    simdjson::dom::parser parser;
    const std::string copy(body);
    auto document = parser.parse(copy);
    if (document.error()) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    auto markets = document.get_array();
    if (markets.error()) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    std::size_t market_count = 0;
    simdjson::dom::element market;
    for (auto item : markets.value()) {
      if (market_count == 0) market = item;
      ++market_count;
      if (market_count > 1) break;
    }
    if (market_count == 0) {
      result.status = GammaResolutionStatus::Pending;
      return result;
    }
    if (market_count != 1) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    auto object = market.get_object();
    if (object.error()) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    auto condition = object.value()["conditionId"].get_string();
    if (condition.error() ||
        condition.value().size() != expected_condition.length ||
        std::memcmp(condition.value().data(), expected_condition.value,
                    expected_condition.length) != 0) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    auto closed = object.value()["closed"].get_bool();
    auto status = object.value()["umaResolutionStatus"].get_string();
    const bool formally_resolved =
        !closed.error() && closed.value() && !status.error() &&
        std::string_view(status.value()) == "resolved";
    if (!formally_resolved) {
      result.status = GammaResolutionStatus::Pending;
      return result;
    }
    std::array<std::string, 3> outcomes{};
    std::array<std::string, 3> prices{};
    std::size_t outcome_count = 0;
    std::size_t price_count = 0;
    simdjson::dom::parser scratch;
    if (object.value()["outcomes"].error() ||
        object.value()["outcomePrices"].error() ||
        !CopyItems(object.value()["outcomes"].value(), outcomes,
                   outcome_count, scratch) ||
        !CopyItems(object.value()["outcomePrices"].value(), prices,
                   price_count, scratch) ||
        outcome_count != 2 || price_count != 2) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    int up_index = -1;
    int down_index = -1;
    for (std::size_t index = 0; index < 2; ++index) {
      if (outcomes[index] == "Up") {
        if (up_index >= 0) {
          result.status = GammaResolutionStatus::InvalidResponse;
          return result;
        }
        up_index = static_cast<int>(index);
      } else if (outcomes[index] == "Down") {
        if (down_index >= 0) {
          result.status = GammaResolutionStatus::InvalidResponse;
          return result;
        }
        down_index = static_cast<int>(index);
      } else {
        result.status = GammaResolutionStatus::InvalidResponse;
        return result;
      }
    }
    if (up_index < 0 || down_index < 0) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    const bool up_wins = prices[static_cast<std::size_t>(up_index)] == "1" &&
                         prices[static_cast<std::size_t>(down_index)] == "0";
    const bool down_wins = prices[static_cast<std::size_t>(up_index)] == "0" &&
                           prices[static_cast<std::size_t>(down_index)] == "1";
    if (up_wins == down_wins) {
      result.status = GammaResolutionStatus::InvalidResponse;
      return result;
    }
    result.status = GammaResolutionStatus::Resolved;
    result.winner = up_wins ? Outcome::Up : Outcome::Down;
    return result;
  } catch (...) {
    result.status = GammaResolutionStatus::InvalidResponse;
    return result;
  }
}

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

bool FairPriceClient::try_push_resolution(
    const ResolutionRequest& request) noexcept {
  auto lease = resolution_requests_.try_reserve();
  if (!lease) return false;
  lease->emplace(request);
  return lease->commit();
}

bool FairPriceClient::publish_resolution(
    const ResolutionResult& result) noexcept {
  auto lease = resolution_results_.try_reserve();
  if (!lease) return false;
  resolution_unconsumed_.fetch_add(1, std::memory_order_release);
  lease->emplace(result);
  if (!lease->commit()) {
    resolution_unconsumed_.fetch_sub(1, std::memory_order_acq_rel);
    return false;
  }
  return true;
}

bool FairPriceClient::try_pop_resolution(ResolutionResult& result) noexcept {
  auto lease = resolution_results_.try_peek();
  if (!lease) return false;
  result = **lease;
  lease->release();
  resolution_unconsumed_.fetch_sub(1, std::memory_order_acq_rel);
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
  net::HttpClient gamma(ssl, 16U << 10U, 64U << 10U, 8U << 10U, 255,
                        64U << 10U);
  simdjson::dom::parser fair_parser;
  bool gamma_active = false;
  bool holding_result = false;
  ResolutionResult held_result{};
  ResolutionRequest gamma_request{};
  std::array<char, 160> gamma_target{};
  SubscribeGate subscribe;
  bool force_disconnect = false;
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
    auto op_element = object.value()["op"];
    if (!op_element.error()) {
      auto op = op_element.value().get_string();
      if (!op.error()) {
        const auto op_text = std::string_view(op.value());
        if (op_text == "subscribe") {
          bool ok = false;
          auto ok_element = object.value()["ok"];
          if (!ok_element.error()) {
            auto flag = ok_element.value().get_bool();
            ok = !flag.error() && flag.value();
          }
          if (subscribe.note_ack(ok)) force_disconnect = true;
          return true;
        }
        if (op_text == "reset") {
          auto reset_channel = object.value()["channel"].get_string();
          if (!reset_channel.error() &&
              std::string_view(reset_channel.value()) == "fairprice") {
            publish({SidecarEvent::Kind::FairPriceUnavailable, {}});
          }
          return true;
        }
      }
    }
    if (subscribe.phase != SubscribePhase::Acked) return true;
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
      subscribe.reset();
      force_disconnect = false;
      ws_terminal_reported = false;
      if (!websocket.start(
              config_.fairprice_host, config_.fairprice_service, target,
              now + std::chrono::milliseconds(config_.connect_timeout_ms))) {
        next_ws = now + std::chrono::seconds(1);
      }
    }
    const bool socket_open =
        websocket.state() == net::WebSocketClientState::Open;
    if (socket_open &&
        (force_disconnect || subscribe.timed_out(SteadyNs(now)))) {
      fairprice_parse_errors_.fetch_add(1, std::memory_order_relaxed);
      websocket.reset();
      subscribe.reset();
      force_disconnect = false;
      if (!ws_terminal_reported) {
        publish({SidecarEvent::Kind::FairPriceDisconnected, {}});
        ws_terminal_reported = true;
      }
      next_ws = now + std::chrono::seconds(1);
    } else if (subscribe.should_send(socket_open, websocket.can_send_data())) {
      std::string control =
          "{\"op\":\"subscribe\",\"profile\":\"" +
          config_.fairprice_profile + "\",\"symbol\":\"" +
          config_.fairprice_symbol +
          "\",\"channel\":\"fairprice\",\"depth\":0}";
      const auto payload = std::span(
          reinterpret_cast<const std::byte*>(control.data()), control.size());
      std::string_view error;
      if (websocket.send(net::WsOpcode::Text, payload, error)) {
        subscribe.mark_sent(SteadyNs(now), config_.connect_timeout_ms);
      } else {
        fairprice_parse_errors_.fetch_add(1, std::memory_order_relaxed);
        force_disconnect = true;
      }
    }
    if (websocket.state() == net::WebSocketClientState::Open &&
        now >= next_ping && websocket.can_send_data()) {
      std::string_view error;
      (void)websocket.ping({}, error);
      next_ping = now + std::chrono::seconds(10);
    }

    if (holding_result && publish_resolution(held_result)) {
      holding_result = false;
    }
    if (resolution_http_allowed(
            holding_result,
            resolution_unconsumed_.load(std::memory_order_acquire),
            gamma_active)) {
      if (auto request = resolution_requests_.try_peek()) {
        gamma_request = **request;
        std::size_t target_length = 0;
        ResolutionResult outcome{};
        outcome.request_id = gamma_request.request_id;
        outcome.condition_id = gamma_request.condition_id;
        if (!format_gamma_markets_target(gamma_request.condition_id,
                                         gamma_target.data(),
                                         gamma_target.size(),
                                         target_length)) {
          outcome.transport_error = true;
          request->release();
          if (!publish_resolution(outcome)) {
            held_result = outcome;
            holding_result = true;
          }
        } else if (!gamma.start_get(
                       "gamma-api.polymarket.com", "443",
                       std::string_view(gamma_target.data(), target_length),
                       now + std::chrono::milliseconds(kGammaHttpTimeoutMs))) {
          outcome.transport_error = true;
          request->release();
          if (!publish_resolution(outcome)) {
            held_result = outcome;
            holding_result = true;
          }
        } else {
          request->release();
          gamma_active = true;
        }
      }
    }

    std::array<pollfd, 2> descriptors{};
    nfds_t count = 0;
    int websocket_index = -1;
    int gamma_index = -1;
    if (websocket.fd() >= 0) {
      websocket_index = static_cast<int>(count);
      descriptors[count++] = {
          websocket.fd(),
          static_cast<short>(
              ((websocket.wanted_events() & EPOLLIN) != 0 ? POLLIN : 0) |
              ((websocket.wanted_events() & EPOLLOUT) != 0 ? POLLOUT : 0)),
          0};
    }
    if (gamma_active && gamma.fd() >= 0) {
      gamma_index = static_cast<int>(count);
      descriptors[count++] = {
          gamma.fd(),
          static_cast<short>(
              ((gamma.wanted_events() & EPOLLIN) != 0 ? POLLIN : 0) |
              ((gamma.wanted_events() & EPOLLOUT) != 0 ? POLLOUT : 0)),
          0};
    }
    (void)::poll(descriptors.data(), count, 20);
    if (websocket_index >= 0 &&
        descriptors[static_cast<std::size_t>(websocket_index)].revents != 0) {
      websocket.on_event(EpollEvents(
          descriptors[static_cast<std::size_t>(websocket_index)].revents));
    }
    if (gamma_index >= 0 &&
        descriptors[static_cast<std::size_t>(gamma_index)].revents != 0) {
      (void)gamma.on_event(EpollEvents(
          descriptors[static_cast<std::size_t>(gamma_index)].revents));
    }
    websocket.check_timeout();
    if (gamma_active) {
      const auto gamma_state = gamma.check_timeout();
      if (gamma_state == net::HttpClientState::Complete ||
          gamma_state == net::HttpClientState::TimedOut ||
          gamma_state == net::HttpClientState::Failed) {
        ResolutionResult outcome{};
        outcome.request_id = gamma_request.request_id;
        outcome.condition_id = gamma_request.condition_id;
        if (gamma_state == net::HttpClientState::Complete &&
            gamma.response().status_code() >= 200 &&
            gamma.response().status_code() < 300) {
          const auto body = gamma.response().body();
          outcome.parsed = parse_gamma_resolution(
              std::string_view(reinterpret_cast<const char*>(body.data()),
                               body.size()),
              gamma_request.condition_id);
        } else {
          outcome.transport_error = true;
        }
        gamma.reset();
        gamma_active = false;
        if (!publish_resolution(outcome)) {
          held_result = outcome;
          holding_result = true;
        }
      }
    }
    if (Terminal(websocket.state()) && !ws_terminal_reported) {
      publish({SidecarEvent::Kind::FairPriceDisconnected, {}});
      next_ws = Clock::now() + std::chrono::seconds(1);
      ws_terminal_reported = true;
    }
  }
  websocket.reset();
  gamma.reset();
}

}  // namespace polymm
