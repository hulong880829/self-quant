#include "mds/producer/producer_runtime.h"

#include "producer_config.h"

#include "mds/exchange/capabilities.h"
#include "mds/publish/wire_publisher.h"
#include "mds/service/venue_connection.h"
#include "net/http_client.h"

#include <algorithm>
#include <cctype>
#include <chrono>
#include <cstdlib>
#include <map>
#include <poll.h>
#include <regex>
#include <set>
#include <string>
#include <sys/epoll.h>
#include <utility>
#include <vector>

namespace mds::producer {
namespace {

struct Endpoint {
  std::string host;
  std::string service{"443"};
  std::string target{"/"};
};

Endpoint parse_endpoint(std::string_view configured) {
  Endpoint result;
  auto value = configured;
  if (const auto scheme = value.find("://");
      scheme != std::string_view::npos) {
    value.remove_prefix(scheme + 3);
  }
  const auto slash = value.find('/');
  auto authority = value.substr(0, slash);
  if (slash != std::string_view::npos) {
    result.target.assign(value.substr(slash));
  }
  if (const auto colon = authority.rfind(':');
      colon != std::string_view::npos && authority.find(':') == colon) {
    result.host.assign(authority.substr(0, colon));
    result.service.assign(authority.substr(colon + 1));
  } else {
    result.host.assign(authority);
  }
  return result;
}

std::string request_target(const Endpoint &endpoint, std::string_view target) {
  if (target.empty()) {
    return endpoint.target;
  }
  if (endpoint.target == "/" || endpoint.target.empty()) {
    return std::string(target);
  }
  std::string combined = endpoint.target;
  if (combined.back() == '/' && target.front() == '/') {
    combined.pop_back();
  }
  combined.append(target);
  return combined;
}

bool fetch_metadata(const VenueEndpoint &configured,
                    const exchange::HttpRequestSpec &request,
                    std::string &body, std::string &error) {
  const auto endpoint = parse_endpoint(configured.rest_endpoint);
  if (endpoint.host.empty()) {
    error = "product discovery REST endpoint is empty";
    return false;
  }
  std::string tls_error;
  auto tls = net::make_client_ssl_context(tls_error);
  if (!tls) {
    error = std::move(tls_error);
    return false;
  }
  net::HttpClient client(std::move(tls), 64U << 10U, 32U << 20U,
                         16U << 20U);
  const auto deadline =
      net::HttpClient::Clock::now() + std::chrono::seconds(15);
  const auto target = request_target(endpoint, request.target);
  const auto request_body = std::as_bytes(std::span(request.body));
  const bool started =
      request.method == exchange::HttpRequestSpec::Method::Post
          ? client.start_post(endpoint.host, endpoint.service, target,
                              request.content_type, request_body, deadline)
          : client.start_get(endpoint.host, endpoint.service, target, deadline);
  if (!started) {
    error = std::string(client.error_message());
    return false;
  }
  while (client.state() != net::HttpClientState::Complete) {
    if (client.state() == net::HttpClientState::Failed ||
        client.state() == net::HttpClientState::TimedOut) {
      error = std::string(client.error_message());
      return false;
    }
    pollfd descriptor{client.fd(), 0, 0};
    const auto wanted = client.wanted_events();
    if ((wanted & EPOLLIN) != 0) {
      descriptor.events |= POLLIN;
    }
    if ((wanted & EPOLLOUT) != 0) {
      descriptor.events |= POLLOUT;
    }
    const int ready = ::poll(&descriptor, 1, 50);
    std::uint32_t events{};
    if (ready > 0) {
      if ((descriptor.revents & POLLIN) != 0) {
        events |= EPOLLIN;
      }
      if ((descriptor.revents & POLLOUT) != 0) {
        events |= EPOLLOUT;
      }
      if ((descriptor.revents & (POLLERR | POLLHUP | POLLNVAL)) != 0) {
        events |= EPOLLERR | EPOLLHUP;
      }
    }
    (void)client.on_event(events);
    (void)client.check_timeout();
  }
  if (client.response().status_class() != net::HttpStatusClass::Success) {
    error = "product discovery HTTP status " +
            std::to_string(client.response().status_code());
    return false;
  }
  const auto response_body = client.response().body();
  body.assign(reinterpret_cast<const char *>(response_body.data()),
              response_body.size());
  return true;
}

std::string canonical_symbol(utils::md::Venue venue,
                             utils::md::ProductType product,
                             std::string symbol) {
  std::transform(symbol.begin(), symbol.end(), symbol.begin(),
                 [](unsigned char value) {
                   return static_cast<char>(std::toupper(value));
                 });
  if (venue == utils::md::Venue::Okx &&
      product == utils::md::ProductType::Perpetual &&
      symbol.ends_with("-SWAP")) {
    symbol.resize(symbol.size() - 5);
  }
  symbol.erase(std::remove_if(symbol.begin(), symbol.end(),
                              [](char value) {
                                return value == '-' || value == '_';
                              }),
               symbol.end());
  return symbol;
}

std::vector<std::string> discover_venue_symbols(
    utils::md::Venue venue, utils::md::ProductType product,
    std::string_view json) {
  std::string_view field{"symbol"};
  if (venue == utils::md::Venue::Okx) {
    field = "instId";
  } else if (venue == utils::md::Venue::Gate) {
    field = product == utils::md::ProductType::Spot ? "id" : "name";
  } else if (venue == utils::md::Venue::Hyperliquid) {
    field = "name";
  }
  const std::regex expression(
      "\"" + std::string(field) + "\"\\s*:\\s*\"([^\"]+)\"");
  const std::string text(json);
  std::set<std::string> unique;
  for (std::sregex_iterator it(text.begin(), text.end(), expression), end;
       it != end; ++it) {
    unique.insert((*it)[1].str());
  }
  return {unique.begin(), unique.end()};
}

bool same_instrument_metadata(
    const exchange::InstrumentMetadata &left,
    const exchange::InstrumentMetadata &right) noexcept {
  return left.canonical_symbol == right.canonical_symbol &&
         left.venue_symbol == right.venue_symbol &&
         left.base_asset == right.base_asset &&
         left.quote_asset == right.quote_asset &&
         left.settle_asset == right.settle_asset &&
         left.price_scale == right.price_scale &&
         left.quantity_scale == right.quantity_scale &&
         left.tick_size == right.tick_size &&
         left.lot_size == right.lot_size &&
         left.contract_multiplier == right.contract_multiplier &&
         left.turnover_24h == right.turnover_24h &&
         left.contract_multiplier_scale ==
             right.contract_multiplier_scale &&
         left.signature_type == right.signature_type &&
         left.negative_risk == right.negative_risk &&
         left.refine_book_tick == right.refine_book_tick;
}

bool expand_product_discovery(ProducerConfig &config, std::string &error) {
  std::size_t instrument_count{};
  std::size_t ring_count{};
  std::uint64_t ring_bytes{};
  for (auto &connection : config.connections) {
    std::vector<StreamSpec> expanded;
    for (const auto &stream : connection.streams) {
      if (!stream.discovery) {
        expanded.push_back(stream);
        ++instrument_count;
        if (stream.ring_layout != publish::RingLayout::Multiplex) {
          const auto rings = std::size_t(stream.subscribe_ticker) +
                             std::size_t(stream.subscribe_orderbook);
          ring_count += rings;
          ring_bytes += rings * stream.ring.ring_bytes;
        }
        continue;
      }
      constexpr std::size_t adapter_levels = 1;
      auto adapter = exchange::make_venue_adapter(
          connection.endpoint.venue, connection.endpoint.product,
          adapter_levels);
      if (!adapter) {
        error = "product discovery adapter is unavailable";
        return false;
      }
      std::map<std::string, exchange::InstrumentMetadata>
          metadata_by_symbol;
      DiscoveryPaginationGuard pagination;
      while (!pagination.complete()) {
        if (!pagination.begin_page(error)) {
          return false;
        }
        std::string json;
        if (!fetch_metadata(
                connection.endpoint,
                adapter->discovery_metadata_request(
                    pagination.cursor()),
                json, error)) {
          return false;
        }
        const auto symbols = discover_venue_symbols(
            connection.endpoint.venue, connection.endpoint.product,
            json);
        std::string next_cursor;
        if (!adapter->discovery_metadata_next_cursor(
                json, next_cursor, error)) {
          return false;
        }
        if (!pagination.accept_page(
                !symbols.empty(), std::move(next_cursor), error)) {
          return false;
        }
        std::vector<std::string> canonical;
        std::vector<exchange::StreamRequest> requests;
        canonical.reserve(symbols.size());
        requests.reserve(symbols.size());
        for (const auto &venue_symbol : symbols) {
          canonical.push_back(canonical_symbol(
              connection.endpoint.venue,
              connection.endpoint.product, venue_symbol));
          requests.push_back(
              {canonical.back(), venue_symbol,
               stream.ticker_channel, stream.orderbook_channel,
               stream.subscribe_ticker, stream.subscribe_orderbook,
               stream.effective_interval_ms});
        }
        std::vector<exchange::InstrumentMetadata> page_metadata;
        if (!adapter->parse_discovery_metadata(
                json, requests, page_metadata, error)) {
          return false;
        }
        for (auto &instrument : page_metadata) {
          const auto [found, inserted] =
              metadata_by_symbol.try_emplace(
                  instrument.venue_symbol, instrument);
          if (!inserted &&
              !same_instrument_metadata(found->second, instrument)) {
            error = "conflicting product discovery metadata for symbol " +
                    instrument.venue_symbol;
            return false;
          }
        }
      }
      std::vector<exchange::InstrumentMetadata> metadata;
      metadata.reserve(metadata_by_symbol.size());
      for (auto &[venue_symbol, instrument] : metadata_by_symbol) {
        (void)venue_symbol;
        metadata.push_back(std::move(instrument));
      }
      if (stream.discovery->minimum_turnover != 0) {
        const auto turnover_request =
            adapter->discovery_turnover_request();
        if (turnover_request.target.empty()) {
          error = "24h turnover discovery is unsupported";
          return false;
        }
        std::string turnover_json;
        if (!fetch_metadata(connection.endpoint, turnover_request,
                            turnover_json, error) ||
            !adapter->enrich_discovery_turnover(
                turnover_json, metadata, error)) {
          return false;
        }
      }
      const auto reconciled =
          reconcile_universe(*stream.discovery, metadata, {});
      if (!reconciled) {
        error = reconciled.message;
        return false;
      }
      for (const auto &instrument : reconciled.value.added) {
        auto resolved = stream;
        resolved.symbol = instrument.canonical_symbol;
        resolved.discovery.reset();
        expanded.push_back(std::move(resolved));
      }
      instrument_count += reconciled.value.added.size();
      if (stream.ring_layout != publish::RingLayout::Multiplex) {
        const auto per_instrument =
            std::size_t(stream.subscribe_ticker) +
            std::size_t(stream.subscribe_orderbook);
        ring_count += per_instrument * reconciled.value.added.size();
        ring_bytes += per_instrument * reconciled.value.added.size() *
                      stream.ring.ring_bytes;
      }
    }
    if (!connection.streams.empty() &&
        connection.streams.front().ring_layout !=
            publish::RingLayout::PerSymbol) {
      const auto &layout = connection.streams.front();
      const bool ticker = std::any_of(
          expanded.begin(), expanded.end(),
          [](const auto &value) { return value.subscribe_ticker; });
      const bool book = std::any_of(
          expanded.begin(), expanded.end(),
          [](const auto &value) { return value.subscribe_orderbook; });
      const auto rings =
          (std::size_t(ticker) + std::size_t(book)) * layout.shard_count;
      ring_count += rings;
      ring_bytes += rings * layout.multiplex_ring.ring_bytes;
    }
    connection.streams = std::move(expanded);
  }
  if (instrument_count == 0 || instrument_count > config.max_instruments ||
      ring_count > config.max_rings ||
      ring_bytes > config.max_total_ring_bytes) {
    error = "discovered universe exceeds startup capacity budget";
    return false;
  }
  config.instrument_count = instrument_count;
  config.ring_count = ring_count;
  return true;
}

std::string environment(std::string_view name) {
  if (name.empty()) {
    return {};
  }
  const std::string owned(name);
  const auto *value = std::getenv(owned.c_str());
  return value == nullptr ? std::string{} : std::string(value);
}

service::VenueConnectionOptions convert(const ConnectionSpec &connection) {
  service::VenueConnectionOptions result;
  result.venue = connection.endpoint.venue;
  result.product = connection.endpoint.product;
  result.websocket_endpoint = connection.endpoint.websocket_endpoint;
  if (result.venue == utils::md::Venue::Binance) {
    const auto scheme = result.websocket_endpoint.find("://");
    const auto path = result.websocket_endpoint.find(
        '/', scheme == std::string::npos ? 0 : scheme + 3);
    if (path == std::string::npos) {
      result.websocket_endpoint.append("/ws");
    }
  }
  result.rest_endpoint = connection.endpoint.rest_endpoint;
  result.discovery_endpoint = connection.endpoint.discovery_endpoint;
  result.max_symbols_per_ws = connection.endpoint.max_symbols_per_ws;
  if (result.venue == utils::md::Venue::Polymarket &&
      result.rest_endpoint.empty()) {
    result.rest_endpoint = result.discovery_endpoint;
  }
  result.snapshot_pacing_ms = connection.endpoint.snapshot_pacing_ms;
  result.snapshot_failure_backoff_ms =
      connection.endpoint.snapshot_failure_backoff_ms;
  result.snapshot_failure_backoff_max_ms =
      connection.endpoint.snapshot_failure_backoff_max_ms;
  result.snapshot_max_consecutive_failures =
      connection.endpoint.snapshot_max_consecutive_failures;
  result.snapshot_rate_limit_backoff_ms =
      connection.endpoint.snapshot_rate_limit_backoff_ms;
  result.snapshot_ban_backoff_ms =
      connection.endpoint.snapshot_ban_backoff_ms;
  result.recovery_deadline =
      std::chrono::milliseconds(connection.endpoint.recovery_deadline_ms);
  result.max_continuous_recovery_duration =
      std::chrono::milliseconds(
          connection.endpoint.max_continuous_recovery_ms);
  result.credentials.api_key =
      environment(connection.endpoint.auth.api_key_env);
  result.credentials.secret =
      environment(connection.endpoint.auth.secret_env);
  result.credentials.passphrase =
      environment(connection.endpoint.auth.passphrase_env);
  result.streams.reserve(connection.streams.size());
  for (const auto &stream : connection.streams) {
    service::SymbolStreamOptions converted;
    converted.symbol = stream.symbol;
    converted.polymarket_rolling = stream.polymarket_rolling;
    converted.polymarket_asset = stream.polymarket_asset;
    converted.polymarket_period = stream.polymarket_period;
    converted.polymarket_outcome = stream.polymarket_outcome;
    converted.polymarket_prediscovery_seconds =
        stream.polymarket_prediscovery_seconds;
    converted.polymarket_rollover_grace_seconds =
        stream.polymarket_rollover_grace_seconds;
    converted.polymarket_resolver_timeout_ms =
        stream.polymarket_resolver_timeout_ms;
    converted.ticker_channel = stream.ticker_channel;
    converted.orderbook_channel = stream.orderbook_channel;
    converted.orderbook_bootstrap = stream.orderbook_bootstrap;
    converted.shm_prefix = stream.shm_prefix;
    converted.ladder_ticks_per_side = stream.ladder_ticks_per_side;
    converted.ladder_price_band_bps = stream.ladder_price_band_bps;
    converted.snapshot_depth = stream.snapshot_depth;
    converted.max_levels_per_message =
        std::max({std::size_t{100}, stream.orderbook_depth,
                  stream.snapshot_depth, stream.max_levels_per_message});
    converted.update_interval_ms = stream.effective_interval_ms;
    converted.ticker = stream.subscribe_ticker;
    converted.orderbook = stream.subscribe_orderbook;
    converted.ring = stream.ring;
    converted.ring_layout = stream.ring_layout;
    converted.shard_count = stream.shard_count;
    converted.multiplex_ring = stream.multiplex_ring;
    converted.reader_lease_timeout = stream.reader_lease_timeout;
    result.streams.push_back(std::move(converted));
  }
  return result;
}

void print_effective(std::ostream &output, const ProducerConfig &config) {
  std::size_t streams{};
  for (const auto &connection : config.connections) {
    streams += connection.streams.size();
  }
  output << "validated connections=" << config.connections.size()
         << " streams=" << streams
         << " instruments=" << config.instrument_count
         << " rings=" << config.ring_count
         << " unlink_on_shutdown="
         << (config.unlink_on_shutdown ? "true" : "false") << '\n';
  for (const auto &connection : config.connections) {
    const auto venue = exchange::venue_name(connection.endpoint.venue);
    const auto product = exchange::product_name(connection.endpoint.product);
    for (const auto &stream : connection.streams) {
      output << "stream venue=" << venue << " product=" << product
             << " symbol=" << stream.symbol
             << " recovery_deadline_ms="
             << connection.endpoint.recovery_deadline_ms
             << " max_continuous_recovery_ms="
             << connection.endpoint.max_continuous_recovery_ms
             << " ticker=" << (stream.subscribe_ticker ? "on" : "off");
      if (stream.subscribe_ticker) {
        output << " ticker_channel=" << stream.ticker_channel;
      }
      output << " orderbook="
             << (stream.subscribe_orderbook ? "on" : "off");
      if (stream.subscribe_orderbook) {
        output << " orderbook_channel=" << stream.orderbook_channel
               << " depth=" << stream.orderbook_depth
               << " interval_ms=" << stream.effective_interval_ms
               << " auth="
               << (stream.requires_public_ws_login ? "required" : "none")
               << " explicit_override="
               << (stream.orderbook_channel_override ? "true" : "false")
               << " ladder_ticks_per_side=" << stream.ladder_ticks_per_side
               << " price_band_bps=" << stream.ladder_price_band_bps;
      }
      output << '\n';
    }
  }
}

std::vector<ResolvedSegment> build_resolved_segments(
    const ProducerConfig &config) {
  std::vector<ResolvedSegment> result;
  std::set<std::string> unique;
  const auto add = [&](std::string name,
                       const transport::RingOptions &ring) {
    if (unique.insert(name).second) {
      result.push_back({
          .name = std::move(name),
          .ring_bytes = ring.ring_bytes,
          .max_record_bytes = ring.max_record_bytes,
      });
    }
  };
  for (const auto &connection : config.connections) {
    const auto profile = exchange::segment_profile(
        connection.endpoint.venue, connection.endpoint.product);
    if (connection.streams.empty()) {
      continue;
    }
    const auto layout = connection.streams.front().ring_layout;
    if (layout != publish::RingLayout::Multiplex) {
      for (const auto &stream : connection.streams) {
        if (stream.subscribe_ticker) {
          add(publish::make_publisher_segment_name(
                  stream.shm_prefix, profile, stream.symbol, "ticker"),
              stream.ring);
        }
        if (stream.subscribe_orderbook) {
          add(publish::make_publisher_segment_name(
                  stream.shm_prefix, profile, stream.symbol, "orderbook"),
              stream.ring);
        }
      }
    }
    if (layout != publish::RingLayout::PerSymbol) {
      const auto &stream = connection.streams.front();
      const auto venue = exchange::venue_name(connection.endpoint.venue);
      const auto product = exchange::product_name(connection.endpoint.product);
      const bool ticker = std::any_of(
          connection.streams.begin(), connection.streams.end(),
          [](const auto &entry) { return entry.subscribe_ticker; });
      const bool orderbook = std::any_of(
          connection.streams.begin(), connection.streams.end(),
          [](const auto &entry) { return entry.subscribe_orderbook; });
      for (std::size_t shard = 0; shard < stream.shard_count; ++shard) {
        if (ticker) {
          add(publish::make_multiplex_segment_name(
                  stream.shm_prefix, venue, product, "ticker", shard),
              stream.multiplex_ring);
        }
        if (orderbook) {
          add(publish::make_multiplex_segment_name(
                  stream.shm_prefix, venue, product, "orderbook", shard),
              stream.multiplex_ring);
        }
      }
    }
  }
  return result;
}

std::string targets(std::span<const service::SymbolStreamOptions> streams) {
  std::string result;
  for (const auto &stream : streams) {
    if (!result.empty()) {
      result.push_back(',');
    }
    result += stream.symbol + '[';
    if (stream.ticker) {
      result += stream.ticker_channel;
    }
    if (stream.ticker && stream.orderbook) {
      result.push_back('+');
    }
    if (stream.orderbook) {
      result += stream.orderbook_channel;
    }
    result.push_back(']');
  }
  return result;
}

}  // namespace

class ProducerRuntime::Impl {
 public:
  explicit Impl(ProducerRuntimeOptions options)
      : options_(std::move(options)) {}

