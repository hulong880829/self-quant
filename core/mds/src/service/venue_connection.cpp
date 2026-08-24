#include "mds/service/venue_connection.h"

#include "mds/book/book_pipeline.h"
#include "mds/exchange/capabilities.h"
#include "mds/exchange/polymarket/polymarket_adapter.h"
#include "net/http_client.h"
#include "net/websocket_client.h"
#include "mds/publish/wire_publisher.h"
#include "mds/service/connection_deadlines.h"
#include "mds/service/snapshot_recovery.h"
#include "mds/service/snapshot_http_policy.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <charconv>
#include <chrono>
#include <cctype>
#include <cmath>
#include <cstring>
#include <cstdlib>
#include <iostream>
#include <limits>
#include <optional>
#include <openssl/evp.h>
#include <openssl/hmac.h>
#include <random>
#include <span>
#include <sys/epoll.h>
#include <utility>

namespace mds::service {
namespace {

using Clock = MarketDataSession::Clock;
using Product = utils::md::ProductType;
using Venue = utils::md::Venue;

struct Endpoint {
  std::string host;
  std::string service{"443"};
  std::string target{"/"};
};

Endpoint parse_endpoint(std::string_view configured) {
  Endpoint result;
  std::string_view value = configured;
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
      colon != std::string_view::npos &&
      authority.find(':') == colon) {
    result.host.assign(authority.substr(0, colon));
    result.service.assign(authority.substr(colon + 1));
  } else {
    result.host.assign(authority);
  }
  return result;
}

std::uint64_t clock_ticks() noexcept {
  return static_cast<std::uint64_t>(
      Clock::now().time_since_epoch().count());
}

template <std::size_t Size>
void copy_text(std::array<char, Size> &destination,
               std::string_view source) noexcept {
  const auto count = std::min(source.size(), Size - 1);
  std::memcpy(destination.data(), source.data(), count);
  destination[count] = '\0';
}

std::string escape_diagnostic_bytes(std::string_view value) {
  static constexpr char hex[] = "0123456789abcdef";
  std::string escaped;
  escaped.reserve(value.size() + 2);
  escaped.push_back('"');
  for (const char character : value) {
    const auto byte = static_cast<unsigned char>(character);
    switch (byte) {
      case '"':
        escaped.append("\\\"");
        break;
      case '\\':
        escaped.append("\\\\");
        break;
      case '\b':
        escaped.append("\\b");
        break;
      case '\f':
        escaped.append("\\f");
        break;
      case '\n':
        escaped.append("\\n");
        break;
      case '\r':
        escaped.append("\\r");
        break;
      case '\t':
        escaped.append("\\t");
        break;
      default:
        if (byte >= 0x20U && byte <= 0x7eU) {
          escaped.push_back(static_cast<char>(byte));
        } else {
          escaped.append("\\x");
          escaped.push_back(hex[byte >> 4U]);
          escaped.push_back(hex[byte & 0x0fU]);
        }
        break;
    }
  }
  escaped.push_back('"');
  return escaped;
}

std::string venue_symbol(Venue venue, Product product,
                         std::string_view canonical) {
  if (venue == Venue::Okx || venue == Venue::Gate) {
    static constexpr std::string_view quotes[] = {"USDT", "USDC", "USD"};
    for (const auto quote : quotes) {
      if (canonical.size() > quote.size() &&
          canonical.substr(canonical.size() - quote.size()) == quote) {
        const auto separator = venue == Venue::Okx ? '-' : '_';
        std::string result(canonical.substr(
            0, canonical.size() - quote.size()));
        result.push_back(separator);
        result.append(quote);
        if (venue == Venue::Okx && product == Product::Perpetual) {
          result.append("-SWAP");
        }
        return result;
      }
    }
  }
  if (venue == Venue::Hyperliquid && product == Product::Perpetual) {
    constexpr std::string_view suffix = "USDT";
    if (canonical.size() > suffix.size() &&
        canonical.substr(canonical.size() - suffix.size()) == suffix) {
      return std::string(
          canonical.substr(0, canonical.size() - suffix.size()));
    }
  }
  if (venue == Venue::Hyperliquid && product == Product::Spot) {
    for (const auto quote :
         {std::string_view{"USDC"}, std::string_view{"USDT"}}) {
      if (canonical.size() > quote.size() &&
          canonical.substr(canonical.size() - quote.size()) == quote) {
        std::string result(canonical.substr(
            0, canonical.size() - quote.size()));
        result.push_back('_');
        result.append(quote);
        return result;
      }
    }
  }
  return std::string(canonical);
}

std::string okx_login(const PublicWsCredentials &credentials,
                      std::string &error) {
  const auto seconds =
      std::chrono::duration_cast<std::chrono::seconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count();
  char timestamp[32]{};
  const auto converted =
      std::to_chars(timestamp, timestamp + sizeof(timestamp), seconds);
  if (converted.ec != std::errc{}) {
    error = "failed to format OKX login timestamp";
    return {};
  }
  const std::string_view timestamp_view(
      timestamp, static_cast<std::size_t>(converted.ptr - timestamp));
  std::string prehash(timestamp_view);
  prehash.append("GET/users/self/verify");
  unsigned char digest[EVP_MAX_MD_SIZE]{};
  unsigned int digest_size{};
  if (HMAC(EVP_sha256(), credentials.secret.data(),
           static_cast<int>(credentials.secret.size()),
           reinterpret_cast<const unsigned char *>(prehash.data()),
           prehash.size(), digest, &digest_size) == nullptr) {
    error = "failed to sign OKX public WebSocket login";
    return {};
  }
  std::array<unsigned char, 128> encoded{};
  const int encoded_size =
      EVP_EncodeBlock(encoded.data(), digest,
                      static_cast<int>(digest_size));
  if (encoded_size <= 0) {
    error = "failed to encode OKX public WebSocket signature";
    return {};
  }
  std::string result;
  result.reserve(credentials.api_key.size() +
                 credentials.passphrase.size() +
                 static_cast<std::size_t>(encoded_size) + 128);
  result.append("{\"op\":\"login\",\"args\":[{\"apiKey\":\"");
  result.append(credentials.api_key);
  result.append("\",\"passphrase\":\"");
  result.append(credentials.passphrase);
  result.append("\",\"timestamp\":\"");
  result.append(timestamp_view);
  result.append("\",\"sign\":\"");
  result.append(reinterpret_cast<const char *>(encoded.data()),
                static_cast<std::size_t>(encoded_size));
  result.append("\"}]}");
  return result;
}

utils::md::EventHeader make_header(utils::md::InstrumentId instrument_id,
                                   std::uint32_t generation,
                                   std::uint64_t source_sequence,
                                   std::uint64_t exchange_time_ms,
                                   utils::md::BookState state) noexcept {
  utils::md::EventHeader header{};
  header.instrument_id = instrument_id;
  header.book_generation = generation;
  header.source_seq = source_sequence;
  header.exchange_ts_ns =
      exchange_time_ms <=
              std::numeric_limits<std::uint64_t>::max() / 1'000'000
          ? exchange_time_ms * 1'000'000
          : 0;
  header.receive_tsc = clock_ticks();
  header.publish_tsc = header.receive_tsc;
  header.state = state;
  header.source_id = 1;
  return header;
}

bool terminal(net::WebSocketClientState state) noexcept {
  return state == net::WebSocketClientState::Closed ||
         state == net::WebSocketClientState::TimedOut ||
         state == net::WebSocketClientState::Failed;
}

bool terminal(net::HttpClientState state) noexcept {
  return state == net::HttpClientState::TimedOut ||
         state == net::HttpClientState::Failed;
}

}  // namespace

std::vector<std::vector<std::size_t>> PartitionWebSocketSymbols(
    std::span<const SymbolStreamOptions> streams,
    std::size_t max_symbols_per_ws, Venue venue) {
  if (streams.empty()) {
    return {};
  }
  std::vector<std::size_t> ordered(streams.size());
  for (std::size_t index = 0; index < ordered.size(); ++index) {
    ordered[index] = index;
  }
  std::stable_sort(
      ordered.begin(), ordered.end(),
      [streams](std::size_t left, std::size_t right) {
        return streams[left].symbol < streams[right].symbol;
      });
  const auto limit =
      venue == Venue::Polymarket || max_symbols_per_ws == 0
          ? std::numeric_limits<std::size_t>::max()
          : max_symbols_per_ws;
  std::vector<std::vector<std::size_t>> result(1);
  std::string_view previous;
  std::size_t canonical_count{};
  for (const auto index : ordered) {
    const auto current = std::string_view(streams[index].symbol);
    if (canonical_count == 0 || current != previous) {
      if (canonical_count != 0 && canonical_count % limit == 0) {
        result.emplace_back();
      }
      ++canonical_count;
      previous = current;
    }
    result.back().push_back(index);
  }
  return result;
}

class VenueConnection::Impl {
 public:
  struct WsShard {
    WsShard(std::size_t shard_id, net::SharedSslContext tls,
            Venue venue, Product product, std::size_t max_levels)
        : websocket(std::make_unique<net::WebSocketClient>(
              std::move(tls))),
          adapter(exchange::make_venue_adapter(
              venue, product, max_levels)),
          event(std::make_unique<exchange::NormalizedEvent>(
              max_levels)),
          id(shard_id) {
      if (adapter) {
        heartbeat = adapter->heartbeat();
      }
    }

    std::unique_ptr<net::WebSocketClient> websocket;
    std::unique_ptr<exchange::VenueAdapter> adapter;
    exchange::HeartbeatSpec heartbeat;
    ConnectionDeadlines deadlines;
    std::vector<std::size_t> symbol_indices;
    std::vector<exchange::StreamRequest> requests;
    std::vector<std::string> batches;
    std::unique_ptr<exchange::NormalizedEvent> event;
    exchange::ParseFailure parse_failure;
    SubscriptionBudget budget;
    std::string login_message;
    std::string pending_reconnect_reason;
    Clock::time_point last_message{};
    Clock::time_point last_heartbeat{};
    Clock::time_point last_data_send{};
    Clock::time_point reconnect_at{};
    Clock::time_point connect_at{};
    Clock::time_point acknowledgement_deadline{};
    Clock::time_point next_budget_retry{};
    std::size_t id{};
    std::size_t next_batch{};
    std::size_t pending_acknowledgements{};
    std::uint32_t reconnect_attempt{};
    int registered_fd{-1};
    std::uint64_t registered_generation{};
    bool requires_login{};
    bool login_sent{};
    bool login_acked{};
    bool awaiting_ack{};
    bool all_subscribed{};
    bool open_seen{};
    bool connection_started{};
    MarketDataState state{MarketDataState::Stopped};
  };

  struct PendingDelta {
    std::uint64_t first{};
    std::uint64_t final{};
    std::uint64_t previous{};
    std::uint64_t time_ms{};
    std::vector<utils::md::Level> bids;
    std::vector<utils::md::Level> asks;
    bool strict_previous_sequence{};
  };

  struct SymbolRuntime {
    SymbolStreamOptions options;
    std::string canonical_identity;
    std::string venue_symbol;
    exchange::InstrumentMetadata metadata;
    utils::md::Instrument instrument{};
    utils::md::InstrumentCatalog catalog{};
    std::unique_ptr<publish::WirePublisher> owned_ticker_publisher;
    std::unique_ptr<publish::WirePublisher> owned_book_publisher;
    publish::WirePublisher *ticker_publisher{};
    publish::WirePublisher *book_publisher{};
    std::unique_ptr<book::BookPipeline> pipeline;
    std::vector<PendingDelta> pending;
    std::optional<utils::md::BboEvent> latest_bbo;
    std::uint64_t last_ticker_sequence{};
    std::uint64_t last_book_sequence{};
    std::uint64_t last_ticker_exchange_time_ms{};
    std::uint64_t last_book_exchange_time_ms{};
    std::uint32_t generation{1};
    LaggingSnapshotRetries lagging_snapshot_retries;
    WsSnapshotRecovery ws_snapshot_recovery;
    Clock::time_point last_resubscribe{};
    Clock::time_point resubscribe_not_before{};
    Clock::time_point recovery_started{};
    std::uint32_t consecutive_snapshot_failures{};
    std::uint32_t catalog_generation{};
    bool metadata_ready{};
    bool image_ready{};
    bool awaiting_snapshot_bridge{};
    bool ticker_live{};
    bool book_live{};
    bool snapshot_quarantined{};
    bool needs_snapshot{};
    bool resubscribe_pending{};
    std::size_t shard_id{};
  };

  Impl(net::EpollLoop &loop, net::SharedSslContext tls,
       VenueConnectionOptions options)
      : loop_(loop), tls_(std::move(tls)), options_(std::move(options)),
        metadata_http_(tls_, 16U << 10U, 32U << 20U, 8U << 10U),
        snapshot_http_(tls_),
        discovery_http_(tls_) {
    if (!options_.instrument_manager) {
      options_.instrument_manager =
          std::make_shared<instrument::InstrumentManager>();
    }
    std::size_t max_levels = 20;
    symbols_.reserve(options_.streams.size());
    requests_.reserve(options_.streams.size());
    for (auto &stream : options_.streams) {
      max_levels = std::max(max_levels, stream.max_levels_per_message);
      SymbolRuntime runtime;
      runtime.venue_symbol =
          venue_symbol(options_.venue, options_.product, stream.symbol);
      runtime.options = stream;
      runtime.pending.reserve(64);
      symbols_.push_back(std::move(runtime));
    }
    snapshot_event_ =
        std::make_unique<exchange::NormalizedEvent>(max_levels);
    for (const auto &symbol : symbols_) {
      requests_.push_back(
          {symbol.options.symbol, symbol.venue_symbol,
           symbol.options.ticker_channel,
           symbol.options.orderbook_channel, symbol.options.ticker,
           symbol.options.orderbook,
           symbol.options.update_interval_ms});
    }
    const auto partitions = PartitionWebSocketSymbols(
        options_.streams, options_.max_symbols_per_ws, options_.venue);
    const auto shard_count = std::max<std::size_t>(1, partitions.size());
    ws_shards_.reserve(shard_count);
    for (std::size_t id = 0; id < shard_count; ++id) {
      ws_shards_.emplace_back(
          id, tls_, options_.venue, options_.product, max_levels);
    }
    for (std::size_t shard_id = 0; shard_id < partitions.size();
         ++shard_id) {
      for (const auto symbol_index : partitions[shard_id]) {
        symbols_[symbol_index].shard_id = shard_id;
        ws_shards_[shard_id].symbol_indices.push_back(symbol_index);
        ws_shards_[shard_id].requests.push_back(requests_[symbol_index]);
      }
    }
    if (options_.venue != Venue::Polymarket) {
      metadata_adapter_ = exchange::make_venue_adapter(
          options_.venue, options_.product, max_levels);
    }
    metrics_.ws_shards = ws_shards_.size();
    for (std::size_t id = 0; id < ws_shards_.size(); ++id) {
      ws_shards_[id].websocket->set_frame_callback(
          [this, id](const net::WsFrameView &frame) {
            return on_frame(ws_shards_[id], frame);
          });
    }
  }

