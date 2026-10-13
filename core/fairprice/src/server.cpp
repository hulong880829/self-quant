#include "fairprice/server.h"

#include "fairprice/json.h"
#include "net/epoll_loop.h"
#include "net/http_parser.h"
#include "net/websocket_codec.h"

#include <algorithm>
#include <array>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <optional>
#include <simdjson.h>
#include <sys/eventfd.h>
#include <sys/socket.h>
#include <unistd.h>
#include <unordered_map>
#include <vector>

namespace fairprice {
namespace {

constexpr std::uint64_t kFairInvalidGraceNs = 2'000'000'000ULL;

struct Subscription {
  std::string key;
  std::string profile;
  std::string symbol;
  std::string channel;
  int depth{20};
  bool acked{};
  bool sent{};
  bool reset_sent{};
  std::uint64_t last_epoch{};
  std::uint64_t last_sequence{};
  std::uint64_t last_write_ns{};
  std::uint64_t fair_invalid_since{};
};

struct Client {
  int fd{-1};
  net::HttpRequestParser parser;
  std::string pending_write;
  std::size_t write_offset{};
  bool websocket{};
  bool closing{};
  bool headers_started{};
  net::WebSocketServerParser ws_parser;
  net::WebSocketFrameWriter ws_writer;
  struct Outbound {
    bool binary{};
    std::string payload;
  };
  std::deque<Outbound> outbound;
  std::unordered_map<std::string, Subscription> subscriptions;
  std::uint64_t accepted_ns{};
  std::uint64_t last_activity_ns{};
  std::uint64_t id{};
  bool history_pending{};
  std::uint64_t history_request_id{};
  std::string history_origin;
  bool history_keep_alive{true};
};

struct HttpOutcome {
  bool enqueue_history{};
  HistoryJobKind history_kind{HistoryJobKind::SpreadHistory};
  int status{200};
  std::string body;
  CatalogMarket market;
  std::uint64_t sample_interval_ms{};
  std::string bps_type;
  std::string origin;
  bool keep_alive{true};
  std::string market_key;
  std::string start;
  std::string end;
  std::string resolution;
  std::string model_id;
};

std::uint64_t now_wall_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

std::uint64_t now_mono_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

bool origin_allowed(const Config &config, std::string_view origin) {
  if (origin.empty()) {
    return true;
  }
  for (const auto &allowed : config.allowed_origins) {
    if (allowed == "*" || allowed == origin) {
      return true;
    }
  }
  return false;
}

bool authorize(const std::string &token, const net::HttpRequest &request) {
  if (token.empty()) {
    return true;
  }
  auto header = std::string(net::header_value(request, "Authorization"));
  constexpr std::string_view bearer = "Bearer ";
  std::string provided;
  if (header.size() > bearer.size() &&
      header.compare(0, bearer.size(), bearer.data()) == 0) {
    provided = header.substr(bearer.size());
  }
  if (provided.empty() && request.path == "/v1/stream") {
    if (auto query = net::query_value(request.query, "token")) {
      provided = *query;
    }
  }
  return provided == token;
}

void apply_cors(net::HttpResponseWriter &writer, std::string_view origin) {
  if (origin.empty()) {
    return;
  }
  writer.set_header("Access-Control-Allow-Origin", origin);
  writer.set_header("Access-Control-Allow-Methods", "GET, OPTIONS");
  writer.set_header("Access-Control-Allow-Headers",
                    "Authorization, Content-Type");
  writer.set_header("Vary", "Origin");
}

void queue_http(Client &client, std::string_view origin, bool keep_alive,
                int status, const std::string &body) {
  net::HttpResponseWriter writer;
  writer.set_status(status,
                    status == 200 || status == 204 ? "OK" : "Error");
  apply_cors(writer, origin);
  writer.set_header("Cache-Control", "no-store");
  if (status != 204) {
    writer.set_body("application/json", body);
  }
  client.pending_write = writer.serialize(keep_alive && status < 400);
  client.write_offset = 0;
  client.closing = !keep_alive || status >= 400;
}

std::optional<MarketView> lookup_market(Store *store, const net::HttpRequest &request,
                                        std::string_view symbol, std::string &error) {
  if (auto profile = net::query_value(request.query, "profile")) {
    auto market = store->copy_profile_symbol(*profile, symbol);
    if (!market) {
      error = "unknown market";
    }
    return market;
  }
  return store->copy_unique_symbol(symbol, error);
}

HttpOutcome json_body(const Config &config, Store *store, Catalog *catalog,
                      Recorder *recorder, const std::string &token,
                      const net::HttpRequest &request) {
  HttpOutcome outcome;
  outcome.origin = std::string(net::header_value(request, "Origin"));
  outcome.keep_alive = request.keep_alive;
  if (!origin_allowed(config, outcome.origin)) {
    outcome.status = 403;
    outcome.body = encode_error_json("origin is not allowed");
    return outcome;
  }
  if (request.method == "OPTIONS") {
    outcome.status = 204;
    return outcome;
  }
  if (request.method != "GET") {
    outcome.status = 405;
    outcome.body = encode_error_json("method not allowed");
    return outcome;
  }
  if (request.path == "/healthz") {
    outcome.status = 200;
    outcome.body = encode_status_json("ok");
    return outcome;
  }
  if (request.path == "/readyz") {
    if (!catalog->snapshot() || !store->ready(now_wall_ns())) {
      outcome.status = 503;
      outcome.body = encode_status_json("not_ready");
      return outcome;
    }
    outcome.status = 200;
    outcome.body = encode_status_json("ready");
    return outcome;
  }
  if (!authorize(token, request)) {
    outcome.status = 401;
    outcome.body = encode_error_json("unauthorized");
    return outcome;
  }
  if (request.path == "/v1/markets") {
    outcome.status = 200;
    outcome.body = encode_markets_json(store->markets_json(now_wall_ns()));
    return outcome;
  }
  constexpr std::string_view prefix = "/v1/markets/";
  if (request.path.rfind(prefix, 0) == 0) {
    auto rest = request.path.substr(prefix.size());
    auto slash = rest.find('/');
    if (slash == std::string::npos) {
      outcome.status = 404;
      outcome.body = encode_error_json("not found");
      return outcome;
    }
    const auto symbol = rest.substr(0, slash);
    const auto action = rest.substr(slash);
    std::string error;
    auto market = lookup_market(store, request, symbol, error);
    if (!market) {
      outcome.status = 404;
      outcome.body = encode_error_json(error.empty() ? "unknown symbol" : error);
      return outcome;
    }
    if (action == "/snapshot") {
      int depth = 20;
      if (auto raw = net::query_value(request.query, "depth")) {
        depth = std::atoi(raw->c_str());
        if (depth <= 0 || depth > kMaxDepth) {
          outcome.status = 400;
          outcome.body = encode_error_json("depth must be within 1..50");
          return outcome;
        }
      }
      const auto now = now_wall_ns();
      BboView bbo = market->bbo;
      BookSnapshot book = market->book;
      if (bbo.ready && !store->fresh(bbo.wall_ns, now)) {
        bbo.ready = false;
      }
      if (book.ready && !store->fresh(book.wall_ns, now)) {
        book.ready = false;
      }
      auto body = encode_snapshot_json(market->catalog.profile,
                                       market->catalog.symbol, &bbo, &book,
                                       depth);
      if (body.empty()) {
        outcome.status = 503;
        outcome.body = encode_error_json("snapshot is not ready");
        return outcome;
      }
      outcome.status = 200;
      outcome.body = std::move(body);
      return outcome;
    }
    if (action == "/spread-history") {
      if (auto range = net::query_value(request.query, "range")) {
        if (*range != "24h") {
          outcome.status = 400;
          outcome.body = encode_error_json("only range=24h is supported");
          return outcome;
        }
      }
      auto type = net::query_value(request.query, "type").value_or("gated");
      if (type != "gated" && type != "raw") {
        outcome.status = 400;
        outcome.body = encode_error_json("type must be raw or gated");
        return outcome;
      }
      const auto *snapshot = catalog->snapshot();
      if (!snapshot) {
        outcome.status = 503;
        outcome.body = encode_error_json("catalog is not available");
        return outcome;
      }
      outcome.enqueue_history = true;
      outcome.status = 200;
      outcome.market = market->catalog;
      outcome.sample_interval_ms = snapshot->sample_interval_ms;
      outcome.bps_type = std::move(type);
      outcome.market_key =
          market_key(market->catalog.profile, market->catalog.symbol);
      return outcome;
    }
    if (action == "/fair-price-history") {
      outcome.start = std::string(net::query_value(request.query, "start").value_or(""));
      outcome.end = std::string(net::query_value(request.query, "end").value_or(""));
      outcome.resolution =
          std::string(net::query_value(request.query, "resolution").value_or(""));
      std::string error;
      const auto code = validate_fair_price_history(
          outcome.start, outcome.end, outcome.resolution, error);
      if (code == 400) {
        outcome.status = 400;
        outcome.body = encode_error_json(error);
        return outcome;
      }
      if (!recorder || !store->fair_enabled()) {
        outcome.status = 503;
        outcome.body = encode_error_json("fair price history is unavailable");
        return outcome;
      }
      outcome.enqueue_history = true;
      outcome.history_kind = HistoryJobKind::FairPriceHistory;
      outcome.status = 200;
      outcome.market = market->catalog;
      outcome.model_id = store->model_id();
      return outcome;
    }
  }
  outcome.status = 404;
  outcome.body = encode_error_json("not found");
  return outcome;
}

bool parse_control(std::string_view payload, std::string &op,
                   std::string &profile, std::string &symbol,
                   std::string &channel, int &depth, std::string &error) {
  const auto json = std::string(payload);
  simdjson::dom::parser parser;
  simdjson::dom::element doc;
  if (parser.parse(json).get(doc)) {
    error = "invalid json";
    return false;
  }
  simdjson::dom::object object;
  if (doc.get(object)) {
    error = "invalid json";
    return false;
  }
  std::string_view value;
  if (!object["op"].get(value)) {
    op = std::string(value);
  }
  if (!object["profile"].get(value)) {
    profile = std::string(value);
  }
  if (!object["symbol"].get(value)) {
    symbol = std::string(value);
  }
  if (!object["channel"].get(value)) {
    channel = std::string(value);
  }
  std::int64_t depth_value = 0;
  if (!object["depth"].get(depth_value)) {
    depth = static_cast<int>(depth_value);
  }
  return true;
}

void queue_ws_text(Client &client, std::string_view payload) {
  if (client.outbound.size() >= 32) {
    return;
  }
  client.outbound.push_back({false, std::string(payload)});
}

void queue_ws_binary(Client &client,
                     const std::shared_ptr<const std::vector<std::byte>> &frame) {
  if (!frame || frame->empty() || client.outbound.size() >= 32) {
    return;
  }
  client.outbound.push_back(
      {true, std::string(reinterpret_cast<const char *>(frame->data()),
                         frame->size())});
}

void flush_ws(Client &client) {
  if (!client.ws_writer.empty() || client.outbound.empty()) {
    return;
  }
  const auto &item = client.outbound.front();
  std::string_view error;
  const auto opcode =
      item.binary ? net::WsOpcode::Binary : net::WsOpcode::Text;
  if (client.ws_writer.prepare_unmasked(
          opcode,
          {reinterpret_cast<const std::byte *>(item.payload.data()),
           item.payload.size()},
          true, error)) {
    client.outbound.pop_front();
  }
}

}  // namespace

Server::Server(Config config, Store *store, Catalog *catalog, History *history,
               Recorder *recorder)
    : config_(std::move(config)), store_(store), catalog_(catalog),
      history_(history), recorder_(recorder),
      browser_token_(config_.browser_token) {}

Server::~Server() { stop(); }

void Server::set_history_provider_for_test(HistoryProvider *provider) {
  history_override_.store(provider, std::memory_order_release);
}

HistoryProvider *Server::history_provider() const {
  if (auto *override = history_override_.load(std::memory_order_acquire)) {
    return override;
  }
  return history_;
}

void Server::drain_history_eventfd() {
  if (history_event_fd_ < 0) {
    return;
  }
  for (;;) {
    std::uint64_t value{};
    const auto n = ::read(history_event_fd_, &value, sizeof(value));
    if (n < 0) {
      if (errno == EINTR) {
        continue;
      }
      break;
    }
    if (n == 0) {
      break;
    }
  }
}

void Server::join_history_workers() {
  history_cv_.notify_all();
  for (auto &worker : history_workers_) {
    if (worker.joinable()) {
      worker.join();
    }
  }
  history_workers_.clear();
}

void Server::history_worker() {
  for (;;) {
    HistoryJob job;
    {
      std::unique_lock lock(history_mutex_);
      history_cv_.wait(lock, [&] {
        return stopping_.load(std::memory_order_acquire) ||
               !history_jobs_.empty();
      });
      if (stopping_.load(std::memory_order_acquire) && history_jobs_.empty()) {
        return;
      }
      if (history_jobs_.empty()) {
        continue;
      }
      job = std::move(history_jobs_.front());
      history_jobs_.pop_front();
    }
    HistoryResult result;
    result.fd = job.fd;
    result.client_id = job.client_id;
    result.request_id = job.request_id;
    result.origin = std::move(job.origin);
    result.keep_alive = job.keep_alive;
    result.close_after = job.close_after;
    result.status = 200;
    if (job.kind == HistoryJobKind::FairPriceHistory) {
      if (!recorder_) {
        result.status = 503;
        result.body = encode_error_json("fair price history is unavailable");
      } else {
        result.body = recorder_->query_history_json(
            job.market.profile, job.market.symbol, job.model_id, job.start,
            job.end, job.resolution, result.status);
      }
    } else if (auto *provider = history_provider()) {
      result.body = encode_history_json(
          provider->query(job.market, job.sample_interval_ms, job.live,
                          job.bps_type, job.now_ns));
    } else {
      result.status = 503;
      result.body = encode_error_json("history is unavailable");
    }
    const bool stopping = stopping_.load(std::memory_order_acquire);
    {
      std::lock_guard lock(history_mutex_);
      if (history_outstanding_ > 0) {
        --history_outstanding_;
      }
      if (!stopping) {
        history_results_.push_back(std::move(result));
      }
    }
    if (!stopping_.load(std::memory_order_acquire) && history_event_fd_ >= 0) {
      const std::uint64_t one = 1;
      [[maybe_unused]] const auto written =
          ::write(history_event_fd_, &one, sizeof(one));
    }
  }
}

bool Server::start() {
  if (!acceptor_.open(config_.listen_address, config_.port)) {
    return false;
  }
  history_event_fd_ = ::eventfd(0, EFD_CLOEXEC | EFD_NONBLOCK);
  if (history_event_fd_ < 0) {
    acceptor_.reset();
    return false;
  }
  const int workers = std::max(1, config_.history_workers);
  const int derived = std::max(8, workers * 4);
  history_queue_capacity_ = static_cast<std::size_t>(
      std::max(1, std::min(config_.max_clients, derived)));
  history_outstanding_ = 0;
  history_jobs_.clear();
  history_results_.clear();
  stopping_.store(false, std::memory_order_release);
  stop_.store(false, std::memory_order_release);
  history_workers_.reserve(static_cast<std::size_t>(workers));
  for (int index = 0; index < workers; ++index) {
    history_workers_.emplace_back([this] { history_worker(); });
  }
  thread_ = std::thread([this] { loop(); });
  return true;
}

void Server::stop() {
  stopping_.store(true, std::memory_order_release);
  {
    std::lock_guard lock(history_mutex_);
    if (history_outstanding_ >= history_jobs_.size()) {
      history_outstanding_ -= history_jobs_.size();
    } else {
      history_outstanding_ = 0;
    }
    history_jobs_.clear();
  }
  join_history_workers();
  stop_.store(true, std::memory_order_release);
  if (history_event_fd_ >= 0) {
    const std::uint64_t one = 1;
    [[maybe_unused]] const auto written =
        ::write(history_event_fd_, &one, sizeof(one));
  }
  if (thread_.joinable()) {
    thread_.join();
  }
  if (history_event_fd_ >= 0) {
    ::close(history_event_fd_);
    history_event_fd_ = -1;
  }
  acceptor_.reset();
}

void Server::loop() {
  net::EpollLoop loop(128);
  loop.add(acceptor_.fd(), EPOLLIN, [&](std::uint32_t) {});
  if (history_event_fd_ >= 0) {
    loop.add(history_event_fd_, EPOLLIN,
             [this](std::uint32_t) { drain_history_eventfd(); });
  }
  std::unordered_map<int, Client> clients;
  auto last_stream = now_mono_ns();
  auto last_fair = now_mono_ns();
  auto last_ping = now_mono_ns();
  while (!stop_.load()) {
    loop.run_once(10);
    drain_history_eventfd();
    for (;;) {
      const auto fd = acceptor_.accept_one();
      if (fd < 0) {
        break;
      }
      if (static_cast<int>(clients.size()) >= config_.max_clients) {
        ::close(fd);
        continue;
      }
      net::EpollLoop::set_nonblocking(fd);
      Client client;
      client.fd = fd;
      client.id = next_client_id_++;
      client.parser = net::HttpRequestParser(
          {.max_header_bytes = config_.max_header_bytes, .max_body_bytes = 0});
      client.accepted_ns = now_mono_ns();
      client.last_activity_ns = client.accepted_ns;
      clients.emplace(fd, std::move(client));
    }
    std::deque<HistoryResult> completed;
    {
      std::lock_guard lock(history_mutex_);
      completed.swap(history_results_);
    }
    std::vector<int> dead;
    for (auto &result : completed) {
      auto found = clients.find(result.fd);
      if (found == clients.end()) {
        continue;
      }
      auto &client = found->second;
      if (client.id != result.client_id || !client.history_pending ||
          client.history_request_id != result.request_id) {
        continue;
      }
      queue_http(client, result.origin, result.keep_alive, result.status,
                 result.body);
      client.history_pending = false;
      client.last_activity_ns = now_mono_ns();
      if (!client.closing) {
        client.parser.reset();
      }
    }
    for (auto &[fd, client] : clients) {
      const bool hold_parser =
          client.history_pending ||
          (!client.websocket && !client.pending_write.empty());
      if (hold_parser) {
        char peek{};
        const auto peeked = ::recv(fd, &peek, 1, MSG_PEEK);
        if (peeked == 0 || (peeked < 0 && errno != EAGAIN &&
                            errno != EWOULDBLOCK && errno != EINTR)) {
          dead.push_back(fd);
          continue;
        }
      } else {
      std::array<std::byte, 4096> buffer{};
      const auto n = ::recv(fd, buffer.data(), buffer.size(), 0);
      if (n > 0) {
        client.last_activity_ns = now_mono_ns();
        std::size_t consumed = 0;
        std::string_view error;
        if (!client.websocket) {
          client.headers_started = true;
          const auto status = client.parser.feed(
              {buffer.data(), static_cast<std::size_t>(n)}, consumed, error);
          if (status == net::HttpParseStatus::Error) {
            dead.push_back(fd);
            continue;
          }
          if (status == net::HttpParseStatus::Complete) {
            const auto &request = client.parser.request();
            if (request.websocket_upgrade && request.path == "/v1/stream") {
              if (!authorize(browser_token_, request) ||
                  !origin_allowed(config_,
                                  net::header_value(request, "Origin"))) {
                const auto origin = net::header_value(request, "Origin");
                std::fprintf(stderr,
                             "fairprice: websocket handshake rejected "
                             "origin=%.*s authorized=%s\n",
                             static_cast<int>(origin.size()), origin.data(),
                             authorize(browser_token_, request) ? "true"
                                                                : "false");
                dead.push_back(fd);
                continue;
              }
              std::array<std::byte, 256> upgrade{};
              std::size_t size = 0;
              std::string_view upgrade_error;
              const auto key = net::header_value(request, "Sec-WebSocket-Key");
              if (!net::prepare_websocket_upgrade_response(key, upgrade, size,
                                                           upgrade_error)) {
                dead.push_back(fd);
                continue;
              }
              client.pending_write.assign(
                  reinterpret_cast<const char *>(upgrade.data()), size);
              client.websocket = true;
            } else {
              auto outcome = json_body(config_, store_, catalog_, recorder_,
                                       browser_token_, request);
              if (outcome.enqueue_history) {
                bool reject = client.history_pending;
                if (!reject) {
                  std::lock_guard lock(history_mutex_);
                  if (stopping_.load(std::memory_order_acquire) ||
                      history_outstanding_ >= history_queue_capacity_) {
                    reject = true;
                  } else {
                    ++history_outstanding_;
                  }
                }
                if (reject) {
                  queue_http(client, outcome.origin, outcome.keep_alive, 503,
                             encode_error_json("history is unavailable"));
                  if (!client.closing) {
                    client.parser.reset();
                  }
                } else {
                  HistoryJob job;
                  job.fd = fd;
                  job.client_id = client.id;
                  job.request_id = ++client.history_request_id;
                  job.origin = outcome.origin;
                  job.keep_alive = outcome.keep_alive;
                  job.close_after = !outcome.keep_alive;
                  job.kind = outcome.history_kind;
                  job.market = std::move(outcome.market);
                  job.sample_interval_ms = outcome.sample_interval_ms;
                  if (outcome.history_kind == HistoryJobKind::SpreadHistory) {
                    job.live = store_->copy_samples(outcome.market_key);
                  }
                  job.bps_type = std::move(outcome.bps_type);
                  job.now_ns = now_wall_ns();
                  job.start = std::move(outcome.start);
                  job.end = std::move(outcome.end);
                  job.resolution = std::move(outcome.resolution);
                  job.model_id = std::move(outcome.model_id);
                  client.history_pending = true;
                  client.history_origin = job.origin;
                  client.history_keep_alive = job.keep_alive;
                  bool stopped = false;
                  {
                    std::lock_guard lock(history_mutex_);
                    if (stopping_.load(std::memory_order_acquire)) {
                      if (history_outstanding_ > 0) {
                        --history_outstanding_;
                      }
                      stopped = true;
                    } else {
                      history_jobs_.push_back(std::move(job));
                    }
                  }
                  if (stopped) {
                    client.history_pending = false;
                    queue_http(client, outcome.origin, outcome.keep_alive, 503,
                               encode_error_json("history is unavailable"));
                    if (!client.closing) {
                      client.parser.reset();
                    }
                  } else {
                    history_cv_.notify_one();
                  }
                }
              } else {
                queue_http(client, outcome.origin, outcome.keep_alive,
                           outcome.status, outcome.body);
                if (!client.closing) {
                  client.parser.reset();
                }
              }
            }
          }
        } else {
          std::string ws_error;
          client.ws_parser.feed(
              {buffer.data(), static_cast<std::size_t>(n)},
              [&](const net::WsFrameView &frame) {
                if (frame.opcode == net::WsOpcode::Ping) {
                  std::string_view ping_error;
                  client.ws_writer.prepare_unmasked(net::WsOpcode::Pong,
                                                    frame.payload, true,
                                                    ping_error);
                  return true;
                }
                if (frame.opcode == net::WsOpcode::Close) {
                  client.closing = true;
                  return true;
                }
                if (frame.opcode != net::WsOpcode::Text) {
                  return true;
                }
                std::string op;
                std::string profile;
                std::string symbol;
                std::string channel;
                std::string parse_error;
                int depth = 0;
                parse_control({reinterpret_cast<const char *>(frame.payload.data()),
                               frame.payload.size()},
                              op, profile, symbol, channel, depth, parse_error);
                std::optional<MarketView> market;
                std::string error = parse_error;
                if (error.empty()) {
                  if (profile.empty()) {
                    market = store_->copy_unique_symbol(symbol, error);
                  } else {
                    market = store_->copy_profile_symbol(profile, symbol);
                    if (!market) {
                      error = "unknown market";
                    }
                  }
                }
                if (error.empty() && channel != "bbo" && channel != "orderbook" &&
                    channel != "fairprice") {
                  error = "channel must be bbo, orderbook, or fairprice";
                }
                if (error.empty() && channel == "fairprice") {
                  if (depth != 0) {
                    error = "depth is not supported for fairprice";
                  } else if (!store_->fair_enabled()) {
                    error = "fairprice is disabled";
                  }
                } else if (error.empty()) {
                  if (depth == 0) {
                    depth = 20;
                  } else if (depth != 20 && depth != 50) {
                    error = "depth must be 20 or 50";
                  }
                }
                const auto sub_key =
                    (market ? market_key(market->catalog.profile,
                                         market->catalog.symbol)
                            : profile + '\0' + symbol) +
                    ":" + channel;
                if (error.empty() && op == "subscribe") {
                  if (client.subscriptions.find(sub_key) ==
                          client.subscriptions.end() &&
                      static_cast<int>(client.subscriptions.size()) >=
                          config_.max_subscriptions_per_client) {
                    error = "subscription limit reached";
                  } else if (market) {
                    Subscription sub;
                    sub.key = market_key(market->catalog.profile,
                                         market->catalog.symbol);
                    sub.profile = market->catalog.profile;
                    sub.symbol = market->catalog.symbol;
                    sub.channel = channel;
                    sub.depth = depth;
                    client.subscriptions[sub_key] = sub;
                  }
                } else if (error.empty() && op == "unsubscribe") {
                  client.subscriptions.erase(sub_key);
                } else if (error.empty()) {
                  error = "op must be subscribe or unsubscribe";
                }
                queue_ws_text(client,
                              encode_ack_json(error.empty(), op,
                                              market ? market->catalog.profile : "",
                                              market ? market->catalog.symbol : "",
                                              channel, error));
                if (error.empty() && op == "subscribe") {
                  client.subscriptions[sub_key].acked = true;
                }
                return true;
              },
              ws_error);
          if (!ws_error.empty()) {
            dead.push_back(fd);
            continue;
          }
        }
      } else if (n == 0 || (errno != EAGAIN && errno != EWOULDBLOCK &&
                            errno != EINTR)) {
        dead.push_back(fd);
        continue;
      }
      }
      if (!client.websocket && !client.headers_started &&
          !client.history_pending &&
          now_mono_ns() - client.accepted_ns >
              config_.read_header_timeout_ms * 1'000'000ULL) {
        dead.push_back(fd);
        continue;
      }
      if (client.websocket) {
        flush_ws(client);
      }
      if (!client.pending_write.empty()) {
        const auto sent =
            ::send(fd, client.pending_write.data() + client.write_offset,
                   client.pending_write.size() - client.write_offset,
                   MSG_NOSIGNAL);
        if (sent > 0) {
          client.write_offset += static_cast<std::size_t>(sent);
          if (client.write_offset == client.pending_write.size()) {
            client.pending_write.clear();
            client.write_offset = 0;
            if (client.closing) {
              dead.push_back(fd);
            }
          }
        }
      } else if (client.websocket) {
        const auto pending = client.ws_writer.pending();
        if (!pending.empty()) {
          const auto sent =
              ::send(fd, pending.data(), pending.size(), MSG_NOSIGNAL);
          if (sent > 0) {
            client.ws_writer.consume(static_cast<std::size_t>(sent));
            if (client.ws_writer.empty()) {
              flush_ws(client);
            }
          }
        }
      }
      if (!client.history_pending && client.headers_started &&
          now_mono_ns() - client.last_activity_ns >
              config_.idle_timeout_ms * 1'000'000ULL) {
        dead.push_back(fd);
      }
    }
    const auto now = now_mono_ns();
    if (now - last_stream >= config_.stream_interval_ms * 1'000'000ULL) {
      last_stream = now;
      const auto wall = now_wall_ns();
      for (auto &[fd, client] : clients) {
        (void)fd;
        if (!client.websocket) {
          continue;
        }
        for (auto &[_, sub] : client.subscriptions) {
          if (!sub.acked || sub.channel == "fairprice") {
            continue;
          }
          auto market = store_->copy_view(sub.key);
          if (!market) {
            continue;
          }
          const bool book = sub.channel == "orderbook";
          const bool ready = book ? market->book.ready : market->bbo.ready;
          const auto wall_ns = book ? market->book.wall_ns : market->bbo.wall_ns;
          const auto epoch =
              book ? market->book.ring_epoch : market->bbo.ring_epoch;
          const auto sequence =
              book ? market->book.ring_sequence : market->bbo.ring_sequence;
          if (!ready || !store_->fresh(wall_ns, wall)) {
            if (sub.sent && !sub.reset_sent) {
              queue_ws_text(client,
                            encode_reset_json(sub.profile, sub.symbol,
                                              sub.channel,
                                              ready ? "source_stale" : "not_ready"));
              sub.sent = false;
              sub.reset_sent = true;
            }
            continue;
          }
          if (sub.sent && epoch == sub.last_epoch &&
              sequence == sub.last_sequence) {
            continue;
          }
          const auto &frame = book ? (sub.depth == 50 ? market->book.browser50
                                                      : market->book.browser20)
                                   : market->bbo.browser;
          if (!frame || frame->empty()) {
            continue;
          }
          queue_ws_binary(client, frame);
          sub.last_epoch = epoch;
          sub.last_sequence = sequence;
          sub.sent = true;
          sub.reset_sent = false;
        }
        flush_ws(client);
      }
    }
    if (now - last_fair >= config_.fair_stream_interval_ms * 1'000'000ULL) {
      last_fair = now;
      const auto wall = now_wall_ns();
      for (auto &[fd, client] : clients) {
        (void)fd;
        if (!client.websocket) {
          continue;
        }
        for (auto &[_, sub] : client.subscriptions) {
          if (!sub.acked || sub.channel != "fairprice") {
            continue;
          }
          store_->refresh_fair_json_key(sub.key);
          auto market = store_->copy_view(sub.key);
          if (!market) {
            continue;
          }
          const bool fresh =
              market->fair.ready && store_->fresh(market->fair.wall_ns, wall);
          if (!fresh) {
            if (!market->fair.ready) {
              if (sub.fair_invalid_since == 0) {
                sub.fair_invalid_since = wall;
              }
              if (wall - sub.fair_invalid_since < kFairInvalidGraceNs) {
                continue;
              }
            }
            if (!sub.reset_sent) {
              queue_ws_text(client, encode_reset_json(sub.profile, sub.symbol,
                                                      "fairprice",
                                                      "source_stale"));
              sub.sent = false;
              sub.reset_sent = true;
            }
            continue;
          }
          sub.fair_invalid_since = 0;
          const bool changed = !sub.sent ||
                               market->fair.ring_epoch != sub.last_epoch ||
                               market->fair.ring_sequence != sub.last_sequence;
          const bool heartbeat =
              sub.sent && now - sub.last_write_ns >=
                              config_.fair_heartbeat_ms * 1'000'000ULL;
          if (!changed && !heartbeat) {
            continue;
          }
          if (market->fair_json) {
            queue_ws_text(client, *market->fair_json);
          }
          sub.last_epoch = market->fair.ring_epoch;
          sub.last_sequence = market->fair.ring_sequence;
          sub.sent = true;
          sub.reset_sent = false;
          sub.last_write_ns = now;
        }
        flush_ws(client);
      }
    }
    if (now - last_ping >= 15'000'000'000ULL) {
      last_ping = now;
      for (auto &[fd, client] : clients) {
        (void)fd;
        if (client.websocket && client.ws_writer.empty()) {
          std::string_view error;
          client.ws_writer.prepare_unmasked(net::WsOpcode::Ping, {}, true,
                                            error);
        }
      }
    }
    std::sort(dead.begin(), dead.end());
    dead.erase(std::unique(dead.begin(), dead.end()), dead.end());
    for (int fd : dead) {
      ::close(fd);
      clients.erase(fd);
    }
  }
  for (auto &[fd, _] : clients) {
    ::close(fd);
  }
}

}  // namespace fairprice