  api::Result<void> start() noexcept {
    try {
      return start_impl();
    } catch (const std::exception &exception) {
      cleanup_connections();
      return fail(api::ErrorCode::InternalError,
                  "producer runtime start failed: " +
                      std::string(exception.what()));
    } catch (...) {
      cleanup_connections();
      return fail(api::ErrorCode::InternalError,
                  "producer runtime start failed");
    }
  }

  api::Result<void> start_impl() {
    if (started_) {
      return {.error = api::ErrorCode::AlreadyStarted,
              .message = "producer runtime is already started"};
    }
    failed_ = false;
    error_.clear();
    auto loaded = load_config(options_.config_path);
    if (!loaded) {
      return fail(loaded.error, loaded.message);
    }
    config_ = std::move(loaded.value);
    if (options_.output != nullptr) {
      print_effective(*options_.output, config_);
    }
    if (options_.validate_only) {
      segments_ = build_resolved_segments(config_);
      started_ = true;
      return {};
    }
    const bool discovery = std::any_of(
        config_.connections.begin(), config_.connections.end(),
        [](const auto &connection) {
          return std::any_of(connection.streams.begin(),
                             connection.streams.end(),
                             [](const auto &stream) {
                               return stream.discovery.has_value();
                             });
        });
    if (discovery) {
      std::string message;
      if (!expand_product_discovery(config_, message)) {
        return fail(api::ErrorCode::InternalError,
                    "product discovery failed: " + message);
      }
      if (options_.output != nullptr) {
        print_effective(*options_.output, config_);
      }
    }
    segments_ = build_resolved_segments(config_);
    if (options_.discover_only) {
      started_ = true;
      return {};
    }
    manager_ = std::make_unique<service::VenueConnectionManager>();
    for (const auto &connection : config_.connections) {
      auto created = manager_->create(convert(connection));
      if (!created) {
        std::string message =
            "venue connection start failed venue=" +
            std::string(exchange::venue_name(connection.endpoint.venue)) +
            " product=" +
            std::string(exchange::product_name(connection.endpoint.product)) +
            " reason=" + created.message + " targets=";
        std::vector<service::SymbolStreamOptions> converted_streams =
            convert(connection).streams;
        message += targets(converted_streams);
        cleanup_connections();
        return fail(created.error, std::move(message));
      }
      connections_.push_back(created.value);
      if (options_.output != nullptr) {
        std::vector<std::pair<std::string, std::string>> shard_ranges(
            created.value->websocket_shard_count());
        for (const auto &stream : connection.streams) {
          const auto shard = created.value->websocket_shard(stream.symbol);
          if (!shard || *shard >= shard_ranges.size()) {
            continue;
          }
          auto &[first, last] = shard_ranges[*shard];
          if (first.empty() || stream.symbol < first) {
            first = stream.symbol;
          }
          if (last.empty() || stream.symbol > last) {
            last = stream.symbol;
          }
        }
        *options_.output
            << "venue connection started venue="
            << exchange::venue_name(connection.endpoint.venue)
            << " product=" << exchange::product_name(connection.endpoint.product)
            << " symbols=" << connection.streams.size()
            << " ws_shards=" << shard_ranges.size();
        for (std::size_t shard = 0; shard < shard_ranges.size(); ++shard) {
          *options_.output << " shard[" << shard << "]="
                           << shard_ranges[shard].first << ".."
                           << shard_ranges[shard].second;
        }
        *options_.output << '\n';
      }
    }
    failure_reported_.assign(connections_.size(), false);
    started_ = true;
    return {};
  }