  exchange::VenueAdapter &metadata_adapter() noexcept {
    return metadata_adapter_ ? *metadata_adapter_
                             : *ws_shards_.front().adapter;
  }

  WsShard &shard_for(const SymbolRuntime &symbol) noexcept {
    return ws_shards_[symbol.shard_id];
  }

  api::Result<void> start(Clock::time_point now) {
    if (started_) {
      return {.error = api::ErrorCode::AlreadyInitialized,
              .message = "venue connection already started"};
    }
    if (symbols_.empty() || ws_shards_.empty() ||
        std::any_of(
            ws_shards_.begin(), ws_shards_.end(),
            [](const WsShard &shard) {
              return shard.adapter == nullptr;
            }) ||
        (options_.venue != Venue::Polymarket &&
         metadata_adapter_ == nullptr)) {
      return {.error = api::ErrorCode::InvalidConfig,
              .message = "invalid venue connection options"};
    }
    const auto *caps =
        exchange::capabilities(options_.venue, options_.product);
    bool requires_login{};
    for (auto &shard : ws_shards_) {
      shard.requires_login = std::any_of(
          shard.symbol_indices.begin(), shard.symbol_indices.end(),
          [this, caps](std::size_t index) {
            const auto &symbol = symbols_[index];
            return symbol.options.orderbook && caps != nullptr &&
                   symbol.options.orderbook_channel ==
                       caps->fastest_top10.channel &&
                   caps->fastest_top10.requires_public_ws_login;
          });
      requires_login = requires_login || shard.requires_login;
    }
    if (requires_login && !options_.credentials.complete()) {
      fail("fastest OKX orderbook requires complete public WebSocket "
           "credentials");
      return {.error = api::ErrorCode::InvalidConfig,
              .message = error_};
    }
    if (!open_multiplex_rings()) {
      stop();
      fail(error_);
      return {.error = api::ErrorCode::ShmCreateFailed,
              .message = error_};
    }
    for (auto &symbol : symbols_) {
      symbol.recovery_started = now;
      if (!open_output_rings(symbol)) {
        stop();
        fail(error_);
        return {.error = api::ErrorCode::ShmCreateFailed,
                .message = error_};
      }
    }
    started_ = true;
    constexpr auto startup_stagger = std::chrono::milliseconds(50);
    for (auto &shard : ws_shards_) {
      shard.last_message = now;
      shard.last_heartbeat = now;
      shard.connect_at = now + startup_stagger * shard.id;
      shard.state = MarketDataState::Connecting;
    }
    if (!begin_metadata(now) ||
        !begin_connection(ws_shards_.front(), now)) {
      const auto message = error_;
      schedule_connection_rebuild(
          message.empty() ? "failed to start venue transport" : message,
          now);
      return {};
    }
    update_aggregate_state();
    return {};
  }

  bool open_output_rings(SymbolRuntime &symbol) {
    const auto profile =
        exchange::segment_profile(options_.venue, options_.product);
    const bool per_symbol =
        symbol.options.ring_layout != publish::RingLayout::Multiplex;
    if (symbol.options.ticker && per_symbol && !symbol.ticker_publisher) {
      auto ring = symbol.options.ring;
      ring.name = publish::make_publisher_segment_name(
          symbol.options.shm_prefix, profile, symbol.options.symbol,
          "ticker");
      auto opened = transport::SharedRing::open(ring);
      if (!opened) {
        error_ = opened.message;
        return false;
      }
      symbol.owned_ticker_publisher =
          std::make_unique<publish::WirePublisher>(
              std::move(opened.value));
      symbol.ticker_publisher = symbol.owned_ticker_publisher.get();
    }
    if (symbol.options.orderbook && per_symbol && !symbol.book_publisher) {
      auto ring = symbol.options.ring;
      ring.name = publish::make_publisher_segment_name(
          symbol.options.shm_prefix, profile, symbol.options.symbol,
          "orderbook");
      auto opened = transport::SharedRing::open(ring);
      if (!opened) {
        error_ = opened.message;
        return false;
      }
      symbol.owned_book_publisher =
          std::make_unique<publish::WirePublisher>(
              std::move(opened.value));
      symbol.book_publisher = symbol.owned_book_publisher.get();
    }
    return true;
  }

  bool open_multiplex_rings() {
    if (options_.streams.empty() ||
        options_.streams.front().ring_layout ==
            publish::RingLayout::PerSymbol) {
      return true;
    }
    const auto &stream = options_.streams.front();
    const auto venue = exchange::venue_name(options_.venue);
    const auto product = exchange::product_name(options_.product);
    bool ticker{};
    bool orderbook{};
    for (const auto &entry : options_.streams) {
      ticker = ticker || entry.ticker;
      orderbook = orderbook || entry.orderbook;
    }
    multiplex_ticker_.resize(stream.shard_count);
    multiplex_book_.resize(stream.shard_count);
    for (std::size_t shard = 0; shard < stream.shard_count; ++shard) {
      const auto open_one = [&](std::string_view kind,
                                std::unique_ptr<publish::WirePublisher> &out) {
        if (out) {
          return true;
        }
        auto ring = stream.multiplex_ring;
        ring.name = publish::make_multiplex_segment_name(
            stream.shm_prefix, venue, product, kind, shard);
        auto opened = transport::SharedRing::open(ring);
        if (!opened) {
          error_ = opened.message;
          return false;
        }
        out = std::make_unique<publish::WirePublisher>(
            std::move(opened.value));
        return true;
      };
      if ((ticker && !open_one("ticker", multiplex_ticker_[shard])) ||
          (orderbook && !open_one("orderbook",
                                  multiplex_book_[shard]))) {
        return false;
      }
    }
    return true;
  }

  publish::WirePublisher *multiplex_publisher(
      bool orderbook, utils::md::InstrumentId id) noexcept {
    auto &publishers = orderbook ? multiplex_book_ : multiplex_ticker_;
    if (publishers.empty()) return nullptr;
    return publishers[id % publishers.size()].get();
  }

  bool publish_catalog_barrier(SymbolRuntime &symbol) {
    auto header = make_header(symbol.instrument.instrument_id,
                              symbol.generation, 0, 0,
                              utils::md::BookState::Building);
    const auto publish_one = [&](publish::WirePublisher *publisher) {
      return publisher == nullptr ||
             (publisher->publish_instrument_catalog(header, symbol.catalog) &&
              publisher->publish_instrument(header, symbol.instrument));
    };
    if (!publish_one(symbol.ticker_publisher) ||
        !publish_one(symbol.book_publisher)) {
      ++metrics_.publish_errors;
      symbol.catalog_generation = 0;
      error_ = "failed to publish instrument catalog barrier";
      begin_recovery(shard_for(symbol), Clock::now());
      return false;
    }
    symbol.catalog_generation = symbol.generation;
    return true;
  }

  bool republish_ticker_state(
      SymbolRuntime &symbol, publish::WirePublisher &publisher) {
    auto header = make_header(
        symbol.instrument.instrument_id, symbol.generation,
        symbol.last_ticker_sequence, 0,
        symbol.ticker_live ? utils::md::BookState::Live
                           : utils::md::BookState::Building);
    if (!publisher.publish_instrument_catalog(header, symbol.catalog) ||
        !publisher.publish_instrument(header, symbol.instrument) ||
        (symbol.latest_bbo &&
         !publisher.publish_bbo(*symbol.latest_bbo))) {
      ++metrics_.publish_errors;
      fail("failed to republish ticker state for late reader");
      return false;
    }
    return true;
  }

  bool republish_book_state(
      SymbolRuntime &symbol, publish::WirePublisher &publisher) {
    auto header = make_header(
        symbol.instrument.instrument_id, symbol.generation,
        symbol.last_book_sequence, 0,
        symbol.book_live ? utils::md::BookState::Live
                         : utils::md::BookState::Building);
    if (!publisher.publish_instrument_catalog(header, symbol.catalog) ||
        !publisher.publish_instrument(header, symbol.instrument) ||
        (symbol.image_ready &&
         (!publisher.publish_snapshot(header, symbol.pipeline->book()) ||
          !symbol.pipeline->PublishCanonical(header)))) {
      ++metrics_.publish_errors;
      fail("failed to republish orderbook state for late reader");
      return false;
    }
    return true;
  }

  bool begin_metadata(Clock::time_point now) {
    metadata_batches_.clear();
    metadata_.clear();
    metadata_batch_index_ = 0;
    auto &adapter = metadata_adapter();
    if (!adapter.build_metadata_request_batches(
            requests_, metadata_batches_, error_) ||
        metadata_batches_.empty()) {
      if (error_.empty()) {
        error_ = "venue produced no metadata requests";
      }
      return false;
    }
    metadata_.reserve(symbols_.size());
    return start_metadata_batch(now);
  }

  bool refresh_metadata(std::string_view reason) {
    if (!metadata_ready_) {
      return true;
    }
    const auto now = Clock::now();
    for (auto &shard : ws_shards_) {
      schedule_reconnect(shard, "venue metadata refresh required", now);
    }
    metadata_ready_ = false;
    for (auto &symbol : symbols_) {
      symbol.metadata_ready = false;
      if (symbol.recovery_started == Clock::time_point{}) {
        symbol.recovery_started = now;
      }
    }
    std::cerr << exchange::venue_name(options_.venue)
              << " metadata refresh scheduled product="
              << exchange::product_name(options_.product)
              << " reason=" << escape_diagnostic_bytes(reason) << '\n';
    if (!begin_metadata(now)) {
      fail(error_.empty() ? "failed to refresh venue metadata" : error_);
      return false;
    }
    update_aggregate_state();
    return true;
  }

  bool start_metadata_batch(Clock::time_point now) {
    if (metadata_batch_index_ >= metadata_batches_.size()) {
      error_ = "metadata request index is out of range";
      return false;
    }
    const auto &batch = metadata_batches_[metadata_batch_index_];
    if (batch.request_count == 0 ||
        batch.request_offset > requests_.size() ||
        batch.request_count > requests_.size() - batch.request_offset) {
      error_ = "invalid metadata request batch range";
      return false;
    }
    const auto endpoint = parse_endpoint(options_.rest_endpoint);
    const auto &request = batch.http;
    bool started{};
    if (request.method == exchange::HttpRequestSpec::Method::Post) {
      started = metadata_http_.start_post(
          endpoint.host, endpoint.service, request.target,
          request.content_type,
          {reinterpret_cast<const std::byte *>(request.body.data()),
           request.body.size()},
          now + options_.request_timeout);
    } else {
      started = metadata_http_.start_get(
          endpoint.host, endpoint.service, request.target,
          now + options_.request_timeout);
    }
    if (!started) {
      error_ = std::string(metadata_http_.error_message());
      return false;
    }
    sync_http_registration(HttpKind::Metadata);
    return true;
  }

  bool begin_connection(WsShard &shard, Clock::time_point now) {
    const auto endpoint = parse_endpoint(options_.websocket_endpoint);
    if (!shard.websocket->start(
            endpoint.host, endpoint.service, endpoint.target,
            now + options_.connect_timeout)) {
      error_ = std::string(shard.websocket->error_message());
      return false;
    }
    shard.connection_started = true;
    shard.state = MarketDataState::Connecting;
    shard.open_seen = false;
    shard.login_sent = false;
    shard.login_acked = false;
    shard.next_batch = 0;
    shard.pending_acknowledgements = 0;
    shard.awaiting_ack = false;
    shard.all_subscribed = false;
    shard.deadlines.reset_connection();
    shard.acknowledgement_deadline = {};
    shard.last_message = now;
    shard.last_heartbeat = now;
    shard.last_data_send = {};
    sync_ws_registration(shard);
    return true;
  }

  enum class HttpKind : std::uint8_t { Metadata, Snapshot, Discovery };

  net::HttpClient &http(HttpKind kind) noexcept {
    switch (kind) {
      case HttpKind::Metadata:
        return metadata_http_;
      case HttpKind::Snapshot:
        return snapshot_http_;
      case HttpKind::Discovery:
        return discovery_http_;
    }
    return metadata_http_;
  }

  int &registered_fd(HttpKind kind) noexcept {
    switch (kind) {
      case HttpKind::Metadata:
        return metadata_fd_;
      case HttpKind::Snapshot:
        return snapshot_fd_;
      case HttpKind::Discovery:
        return discovery_fd_;
    }
    return metadata_fd_;
  }

  std::uint64_t &registered_generation(HttpKind kind) noexcept {
    switch (kind) {
      case HttpKind::Metadata:
        return metadata_generation_;
      case HttpKind::Snapshot:
        return snapshot_generation_;
      case HttpKind::Discovery:
        return discovery_generation_;
    }
    return metadata_generation_;
  }

  void sync_ws_registration(WsShard &shard) {
    const int fd = shard.websocket->fd();
    const auto generation = shard.websocket->socket_generation();
    if (shard.registered_fd >= 0 &&
        (shard.registered_fd != fd ||
         shard.registered_generation != generation)) {
      loop_.remove(shard.registered_fd);
      shard.registered_fd = -1;
    }
    if (fd < 0) {
      return;
    }
    if (shard.registered_fd < 0) {
      const auto shard_index =
          static_cast<std::size_t>(&shard - ws_shards_.data());
      if (loop_.add(fd, shard.websocket->wanted_events(),
                    [this, shard_index](std::uint32_t events) {
                      on_ws_event(ws_shards_[shard_index], events);
                    })) {
        shard.registered_fd = fd;
        shard.registered_generation = generation;
      }
    } else {
      loop_.modify(fd, shard.websocket->wanted_events());
    }
  }

  void sync_http_registration(HttpKind kind) {
    auto &client = http(kind);
    auto &stored_fd = registered_fd(kind);
    auto &stored_generation = registered_generation(kind);
    const int fd = client.fd();
    const auto generation = client.socket_generation();
    if (stored_fd >= 0 &&
        (stored_fd != fd || stored_generation != generation)) {
      loop_.remove(stored_fd);
      stored_fd = -1;
    }
    if (fd < 0) {
      return;
    }
    if (stored_fd < 0) {
      if (loop_.add(fd, client.wanted_events(),
                    [this, kind](std::uint32_t events) {
                      on_http_event(kind, events);
                    })) {
        stored_fd = fd;
        stored_generation = generation;
      }
    } else {
      loop_.modify(fd, client.wanted_events());
    }
  }