  int run_once(int timeout_ms) noexcept {
    if (!started_) {
      set_failure("producer runtime is not started");
      return -1;
    }
    if (options_.validate_only || options_.discover_only) {
      return 0;
    }
    int result{};
    try {
      result = connections_.empty() ? 0 : manager_->run_once(timeout_ms);
    } catch (const std::exception &exception) {
      set_failure("producer runtime iteration failed: " +
                  std::string(exception.what()));
      return -1;
    } catch (...) {
      set_failure("producer runtime iteration failed");
      return -1;
    }
    if (result < 0) {
      set_failure("epoll wait failed");
      return result;
    }
    for (std::size_t index = 0; index < connections_.size(); ++index) {
      const auto *connection = connections_[index];
      if (connection->state() != service::MarketDataState::Failed) {
        failure_reported_[index] = false;
        continue;
      }
      if (failure_reported_[index]) {
        continue;
      }
      failure_reported_[index] = true;
      std::string message =
          "venue connection failed venue=" +
          std::string(exchange::venue_name(connection->venue())) +
          " product=" +
          std::string(exchange::product_name(connection->product())) +
          " reason=" + std::string(connection->error_message()) +
          " targets=" + targets(connection->streams());
      if (options_.error_output != nullptr) {
        *options_.error_output << message << '\n';
      }
    }
    return result;
  }

  void stop() noexcept {
    if (!started_) {
      return;
    }
    if (manager_) {
      manager_->stop();
    }
    if (options_.output != nullptr) {
      for (const auto *connection : connections_) {
        const auto &metrics = connection->metrics();
        *options_.output
            << exchange::venue_name(connection->venue()) << '/'
            << exchange::product_name(connection->product())
            << " stopped ws_shards=" << metrics.ws_shards
            << " ws_shards_live=" << metrics.ws_shards_live
            << " ws_shards_reconnecting=" << metrics.ws_shards_reconnecting
            << " reconnects=" << metrics.reconnects
            << " connection_rebuild_attempts="
            << metrics.connection_rebuild_attempts
            << " connection_rebuild_successes="
            << metrics.connection_rebuild_successes
            << " connection_rebuild_failures="
            << metrics.connection_rebuild_failures
            << " connection_degraded_duration_ms="
            << metrics.connection_degraded_duration_ms
            << " last_reconnect_shard=" << metrics.last_reconnect_shard
            << " resyncs=" << metrics.resyncs
            << " parse_errors=" << metrics.parse_errors
            << " publish_errors=" << metrics.publish_errors
            << " snapshot_failures=" << metrics.snapshot_failures
            << " snapshot_rate_limits=" << metrics.snapshot_rate_limits
            << " snapshot_quarantines=" << metrics.snapshot_quarantines
            << " depth=" << metrics.depth_updates
            << " ticker=" << metrics.ticker_updates
            << " subscription_requests=" << metrics.subscription_requests
            << " budget_deferrals=" << metrics.budget_deferrals
            << " cooldown_deferrals=" << metrics.cooldown_deferrals
            << " recovery_deadline_extensions="
            << metrics.recovery_deadline_extensions
            << " snapshot_bridge_gaps=" << metrics.snapshot_bridge_gaps
            << " live_sequence_gaps=" << metrics.live_sequence_gaps
            << " invalid_images=" << metrics.invalid_images
            << " pipeline_resyncs=" << metrics.pipeline_resyncs
            << " input_capacity_resyncs=" << metrics.input_capacity_resyncs
            << " dirty_data_resyncs=" << metrics.dirty_data_resyncs
            << " discovery_requests=" << metrics.discovery_requests
            << " discovery_failures=" << metrics.discovery_failures
            << " market_rollovers=" << metrics.market_rollovers
            << " stale_market_messages=" << metrics.stale_market_messages
            << " heartbeat_timeouts=" << metrics.heartbeat_timeouts
            << " last_discovery_latency_us="
            << metrics.last_discovery_latency_us
            << " last_rollover_latency_us="
            << metrics.last_rollover_latency_us;
        if (metrics.pipeline_resyncs != 0) {
          *options_.output << " pipeline_resync_causes=";
          bool first = true;
          for (std::size_t index = 1;
               index < metrics.pipeline_resync_causes.size(); ++index) {
            const auto count = metrics.pipeline_resync_causes[index];
            if (count == 0) {
              continue;
            }
            if (!first) {
              *options_.output << ',';
            }
            first = false;
            *options_.output
                << book::to_string(
                       static_cast<book::BridgeResyncReason>(index))
                << ':' << count;
          }
          *options_.output
              << " last_pipeline_resync_cause="
              << book::to_string(metrics.last_pipeline_resync_cause);
        }
        if (metrics.last_resync_reason != service::ResyncReason::None) {
          *options_.output
              << " last_resync_symbol=" << metrics.last_resync_symbol.data()
              << " last_resync_reason="
              << service::to_string(metrics.last_resync_reason)
              << " last_resync_expected=" << metrics.last_resync_expected
              << " last_resync_previous=" << metrics.last_resync_previous
              << " last_resync_first=" << metrics.last_resync_first
              << " last_resync_final=" << metrics.last_resync_final;
          if (metrics.last_resync_reason ==
              service::ResyncReason::InputCapacity) {
            *options_.output
                << " last_input_side=" << metrics.last_input_side
                << " last_input_capacity=" << metrics.last_input_capacity;
          }
        }
        *options_.output << '\n';
      }
    }
    connections_.clear();
    failure_reported_.clear();
    manager_.reset();
    started_ = false;
  }