  void on_ws_event(WsShard &shard, std::uint32_t events) {
    shard.websocket->on_event(events);
    sync_ws_registration(shard);
    if (terminal(shard.websocket->state())) {
      const auto reason = shard.pending_reconnect_reason.empty()
                              ? std::string(shard.websocket->error_message())
                              : std::move(shard.pending_reconnect_reason);
      shard.pending_reconnect_reason.clear();
      schedule_reconnect(shard, reason);
    }
  }

  void on_http_event(HttpKind kind, std::uint32_t events) {
    auto &client = http(kind);
    const auto state = client.on_event(events);
    sync_http_registration(kind);
    if (state == net::HttpClientState::Complete) {
      if (registered_fd(kind) >= 0) {
        loop_.remove(registered_fd(kind));
        registered_fd(kind) = -1;
      }
      const auto body = client.response().body();
      const std::string_view json(
          reinterpret_cast<const char *>(body.data()), body.size());
      if (kind == HttpKind::Metadata) {
        handle_metadata(json);
      } else if (kind == HttpKind::Snapshot) {
        handle_snapshot_response(json, client.response().status_code());
      } else {
        handle_discovery(json, client.response().status_class());
      }
    } else if (terminal(state)) {
      if (kind == HttpKind::Discovery) {
        discovery_failed(client.error_message());
      } else if (kind == HttpKind::Snapshot) {
        snapshot_backoff(client.error_message(), Clock::now());
      } else {
        fail(std::string("metadata HTTP: ") +
             std::string(client.error_message()));
      }
    }
  }

  void log_ws_diagnostic(const WsShard &shard, std::string_view kind,
                         std::string_view reason, std::string_view payload,
                         std::string_view symbol = {}) const {
    std::cerr << exchange::venue_name(options_.venue) << " websocket "
              << kind << " failed product="
              << exchange::product_name(options_.product)
              << " shard=" << shard.id
              << " connection_generation="
              << shard.websocket->socket_generation();
    if (!symbol.empty()) {
      std::cerr << " symbol=" << escape_diagnostic_bytes(symbol);
    }
    std::cerr << " payload_bytes=" << payload.size()
              << " error=" << escape_diagnostic_bytes(reason)
              << " payload=" << escape_diagnostic_bytes(payload) << '\n';
  }

  bool on_frame(WsShard &shard, const net::WsFrameView &frame) {
    if (frame.opcode != net::WsOpcode::Text) {
      return true;
    }
    shard.last_message = Clock::now();
    const std::string_view json(
        reinterpret_cast<const char *>(frame.payload.data()),
        frame.payload.size());
    shard.event->reset();
    shard.parse_failure.clear();
    if (!shard.adapter->parse_ws(
            json, *shard.event, shard.parse_failure)) {
      ++metrics_.parse_errors;
      const auto reason =
          shard.parse_failure.diagnostic_view().empty()
              ? std::string_view("malformed venue WebSocket message")
              : shard.parse_failure.diagnostic_view();
      if ((shard.parse_failure.category ==
               exchange::ParseFailureCategory::DirtyData ||
           shard.parse_failure.category ==
               exchange::ParseFailureCategory::SequenceGap) &&
          shard.parse_failure.scope ==
              exchange::ParseFailureScope::Symbol &&
          shard.parse_failure.has_symbol()) {
        auto *symbol = find_symbol(
            shard, shard.parse_failure.symbol_view());
        if (symbol != nullptr && symbol->metadata_ready) {
          log_ws_diagnostic(shard, "parse", reason, json,
                            shard.parse_failure.symbol_view());
          return resync_symbol(
              shard, *symbol,
              shard.parse_failure.category ==
                      exchange::ParseFailureCategory::SequenceGap
                  ? ResyncReason::LiveSequenceGap
                  : ResyncReason::DirtyData,
              reason);
        }
      }
      if (shard.parse_failure.category ==
          exchange::ParseFailureCategory::ConfigurationMetadata) {
        (void)refresh_metadata(reason);
        return false;
      }
      log_ws_diagnostic(shard, "parse", reason, json,
                        shard.parse_failure.symbol_view());
      shard.pending_reconnect_reason.assign(reason);
      return false;
    }
    if (!dispatch_adapter_event(
            shard, *shard.event,
            shard.parse_failure.diagnostic_view())) {
      if (options_.venue == Venue::Binance) {
        log_ws_diagnostic(
            shard, "dispatch",
            error_.empty() ? "Binance adapter event dispatch failed" : error_,
            json, shard.event->symbol_view());
      }
      return false;
    }
    std::size_t drained = 0;
    while (shard.adapter->has_pending_events()) {
      if (++drained > 4096) {
        ++metrics_.parse_errors;
        schedule_reconnect(
            shard, "venue adapter pending event budget exceeded");
        if (options_.venue == Venue::Binance) {
          log_ws_diagnostic(shard, "drain", error_, json,
                            shard.event->symbol_view());
        }
        return false;
      }
      shard.event->reset();
      shard.parse_failure.clear();
      if (!shard.adapter->parse_ws(
              {}, *shard.event, shard.parse_failure)) {
        ++metrics_.parse_errors;
        const auto drain_error =
            shard.parse_failure.diagnostic_view().empty()
                ? std::string_view(
                      "failed to drain venue adapter event")
                : shard.parse_failure.diagnostic_view();
        schedule_reconnect(
            shard, drain_error);
        if (options_.venue == Venue::Binance) {
          log_ws_diagnostic(shard, "drain", error_, json,
                            shard.event->symbol_view());
        }
        return false;
      }
      if (!dispatch_adapter_event(
              shard, *shard.event,
              shard.parse_failure.diagnostic_view())) {
        if (options_.venue == Venue::Binance) {
          log_ws_diagnostic(
              shard, "dispatch",
              error_.empty() ? "Binance pending event dispatch failed"
                             : error_,
              json, shard.event->symbol_view());
        }
        return false;
      }
    }
    return true;
  }

  bool dispatch_adapter_event(
      WsShard &shard,
      const exchange::NormalizedEvent &event,
      std::string_view parse_error) {
    switch (event.type) {
    case exchange::AdapterEventType::SubscribeError:
      ++metrics_.subscription_rejections;
      schedule_reconnect(
          shard,
          parse_error.empty() ? "venue rejected subscription or login"
                              : parse_error);
      return false;
    case exchange::AdapterEventType::SubscribeAck:
      if (shard.requires_login && shard.login_sent &&
          !shard.login_acked) {
        shard.login_acked = true;
        shard.pending_acknowledgements = 0;
        shard.awaiting_ack = false;
        shard.acknowledgement_deadline = {};
      } else if (shard.awaiting_ack) {
        if (shard.pending_acknowledgements > 1) {
          --shard.pending_acknowledgements;
          return true;
        }
        shard.pending_acknowledgements = 0;
        shard.awaiting_ack = false;
        shard.acknowledgement_deadline = {};
        ++shard.next_batch;
        if (shard.next_batch == shard.batches.size()) {
          shard.all_subscribed = true;
          shard.deadlines.subscriptions_ready(
              Clock::now(),
              shard.deadlines.reached_live_once()
                  ? options_.recovery_deadline
                  : options_.request_timeout);
          update_live_state();
        }
      }
      return true;
    case exchange::AdapterEventType::Bbo:
    case exchange::AdapterEventType::BookSnapshot:
    case exchange::AdapterEventType::BookDelta:
    case exchange::AdapterEventType::BookGap:
      return process_market_event(shard, event);
    case exchange::AdapterEventType::InstrumentUpdate:
      return process_instrument_update(shard, event);
    case exchange::AdapterEventType::Pong:
    case exchange::AdapterEventType::Ignored:
      return true;
    }
    return true;
  }

  bool process_instrument_update(
      WsShard &shard,
      const exchange::NormalizedEvent &event) {
    auto *symbol = find_symbol(shard, event.symbol_view());
    if (symbol == nullptr) {
      ++metrics_.parse_errors;
      (void)refresh_metadata(
          "instrument update references unknown symbol");
      return false;
    }
    if (!symbol->metadata_ready || event.tick_size <= 0) {
      ++metrics_.parse_errors;
      fail("instrument update has invalid or unavailable metadata");
      return false;
    }
    if (symbol->instrument.tick_size == event.tick_size) {
      return true;
    }
    const bool was_live = symbol->image_ready;
    if (was_live) {
      ++symbol->generation;
      symbol->catalog_generation = 0;
      symbol->image_ready = false;
      symbol->book_live = false;
      symbol->last_book_sequence = 0;
    }
    symbol->metadata.tick_size = event.tick_size;
    symbol->instrument.tick_size = event.tick_size;
    symbol->catalog.tick_size = event.tick_size;
    if (!publish_catalog_barrier(*symbol) &&
        options_.venue != Venue::Polymarket) {
      fail("failed to publish tick-size catalog barrier");
      return false;
    }
    if (symbol->options.orderbook) {
      symbol->pipeline = std::make_unique<book::BookPipeline>(
          symbol->instrument.instrument_id,
          symbol->options.ladder_ticks_per_side, event.tick_size,
          symbol->book_publisher, false,
          options_.venue == Venue::Polymarket);
    }
    if (was_live && options_.venue == Venue::Polymarket) {
      begin_recovery(shard, Clock::now());
      return true;
    }
    return !was_live ||
           queue_symbol_resubscribe(shard, *symbol, Clock::now());
  }

  SymbolRuntime *find_symbol(
      const WsShard &shard, std::string_view symbol) noexcept {
    const auto found = std::find_if(
        symbols_.begin(), symbols_.end(),
        [&shard, symbol](const SymbolRuntime &entry) {
          if (entry.shard_id != shard.id) {
            return false;
          }
          return entry.venue_symbol == symbol ||
                 entry.options.symbol == symbol;
        });
    return found == symbols_.end() ? nullptr : &*found;
  }