  std::span<const ResolvedSegment> resolved_segments() const noexcept {
    return segments_;
  }

  bool failed() const noexcept { return failed_; }
  std::string_view error() const noexcept { return error_; }

 private:
  api::Result<void> fail(api::ErrorCode code, std::string message) {
    set_failure(message);
    return {.error = code, .message = std::move(message)};
  }

  void set_failure(std::string message) {
    failed_ = true;
    error_ = std::move(message);
    if (options_.error_output != nullptr) {
      *options_.error_output << error_ << '\n';
    }
  }

  void cleanup_connections() noexcept {
    if (manager_) {
      manager_->stop();
      manager_.reset();
    }
    connections_.clear();
    failure_reported_.clear();
    started_ = false;
  }

  ProducerRuntimeOptions options_;
  ProducerConfig config_;
  std::unique_ptr<service::VenueConnectionManager> manager_;
  std::vector<service::VenueConnection *> connections_;
  std::vector<bool> failure_reported_;
  std::vector<ResolvedSegment> segments_;
  std::string error_;
  bool started_{};
  bool failed_{};
};

ProducerRuntime::ProducerRuntime(ProducerRuntimeOptions options)
    : impl_(std::make_unique<Impl>(std::move(options))) {}

std::unique_ptr<ProducerRuntime> ProducerRuntime::create(
    std::string config_path, std::string &error) noexcept {
  try {
    error.clear();
    return std::make_unique<ProducerRuntime>(
        ProducerRuntimeOptions{.config_path = std::move(config_path)});
  } catch (const std::exception &exception) {
    error = exception.what();
  } catch (...) {
    error = "failed to create producer runtime";
  }
  return {};
}

ProducerRuntime::~ProducerRuntime() {
  if (impl_) {
    impl_->stop();
  }
}
ProducerRuntime::ProducerRuntime(ProducerRuntime &&) noexcept = default;
ProducerRuntime &ProducerRuntime::operator=(ProducerRuntime &&) noexcept =
    default;

api::Result<void> ProducerRuntime::start() noexcept { return impl_->start(); }
int ProducerRuntime::run_once(int timeout_ms) noexcept {
  return impl_->run_once(timeout_ms);
}
void ProducerRuntime::stop() noexcept { impl_->stop(); }
std::span<const ResolvedSegment>
ProducerRuntime::resolved_segments() const noexcept {
  return impl_->resolved_segments();
}
bool ProducerRuntime::failed() const noexcept { return impl_->failed(); }
std::string_view ProducerRuntime::error() const noexcept {
  return impl_->error();
}

}  // namespace mds::producer