  bool process_market_event(
      WsShard &shard, const exchange::NormalizedEvent &event,
      bool publish_book = true) {
    auto *symbol = find_symbol(shard, event.symbol_view());
    if (symbol == nullptr) {
      ++metrics_.parse_errors;
      (void)refresh_metadata(
          "market event references unknown symbol");
      return false;
    }
    if (!symbol->metadata_ready) {
      ++metrics_.parse_errors;
      fail("market event references symbol without metadata");
      return false;
    }
    if (options_.venue == Venue::Polymarket &&
        symbol->catalog_generation != symbol->generation) {
      return true;
    }
    if (event.type == exchange::AdapterEventType::BookGap) {
      const auto expected =
          symbol->last_book_sequence == 0
              ? 0
              : symbol->last_book_sequence ==
                        std::numeric_limits<std::uint64_t>::max()
                    ? symbol->last_book_sequence
                    : symbol->last_book_sequence + 1;
      metrics_.last_input_capacity = event.input_capacity;
      metrics_.last_input_side =
          event.input_side == exchange::InputSide::Bid
              ? 'b'
              : event.input_side == exchange::InputSide::Ask ? 'a' : '?';
      return resync_symbol(
          shard, *symbol, ResyncReason::InputCapacity,
          "orderbook input capacity gap", expected,
          event.previous_sequence, event.first_sequence,
          event.final_sequence);
    }
    if (event.type == exchange::AdapterEventType::Bbo) {
      if (event.exchange_time_ms != 0) {
        symbol->last_ticker_exchange_time_ms =
            std::max(symbol->last_ticker_exchange_time_ms,
                     event.exchange_time_ms);
      }
      if (!symbol->options.ticker) {
        return true;
      }
      if (event.final_sequence != 0 &&
          event.final_sequence == symbol->last_ticker_sequence) {
        return true;
      }
      auto header = make_header(symbol->instrument.instrument_id,
                                symbol->generation, event.final_sequence,
                                event.exchange_time_ms,
                                utils::md::BookState::Live);
      utils::md::BboEvent bbo{header, event.bid, event.ask};
      if (symbol->ticker_publisher &&
          !symbol->ticker_publisher->publish_bbo(bbo)) {
        ++metrics_.publish_errors;
        if (options_.venue == Venue::Polymarket) {
          schedule_reconnect(shard, "Polymarket BBO publish failed");
          return true;
        }
        fail("failed to publish venue BBO");
        return false;
      }
      symbol->latest_bbo = bbo;
      symbol->last_ticker_sequence = event.final_sequence;
      symbol->ticker_live = true;
      ++metrics_.ticker_updates;
      update_live_state();
      return true;
    }
    if (!symbol->options.orderbook || !symbol->pipeline) {
      return true;
    }
    if (event.exchange_time_ms != 0 &&
        symbol->last_book_exchange_time_ms != 0 &&
        event.exchange_time_ms <
            symbol->last_book_exchange_time_ms &&
        options_.venue == Venue::Hyperliquid) {
      ++symbol->generation;
      symbol->image_ready = false;
      symbol->awaiting_snapshot_bridge = false;
      symbol->book_live = false;
      symbol->last_book_sequence = 0;
      ++metrics_.resyncs;
    }
    symbol->last_book_exchange_time_ms =
        std::max(symbol->last_book_exchange_time_ms,
                 event.exchange_time_ms);
    const bool image_only =
        symbol->options.orderbook_bootstrap ==
            exchange::BookBootstrap::WsImageOnly;
    if (event.sequence_reset && !image_only &&
        options_.venue != Venue::Polymarket &&
        symbol->image_ready) {
      ++symbol->generation;
      symbol->image_ready = false;
      symbol->awaiting_snapshot_bridge = false;
      symbol->last_book_sequence = 0;
      ++metrics_.resyncs;
    }
    auto header = make_header(symbol->instrument.instrument_id,
                              symbol->generation, event.final_sequence,
                              event.exchange_time_ms,
                              utils::md::BookState::Live);
    if (event.type == exchange::AdapterEventType::BookSnapshot) {
      const bool insufficient_depth =
          options_.venue == Venue::Polymarket
              ? event.bids.empty() && event.asks.empty()
              : event.bids.size() < 10 || event.asks.size() < 10;
      if (insufficient_depth) {
        return resync_symbol(
            shard, *symbol, ResyncReason::InvalidImage,
            "orderbook snapshot does not satisfy the top-10 contract");
      }
      symbol->pipeline->PrepareSnapshotTick(event.bids, event.asks);
      for (const auto &pending : symbol->pending) {
        symbol->pipeline->PrepareSnapshotTick(
            pending.bids, pending.asks);
      }
      const auto reference = std::max(
          event.bids.empty() ? std::int64_t{} : event.bids.front().price,
          event.asks.empty() ? std::int64_t{} : event.asks.front().price);
      const auto book_tick_size =
          symbol->pipeline->SnapshotTickSize(event.bids, event.asks);
      const auto required_ticks =
          reference > 0 && book_tick_size > 0
              ? static_cast<std::size_t>(
                    std::ceil(
                        static_cast<long double>(reference) *
                        symbol->options.ladder_price_band_bps /
                        (static_cast<long double>(book_tick_size) *
                         10'000.0L)))
              : std::size_t{};
      if (required_ticks > symbol->options.ladder_ticks_per_side) {
        error_ = "ladder_ticks_per_side is too small for configured "
                 "price_band_bps; "
                 "required at least " +
                 std::to_string(required_ticks);
        fail(error_);
        return false;
      }
      const auto result = symbol->pipeline->LoadImage(
          header, event.final_sequence, event.bids, event.asks,
          publish_book);
      if (result != book::PipelineResult::Applied) {
        return pipeline_failure(shard, result, *symbol);
      }
      symbol->image_ready = true;
      symbol->awaiting_snapshot_bridge = !publish_book;
      symbol->book_live = publish_book;
      symbol->last_book_sequence = event.final_sequence;
      symbol->ws_snapshot_recovery.snapshot_ready();
      ++metrics_.depth_updates;
      if (publish_book) {
        update_live_state();
      }
      return true;
    }
    if (!symbol->image_ready) {
      if (shard.adapter->needs_rest_snapshot()) {
        if (!buffer_delta(*symbol, event)) {
          record_resync(*symbol, ResyncReason::SnapshotBridgeGap,
                        symbol->last_book_sequence, event.previous_sequence,
                        event.first_sequence, event.final_sequence);
          symbol->lagging_snapshot_retries.reset();
          symbol->pending.clear();
          if (!buffer_delta(*symbol, event)) {
            return resync_symbol(
                shard, *symbol, ResyncReason::SnapshotBridgeGap,
                "orderbook bootstrap delta buffer reset failed");
          }
          begin_recovery(shard, Clock::now());
        }
        request_next_snapshot(Clock::now());
        return true;
      }
      if (options_.venue == Venue::Polymarket) {
        return true;
      }
      if (ShouldIgnorePreSnapshotDelta(
              options_.venue,
              symbol->ws_snapshot_recovery.pre_snapshot_delta())) {
        return true;
      }
      return resync_symbol(
          shard, *symbol, ResyncReason::SnapshotBridgeGap,
          "orderbook delta arrived before initial snapshot");
    }
    if (symbol->last_book_sequence != 0 &&
        event.final_sequence <= symbol->last_book_sequence) {
      return true;
    }
    const auto expected_sequence =
        symbol->last_book_sequence ==
                std::numeric_limits<std::uint64_t>::max()
            ? symbol->last_book_sequence
            : symbol->last_book_sequence + 1;
    bool sequence_gap = false;
    if (symbol->last_book_sequence != 0) {
      if (symbol->awaiting_snapshot_bridge) {
        SnapshotBridgeValidator bridge(symbol->last_book_sequence);
        const auto decision = bridge.Observe(
            event.first_sequence, event.final_sequence,
            event.strict_previous_sequence,
            event.previous_sequence);
        sequence_gap =
            decision == SnapshotBridgeDecision::SnapshotTooOld ||
            decision == SnapshotBridgeDecision::Gap ||
            decision == SnapshotBridgeDecision::Invalid;
      } else {
        sequence_gap =
            (event.strict_previous_sequence &&
             event.previous_sequence != 0 &&
             event.previous_sequence != symbol->last_book_sequence) ||
            (event.strict_previous_sequence &&
             event.previous_sequence == 0 &&
             event.first_sequence != expected_sequence) ||
            (!event.strict_previous_sequence &&
             event.first_sequence > expected_sequence);
      }
    }
    if (sequence_gap) {
      if (shard.adapter->needs_rest_snapshot()) {
        if (!resync_symbol(
                shard, *symbol, ResyncReason::LiveSequenceGap,
                "live orderbook sequence gap", expected_sequence,
                event.previous_sequence, event.first_sequence,
                event.final_sequence)) {
          return false;
        }
        if (!buffer_delta(*symbol, event)) {
          return resync_symbol(
              shard, *symbol, ResyncReason::SnapshotBridgeGap,
              "orderbook bootstrap delta buffer exceeded");
        }
        request_next_snapshot(Clock::now());
      } else if (!resync_symbol(
                     shard, *symbol, ResyncReason::LiveSequenceGap,
                     "live orderbook sequence gap", expected_sequence,
                     event.previous_sequence, event.first_sequence,
                     event.final_sequence)) {
        return false;
      }
      update_live_state();
      return true;
    }
    const bool completing_snapshot_bridge =
        symbol->awaiting_snapshot_bridge;
    const auto result = symbol->pipeline->ApplyDelta(
        header,
        {event.first_sequence, event.final_sequence, event.bids, event.asks},
        !completing_snapshot_bridge);
    if (result != book::PipelineResult::Applied) {
      return pipeline_failure(shard, result, *symbol);
    }
    symbol->last_book_sequence = event.final_sequence;
    if (completing_snapshot_bridge &&
        !publish_complete_book(*symbol, event.final_sequence,
                               event.exchange_time_ms)) {
      ++metrics_.publish_errors;
      fail("failed to publish bridged orderbook image");
      return false;
    }
    symbol->awaiting_snapshot_bridge = false;
    symbol->book_live = true;
    ++metrics_.depth_updates;
    update_live_state();
    return true;
  }

  bool pipeline_failure(WsShard &shard, book::PipelineResult result,
                        SymbolRuntime &symbol) {
    if (result == book::PipelineResult::PublishFailed) {
      ++metrics_.publish_errors;
    } else {
      if (result == book::PipelineResult::Resync) {
        const auto cause = symbol.pipeline->last_resync_reason();
        const auto cause_index = static_cast<std::size_t>(cause);
        if (cause != book::BridgeResyncReason::None &&
            cause_index < metrics_.pipeline_resync_causes.size()) {
          ++metrics_.pipeline_resync_causes[cause_index];
        }
        metrics_.last_pipeline_resync_cause = cause;
      }
    }
    if (result == book::PipelineResult::PublishFailed) {
      symbol.book_live = false;
      symbol.image_ready = false;
      symbol.awaiting_snapshot_bridge = false;
      symbol.lagging_snapshot_retries.reset();
      begin_recovery(shard, Clock::now());
      if (options_.venue == Venue::Polymarket) {
        schedule_reconnect(
            shard, "Polymarket orderbook publish failed");
        return true;
      }
      fail("orderbook publish failed");
      return false;
    }
    return resync_symbol(
        shard, symbol,
        result == book::PipelineResult::InvalidImage
            ? ResyncReason::InvalidImage
            : ResyncReason::PipelineResync,
        "orderbook pipeline requires symbol rebuild");
  }

  void record_resync(SymbolRuntime &symbol, ResyncReason reason,
                     std::uint64_t expected,
                     std::uint64_t previous,
                     std::uint64_t first,
                     std::uint64_t final) noexcept {
    ++metrics_.resyncs;
    switch (reason) {
      case ResyncReason::SnapshotBridgeGap:
        ++metrics_.snapshot_bridge_gaps;
        break;
      case ResyncReason::LiveSequenceGap:
        ++metrics_.live_sequence_gaps;
        break;
      case ResyncReason::InvalidImage:
        ++metrics_.invalid_images;
        break;
      case ResyncReason::PipelineResync:
        ++metrics_.pipeline_resyncs;
        break;
      case ResyncReason::InputCapacity:
        ++metrics_.input_capacity_resyncs;
        break;
      case ResyncReason::DirtyData:
        ++metrics_.dirty_data_resyncs;
        break;
      case ResyncReason::None:
        break;
    }
    copy_text(metrics_.last_resync_symbol, symbol.options.symbol);
    metrics_.last_resync_reason = reason;
    metrics_.last_resync_expected = expected;
    metrics_.last_resync_previous = previous;
    metrics_.last_resync_first = first;
    metrics_.last_resync_final = final;
  }

  bool resync_symbol(WsShard &shard, SymbolRuntime &symbol,
                     ResyncReason reason, std::string_view detail,
                     std::uint64_t expected = 0,
                     std::uint64_t previous = 0,
                     std::uint64_t first = 0,
                     std::uint64_t final = 0) {
    record_resync(symbol, reason, expected, previous, first, final);
    ++symbol.generation;
    symbol.catalog_generation = 0;
    symbol.image_ready = false;
    symbol.awaiting_snapshot_bridge = false;
    symbol.ticker_live = false;
    symbol.book_live = false;
    symbol.last_ticker_sequence = 0;
    symbol.last_book_sequence = 0;
    symbol.last_ticker_exchange_time_ms = 0;
    symbol.last_book_exchange_time_ms = 0;
    symbol.latest_bbo.reset();
    symbol.pending.clear();
    symbol.lagging_snapshot_retries.reset();
    symbol.ws_snapshot_recovery.begin_recovery();
    symbol.snapshot_quarantined = false;
    symbol.consecutive_snapshot_failures = 0;
    symbol.needs_snapshot =
        symbol.options.orderbook &&
        shard.adapter->needs_rest_snapshot();
    const auto now = Clock::now();
    if (symbol.recovery_started == Clock::time_point{}) {
      symbol.recovery_started = now;
    }
    begin_recovery(shard, now);
    if (!publish_catalog_barrier(symbol)) {
      fail("failed to publish symbol recovery catalog barrier");
      return false;
    }
    if (symbol.needs_snapshot) {
      request_snapshot(symbol, now);
      return true;
    }
    if (options_.venue == Venue::Polymarket) {
      schedule_reconnect(shard, detail, now);
      return true;
    }
    return queue_symbol_resubscribe(shard, symbol, now);
  }

  bool queue_symbol_resubscribe(WsShard &shard,
                                SymbolRuntime &symbol,
                                Clock::time_point now) {
    constexpr auto cooldown = std::chrono::seconds(1);
    auto not_before = now;
    if (symbol.last_resubscribe != Clock::time_point{}) {
      not_before = std::max(not_before, symbol.last_resubscribe + cooldown);
      if (not_before > now) {
        ++metrics_.cooldown_deferrals;
      }
    }
    if (symbol.resubscribe_pending) {
      symbol.resubscribe_not_before =
          std::max(symbol.resubscribe_not_before, not_before);
      return true;
    }
    symbol.resubscribe_pending = true;
    symbol.resubscribe_not_before = not_before;
    shard.all_subscribed = false;
    begin_recovery(shard, now);
    return true;
  }

  bool append_pending_symbol_resubscribe(WsShard &shard,
                                         Clock::time_point now) {
    if (shard.awaiting_ack || shard.next_batch < shard.batches.size()) {
      return true;
    }
    for (const auto symbol_index : shard.symbol_indices) {
      auto &symbol = symbols_[symbol_index];
      if (!symbol.resubscribe_pending ||
          now < symbol.resubscribe_not_before) {
        continue;
      }
      const exchange::StreamRequest request{
          symbol.options.symbol, symbol.venue_symbol,
          symbol.options.ticker_channel,
          symbol.options.orderbook_channel, symbol.options.ticker,
          symbol.options.orderbook,
          symbol.options.update_interval_ms};
      std::vector<std::string> subscribe;
      std::vector<std::string> unsubscribe;
      std::string build_error;
      if (!shard.adapter->build_subscription_batches(
              std::span<const exchange::StreamRequest>(&request, 1),
              subscribe, build_error) ||
          !shard.adapter->build_unsubscription_batches(
              std::span<const exchange::StreamRequest>(&request, 1),
              unsubscribe, build_error) ||
          unsubscribe.size() != subscribe.size() || subscribe.empty()) {
        fail(
            build_error.empty() ? "failed to build symbol resubscription"
                                : build_error);
        return false;
      }
      shard.batches.clear();
      shard.next_batch = 0;
      for (std::size_t index = 0; index < subscribe.size(); ++index) {
        shard.batches.push_back(std::move(unsubscribe[index]));
        shard.batches.push_back(std::move(subscribe[index]));
      }
      symbol.resubscribe_pending = false;
      symbol.resubscribe_not_before = {};
      symbol.last_resubscribe = now;
      symbol.ws_snapshot_recovery.begin_recovery();
      shard.all_subscribed = false;
      return true;
    }
    return true;
  }

  bool buffer_delta(SymbolRuntime &symbol,
                    const exchange::NormalizedEvent &event) {
    if (symbol.pending.size() >= 4096) {
      return false;
    }
    PendingDelta delta;
    delta.first = event.first_sequence;
    delta.final = event.final_sequence;
    delta.previous = event.previous_sequence;
    delta.time_ms = event.exchange_time_ms;
    delta.bids = event.bids;
    delta.asks = event.asks;
    delta.strict_previous_sequence =
        event.strict_previous_sequence;
    symbol.pending.push_back(std::move(delta));
    note_recovery_progress(symbol, Clock::now());
    return true;
  }

  void reset_snapshot_client() noexcept {
    if (snapshot_fd_ >= 0) {
      loop_.remove(snapshot_fd_);
      snapshot_fd_ = -1;
    }
    snapshot_http_.reset();
    snapshot_active_ = false;
  }

  void snapshot_backoff(std::string_view reason, Clock::time_point now,
                        std::optional<std::chrono::milliseconds> forced_delay =
                            std::nullopt,
                        bool quarantine = false) {
    const std::string bounded_reason(reason.substr(0, 512));
    reset_snapshot_client();
    ++metrics_.snapshot_failures;
    error_ = bounded_reason;
    if (snapshot_symbol_ >= symbols_.size()) {
      next_snapshot_allowed_ =
          now + std::chrono::milliseconds(
                    options_.snapshot_failure_backoff_ms);
      return;
    }
    auto &symbol = symbols_[snapshot_symbol_];
    if (!forced_delay) {
      ++symbol.consecutive_snapshot_failures;
    }
    if (quarantine ||
        symbol.consecutive_snapshot_failures >=
            options_.snapshot_max_consecutive_failures) {
      if (!symbol.snapshot_quarantined) {
        ++metrics_.snapshot_quarantines;
      }
      symbol.snapshot_quarantined = true;
      symbol.pending.clear();
      symbol.image_ready = false;
      symbol.book_live = false;
      next_snapshot_allowed_ =
          now + std::chrono::milliseconds(options_.snapshot_pacing_ms);
      return;
    }
    std::chrono::milliseconds delay{};
    if (forced_delay) {
      delay = *forced_delay;
    } else {
      const auto exponent =
          std::min<std::uint32_t>(symbol.consecutive_snapshot_failures - 1, 16);
      const auto multiplier = std::uint64_t{1} << exponent;
      const auto raw = std::uint64_t{options_.snapshot_failure_backoff_ms} *
                       multiplier;
      const auto capped = std::min<std::uint64_t>(
          raw, options_.snapshot_failure_backoff_max_ms);
      const auto jitter =
          (capped / 8U) * (symbol.consecutive_snapshot_failures % 3U);
      delay = std::chrono::milliseconds(std::min<std::uint64_t>(
          capped + jitter, options_.snapshot_failure_backoff_max_ms));
    }
    next_snapshot_allowed_ = std::max(next_snapshot_allowed_, now + delay);
  }

  std::chrono::milliseconds retry_after_delay() const noexcept {
    return parse_retry_after(
        snapshot_http_.response().header_value("retry-after"));
  }

  std::string snapshot_http_error(unsigned status,
                                  std::string_view body) const {
    std::string message = "REST snapshot HTTP ";
    message.append(std::to_string(status));
    if (snapshot_symbol_ < symbols_.size()) {
      message.append(" symbol=");
      message.append(symbols_[snapshot_symbol_].venue_symbol);
    }
    if (!body.empty()) {
      message.append(" body=");
      message.append(body.substr(0, 256));
    }
    const auto retry_after =
        snapshot_http_.response().header_value("retry-after");
    if (!retry_after.empty()) {
      message.append(" retry_after=");
      message.append(retry_after.substr(0, 32));
    }
    return message;
  }

  void handle_snapshot_response(std::string_view json, unsigned status) {
    const auto now = Clock::now();
    const auto action =
        classify_snapshot_http(options_.venue, status, json);
    if (action == SnapshotHttpAction::CooldownVenue ||
        action == SnapshotHttpAction::BanCooldownVenue) {
      ++metrics_.snapshot_rate_limits;
      auto delay = retry_after_delay();
      if (delay.count() == 0) {
        delay = std::chrono::milliseconds(
            action == SnapshotHttpAction::BanCooldownVenue
                ? options_.snapshot_ban_backoff_ms
                : options_.snapshot_rate_limit_backoff_ms);
      }
      snapshot_backoff(snapshot_http_error(status, json), now, delay);
      return;
    }
    if (action != SnapshotHttpAction::Parse) {
      snapshot_backoff(snapshot_http_error(status, json), now, std::nullopt,
                       action == SnapshotHttpAction::QuarantineSymbol);
      return;
    }
    handle_snapshot(json);
  }

  void request_snapshot(SymbolRuntime &symbol,
                        Clock::time_point now) {
    if (snapshot_active_ || now < next_snapshot_allowed_) {
      return;
    }
    const auto endpoint = parse_endpoint(options_.rest_endpoint);
    auto &shard = shard_for(symbol);
    const auto request = shard.adapter->snapshot_request(
        symbol.venue_symbol, symbol.options.snapshot_depth);
    snapshot_symbol_ =
        static_cast<std::size_t>(&symbol - symbols_.data());
    snapshot_active_ = snapshot_http_.start_get(
        endpoint.host, endpoint.service, request.target,
        now + options_.request_timeout);
    if (!snapshot_active_) {
      snapshot_backoff(snapshot_http_.error_message(), now);
      return;
    }
    note_recovery_progress(symbol, now);
    sync_http_registration(HttpKind::Snapshot);
  }

  void request_next_snapshot(Clock::time_point now) {
    if (snapshot_active_ || now < next_snapshot_allowed_) {
      return;
    }
    const auto selected = NextRoundRobin(
        symbols_.size(), next_snapshot_symbol_,
        [this](std::size_t index) {
          const auto &symbol = symbols_[index];
          return symbol.options.orderbook &&
                 ws_shards_[symbol.shard_id].all_subscribed &&
                 !symbol.image_ready &&
                 (symbol.needs_snapshot || !symbol.pending.empty()) &&
                 !symbol.snapshot_quarantined;
        });
    if (!selected) {
      return;
    }
    next_snapshot_symbol_ = (*selected + 1) % symbols_.size();
    request_snapshot(symbols_[*selected], now);
  }

  bool publish_complete_book(SymbolRuntime &symbol,
                             std::uint64_t sequence,
                             std::uint64_t exchange_time_ms) {
    if (symbol.book_publisher == nullptr) {
      return true;
    }
    const auto header = make_header(
        symbol.instrument.instrument_id, symbol.generation, sequence,
        exchange_time_ms, utils::md::BookState::Live);
    return symbol.book_publisher->publish_snapshot(
               header, symbol.pipeline->book()) &&
           symbol.pipeline->PublishCanonical(header);
  }

  void handle_snapshot(std::string_view json) {
    if (!snapshot_active_ || snapshot_symbol_ >= symbols_.size()) {
      fail("unexpected REST snapshot response");
      return;
    }
    snapshot_active_ = false;
    next_snapshot_allowed_ =
        Clock::now() +
        std::chrono::milliseconds(options_.snapshot_pacing_ms);
    auto &symbol = symbols_[snapshot_symbol_];
    auto &shard = shard_for(symbol);
    snapshot_event_->reset();
    std::string parse_error;
    if (!shard.adapter->parse_snapshot(
            json, symbol.venue_symbol, *snapshot_event_, parse_error) ||
        snapshot_event_->type !=
            exchange::AdapterEventType::BookSnapshot) {
      snapshot_backoff(
          parse_error.empty() ? "failed to parse REST snapshot" : parse_error,
          Clock::now());
      return;
    }
    symbol.consecutive_snapshot_failures = 0;
    symbol.needs_snapshot = false;
    note_recovery_progress(symbol, Clock::now());
    SnapshotBridgeValidator bridge(snapshot_event_->final_sequence);
    for (const auto &pending : symbol.pending) {
      const auto expected =
          bridge.sequence() == std::numeric_limits<std::uint64_t>::max()
              ? bridge.sequence()
              : bridge.sequence() + 1;
      const auto decision = bridge.Observe(
          pending.first, pending.final,
          pending.strict_previous_sequence, pending.previous);
      const bool is_binance = options_.venue == Venue::Binance;
      const bool retry_lagging_snapshot =
          is_binance ||
          (options_.venue == Venue::Gate &&
           options_.product == Product::Spot);
      if (ShouldRetryLaggingSnapshot(
              decision, retry_lagging_snapshot,
              symbol.lagging_snapshot_retries)) {
        const auto now = Clock::now();
        symbol.image_ready = false;
        symbol.awaiting_snapshot_bridge = false;
        symbol.book_live = false;
        note_recovery_progress(symbol, now);
        begin_recovery(shard_for(symbol), now);
        return;
      }
      if (decision == SnapshotBridgeDecision::SnapshotTooOld ||
          decision == SnapshotBridgeDecision::Gap ||
          decision == SnapshotBridgeDecision::Invalid) {
        (void)resync_symbol(
            shard, symbol, ResyncReason::SnapshotBridgeGap,
            "REST snapshot could not bridge buffered deltas",
            expected, pending.previous, pending.first, pending.final);
        return;
      }
    }
    symbol.lagging_snapshot_retries.reset();
    if (!process_market_event(shard, *snapshot_event_, false)) {
      return;
    }
    std::uint64_t final_sequence = symbol.last_book_sequence;
    std::uint64_t final_time_ms = snapshot_event_->exchange_time_ms;
    for (const auto &pending : symbol.pending) {
      if (pending.final <= symbol.last_book_sequence) {
        continue;
      }
      auto header = make_header(symbol.instrument.instrument_id,
                                symbol.generation, pending.final,
                                pending.time_ms,
                                utils::md::BookState::Live);
      const auto result = symbol.pipeline->ApplyDelta(
          header, {pending.first, pending.final, pending.bids,
                   pending.asks}, false);
      if (result != book::PipelineResult::Applied) {
        pipeline_failure(shard, result, symbol);
        return;
      }
      symbol.last_book_sequence = pending.final;
      final_sequence = pending.final;
      final_time_ms = pending.time_ms;
    }
    symbol.awaiting_snapshot_bridge = !bridge.has_accepted();
    symbol.pending.clear();
    if (!symbol.awaiting_snapshot_bridge) {
      if (!publish_complete_book(
              symbol, final_sequence, final_time_ms)) {
        ++metrics_.publish_errors;
        fail("failed to publish rebuilt orderbook image");
        return;
      }
      symbol.book_live = true;
      update_live_state();
    }
  }

  void handle_metadata(std::string_view json) {
    if (metadata_batch_index_ >= metadata_batches_.size()) {
      fail("unexpected venue metadata response");
      return;
    }
    const auto &batch = metadata_batches_[metadata_batch_index_];
    const auto batch_requests =
        std::span<const exchange::StreamRequest>(requests_)
            .subspan(batch.request_offset, batch.request_count);
    std::vector<exchange::InstrumentMetadata> parsed;
    parsed.reserve(batch.request_count);
    std::string parse_error;
    if (metadata_http_.response().status_class() !=
            net::HttpStatusClass::Success ||
        !metadata_adapter().parse_discovery_metadata(
            json, batch_requests, parsed, parse_error)) {
      std::string reason =
          parse_error.empty() ? "failed to parse venue metadata" : parse_error;
      if (metadata_http_.response().status_class() !=
          net::HttpStatusClass::Success) {
        reason.append(" HTTP ");
        reason.append(
            std::to_string(metadata_http_.response().status_code()));
      }
      fail(std::string("metadata: ") + reason);
      return;
    }
    for (auto &entry : parsed) {
      metadata_.push_back(std::move(entry));
    }
    if (options_.venue != Venue::Polymarket) {
      const auto batch_end = batch.request_offset + batch.request_count;
      for (auto &shard : ws_shards_) {
        std::vector<exchange::StreamRequest> shard_batch_requests;
        for (const auto symbol_index : shard.symbol_indices) {
          if (symbol_index >= batch.request_offset &&
              symbol_index < batch_end) {
            shard_batch_requests.push_back(requests_[symbol_index]);
          }
        }
        if (shard_batch_requests.empty()) {
          continue;
        }
        std::vector<exchange::InstrumentMetadata> shard_metadata;
        std::string shard_error;
        if (!shard.adapter->parse_metadata(
                json, shard_batch_requests, shard_metadata, shard_error)) {
          fail("ws shard " + std::to_string(shard.id) +
               ": failed to initialize adapter metadata: " +
               shard_error);
          return;
        }
      }
    }
    ++metadata_batch_index_;
    if (metadata_batch_index_ < metadata_batches_.size()) {
      if (!start_metadata_batch(Clock::now())) {
        fail(error_);
      }
      return;
    }
    for (auto &symbol : symbols_) {
      const auto found = std::find_if(
          metadata_.begin(), metadata_.end(),
          [&symbol](const exchange::InstrumentMetadata &entry) {
            return entry.canonical_symbol == symbol.options.symbol ||
                   entry.venue_symbol == symbol.venue_symbol;
          });
      if (found == metadata_.end() || found->tick_size <= 0 ||
          found->venue_symbol.empty()) {
        if (error_.empty()) {
          error_ = "requested symbol was not found in venue metadata: ";
          error_.append(symbol.options.symbol);
        }
        fail(error_);
        return;
      }
      symbol.venue_symbol = found->venue_symbol;
      if (!open_symbol(symbol, *found)) {
        fail(error_);
        return;
      }
    }
    requests_.clear();
    for (const auto &symbol : symbols_) {
      requests_.push_back(
          {symbol.options.symbol, symbol.venue_symbol,
           symbol.options.ticker_channel,
           symbol.options.orderbook_channel, symbol.options.ticker,
           symbol.options.orderbook,
           symbol.options.update_interval_ms});
    }
    for (auto &shard : ws_shards_) {
      shard.requests.clear();
      shard.requests.reserve(shard.symbol_indices.size());
      for (const auto symbol_index : shard.symbol_indices) {
        shard.requests.push_back(requests_[symbol_index]);
      }
      shard.batches.clear();
      if (!shard.adapter->build_subscription_batches(
              shard.requests, shard.batches, error_) ||
          shard.batches.empty()) {
        fail(error_.empty()
                 ? "venue produced no subscription batches for shard " +
                       std::to_string(shard.id)
                 : "ws shard " + std::to_string(shard.id) + ": " + error_);
        return;
      }
      shard.next_batch = 0;
      shard.pending_acknowledgements = 0;
      shard.awaiting_ack = false;
      shard.all_subscribed = false;
    }
    metadata_batches_.clear();
    metadata_.clear();
    metadata_batch_index_ = 0;
    metadata_ready_ = true;
    if (auto *adapter = polymarket()) {
      if (const auto *market = adapter->resolved_market(
              exchange::polymarket::ResolveSlot::Current)) {
        active_market_window_ = market->window.start_unix;
      }
    }
    for (auto &shard : ws_shards_) {
      if (shard.connection_started && !shard.all_subscribed) {
        shard.state =
            shard.requires_login && !shard.login_acked
                ? MarketDataState::Authenticating
                : MarketDataState::Subscribing;
      }
    }
    update_aggregate_state();
  }

  bool open_symbol(SymbolRuntime &symbol,
                   const exchange::InstrumentMetadata &metadata) {
    symbol.metadata = metadata;
    symbol.canonical_identity = symbol.options.symbol;
    if (options_.venue == Venue::Polymarket) {
      std::transform(symbol.canonical_identity.begin(),
                     symbol.canonical_identity.end(),
                     symbol.canonical_identity.begin(), [](char value) {
                       return static_cast<char>(
                           std::tolower(static_cast<unsigned char>(value)));
                     });
    }
    if (options_.venue == Venue::Polymarket) {
      const auto *poly = polymarket();
      const auto *market =
          poly == nullptr
              ? nullptr
              : poly->resolved_market(
                    exchange::polymarket::ResolveSlot::Current);
      if (market == nullptr) {
        error_ = "polymarket physical instrument identity is unavailable";
        return false;
      }
      std::string instance_identity(market->slug.view());
      instance_identity.push_back('|');
      instance_identity.append(symbol.options.polymarket_outcome);
      if (!options_.instrument_manager->resolve_instance(
              options_.venue, options_.product, symbol.canonical_identity,
              instance_identity, symbol.instrument.instrument_id, error_)) {
        return false;
      }
    } else if (!options_.instrument_manager->resolve(
                   options_.venue, options_.product,
                   symbol.canonical_identity,
                   symbol.instrument.instrument_id, error_)) {
      return false;
    }
    symbol.instrument.venue = options_.venue;
    symbol.instrument.product_type = options_.product;
    symbol.instrument.price_scale = metadata.price_scale;
    symbol.instrument.quantity_scale = metadata.quantity_scale;
    symbol.instrument.flags =
        metadata.refine_book_tick
            ? utils::md::kInstrumentRefineBookTick
            : 0;
    symbol.instrument.tick_size = metadata.tick_size;
    symbol.instrument.lot_size = metadata.lot_size;
    symbol.instrument.contract_multiplier = metadata.contract_multiplier;
    symbol.instrument.contract_multiplier_scale =
        metadata.contract_multiplier_scale;
    copy_text(symbol.instrument.base_asset, metadata.base_asset);
    copy_text(symbol.instrument.quote_asset, metadata.quote_asset);
    copy_text(symbol.instrument.settle_asset, metadata.settle_asset);
    copy_text(symbol.instrument.canonical_symbol, symbol.canonical_identity);
    copy_text(symbol.instrument.venue_symbol, symbol.venue_symbol);
    const auto key = instrument::InstrumentManager::canonical_key(
        options_.venue, options_.product, symbol.canonical_identity);
    copy_text(symbol.instrument.instrument_key, key);

    symbol.catalog = {};
    auto &catalog = symbol.catalog;
    catalog.instrument_id = symbol.instrument.instrument_id;
    catalog.venue = symbol.instrument.venue;
    catalog.product_type = symbol.instrument.product_type;
    catalog.price_scale = symbol.instrument.price_scale;
    catalog.quantity_scale = symbol.instrument.quantity_scale;
    catalog.contract_multiplier_scale =
        symbol.instrument.contract_multiplier_scale;
    catalog.flags = symbol.instrument.flags;
    catalog.tick_size = symbol.instrument.tick_size;
    catalog.lot_size = symbol.instrument.lot_size;
    catalog.contract_multiplier = symbol.instrument.contract_multiplier;
    catalog.signature_type = metadata.signature_type;
    catalog.negative_risk = metadata.negative_risk ? 1 : 0;
    copy_text(catalog.base_asset, metadata.base_asset);
    copy_text(catalog.quote_asset, metadata.quote_asset);
    copy_text(catalog.settle_asset, metadata.settle_asset);
    copy_text(catalog.canonical_symbol, symbol.canonical_identity);
    copy_text(catalog.venue_symbol, symbol.venue_symbol);
    if (auto *poly = polymarket()) {
      if (const auto *market = poly->resolved_market(
              exchange::polymarket::ResolveSlot::Current)) {
        copy_text(catalog.market_slug, market->slug.view());
        copy_text(catalog.condition_id, market->condition_id.view());
        copy_text(catalog.outcome, symbol.options.polymarket_outcome);
        if (market->window.end_unix > 0) {
          catalog.expiry_unix_ns =
              static_cast<std::uint64_t>(market->window.end_unix) *
              1'000'000'000ULL;
        }
      }
    }

    const auto configure_output = [&](bool orderbook) {
      auto *multiplex =
          multiplex_publisher(orderbook, symbol.instrument.instrument_id);
      auto *&active =
          orderbook ? symbol.book_publisher : symbol.ticker_publisher;
      auto &owned = orderbook ? symbol.owned_book_publisher
                              : symbol.owned_ticker_publisher;
      if (symbol.options.ring_layout == publish::RingLayout::Multiplex) {
        active = multiplex;
      } else if (symbol.options.ring_layout ==
                     publish::RingLayout::Both &&
                 owned) {
        owned->set_mirror(multiplex);
        active = owned.get();
      }
    };
    configure_output(false);
    configure_output(true);
    if (symbol.options.ticker) {
      if (!symbol.ticker_publisher && !open_output_rings(symbol)) {
        return false;
      }
    }
    if (symbol.options.orderbook) {
      if (!symbol.book_publisher && !open_output_rings(symbol)) {
        return false;
      }
      symbol.pipeline = std::make_unique<book::BookPipeline>(
          symbol.instrument.instrument_id,
          symbol.options.ladder_ticks_per_side, metadata.tick_size,
          symbol.book_publisher, metadata.refine_book_tick,
          options_.venue == Venue::Polymarket);
    }
    symbol.metadata_ready = true;
    (void)publish_catalog_barrier(symbol);
    return true;
  }

  exchange::polymarket::PolymarketAdapter *polymarket() noexcept {
    return dynamic_cast<exchange::polymarket::PolymarketAdapter *>(
        ws_shards_.front().adapter.get());
  }

  static std::int64_t unix_seconds() noexcept {
    return std::chrono::duration_cast<std::chrono::seconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
  }

  void discovery_failed(std::string_view reason) noexcept {
    (void)reason;
    if (discovery_fd_ >= 0) {
      loop_.remove(discovery_fd_);
      discovery_fd_ = -1;
    }
    discovery_http_.reset();
    if (auto *adapter = polymarket()) {
      adapter->retry_gamma(discovery_slot_);
    }
    discovery_active_ = false;
    ++metrics_.discovery_failures;
    next_discovery_attempt_ = Clock::now() + std::chrono::seconds(1);
  }

  void handle_discovery(
      std::string_view json, net::HttpStatusClass status) {
    auto *adapter = polymarket();
    if (adapter == nullptr || !discovery_active_) {
      return;
    }
    discovery_active_ = false;
    metrics_.last_discovery_latency_us =
        static_cast<std::uint64_t>(
            std::chrono::duration_cast<std::chrono::microseconds>(
                Clock::now() - discovery_started_)
                .count());
    std::string parse_error;
    if (status != net::HttpStatusClass::Success ||
        !adapter->apply_gamma_response(discovery_slot_, json,
                                       parse_error)) {
      adapter->retry_gamma(discovery_slot_);
      ++metrics_.discovery_failures;
      next_discovery_attempt_ = Clock::now() + std::chrono::seconds(1);
    }
  }

  bool rollover_polymarket(std::int64_t now_seconds,
                           Clock::time_point now) {
    auto *adapter = polymarket();
    if (adapter == nullptr) {
      return true;
    }
    const auto *old_market =
        adapter->resolved_market(exchange::polymarket::ResolveSlot::Current);
    const auto *next_market =
        adapter->resolved_market(exchange::polymarket::ResolveSlot::Next);
    if (old_market == nullptr || next_market == nullptr ||
        next_market->window.start_unix > now_seconds) {
      return true;
    }
    std::array<std::string, 2> old_tokens{};
    std::size_t token_count = 0;
    for (const auto &symbol : symbols_) {
      const bool down = symbol.options.polymarket_outcome == "DOWN";
      old_tokens[token_count++] = std::string(
          down ? old_market->down_token_id.view()
               : old_market->up_token_id.view());
    }
    adapter->prepare_resolution(now_seconds);
    const auto *current =
        adapter->resolved_market(exchange::polymarket::ResolveSlot::Current);
    if (current == nullptr ||
        current->window.start_unix == active_market_window_) {
      return true;
    }

    std::vector<exchange::InstrumentMetadata> metadata;
    std::vector<utils::md::InstrumentId> old_instrument_ids;
    old_instrument_ids.reserve(symbols_.size());
    for (const auto &symbol : symbols_) {
      old_instrument_ids.push_back(symbol.instrument.instrument_id);
    }
    std::string build_error;
    if (!adapter->build_resolved_metadata(
            exchange::polymarket::ResolveSlot::Current, requests_,
            metadata, build_error) ||
        metadata.size() != symbols_.size()) {
      fail(build_error.empty() ? "failed to build rollover metadata"
                               : build_error);
      return false;
    }
    for (auto &symbol : symbols_) {
      const auto found = std::find_if(
          metadata.begin(), metadata.end(),
          [&symbol](const exchange::InstrumentMetadata &entry) {
            return entry.canonical_symbol == symbol.options.symbol;
          });
      if (found == metadata.end()) {
        fail("rollover metadata omitted a logical symbol");
        return false;
      }
      ++symbol.generation;
      symbol.catalog_generation = 0;
      symbol.venue_symbol = found->venue_symbol;
      symbol.image_ready = false;
      symbol.awaiting_snapshot_bridge = false;
      symbol.ticker_live = false;
      symbol.book_live = false;
      symbol.last_book_sequence = 0;
      symbol.last_ticker_sequence = 0;
      symbol.last_book_exchange_time_ms = 0;
      symbol.last_ticker_exchange_time_ms = 0;
      symbol.latest_bbo.reset();
      symbol.pending.clear();
      if (!open_symbol(symbol, *found)) {
        fail(error_);
        return false;
      }
    }
    for (const auto old_instrument_id : old_instrument_ids) {
      const bool still_active = std::any_of(
          symbols_.begin(), symbols_.end(),
          [old_instrument_id](const auto &symbol) {
            return symbol.instrument.instrument_id == old_instrument_id;
          });
      if (!still_active) {
        (void)options_.instrument_manager->retire(old_instrument_id);
      }
    }
    requests_.clear();
    for (const auto &symbol : symbols_) {
      requests_.push_back(
          {symbol.options.symbol, symbol.venue_symbol,
           symbol.options.ticker_channel, symbol.options.orderbook_channel,
           symbol.options.ticker, symbol.options.orderbook,
           symbol.options.update_interval_ms});
    }
    ws_shards_.front().requests = requests_;

    std::array<std::string_view, 2> old_views{};
    std::array<std::string_view, 2> new_views{};
    for (std::size_t index = 0; index < token_count; ++index) {
      old_views[index] = old_tokens[index];
      const bool down =
          symbols_[index].options.polymarket_outcome == "DOWN";
      new_views[index] = down ? current->down_token_id.view()
                              : current->up_token_id.view();
    }
    std::string unsubscribe;
    std::string subscribe;
    if (!adapter->build_dynamic_operation(
            std::span<const std::string_view>(old_views.data(), token_count),
            false, unsubscribe, build_error) ||
        !adapter->build_dynamic_operation(
            std::span<const std::string_view>(new_views.data(), token_count),
            true, subscribe, build_error)) {
      fail(build_error);
      return false;
    }
    auto &shard = ws_shards_.front();
    shard.batches.clear();
    shard.batches.push_back(std::move(unsubscribe));
    shard.batches.push_back(std::move(subscribe));
    shard.next_batch = 0;
    shard.pending_acknowledgements = 0;
    shard.awaiting_ack = false;
    shard.all_subscribed = false;
    active_market_window_ = current->window.start_unix;
    ++metrics_.market_rollovers;
    metrics_.last_rollover_latency_us =
        now_seconds > current->window.start_unix
            ? static_cast<std::uint64_t>(
                  now_seconds - current->window.start_unix) *
                  1'000'000
            : 0;
    begin_recovery(ws_shards_.front(), now);
    return true;
  }

  void drive_polymarket_resolution(Clock::time_point now) {
    auto *adapter = polymarket();
    if (adapter == nullptr || symbols_.empty() || !metadata_ready_) {
      return;
    }
    const auto stale = adapter->stale_messages();
    if (stale >= observed_stale_messages_) {
      metrics_.stale_market_messages +=
          stale - observed_stale_messages_;
    }
    observed_stale_messages_ = stale;

    const auto seconds = unix_seconds();
    if (!rollover_polymarket(seconds, now) ||
        discovery_active_ || now < next_discovery_attempt_) {
      return;
    }
    const auto *current =
        adapter->resolved_market(exchange::polymarket::ResolveSlot::Current);
    if (current == nullptr) {
      return;
    }
    const auto prediscovery =
        static_cast<std::int64_t>(
            symbols_.front().options.polymarket_prediscovery_seconds);
    if (seconds < current->window.end_unix - prediscovery) {
      return;
    }
    const auto request = adapter->next_gamma_request();
    if (!request ||
        request->slot != exchange::polymarket::ResolveSlot::Next ||
        !adapter->mark_gamma_requested(request->slot)) {
      return;
    }
    const auto endpoint = parse_endpoint(options_.discovery_endpoint);
    const auto timeout = std::chrono::milliseconds(
        symbols_.front().options.polymarket_resolver_timeout_ms);
    if (!discovery_http_.start_get(
            endpoint.host, endpoint.service, request->target.view(),
            now + timeout)) {
      adapter->retry_gamma(request->slot);
      ++metrics_.discovery_failures;
      next_discovery_attempt_ = now + std::chrono::seconds(1);
      return;
    }
    discovery_slot_ = request->slot;
    discovery_started_ = now;
    discovery_active_ = true;
    ++metrics_.discovery_requests;
    sync_http_registration(HttpKind::Discovery);
  }

  void tick(Clock::time_point now) noexcept {
    if (!started_ || state_ == MarketDataState::Failed ||
        state_ == MarketDataState::Stopped) {
      return;
    }
    if (connection_rebuild_pending_) {
      const auto degraded = std::chrono::duration_cast<std::chrono::milliseconds>(
          now - connection_degraded_since_);
      metrics_.connection_degraded_duration_ms =
          connection_degraded_accumulated_ms_ +
          static_cast<std::uint64_t>(std::max<std::int64_t>(
              0, degraded.count()));
      if (now < connection_rebuild_at_) {
        return;
      }
      if (!attempt_connection_rebuild(now)) {
        return;
      }
    }
    for (auto &symbol : symbols_) {
      if (symbol_ready(symbol)) {
        symbol.recovery_started = {};
        continue;
      }
      if (symbol.recovery_started == Clock::time_point{}) {
        symbol.recovery_started = now;
        continue;
      }
      if (ContinuousRecoveryExpired(
              symbol.recovery_started, now,
              options_.max_continuous_recovery_duration)) {
        fail("continuous recovery timeout symbol=" +
             symbol.options.symbol);
        return;
      }
    }
    metadata_http_.check_timeout(now);
    if (snapshot_active_) {
      snapshot_http_.check_timeout(now);
    }
    if (discovery_active_) {
      discovery_http_.check_timeout(now);
    }
    if (terminal(metadata_http_.state())) {
      fail(std::string("metadata HTTP: ") +
           std::string(metadata_http_.error_message()));
      return;
    }
    if (snapshot_active_ && terminal(snapshot_http_.state())) {
      snapshot_backoff(snapshot_http_.error_message(), now);
    }
    if (discovery_active_ && terminal(discovery_http_.state())) {
      discovery_failed(discovery_http_.error_message());
    }
    drive_polymarket_resolution(now);
    for (auto &shard : ws_shards_) {
      if (!shard.connection_started &&
          shard.state == MarketDataState::Connecting &&
          now >= shard.connect_at) {
        if (!begin_connection(shard, now)) {
          const auto reason = error_;
          schedule_reconnect(shard, reason, now);
          continue;
        }
      }
      if (shard.connection_started) {
        shard.websocket->check_timeout(now);
      }
      if (terminal(shard.websocket->state())) {
        schedule_reconnect(
            shard, shard.websocket->error_message(), now);
      }
      if (shard.state == MarketDataState::ReconnectWait &&
          now >= shard.reconnect_at) {
        shard.state = MarketDataState::Connecting;
        if (!begin_connection(shard, now)) {
          const auto reason = error_;
          schedule_reconnect(shard, reason, now);
          continue;
        }
      }
      if (shard.websocket->state() ==
          net::WebSocketClientState::Open) {
        if (!shard.open_seen) {
          shard.open_seen = true;
          if (shard.reconnect_attempt != 0) {
            std::cerr << exchange::venue_name(options_.venue)
                      << " websocket reconnect restored product="
                      << exchange::product_name(options_.product)
                      << " shard=" << shard.id
                      << " attempt=" << shard.reconnect_attempt << '\n';
          }
          shard.state =
              shard.requires_login
                  ? MarketDataState::Authenticating
                  : MarketDataState::Subscribing;
        }
        drive_outbound(shard, now);
        drive_heartbeat(shard, now);
        if (now - shard.last_message >= options_.idle_timeout) {
          if (options_.venue == Venue::Polymarket) {
            ++metrics_.heartbeat_timeouts;
          }
          schedule_reconnect(
              shard, "venue WebSocket idle timeout", now);
        }
      }
      switch (shard.deadlines.expiration(
          now, shard.all_subscribed,
          shard.state == MarketDataState::Live)) {
        case ConnectionDeadlines::Expiration::Startup:
          schedule_reconnect(
              shard,
              "target topic did not deliver a valid first snapshot/image "
              "before the startup deadline",
              now);
          continue;
        case ConnectionDeadlines::Expiration::Recovery:
          schedule_reconnect(
              shard,
              options_.venue == Venue::Polymarket
                  ? "Polymarket recovery deadline requires fresh image"
                  : "runtime market data recovery did not return all target "
                    "streams to Live before the recovery deadline",
              now);
          continue;
        case ConnectionDeadlines::Expiration::None:
          break;
      }
      if (shard.awaiting_ack &&
          shard.acknowledgement_deadline != Clock::time_point{} &&
          now >= shard.acknowledgement_deadline) {
        schedule_reconnect(
            shard,
            "subscription or login acknowledgement timed out", now);
        continue;
      }
      sync_ws_registration(shard);
    }
    update_aggregate_state();
    sync_http_registration(HttpKind::Metadata);
    if (discovery_active_) {
      sync_http_registration(HttpKind::Discovery);
    }
    if (snapshot_active_) {
      sync_http_registration(HttpKind::Snapshot);
    } else if (now >= next_snapshot_allowed_) {
      request_next_snapshot(now);
    }
    const bool reclaim =
        last_reader_reclaim_ == Clock::time_point{} ||
        now - last_reader_reclaim_ >= std::chrono::seconds(1);
    const auto now_ns = static_cast<std::uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            now.time_since_epoch())
            .count());
    for (auto &symbol : symbols_) {
      if (reclaim) {
        const auto timeout_ns = static_cast<std::uint64_t>(
            std::max<std::int64_t>(
                1, symbol.options.reader_lease_timeout.count()));
        if (symbol.ticker_publisher) {
          (void)symbol.ticker_publisher->reclaim_stale_readers(
              now_ns, timeout_ns);
        }
        if (symbol.book_publisher) {
          (void)symbol.book_publisher->reclaim_stale_readers(
              now_ns, timeout_ns);
        }
      }
      if (options_.venue == Venue::Polymarket &&
          symbol.metadata_ready &&
          symbol.catalog_generation != symbol.generation) {
        (void)publish_catalog_barrier(symbol);
      }
    }
    for (auto &publisher : multiplex_ticker_) {
      if (!publisher || !publisher->poll_reader_change()) {
        continue;
      }
      for (auto &symbol : symbols_) {
        if (symbol.ticker_publisher == publisher.get() &&
            !republish_ticker_state(symbol, *publisher)) {
          return;
        }
      }
    }
    for (auto &publisher : multiplex_book_) {
      if (!publisher || !publisher->poll_reader_change()) {
        continue;
      }
      for (auto &symbol : symbols_) {
        if (symbol.book_publisher == publisher.get() &&
            !republish_book_state(symbol, *publisher)) {
          return;
        }
      }
    }
    for (auto &symbol : symbols_) {
      if (symbol.owned_ticker_publisher &&
          symbol.ticker_publisher->poll_reader_change() &&
          !republish_ticker_state(
              symbol, *symbol.ticker_publisher)) {
        return;
      }
      if (symbol.owned_book_publisher &&
          symbol.book_publisher->poll_reader_change() &&
          !republish_book_state(symbol, *symbol.book_publisher)) {
        return;
      }
    }
    if (reclaim) {
      last_reader_reclaim_ = now;
    }
  }

  void drive_outbound(WsShard &shard, Clock::time_point now) {
    if (!shard.websocket->can_send_data()) {
      return;
    }
    if (shard.next_budget_retry != Clock::time_point{} &&
        now < shard.next_budget_retry) {
      return;
    }
    if (shard.next_budget_retry != Clock::time_point{} &&
        now >= shard.next_budget_retry) {
      shard.next_budget_retry = {};
    }
    const auto spacing =
        options_.venue == Venue::Bitget
            ? std::chrono::milliseconds(100)
            : std::chrono::milliseconds(0);
    if (shard.last_data_send != Clock::time_point{} &&
        now - shard.last_data_send < spacing) {
      return;
    }
    std::string_view send_error;
    if (shard.requires_login && !shard.login_sent) {
      if (!shard.budget.allow(now, subscription_limit())) {
        ++metrics_.budget_deferrals;
        shard.next_budget_retry =
            shard.budget.retry_at(now, subscription_limit());
        return;
      }
      std::string login_error;
      shard.login_message =
          okx_login(options_.credentials, login_error);
      if (shard.login_message.empty()) {
        fail(login_error);
        return;
      }
      const auto bytes = std::span<const std::byte>(
          reinterpret_cast<const std::byte *>(
              shard.login_message.data()),
          shard.login_message.size());
      if (!shard.websocket->send(
              net::WsOpcode::Text, bytes, send_error)) {
        schedule_reconnect(shard, send_error, now);
        return;
      }
      shard.login_sent = true;
      shard.pending_acknowledgements = 1;
      shard.awaiting_ack = true;
      shard.acknowledgement_deadline =
          now + options_.request_timeout;
      shard.budget.record(now);
      shard.next_budget_retry = {};
      shard.last_data_send = now;
      ++metrics_.subscription_requests;
      return;
    }
    if (shard.requires_login && !shard.login_acked) {
      return;
    }
    if (!metadata_ready_) {
      shard.state = MarketDataState::Metadata;
      update_aggregate_state();
      return;
    }
    if (!append_pending_symbol_resubscribe(shard, now)) {
      return;
    }
    if (!shard.awaiting_ack &&
        shard.next_batch < shard.batches.size()) {
      if (!shard.budget.allow(now, subscription_limit())) {
        ++metrics_.budget_deferrals;
        shard.next_budget_retry =
            shard.budget.retry_at(now, subscription_limit());
        return;
      }
      const auto &batch = shard.batches[shard.next_batch];
      const auto bytes = std::span<const std::byte>(
          reinterpret_cast<const std::byte *>(batch.data()), batch.size());
      if (!shard.websocket->send(
              net::WsOpcode::Text, bytes, send_error)) {
        schedule_reconnect(shard, send_error, now);
        return;
      }
      shard.budget.record(now);
      shard.next_budget_retry = {};
      shard.pending_acknowledgements =
          shard.adapter->expected_subscription_acks(batch);
      shard.awaiting_ack = shard.pending_acknowledgements != 0;
      if (shard.awaiting_ack) {
        shard.acknowledgement_deadline =
            now + options_.request_timeout;
      } else {
        shard.acknowledgement_deadline = {};
        ++shard.next_batch;
        if (shard.next_batch == shard.batches.size()) {
          shard.all_subscribed = true;
          shard.deadlines.subscriptions_ready(
              now, shard.deadlines.reached_live_once()
                       ? options_.recovery_deadline
                       : options_.request_timeout);
          update_live_state();
        }
      }
      shard.last_data_send = now;
      ++metrics_.subscription_requests;
    }
  }

  std::size_t subscription_limit() const noexcept {
    if (options_.venue == Venue::Okx) {
      return 480;
    }
    if (options_.venue == Venue::Bitget) {
      return 240;
    }
    return 480;
  }

  void drive_heartbeat(WsShard &shard, Clock::time_point now) {
    if (now - shard.last_heartbeat <
        std::chrono::milliseconds(shard.heartbeat.interval_ms)) {
      return;
    }
    if (options_.venue == Venue::Bitget &&
        shard.last_data_send != Clock::time_point{} &&
        now - shard.last_data_send < std::chrono::milliseconds(100)) {
      return;
    }
    if (!shard.websocket->can_send_data() &&
        shard.heartbeat.kind != exchange::HeartbeatKind::Rfc6455Ping) {
      return;
    }
    std::string_view send_error;
    const auto payload = std::span<const std::byte>(
        reinterpret_cast<const std::byte *>(
            shard.heartbeat.payload.data()),
        shard.heartbeat.payload.size());
    bool sent{};
    if (shard.heartbeat.kind ==
        exchange::HeartbeatKind::Rfc6455Ping) {
      sent = shard.websocket->ping(payload, send_error);
    } else {
      sent = shard.websocket->send(
          net::WsOpcode::Text, payload, send_error);
    }
    if (!sent) {
      schedule_reconnect(shard, send_error, now);
      return;
    }
    if (shard.heartbeat.kind !=
        exchange::HeartbeatKind::Rfc6455Ping) {
      shard.last_data_send = now;
    }
    shard.last_heartbeat = now;
  }

  bool symbol_ready(const SymbolRuntime &symbol) const noexcept {
    return RecoveryStreamReady(
        symbol.options.ticker, symbol.ticker_live,
        symbol.options.orderbook, symbol.metadata_ready,
        symbol.image_ready, symbol.book_live,
        symbol.awaiting_snapshot_bridge);
  }

  void begin_recovery(WsShard &shard, Clock::time_point now) noexcept {
    shard.state = MarketDataState::Buffering;
    shard.deadlines.begin_recovery(now, options_.recovery_deadline);
    update_aggregate_state();
  }

  void note_recovery_progress(SymbolRuntime &symbol,
                              Clock::time_point now) noexcept {
    auto &shard = shard_for(symbol);
    if (shard.deadlines.continue_recovery(now,
                                          options_.recovery_deadline)) {
      ++metrics_.recovery_deadline_extensions;
    }
  }

  void update_live_state() {
    if (!metadata_ready_) {
      update_aggregate_state();
      return;
    }
    for (auto &shard : ws_shards_) {
      if (!shard.all_subscribed) {
        continue;
      }
      const bool all_live = std::all_of(
          shard.symbol_indices.begin(), shard.symbol_indices.end(),
          [this](std::size_t index) {
            const auto &symbol = symbols_[index];
            return symbol_ready(symbol);
          });
      if (all_live) {
        const bool recovered =
            shard.state != MarketDataState::Live &&
            shard.reconnect_attempt != 0;
        shard.state = MarketDataState::Live;
        shard.deadlines.mark_live();
        if (recovered) {
          std::cerr << exchange::venue_name(options_.venue)
                    << " websocket reconnect live product="
                    << exchange::product_name(options_.product)
                    << " shard=" << shard.id
                    << " attempt=" << shard.reconnect_attempt << '\n';
          shard.reconnect_attempt = 0;
        }
      } else if (shard.state != MarketDataState::Buffering) {
        shard.state = MarketDataState::Buffering;
        shard.deadlines.begin_recovery(
            Clock::now(), options_.recovery_deadline);
      }
    }
    update_aggregate_state();
  }

  void update_aggregate_state() noexcept {
    if (state_ == MarketDataState::Failed ||
        (state_ == MarketDataState::Stopped && !started_)) {
      return;
    }
    std::size_t live{};
    std::size_t reconnecting{};
    for (const auto &shard : ws_shards_) {
      live += shard.state == MarketDataState::Live ? 1 : 0;
      reconnecting +=
          shard.state == MarketDataState::ReconnectWait ? 1 : 0;
    }
    metrics_.ws_shards = ws_shards_.size();
    metrics_.ws_shards_live = live;
    metrics_.ws_shards_reconnecting = reconnecting;
    if (live == ws_shards_.size() && metadata_ready_) {
      state_ = MarketDataState::Live;
      if (connection_rebuild_attempt_ != 0 &&
          !connection_rebuild_pending_) {
        ++metrics_.connection_rebuild_successes;
        const auto now = Clock::now();
        const auto degraded =
            std::chrono::duration_cast<std::chrono::milliseconds>(
                now - connection_degraded_since_);
        connection_degraded_accumulated_ms_ +=
            static_cast<std::uint64_t>(std::max<std::int64_t>(
                0, degraded.count()));
        metrics_.connection_degraded_duration_ms =
            connection_degraded_accumulated_ms_;
        std::cerr << exchange::venue_name(options_.venue)
                  << " connection rebuild live product="
                  << exchange::product_name(options_.product)
                  << " attempt=" << connection_rebuild_attempt_
                  << " degraded_ms=" << degraded.count() << '\n';
        connection_rebuild_attempt_ = 0;
        connection_degraded_since_ = {};
      }
      return;
    }
    if (reconnecting != 0) {
      state_ = MarketDataState::ReconnectWait;
      return;
    }
    const auto has_state = [this](MarketDataState wanted) {
      return std::any_of(
          ws_shards_.begin(), ws_shards_.end(),
          [wanted](const WsShard &shard) {
            return shard.state == wanted;
          });
    };
    if (has_state(MarketDataState::Connecting)) {
      state_ = MarketDataState::Connecting;
    } else if (!metadata_ready_ ||
               has_state(MarketDataState::Metadata)) {
      state_ = MarketDataState::Metadata;
    } else if (has_state(MarketDataState::Authenticating)) {
      state_ = MarketDataState::Authenticating;
    } else if (has_state(MarketDataState::Subscribing)) {
      state_ = MarketDataState::Subscribing;
    } else {
      state_ = MarketDataState::Buffering;
    }
  }

  void schedule_reconnect(WsShard &shard, std::string_view reason,
                          Clock::time_point now = Clock::now()) {
    if (!started_ || state_ == MarketDataState::Failed) {
      return;
    }
    if (shard.state == MarketDataState::ReconnectWait) {
      return;
    }
    error_ = "ws shard " + std::to_string(shard.id) + ": ";
    error_.append(reason);
    if (shard.registered_fd >= 0) {
      loop_.remove(shard.registered_fd);
      shard.registered_fd = -1;
    }
    shard.websocket->reset();
    shard.connection_started = false;
    shard.adapter->reset_connection_state();
    shard.open_seen = false;
    shard.login_sent = false;
    shard.login_acked = false;
    shard.next_batch = 0;
    shard.pending_acknowledgements = 0;
    shard.awaiting_ack = false;
    shard.all_subscribed = false;
    shard.deadlines.reset_connection();
    shard.acknowledgement_deadline = {};
    shard.last_data_send = {};
    std::vector<std::string> reconnect_batches;
    std::string build_error;
    if (!shard.adapter->build_subscription_batches(
            shard.requests, reconnect_batches, build_error) ||
        reconnect_batches.empty()) {
      fail(build_error.empty() ? "failed to rebuild subscriptions"
                               : build_error);
      return;
    }
    shard.batches = std::move(reconnect_batches);
    for (const auto symbol_index : shard.symbol_indices) {
      auto &symbol = symbols_[symbol_index];
      ++symbol.generation;
      symbol.catalog_generation = 0;
      symbol.image_ready = false;
      symbol.awaiting_snapshot_bridge = false;
      symbol.ticker_live = false;
      symbol.book_live = false;
      symbol.last_book_sequence = 0;
      symbol.last_ticker_sequence = 0;
      symbol.ws_snapshot_recovery.reset_connection();
      symbol.last_ticker_exchange_time_ms = 0;
      symbol.last_book_exchange_time_ms = 0;
      symbol.lagging_snapshot_retries.reset();
      symbol.snapshot_quarantined = false;
      symbol.consecutive_snapshot_failures = 0;
      symbol.resubscribe_pending = false;
      symbol.resubscribe_not_before = {};
      symbol.needs_snapshot =
          symbol.options.orderbook &&
          shard.adapter->needs_rest_snapshot();
      if (symbol.recovery_started == Clock::time_point{}) {
        symbol.recovery_started = now;
      }
      symbol.pending.clear();
    }
    ++metrics_.reconnects;
    metrics_.last_reconnect_shard = shard.id;
    const auto exponent =
        std::min<std::uint32_t>(shard.reconnect_attempt++, 10);
    auto delay = options_.reconnect_base * (1U << exponent);
    delay = std::min(delay, options_.reconnect_max);
    if (delay.count() > 0) {
      const auto range =
          std::max<std::int64_t>(1, delay.count() / 4);
      const auto mixed =
          (static_cast<std::uint64_t>(shard.id + 1) *
               0x9e3779b97f4a7c15ULL) ^
          (static_cast<std::uint64_t>(shard.reconnect_attempt) *
               0xbf58476d1ce4e5b9ULL);
      const auto jitter = std::chrono::milliseconds(
          static_cast<std::int64_t>(mixed %
                                    static_cast<std::uint64_t>(range)));
      delay = std::min(options_.reconnect_max, delay + jitter);
    }
    shard.reconnect_at = now + delay;
    shard.state = MarketDataState::ReconnectWait;
    std::cerr << exchange::venue_name(options_.venue)
              << " websocket reconnect scheduled product="
              << exchange::product_name(options_.product)
              << " shard=" << shard.id
              << " attempt=" << shard.reconnect_attempt
              << " delay_ms=" << delay.count()
              << " reason=" << error_ << '\n';
    update_aggregate_state();
  }

  void schedule_connection_rebuild(
      std::string_view reason, Clock::time_point now = Clock::now(),
      bool failed_attempt = false) {
    const std::string detail(reason);
    if (!started_) {
      error_ = detail;
      state_ = MarketDataState::Failed;
      return;
    }
    if (!connection_rebuild_pending_ &&
        connection_rebuild_attempt_ == 0) {
      connection_degraded_since_ = now;
    }
    if (failed_attempt) {
      ++metrics_.connection_rebuild_failures;
    }
    error_ = detail;

    for (auto &shard : ws_shards_) {
      if (shard.registered_fd >= 0) {
        loop_.remove(shard.registered_fd);
        shard.registered_fd = -1;
      }
      shard.websocket->reset();
      shard.connection_started = false;
      shard.adapter->reset_connection_state();
      shard.pending_reconnect_reason.clear();
      shard.open_seen = false;
      shard.login_sent = false;
      shard.login_acked = false;
      shard.next_batch = 0;
      shard.pending_acknowledgements = 0;
      shard.awaiting_ack = false;
      shard.all_subscribed = false;
      shard.deadlines.reset_connection();
      shard.acknowledgement_deadline = {};
      shard.last_data_send = {};
      shard.state = MarketDataState::ReconnectWait;
    }
    const auto reset_http = [this](HttpKind kind) {
      auto &fd = registered_fd(kind);
      if (fd >= 0) {
        loop_.remove(fd);
        fd = -1;
      }
      http(kind).reset();
    };
    reset_http(HttpKind::Metadata);
    reset_http(HttpKind::Snapshot);
    reset_http(HttpKind::Discovery);
    metadata_batches_.clear();
    metadata_.clear();
    metadata_batch_index_ = 0;
    metadata_ready_ = false;
    snapshot_active_ = false;
    discovery_active_ = false;

    if (!connection_rebuild_pending_) {
      for (auto &symbol : symbols_) {
        ++symbol.generation;
        symbol.catalog_generation = 0;
        symbol.metadata_ready = false;
        symbol.image_ready = false;
        symbol.awaiting_snapshot_bridge = false;
        symbol.ticker_live = false;
        symbol.book_live = false;
        symbol.last_book_sequence = 0;
        symbol.last_ticker_sequence = 0;
        symbol.last_ticker_exchange_time_ms = 0;
        symbol.last_book_exchange_time_ms = 0;
        symbol.latest_bbo.reset();
        symbol.pending.clear();
        symbol.ws_snapshot_recovery.reset_connection();
        symbol.lagging_snapshot_retries.reset();
        symbol.snapshot_quarantined = false;
        symbol.consecutive_snapshot_failures = 0;
        symbol.needs_snapshot =
            symbol.options.orderbook &&
            shard_for(symbol).adapter->needs_rest_snapshot();
        symbol.recovery_started = now;
        if (symbol.instrument.instrument_id != 0) {
          (void)publish_catalog_barrier(symbol);
        }
      }
    }

    ++connection_rebuild_attempt_;
    ++metrics_.connection_rebuild_attempts;
    const auto exponent =
        std::min<std::uint32_t>(connection_rebuild_attempt_ - 1, 10);
    auto delay = options_.reconnect_base * (1U << exponent);
    delay = std::min(delay, options_.reconnect_max);
    if (delay.count() > 0) {
      const auto range =
          std::max<std::int64_t>(1, delay.count() / 4);
      const auto mixed =
          (static_cast<std::uint64_t>(connection_rebuild_attempt_) *
           0x9e3779b97f4a7c15ULL) ^
          static_cast<std::uint64_t>(options_.venue);
      delay = std::min(
          options_.reconnect_max,
          delay + std::chrono::milliseconds(
                      static_cast<std::int64_t>(
                          mixed % static_cast<std::uint64_t>(range))));
    }
    connection_rebuild_at_ = now + delay;
    connection_rebuild_pending_ = true;
    state_ = MarketDataState::ReconnectWait;
    metrics_.ws_shards_live = 0;
    metrics_.ws_shards_reconnecting = ws_shards_.size();
    std::cerr << exchange::venue_name(options_.venue)
              << " connection rebuild scheduled product="
              << exchange::product_name(options_.product)
              << " attempt=" << connection_rebuild_attempt_
              << " delay_ms=" << delay.count()
              << " reason=" << escape_diagnostic_bytes(error_) << '\n';
  }

  bool attempt_connection_rebuild(Clock::time_point now) {
    constexpr auto startup_stagger = std::chrono::milliseconds(50);
    for (auto &shard : ws_shards_) {
      shard.connect_at = now + startup_stagger * shard.id;
      shard.state = MarketDataState::Connecting;
    }
    if (!begin_metadata(now) ||
        !begin_connection(ws_shards_.front(), now)) {
      const std::string reason =
          error_.empty() ? "failed to restart venue connection" : error_;
      schedule_connection_rebuild(reason, now, true);
      return false;
    }
    connection_rebuild_pending_ = false;
    state_ = MarketDataState::Connecting;
    for (auto &symbol : symbols_) {
      symbol.recovery_started = now;
    }
    std::cerr << exchange::venue_name(options_.venue)
              << " connection rebuild started product="
              << exchange::product_name(options_.product)
              << " attempt=" << connection_rebuild_attempt_ << '\n';
    return true;
  }

  void fail(std::string_view reason) {
    const bool failed_attempt =
        started_ && connection_rebuild_attempt_ != 0 &&
        !connection_rebuild_pending_;
    schedule_connection_rebuild(reason, Clock::now(), failed_attempt);
  }

  void stop() noexcept {
    for (auto &shard : ws_shards_) {
      if (shard.registered_fd >= 0) {
        loop_.remove(shard.registered_fd);
        shard.registered_fd = -1;
      }
      shard.websocket->reset();
      shard.connection_started = false;
      shard.state = MarketDataState::Stopped;
    }
    if (metadata_fd_ >= 0) {
      loop_.remove(metadata_fd_);
      metadata_fd_ = -1;
    }
    if (snapshot_fd_ >= 0) {
      loop_.remove(snapshot_fd_);
      snapshot_fd_ = -1;
    }
    if (discovery_fd_ >= 0) {
      loop_.remove(discovery_fd_);
      discovery_fd_ = -1;
    }
    metadata_http_.reset();
    snapshot_http_.reset();
    discovery_http_.reset();
    metadata_batches_.clear();
    metadata_.clear();
    metadata_batch_index_ = 0;
    connection_rebuild_pending_ = false;
    connection_rebuild_attempt_ = 0;
    connection_rebuild_at_ = {};
    connection_degraded_since_ = {};
    started_ = false;
    metrics_.ws_shards_live = 0;
    metrics_.ws_shards_reconnecting = 0;
    state_ = MarketDataState::Stopped;
  }

  net::EpollLoop &loop_;
  net::SharedSslContext tls_;
  VenueConnectionOptions options_;
  std::unique_ptr<exchange::VenueAdapter> metadata_adapter_;
  std::vector<WsShard> ws_shards_;
  net::HttpClient metadata_http_;
  net::HttpClient snapshot_http_;
  net::HttpClient discovery_http_;
  std::vector<SymbolRuntime> symbols_;
  std::vector<std::unique_ptr<publish::WirePublisher>> multiplex_ticker_;
  std::vector<std::unique_ptr<publish::WirePublisher>> multiplex_book_;
  std::vector<exchange::StreamRequest> requests_;
  std::vector<exchange::MetadataRequestBatch> metadata_batches_;
  std::vector<exchange::InstrumentMetadata> metadata_;
  std::unique_ptr<exchange::NormalizedEvent> snapshot_event_;
  MarketDataMetrics metrics_{};
  std::atomic<MarketDataState> state_{MarketDataState::Stopped};
  std::string error_;
  Clock::time_point last_reader_reclaim_{};
  Clock::time_point next_snapshot_allowed_{};
  Clock::time_point discovery_started_{};
  Clock::time_point next_discovery_attempt_{};
  Clock::time_point connection_rebuild_at_{};
  Clock::time_point connection_degraded_since_{};
  std::size_t metadata_batch_index_{};
  std::size_t snapshot_symbol_{};
  std::size_t next_snapshot_symbol_{};
  int metadata_fd_{-1};
  int snapshot_fd_{-1};
  int discovery_fd_{-1};
  std::uint64_t metadata_generation_{};
  std::uint64_t snapshot_generation_{};
  std::uint64_t discovery_generation_{};
  std::uint64_t observed_stale_messages_{};
  std::uint64_t connection_degraded_accumulated_ms_{};
  std::uint32_t connection_rebuild_attempt_{};
  std::int64_t active_market_window_{};
  exchange::polymarket::ResolveSlot discovery_slot_{
      exchange::polymarket::ResolveSlot::Next};
  bool metadata_ready_{};
  bool snapshot_active_{};
  bool discovery_active_{};
  bool connection_rebuild_pending_{};
  bool started_{};
};

VenueConnection::VenueConnection(net::EpollLoop &loop,
                                 net::SharedSslContext tls,
                                 VenueConnectionOptions options)
    : impl_(std::make_unique<Impl>(loop, std::move(tls),
                                   std::move(options))) {}

VenueConnection::~VenueConnection() { stop(); }

api::Result<void> VenueConnection::start(Clock::time_point now) {
  return impl_->start(now);
}

void VenueConnection::tick(Clock::time_point now) noexcept {
  impl_->tick(now);
}

void VenueConnection::stop() noexcept { impl_->stop(); }

MarketDataState VenueConnection::state() const noexcept {
  return impl_->state_.load(std::memory_order_acquire);
}

const MarketDataMetrics &VenueConnection::metrics() const noexcept {
  return impl_->metrics_;
}

std::string_view VenueConnection::error_message() const noexcept {
  return impl_->error_;
}

Venue VenueConnection::venue() const noexcept {
  return impl_->options_.venue;
}

Product VenueConnection::product() const noexcept {
  return impl_->options_.product;
}

std::span<const SymbolStreamOptions>
VenueConnection::streams() const noexcept {
  return impl_->options_.streams;
}

std::size_t VenueConnection::websocket_shard_count() const noexcept {
  return impl_->ws_shards_.size();
}

std::optional<std::size_t> VenueConnection::websocket_shard(
    std::string_view canonical_symbol) const noexcept {
  const auto found = std::find_if(
      impl_->symbols_.begin(), impl_->symbols_.end(),
      [canonical_symbol](const Impl::SymbolRuntime &symbol) {
        return symbol.options.symbol == canonical_symbol;
      });
  if (found == impl_->symbols_.end()) {
    return std::nullopt;
  }
  return found->shard_id;
}

VenueConnectionManager::VenueConnectionManager() {
  std::string error;
  tls_ = net::make_client_ssl_context(error);
}

VenueConnectionManager::VenueConnectionManager(
    net::SharedSslContext tls)
    : tls_(std::move(tls)) {}

api::Result<VenueConnection *> VenueConnectionManager::create(
    VenueConnectionOptions options, bool start_immediately) {
  if (!tls_) {
    return {.value = nullptr,
            .error = api::ErrorCode::InternalError,
            .message = "TLS context is unavailable"};
  }
  if (!options.instrument_manager) {
    options.instrument_manager = instrument_manager_;
  }
  auto connection =
      std::make_unique<VenueConnection>(loop_, tls_, std::move(options));
  auto *pointer = connection.get();
  if (start_immediately) {
    const auto started = pointer->start();
    if (!started) {
      return {.error = started.error, .message = started.message};
    }
  }
  connections_.push_back(std::move(connection));
  return {.value = pointer};
}

int VenueConnectionManager::run_once(int timeout_ms) {
  const int result = loop_.run_once(timeout_ms);
  const auto now = Clock::now();
  for (auto &connection : connections_) {
    connection->tick(now);
  }
  return result;
}

void VenueConnectionManager::stop() noexcept {
  for (auto &connection : connections_) {
    connection->stop();
  }
  loop_.stop();
}

}  // namespace mds::service
