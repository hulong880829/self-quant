#include "mds/service/venue_connection.h"

#include "mds/book/book_pipeline.h"
#include "mds/exchange/capabilities.h"
#include "mds/exchange/polymarket/polymarket_adapter.h"
#include "mds/exchange/symbol_policy.h"
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
#include <cstdio>
#include <cstring>
#include <cstdlib>
#include <ctime>
#include <iostream>
#include <limits>
#include <optional>
#include <set>
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

constexpr std::array<net::HttpHeader, 1> kGateDecimalHttpHeaders{{
    {"X-Gate-Size-Decimal", "1"},
}};
constexpr std::array<net::WebSocketHeader, 1> kGateDecimalWsHeaders{{
    {"X-Gate-Size-Decimal", "1"},
}};

bool is_gate_perpetual(Venue venue, Product product) noexcept {
  return venue == Venue::Gate && product == Product::Perpetual;
}

std::span<const net::HttpHeader>
gate_decimal_http_headers(Venue venue, Product product) noexcept {
  if (is_gate_perpetual(venue, product)) {
    return kGateDecimalHttpHeaders;
  }
  return {};
}

std::span<const net::WebSocketHeader>
gate_decimal_ws_headers(Venue venue, Product product) noexcept {
  if (is_gate_perpetual(venue, product)) {
    return kGateDecimalWsHeaders;
  }
  return {};
}

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

bool start_http_request(
    net::HttpClient &client, const Endpoint &endpoint,
    const exchange::HttpRequestSpec &request,
    std::span<const net::HttpHeader> headers,
    Clock::time_point deadline) noexcept {
  const auto method =
      request.method == exchange::HttpRequestSpec::Method::Post
          ? net::HttpMethod::Post
          : net::HttpMethod::Get;
  return client.start_request(
      endpoint.host, endpoint.service,
      net::HttpRequest{
          method,
          request.target,
          request.content_type,
          {reinterpret_cast<const std::byte *>(request.body.data()),
           request.body.size()},
          headers,
      },
      deadline);
}

std::uint64_t clock_ticks() noexcept {
  return static_cast<std::uint64_t>(
      Clock::now().time_since_epoch().count());
}

std::string utc_timestamp() {
  const auto now = std::chrono::system_clock::now();
  const auto time = std::chrono::system_clock::to_time_t(now);
  std::tm utc{};
  gmtime_r(&time, &utc);
  char date[32]{};
  std::strftime(date, sizeof(date), "%Y-%m-%dT%H:%M:%S", &utc);
  const auto milliseconds =
      std::chrono::duration_cast<std::chrono::milliseconds>(
          now.time_since_epoch())
          .count() %
      1000;
  char result[40]{};
  std::snprintf(result, sizeof(result), "%s.%03lldZ", date,
                static_cast<long long>(milliseconds));
  return result;
}

std::string_view failure_category_name(
    exchange::ParseFailureCategory value) noexcept {
  using Category = exchange::ParseFailureCategory;
  switch (value) {
    case Category::None:
      return "none";
    case Category::DirtyData:
      return "dirty_data";
    case Category::SequenceGap:
      return "sequence_gap";
    case Category::Protocol:
      return "protocol";
    case Category::ConfigurationMetadata:
      return "configuration_metadata";
  }
  return "unknown";
}

std::string_view failure_scope_name(
    exchange::ParseFailureScope value) noexcept {
  using Scope = exchange::ParseFailureScope;
  switch (value) {
    case Scope::None:
      return "none";
    case Scope::Symbol:
      return "symbol";
    case Scope::Shard:
      return "shard";
    case Scope::Venue:
      return "venue";
  }
  return "unknown";
}

std::string_view failure_code_name(
    exchange::ParseFailureCode value) noexcept {
  using Code = exchange::ParseFailureCode;
  switch (value) {
    case Code::None:
      return "none";
    case Code::MalformedPayload:
      return "malformed_payload";
    case Code::InvalidSymbol:
      return "invalid_symbol";
    case Code::InvalidPrice:
      return "invalid_price";
    case Code::InvalidQuantity:
      return "invalid_quantity";
    case Code::CapacityExceeded:
      return "capacity_exceeded";
    case Code::SequenceDiscontinuity:
      return "sequence_discontinuity";
    case Code::UnsupportedMessage:
      return "unsupported_message";
    case Code::MetadataUnavailable:
      return "metadata_unavailable";
    case Code::ScaleMismatch:
      return "scale_mismatch";
  }
  return "unknown";
}

template <std::size_t Size>
bool copy_text(std::array<char, Size> &destination,
               std::string_view source) noexcept {
  destination.fill('\0');
  if (source.size() >= Size) {
    return false;
  }
  std::memcpy(destination.data(), source.data(), source.size());
  destination[source.size()] = '\0';
  return true;
}

std::string truncated_cursor_hash(std::string_view cursor) {
  std::uint64_t hash = 14695981039346656037ULL;
  for (const char raw : cursor) {
    hash ^= static_cast<unsigned char>(raw);
    hash *= 1099511628211ULL;
  }
  char digits[17];
  std::snprintf(digits, sizeof(digits), "%016llx",
                static_cast<unsigned long long>(hash));
  return {digits, 8};
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
    for (const auto suffix :
         {std::string_view{"USDC"}, std::string_view{"USDT"}}) {
      if (canonical.size() > suffix.size() &&
          canonical.substr(canonical.size() - suffix.size()) == suffix) {
        return std::string(
            canonical.substr(0, canonical.size() - suffix.size()));
      }
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
  if (venue == Venue::Lighter && canonical.ends_with("USDC") &&
      canonical.size() > std::string_view{"USDC"}.size()) {
    const auto base =
        canonical.substr(0, canonical.size() - std::string_view{"USDC"}.size());
    if (product == Product::Perpetual) {
      return std::string(base);
    }
    if (product == Product::Spot) {
      std::string result(base);
      result.append("/USDC");
      return result;
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

std::chrono::milliseconds outbound_send_spacing(
    Venue venue, Product product) noexcept {
  if (venue == Venue::Bitget) {
    return std::chrono::milliseconds(125);
  }
  if (venue == Venue::Aster && product == Product::Spot) {
    return std::chrono::milliseconds(250);
  }
  return std::chrono::milliseconds(0);
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
  enum class SymbolResubscribeMode : std::uint8_t {
    UnsubscribeThenSubscribe,
    SubscribeOnly,
  };

  enum class ReconnectKind : std::uint8_t {
    Failure,
    ServerExpiration,
  };

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
    std::vector<std::size_t> batch_symbol_indices;
    std::vector<std::size_t> batch_pending_acknowledgements;
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
    std::size_t next_batch_to_send{};
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
    bool subscription_isolation_mode{};
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
    Clock::time_point metadata_refresh_not_before{};
    Clock::time_point last_metadata_refresh_attempt{};
    Clock::time_point recovery_started{};
    std::uint32_t consecutive_snapshot_failures{};
    std::uint32_t consecutive_metadata_refresh_failures{};
    std::uint32_t subscription_rate_limit_retries{};
    std::uint32_t catalog_generation{};
    bool metadata_ready{};
    bool image_ready{};
    bool awaiting_snapshot_bridge{};
    bool ticker_live{};
    bool book_live{};
    bool snapshot_quarantined{};
    bool needs_snapshot{};
    bool resubscribe_pending{};
    bool rate_limit_recovery_active{};
    bool resubscribe_ticker{};
    bool resubscribe_orderbook{};
    SymbolResubscribeMode resubscribe_mode{
        SymbolResubscribeMode::UnsubscribeThenSubscribe};
    bool quarantined{};
    bool scale_mismatch_seen{};
    bool metadata_refresh_pending{};
    bool metadata_refresh_quarantined{};
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
    std::vector<std::string> config_quarantine_examples;
    std::erase_if(options_.streams, [&](const auto &stream) {
      const auto mapped = stream.venue_symbol.empty()
                              ? venue_symbol(options_.venue, options_.product,
                                             stream.symbol)
                              : stream.venue_symbol;
      if (exchange::valid_utf8_symbol(stream.symbol) &&
          exchange::valid_utf8_symbol(mapped)) {
        return false;
      }
      ++metrics_.config_symbol_quarantines;
      if (config_quarantine_examples.size() < 3U) {
        config_quarantine_examples.push_back(stream.symbol);
      }
      return true;
    });
    if (metrics_.config_symbol_quarantines != 0) {
      std::cerr << utc_timestamp() << ' '
                << exchange::venue_name(options_.venue)
                << " config symbols quarantined product="
                << exchange::product_name(options_.product)
                << " count=" << metrics_.config_symbol_quarantines;
      for (const auto &example : config_quarantine_examples) {
        std::cerr << " example=" << escape_diagnostic_bytes(example);
      }
      std::cerr << '\n';
    }
    std::size_t max_levels = 20;
    symbols_.reserve(options_.streams.size());
    requests_.reserve(options_.streams.size());
    metadata_refresh_queue_.reserve(options_.streams.size());
    metadata_refresh_active_symbols_.reserve(options_.streams.size());
    metadata_refresh_requests_.reserve(options_.streams.size());
    for (auto &stream : options_.streams) {
      max_levels = std::max(max_levels, stream.max_levels_per_message);
      SymbolRuntime runtime;
      runtime.venue_symbol = stream.venue_symbol.empty()
                                 ? venue_symbol(options_.venue,
                                                options_.product,
                                                stream.symbol)
                                 : stream.venue_symbol;
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

  [[nodiscard]] bool can_adopt_multiplex_publishers(
      const Impl &source) const noexcept {
    if (options_.venue != source.options_.venue ||
        options_.product != source.options_.product ||
        options_.streams.empty() || source.options_.streams.empty()) {
      return false;
    }
    const auto &destination = options_.streams.front();
    const auto &origin = source.options_.streams.front();
    if (destination.ring_layout == publish::RingLayout::PerSymbol ||
        destination.shm_prefix != origin.shm_prefix ||
        destination.ring_layout != origin.ring_layout ||
        destination.shard_count != origin.shard_count) {
      return false;
    }
    const auto same_ring = [](const transport::RingOptions &left,
                              const transport::RingOptions &right) {
      return left.backend == right.backend && left.mode == right.mode &&
             left.hugetlbfs_mount == right.hugetlbfs_mount &&
             left.ring_bytes == right.ring_bytes &&
             left.max_record_bytes == right.max_record_bytes &&
             left.max_readers == right.max_readers &&
             left.create == right.create &&
             left.allow_hugepage_fallback ==
                 right.allow_hugepage_fallback &&
             left.unlink_on_close == right.unlink_on_close;
    };
    return same_ring(destination.multiplex_ring,
                     origin.multiplex_ring);
  }

  void adopt_multiplex_publishers(Impl &source) noexcept {
    if (!can_adopt_multiplex_publishers(source)) {
      return;
    }
    multiplex_ticker_ = std::move(source.multiplex_ticker_);
    multiplex_book_ = std::move(source.multiplex_book_);
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
    metadata_http_purpose_ = MetadataHttpPurpose::Startup;
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
    if (symbol.latest_bbo) {
      symbol.latest_bbo->header.bbo_origin =
          utils::md::BboOrigin::TickerStream;
    }
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
    reset_metadata_pagination();
    metadata_bootstrap_started_ = now;
    auto &adapter = metadata_adapter();
    if (!adapter.build_bootstrap_metadata_request_batches(
            requests_, metadata_batches_, error_) ||
        metadata_batches_.empty()) {
      if (error_.empty()) {
        error_ = "venue produced no metadata requests";
      }
      return false;
    }
    metadata_bootstrap_paginated_ =
        std::any_of(metadata_batches_.begin(), metadata_batches_.end(),
                    [](const exchange::MetadataRequestBatch &batch) {
                      return batch.cursor_paginated;
                    });
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
    metadata_http_purpose_ = MetadataHttpPurpose::VenueRefresh;
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
    if (batch.cursor_paginated) {
      constexpr std::size_t kMaximumMetadataPages = 64;
      if (metadata_page_count_ >= kMaximumMetadataPages) {
        error_ = "Bybit metadata pagination exceeded page limit";
        return false;
      }
      ++metadata_page_count_;
    }
    const auto endpoint = parse_endpoint(options_.rest_endpoint);
    const auto &request = batch.http;
    const bool started = start_http_request(
        metadata_http_, endpoint, request,
        gate_decimal_http_headers(options_.venue, options_.product),
        now + options_.request_timeout);
    if (!started) {
      error_ = std::string(metadata_http_.error_message());
      return false;
    }
    sync_http_registration(HttpKind::Metadata);
    return true;
  }

  bool request_symbol_metadata_refresh(
      SymbolRuntime &symbol, Clock::time_point now) {
    if (options_.venue == Venue::Polymarket ||
        symbol.metadata_refresh_quarantined) {
      return true;
    }
    if (symbol.metadata_refresh_pending) {
      ++metrics_.metadata_refresh_coalesced;
      return true;
    }
    const auto index = static_cast<std::size_t>(
        &symbol - symbols_.data());
    if (index >= symbols_.size()) {
      return false;
    }
    constexpr auto cooldown = std::chrono::seconds(1);
    symbol.metadata_refresh_pending = true;
    symbol.metadata_refresh_not_before = now;
    if (symbol.last_metadata_refresh_attempt != Clock::time_point{}) {
      symbol.metadata_refresh_not_before = std::max(
          symbol.metadata_refresh_not_before,
          symbol.last_metadata_refresh_attempt + cooldown);
      if (symbol.metadata_refresh_not_before > now) {
        ++metrics_.cooldown_deferrals;
      }
    }
    metadata_refresh_queue_.push_back(index);
    return true;
  }

  void clear_active_metadata_refresh() noexcept {
    if (metadata_fd_ >= 0) {
      loop_.remove(metadata_fd_);
      metadata_fd_ = -1;
    }
    metadata_http_.reset();
    metadata_http_purpose_ = MetadataHttpPurpose::Idle;
    metadata_refresh_active_symbols_.clear();
    metadata_refresh_requests_.clear();
    metadata_refresh_batches_.clear();
    metadata_refresh_batch_index_ = 0;
  }

  void metadata_refresh_backoff(
      std::string_view reason, Clock::time_point now,
      std::optional<std::chrono::milliseconds> forced_delay =
          std::nullopt,
      bool quarantine = false) {
    ++metrics_.metadata_refresh_failures;
    const std::string bounded_reason(reason.substr(0, 512));
    for (const auto symbol_index : metadata_refresh_active_symbols_) {
      if (symbol_index >= symbols_.size()) {
        continue;
      }
      auto &symbol = symbols_[symbol_index];
      if (!symbol.metadata_refresh_pending) {
        continue;
      }
      ++symbol.consecutive_metadata_refresh_failures;
      if (quarantine ||
          symbol.consecutive_metadata_refresh_failures >=
              options_.snapshot_max_consecutive_failures) {
        if (!symbol.metadata_refresh_quarantined) {
          symbol.metadata_refresh_quarantined = true;
          ++metrics_.metadata_symbol_quarantines;
          std::cerr
              << utc_timestamp() << ' '
              << exchange::venue_name(options_.venue)
              << " symbol metadata refresh quarantined product="
              << exchange::product_name(options_.product)
              << " symbol="
              << escape_diagnostic_bytes(symbol.options.symbol)
              << " reason="
              << escape_diagnostic_bytes(bounded_reason) << '\n';
        }
        symbol.metadata_refresh_pending = false;
        continue;
      }
      const auto exponent = std::min<std::uint32_t>(
          symbol.consecutive_metadata_refresh_failures - 1U, 16U);
      const auto multiplier = std::uint64_t{1} << exponent;
      const auto raw =
          std::uint64_t{options_.snapshot_failure_backoff_ms} *
          multiplier;
      const auto capped = std::min<std::uint64_t>(
          raw, options_.snapshot_failure_backoff_max_ms);
      const auto jitter =
          (capped / 8U) *
          (symbol.consecutive_metadata_refresh_failures % 3U);
      auto delay = std::chrono::milliseconds(
          std::min<std::uint64_t>(
              capped + jitter,
              options_.snapshot_failure_backoff_max_ms));
      if (forced_delay) {
        delay = std::max(delay, *forced_delay);
      }
      constexpr auto cooldown = std::chrono::seconds(1);
      symbol.metadata_refresh_not_before =
          std::max(now + delay,
                   symbol.last_metadata_refresh_attempt + cooldown);
      metadata_refresh_queue_.push_back(symbol_index);
    }
    if (!bounded_reason.empty()) {
      std::cerr << exchange::venue_name(options_.venue)
                << " symbol metadata refresh deferred product="
                << exchange::product_name(options_.product)
                << " reason="
                << escape_diagnostic_bytes(bounded_reason) << '\n';
    }
    clear_active_metadata_refresh();
  }

  bool start_symbol_metadata_batch(Clock::time_point now) {
    if (metadata_refresh_batch_index_ >=
        metadata_refresh_batches_.size()) {
      return false;
    }
    const auto &batch =
        metadata_refresh_batches_[metadata_refresh_batch_index_];
    if (batch.request_count == 0 ||
        batch.request_offset > metadata_refresh_requests_.size() ||
        batch.request_count >
            metadata_refresh_requests_.size() - batch.request_offset) {
      return false;
    }
    const auto endpoint = parse_endpoint(options_.rest_endpoint);
    const auto &request = batch.http;
    const bool started = start_http_request(
        metadata_http_, endpoint, request,
        gate_decimal_http_headers(options_.venue, options_.product),
        now + options_.request_timeout);
    if (!started) {
      return false;
    }
    ++metrics_.metadata_refresh_requests;
    sync_http_registration(HttpKind::Metadata);
    return true;
  }

  void drive_symbol_metadata_refresh(Clock::time_point now) {
    if (!metadata_ready_ ||
        metadata_http_purpose_ != MetadataHttpPurpose::Idle ||
        metadata_refresh_queue_.empty()) {
      return;
    }
    metadata_refresh_active_symbols_.clear();
    std::erase_if(
        metadata_refresh_queue_,
        [this, now](std::size_t symbol_index) {
          if (symbol_index >= symbols_.size()) {
            return true;
          }
          auto &symbol = symbols_[symbol_index];
          if (!symbol.metadata_refresh_pending ||
              symbol.metadata_refresh_quarantined) {
            return true;
          }
          if (now < symbol.metadata_refresh_not_before) {
            return false;
          }
          symbol.last_metadata_refresh_attempt = now;
          metadata_refresh_active_symbols_.push_back(symbol_index);
          return true;
        });
    if (metadata_refresh_active_symbols_.empty()) {
      return;
    }
    metadata_refresh_requests_.clear();
    for (const auto symbol_index :
         metadata_refresh_active_symbols_) {
      metadata_refresh_requests_.push_back(requests_[symbol_index]);
    }
    metadata_refresh_batches_.clear();
    std::string build_error;
    if (!metadata_adapter().build_metadata_request_batches(
            metadata_refresh_requests_, metadata_refresh_batches_,
            build_error) ||
        metadata_refresh_batches_.empty()) {
      metadata_http_purpose_ = MetadataHttpPurpose::SymbolRefresh;
      metadata_refresh_backoff(
          build_error.empty()
              ? "venue produced no symbol metadata refresh request"
              : build_error,
          now);
      return;
    }
    metadata_refresh_batch_index_ = 0;
    metadata_http_purpose_ = MetadataHttpPurpose::SymbolRefresh;
    if (!start_symbol_metadata_batch(now)) {
      metadata_refresh_backoff(
          metadata_http_.error_message().empty()
              ? std::string_view(
                    "failed to start symbol metadata refresh")
              : metadata_http_.error_message(),
          now);
    }
  }

  bool begin_connection(WsShard &shard, Clock::time_point now) {
    if (options_.connection_attempt_budget &&
        !options_.connection_attempt_budget->allow(now)) {
      shard.connect_at =
          options_.connection_attempt_budget->retry_at(now);
      ++metrics_.cooldown_deferrals;
      return true;
    }
    if (options_.connection_attempt_budget) {
      options_.connection_attempt_budget->record(now);
    }
    const auto endpoint = parse_endpoint(options_.websocket_endpoint);
    if (!shard.websocket->set_upgrade_headers(
            gate_decimal_ws_headers(options_.venue, options_.product))) {
      error_ = "invalid WebSocket upgrade headers";
      return false;
    }
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
    shard.next_batch_to_send = 0;
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
  enum class MetadataHttpPurpose : std::uint8_t {
    Idle,
    Startup,
    VenueRefresh,
    SymbolRefresh,
  };

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

  void schedule_terminal_reconnect(
      WsShard &shard, Clock::time_point now = Clock::now()) {
    const bool has_pending_reason =
        !shard.pending_reconnect_reason.empty();
    const auto close_code = shard.websocket->close_code();
    const std::string close_reason(shard.websocket->close_reason());
    const std::string reason =
        has_pending_reason
            ? std::move(shard.pending_reconnect_reason)
            : std::string(shard.websocket->error_message());
    shard.pending_reconnect_reason.clear();
    const auto kind =
        !has_pending_reason && options_.venue == Venue::Hyperliquid &&
                close_code.has_value() && *close_code == 1000 &&
                close_reason == "Expired"
            ? ReconnectKind::ServerExpiration
            : ReconnectKind::Failure;
    schedule_reconnect(shard, reason, now, kind);
  }

  void on_ws_event(WsShard &shard, std::uint32_t events) {
    shard.websocket->on_event(events);
    sync_ws_registration(shard);
    if (terminal(shard.websocket->state())) {
      schedule_terminal_reconnect(shard);
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
      } else if (metadata_http_purpose_ ==
                 MetadataHttpPurpose::SymbolRefresh) {
        metadata_refresh_backoff(
            client.error_message(), Clock::now());
      } else {
        fail_metadata_response(
            "http",
            std::string("metadata HTTP: ") +
                std::string(client.error_message()));
      }
    }
  }

  void log_ws_diagnostic(const WsShard &shard, std::string_view kind,
                         std::string_view reason, std::string_view payload,
                         std::string_view symbol = {}) const {
    std::cerr << utc_timestamp() << ' '
              << exchange::venue_name(options_.venue) << " websocket "
              << kind << " failed product="
              << exchange::product_name(options_.product)
              << " shard=" << shard.id
              << " connection_generation="
              << shard.websocket->socket_generation();
    if (!symbol.empty()) {
      std::cerr << " symbol=" << escape_diagnostic_bytes(symbol);
    }
    std::cerr << " failure_code="
              << failure_code_name(shard.parse_failure.code)
              << " category="
              << failure_category_name(shard.parse_failure.category)
              << " scope=" << failure_scope_name(shard.parse_failure.scope)
              << " payload_bytes=" << payload.size()
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
              exchange::ParseFailureCategory::ConfigurationMetadata &&
          shard.parse_failure.scope ==
              exchange::ParseFailureScope::Symbol &&
          shard.parse_failure.code ==
              exchange::ParseFailureCode::ScaleMismatch &&
          shard.parse_failure.has_symbol()) {
        auto *symbol = find_symbol(
            shard, shard.parse_failure.symbol_view());
        if (symbol != nullptr && symbol->metadata_ready) {
          ++metrics_.scale_mismatch_frames;
          if (!symbol->scale_mismatch_seen) {
            symbol->scale_mismatch_seen = true;
            ++metrics_.scale_mismatch_symbols;
            log_ws_diagnostic(
                shard, "scale-mismatch", reason, json,
                shard.parse_failure.symbol_view());
          }
          (void)request_symbol_metadata_refresh(
              *symbol, Clock::now());
          return true;
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

  bool rebuild_subscription_batches(WsShard &shard, bool isolate,
                                    std::string &error) {
    shard.batches.clear();
    shard.batch_symbol_indices.clear();
    if (!isolate &&
        shard.adapter->subscription_send_window() == 1U) {
      if (!shard.adapter->build_subscription_batches(
              shard.requests, shard.batches, error) ||
          shard.batches.empty()) {
        return false;
      }
      shard.batch_symbol_indices.assign(
          shard.batches.size(), std::numeric_limits<std::size_t>::max());
      return true;
    }
    for (std::size_t index = 0; index < shard.requests.size(); ++index) {
      std::vector<std::string> symbol_batches;
      const auto request =
          std::span<const exchange::StreamRequest>(&shard.requests[index], 1);
      if (!shard.adapter->build_subscription_batches(
              request, symbol_batches, error) ||
          symbol_batches.empty()) {
        return false;
      }
      for (auto &batch : symbol_batches) {
        shard.batches.push_back(std::move(batch));
        shard.batch_symbol_indices.push_back(shard.symbol_indices[index]);
      }
    }
    return !shard.batches.empty();
  }

  void reset_subscription_cursor(WsShard &shard) noexcept {
    shard.next_batch = 0;
    shard.next_batch_to_send = 0;
    shard.pending_acknowledgements = 0;
    shard.batch_pending_acknowledgements.assign(
        shard.batches.size(), 0);
    shard.awaiting_ack = false;
    shard.acknowledgement_deadline = {};
    shard.all_subscribed = false;
  }

  [[nodiscard]] bool has_pending_symbol_subscription(
      const WsShard &shard) const noexcept {
    return std::any_of(
        shard.symbol_indices.begin(), shard.symbol_indices.end(),
        [this](std::size_t symbol_index) {
          if (symbol_index >= symbols_.size()) {
            return false;
          }
          const auto &symbol = symbols_[symbol_index];
          return symbol.resubscribe_pending ||
                 symbol.rate_limit_recovery_active;
        });
  }

  void complete_subscriptions_if_ready(
      WsShard &shard, Clock::time_point now) {
    const bool batches_complete =
        !shard.awaiting_ack &&
        shard.next_batch == shard.batches.size() &&
        shard.next_batch_to_send == shard.batches.size();
    if (!batches_complete || has_pending_symbol_subscription(shard)) {
      shard.all_subscribed = false;
      return;
    }
    shard.all_subscribed = true;
    shard.deadlines.subscriptions_ready(
        now, shard.deadlines.reached_live_once()
                 ? options_.recovery_deadline
                 : options_.request_timeout);
    update_live_state();
  }

  std::optional<std::size_t> settle_subscription_acknowledgement(
      WsShard &shard, std::string_view symbol) {
    if (!shard.awaiting_ack || shard.pending_acknowledgements == 0) {
      return std::nullopt;
    }
    auto acknowledged_batch = shard.next_batch_to_send;
    if (!symbol.empty()) {
      for (std::size_t index = shard.next_batch;
           index < shard.next_batch_to_send; ++index) {
        if (index >= shard.batch_pending_acknowledgements.size() ||
            shard.batch_pending_acknowledgements[index] == 0 ||
            index >= shard.batch_symbol_indices.size()) {
          continue;
        }
        const auto symbol_index = shard.batch_symbol_indices[index];
        if (symbol_index >= symbols_.size()) {
          continue;
        }
        if (symbols_[symbol_index].venue_symbol == symbol ||
            symbols_[symbol_index].options.symbol == symbol) {
          acknowledged_batch = index;
          break;
        }
      }
    }
    if (acknowledged_batch == shard.next_batch_to_send) {
      for (std::size_t index = shard.next_batch;
           index < shard.next_batch_to_send; ++index) {
        if (index < shard.batch_pending_acknowledgements.size() &&
            shard.batch_pending_acknowledgements[index] != 0) {
          acknowledged_batch = index;
          break;
        }
      }
    }
    if (acknowledged_batch >= shard.next_batch_to_send ||
        acknowledged_batch >=
            shard.batch_pending_acknowledgements.size() ||
        shard.batch_pending_acknowledgements[acknowledged_batch] == 0 ||
        shard.pending_acknowledgements == 0) {
      return std::nullopt;
    }
    --shard.batch_pending_acknowledgements[acknowledged_batch];
    --shard.pending_acknowledgements;
    while (shard.next_batch < shard.next_batch_to_send &&
           shard.next_batch <
               shard.batch_pending_acknowledgements.size() &&
           shard.batch_pending_acknowledgements[shard.next_batch] == 0) {
      ++shard.next_batch;
    }
    shard.awaiting_ack = shard.pending_acknowledgements != 0;
    shard.acknowledgement_deadline =
        shard.awaiting_ack ? Clock::now() + options_.request_timeout
                           : Clock::time_point{};
    return acknowledged_batch;
  }

  void complete_rate_limit_recovery(
      WsShard &shard, std::size_t acknowledged_batch) {
    if (acknowledged_batch >=
            shard.batch_pending_acknowledgements.size() ||
        shard.batch_pending_acknowledgements[acknowledged_batch] != 0 ||
        acknowledged_batch >= shard.batch_symbol_indices.size()) {
      return;
    }
    const auto symbol_index =
        shard.batch_symbol_indices[acknowledged_batch];
    if (symbol_index >= symbols_.size()) {
      return;
    }
    auto &symbol = symbols_[symbol_index];
    if (!symbol.rate_limit_recovery_active ||
        symbol.resubscribe_pending) {
      return;
    }
    symbol.rate_limit_recovery_active = false;
    symbol.subscription_rate_limit_retries = 0;
    symbol.resubscribe_mode =
        SymbolResubscribeMode::UnsubscribeThenSubscribe;
    symbol.resubscribe_ticker = false;
    symbol.resubscribe_orderbook = false;
  }

  bool quarantine_subscription_symbol(WsShard &shard,
                                      std::size_t symbol_index,
                                      std::string_view reason) {
    if (symbol_index >= symbols_.size()) {
      return false;
    }
    auto &symbol = symbols_[symbol_index];
    if (!symbol.quarantined) {
      symbol.quarantined = true;
      ++metrics_.subscription_symbol_quarantines;
      std::cerr << utc_timestamp() << ' '
                << exchange::venue_name(options_.venue)
                << " subscription symbol quarantined product="
                << exchange::product_name(options_.product)
                << " shard=" << shard.id
                << " symbol="
                << escape_diagnostic_bytes(symbol.options.symbol)
                << " reason=" << escape_diagnostic_bytes(reason) << '\n';
    }
    for (std::size_t index = 0; index < shard.symbol_indices.size();) {
      if (shard.symbol_indices[index] == symbol_index) {
        shard.symbol_indices.erase(
            shard.symbol_indices.begin() +
            static_cast<std::ptrdiff_t>(index));
        shard.requests.erase(
            shard.requests.begin() + static_cast<std::ptrdiff_t>(index));
      } else {
        ++index;
      }
    }
    if (std::none_of(symbols_.begin(), symbols_.end(),
                     [](const SymbolRuntime &item) {
                       return !item.quarantined && item.metadata_ready;
                     })) {
      error_ = "venue subscriptions produced no usable symbols";
      return false;
    }
    if (shard.requests.empty()) {
      shard.batches.clear();
      shard.batch_symbol_indices.clear();
      reset_subscription_cursor(shard);
      shard.all_subscribed = true;
      shard.state = MarketDataState::Live;
      shard.deadlines.mark_live();
      update_live_state();
      return true;
    }
    std::string build_error;
    if (!rebuild_subscription_batches(shard, true, build_error)) {
      error_ = build_error.empty()
                   ? "failed to rebuild isolated subscription batches"
                   : std::move(build_error);
      return false;
    }
    shard.subscription_isolation_mode = true;
    reset_subscription_cursor(shard);
    shard.state = MarketDataState::Subscribing;
    return true;
  }

  bool handle_subscription_rate_limit(
      WsShard &shard, const exchange::NormalizedEvent &event,
      std::string_view reason) {
    const auto found = std::find_if(
        shard.symbol_indices.begin(), shard.symbol_indices.end(),
        [&](std::size_t index) {
          return index < symbols_.size() &&
                 (symbols_[index].venue_symbol == event.symbol_view() ||
                  symbols_[index].options.symbol == event.symbol_view());
        });
    if (event.symbol_view().empty() ||
        found == shard.symbol_indices.end()) {
      schedule_reconnect(
          shard,
          "Bitget rate-limit rejection omitted a known subscription symbol");
      return false;
    }
    auto stream = event.subscription_stream;
    auto &symbol = symbols_[*found];
    if (stream == exchange::SubscriptionStream::Unknown) {
      if (symbol.options.ticker && !symbol.options.orderbook) {
        stream = exchange::SubscriptionStream::Ticker;
      } else if (!symbol.options.ticker && symbol.options.orderbook) {
        stream = exchange::SubscriptionStream::Orderbook;
      } else {
        schedule_reconnect(
            shard,
            "Bitget rate-limit rejection omitted the subscription channel");
        return false;
      }
    }
    if (!settle_subscription_acknowledgement(
             shard, event.symbol_view())
             .has_value()) {
      schedule_reconnect(
          shard,
          "Bitget rate-limit rejection did not match a pending acknowledgement");
      return false;
    }
    constexpr std::uint32_t maximum_retries = 5;
    ++metrics_.subscription_rate_limit_deferrals;
    ++symbol.subscription_rate_limit_retries;
    symbol.rate_limit_recovery_active = true;
    shard.all_subscribed = false;
    begin_recovery(shard, Clock::now());
    if (symbol.subscription_rate_limit_retries > maximum_retries) {
      symbol.resubscribe_pending = false;
      std::cerr << utc_timestamp() << ' '
                << exchange::venue_name(options_.venue)
                << " subscription rate-limit retries exhausted product="
                << exchange::product_name(options_.product)
                << " shard=" << shard.id
                << " symbol="
                << escape_diagnostic_bytes(symbol.options.symbol)
                << " retries=" << symbol.subscription_rate_limit_retries
                << " reason=" << escape_diagnostic_bytes(reason) << '\n';
      return true;
    }
    return queue_symbol_resubscribe(
        shard, symbol, Clock::now(),
        SymbolResubscribeMode::SubscribeOnly, stream);
  }

  bool dispatch_adapter_event(
      WsShard &shard,
      const exchange::NormalizedEvent &event,
      std::string_view parse_error) {
    metrics_.numeric_tail_normalizations += event.normalized_tail_fields;
    switch (event.type) {
    case exchange::AdapterEventType::SubscribeError:
      ++metrics_.subscription_rejections;
      if (shard.requires_login && shard.login_sent && !shard.login_acked) {
        schedule_reconnect(
            shard, parse_error.empty() ? "venue rejected public login"
                                       : parse_error);
        return false;
      }
      if (event.subscribe_error_kind ==
          exchange::SubscribeErrorKind::TransientRateLimit) {
        return handle_subscription_rate_limit(
            shard, event,
            parse_error.empty()
                ? std::string_view{"Bitget subscription rate limited"}
                : parse_error);
      }
      {
        std::size_t rejected = std::numeric_limits<std::size_t>::max();
        if (!event.symbol_view().empty()) {
          const auto found = std::find_if(
              shard.symbol_indices.begin(), shard.symbol_indices.end(),
              [&](std::size_t index) {
                return symbols_[index].venue_symbol == event.symbol_view() ||
                       symbols_[index].options.symbol == event.symbol_view();
              });
          if (found != shard.symbol_indices.end()) {
            rejected = *found;
          }
        }
        if (rejected == std::numeric_limits<std::size_t>::max() &&
            shard.next_batch < shard.batch_symbol_indices.size() &&
            shard.batch_symbol_indices[shard.next_batch] !=
                std::numeric_limits<std::size_t>::max()) {
          rejected = shard.batch_symbol_indices[shard.next_batch];
        }
        if (rejected == std::numeric_limits<std::size_t>::max() &&
            shard.requests.size() == 1U) {
          rejected = shard.symbol_indices.front();
        }
        const auto reason =
            parse_error.empty() ? std::string_view{"venue rejected subscription"}
                                : parse_error;
        if (rejected != std::numeric_limits<std::size_t>::max()) {
          if (!quarantine_subscription_symbol(shard, rejected, reason)) {
            schedule_reconnect(shard, error_.empty() ? reason : error_);
            return false;
          }
          if (shard.adapter->subscription_send_window() > 1U) {
            schedule_reconnect(
                shard,
                "restarting windowed subscriptions after rejection");
            return false;
          }
          return true;
        }
        std::string build_error;
        if (!rebuild_subscription_batches(shard, true, build_error)) {
          schedule_reconnect(
              shard, build_error.empty()
                         ? "failed to isolate rejected subscription batch"
                         : build_error);
          return false;
        }
        shard.subscription_isolation_mode = true;
        reset_subscription_cursor(shard);
        shard.state = MarketDataState::Subscribing;
        return true;
      }
    case exchange::AdapterEventType::SubscribeAck:
      if (shard.requires_login && shard.login_sent &&
          !shard.login_acked) {
        shard.login_acked = true;
        shard.pending_acknowledgements = 0;
        shard.awaiting_ack = false;
        shard.acknowledgement_deadline = {};
      } else if (shard.awaiting_ack) {
        const auto acknowledged_batch =
            settle_subscription_acknowledgement(
                shard, event.symbol_view());
        if (acknowledged_batch.has_value()) {
          complete_rate_limit_recovery(
              shard, *acknowledged_batch);
        }
        complete_subscriptions_if_ready(shard, Clock::now());
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
      return true;
    case exchange::AdapterEventType::Ignored:
      if (event.ignore_reason == exchange::IgnoreReason::OneSidedBook) {
        ++metrics_.one_sided_book_frames;
      }
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
      bbo.header.bbo_origin = utils::md::BboOrigin::TickerStream;
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
      fail(error_.empty() ? "failed to publish bridged orderbook image"
                          : error_);
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
      case ResyncReason::ScaleMismatch:
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
                                Clock::time_point now,
                                SymbolResubscribeMode mode =
                                    SymbolResubscribeMode::
                                        UnsubscribeThenSubscribe,
                                exchange::SubscriptionStream stream =
                                    exchange::SubscriptionStream::Unknown) {
    auto cooldown = std::chrono::seconds(1);
    if (mode == SymbolResubscribeMode::SubscribeOnly) {
      const auto exponent = std::min<std::uint32_t>(
          symbol.subscription_rate_limit_retries > 0
              ? symbol.subscription_rate_limit_retries - 1
              : 0,
          5);
      cooldown = std::chrono::seconds(1U << exponent);
    }
    auto not_before =
        mode == SymbolResubscribeMode::SubscribeOnly
            ? now + cooldown
            : now;
    if (symbol.last_resubscribe != Clock::time_point{}) {
      not_before = std::max(not_before, symbol.last_resubscribe + cooldown);
      if (not_before > now) {
        ++metrics_.cooldown_deferrals;
      }
    }
    if (symbol.resubscribe_pending) {
      symbol.resubscribe_not_before =
          std::max(symbol.resubscribe_not_before, not_before);
      if (mode == SymbolResubscribeMode::SubscribeOnly &&
          symbol.resubscribe_mode ==
              SymbolResubscribeMode::SubscribeOnly) {
        symbol.resubscribe_ticker =
            symbol.resubscribe_ticker ||
            stream == exchange::SubscriptionStream::Ticker;
        symbol.resubscribe_orderbook =
            symbol.resubscribe_orderbook ||
            stream == exchange::SubscriptionStream::Orderbook;
      }
      return true;
    }
    symbol.resubscribe_pending = true;
    symbol.resubscribe_not_before = not_before;
    symbol.resubscribe_mode = mode;
    symbol.resubscribe_ticker =
        mode == SymbolResubscribeMode::UnsubscribeThenSubscribe
            ? symbol.options.ticker
            : stream == exchange::SubscriptionStream::Ticker;
    symbol.resubscribe_orderbook =
        mode == SymbolResubscribeMode::UnsubscribeThenSubscribe
            ? symbol.options.orderbook
            : stream == exchange::SubscriptionStream::Orderbook;
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
          symbol.options.orderbook_channel, symbol.resubscribe_ticker,
          symbol.resubscribe_orderbook,
          symbol.options.update_interval_ms};
      std::vector<std::string> subscribe;
      std::vector<std::string> unsubscribe;
      std::string build_error;
      if (!shard.adapter->build_subscription_batches(
              std::span<const exchange::StreamRequest>(&request, 1),
              subscribe, build_error) ||
          subscribe.empty()) {
        fail(
            build_error.empty() ? "failed to build symbol resubscription"
                                : build_error);
        return false;
      }
      if (symbol.resubscribe_mode ==
              SymbolResubscribeMode::UnsubscribeThenSubscribe &&
          (!shard.adapter->build_unsubscription_batches(
               std::span<const exchange::StreamRequest>(&request, 1),
               unsubscribe, build_error) ||
           unsubscribe.size() != subscribe.size())) {
        fail(
            build_error.empty() ? "failed to build symbol unsubscription"
                                : build_error);
        return false;
      }
      shard.batches.clear();
      shard.batch_symbol_indices.clear();
      shard.next_batch = 0;
      shard.next_batch_to_send = 0;
      for (std::size_t index = 0; index < subscribe.size(); ++index) {
        if (symbol.resubscribe_mode ==
            SymbolResubscribeMode::UnsubscribeThenSubscribe) {
          shard.batches.push_back(std::move(unsubscribe[index]));
          shard.batch_symbol_indices.push_back(symbol_index);
        }
        shard.batches.push_back(std::move(subscribe[index]));
        shard.batch_symbol_indices.push_back(symbol_index);
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
    const auto snapshot = symbol.book_publisher->publish_snapshot(
        header, symbol.pipeline->book());
    if (!snapshot) {
      error_ = "failed to publish complete orderbook snapshot";
      if (!snapshot.message.empty()) {
        error_.append(": ");
        error_.append(snapshot.message);
      }
      return false;
    }
    if (!symbol.pipeline->book().Bbo(symbol.instrument.instrument_id)) {
      error_ = "failed to publish complete orderbook BBO: book has no "
               "two-sided top";
      return false;
    }
    if (!symbol.pipeline->PublishCanonical(header)) {
      error_ = "failed to publish complete orderbook BBO record";
      return false;
    }
    error_.clear();
    return true;
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
      const bool is_binance_compatible =
          options_.venue == Venue::Binance ||
          options_.venue == Venue::Aster;
      const bool retry_lagging_snapshot =
          is_binance_compatible ||
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
    if (!process_market_event(shard, *snapshot_event_, false) ||
        !symbol.image_ready) {
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
        fail(error_.empty() ? "failed to publish rebuilt orderbook image"
                            : error_);
        return;
      }
      symbol.book_live = true;
      update_live_state();
    }
  }

  std::optional<std::size_t> metadata_refresh_symbol_index(
      const exchange::StreamRequest &request) const noexcept {
    for (std::size_t index = 0; index < symbols_.size(); ++index) {
      const auto &symbol = symbols_[index];
      if (symbol.venue_symbol == request.venue_symbol ||
          symbol.options.symbol == request.canonical_symbol) {
        return index;
      }
    }
    return std::nullopt;
  }

  void handle_symbol_metadata(std::string_view json) {
    if (metadata_refresh_batch_index_ >=
        metadata_refresh_batches_.size()) {
      metadata_refresh_backoff(
          "unexpected symbol metadata response", Clock::now());
      return;
    }
    const auto status = metadata_http_.response().status_code();
    const auto action =
        classify_snapshot_http(options_.venue, status, json);
    if (action != SnapshotHttpAction::Parse) {
      std::string reason = "symbol metadata HTTP ";
      reason.append(std::to_string(status));
      if (!json.empty()) {
        reason.append(" body=");
        reason.append(json.substr(0, 256));
      }
      if (action == SnapshotHttpAction::CooldownVenue ||
          action == SnapshotHttpAction::BanCooldownVenue) {
        ++metrics_.metadata_refresh_rate_limits;
        auto delay = parse_retry_after(
            metadata_http_.response().header_value("retry-after"));
        if (delay == std::chrono::milliseconds{}) {
          delay = std::chrono::milliseconds(
              action == SnapshotHttpAction::BanCooldownVenue
                  ? options_.snapshot_ban_backoff_ms
                  : options_.snapshot_rate_limit_backoff_ms);
        }
        metadata_refresh_backoff(reason, Clock::now(), delay);
      } else {
        metadata_refresh_backoff(
            reason, Clock::now(), std::nullopt,
            action == SnapshotHttpAction::QuarantineSymbol);
      }
      return;
    }

    const auto &batch =
        metadata_refresh_batches_[metadata_refresh_batch_index_];
    if (batch.request_offset > metadata_refresh_requests_.size() ||
        batch.request_count >
            metadata_refresh_requests_.size() - batch.request_offset) {
      metadata_refresh_backoff(
          "invalid symbol metadata batch range", Clock::now());
      return;
    }
    const auto batch_requests =
        std::span<const exchange::StreamRequest>(
            metadata_refresh_requests_)
            .subspan(batch.request_offset, batch.request_count);
    auto validator = exchange::make_venue_adapter(
        options_.venue, options_.product, 20);
    if (!validator) {
      metadata_refresh_backoff(
          "failed to create symbol metadata validator", Clock::now());
      return;
    }
    std::vector<exchange::InstrumentMetadata> parsed;
    parsed.reserve(batch.request_count);
    std::string parse_error;
    if (!validator->upsert_metadata(
            json, batch_requests, parsed, parse_error)) {
      metadata_refresh_backoff(
          parse_error.empty()
              ? std::string_view(
                    "failed to parse symbol metadata refresh")
              : std::string_view(parse_error),
          Clock::now());
      return;
    }

    for (const auto &request : batch_requests) {
      const auto symbol_index =
          metadata_refresh_symbol_index(request);
      if (!symbol_index) {
        metadata_refresh_backoff(
            "metadata refresh references unknown symbol", Clock::now());
        return;
      }
      const auto &symbol = symbols_[*symbol_index];
      const auto found = std::find_if(
          parsed.begin(), parsed.end(),
          [&request](
              const exchange::InstrumentMetadata &entry) {
            return entry.venue_symbol == request.venue_symbol ||
                   entry.canonical_symbol ==
                       request.canonical_symbol;
          });
      std::string capacity_error;
      if (found == parsed.end() || found->tick_size <= 0 ||
          !metadata_fits_fixed_fields(
              symbol, *found, capacity_error)) {
        metadata_refresh_backoff(
            found == parsed.end()
                ? std::string_view(
                      "metadata refresh omitted requested symbol")
                : std::string_view(capacity_error),
            Clock::now());
        return;
      }
    }

    std::vector<exchange::InstrumentMetadata> ignored;
    if (!metadata_adapter().upsert_metadata(
            json, batch_requests, ignored, parse_error)) {
      metadata_refresh_backoff(
          parse_error.empty()
              ? std::string_view(
                    "failed to update authoritative adapter metadata")
              : std::string_view(parse_error),
          Clock::now());
      return;
    }

    for (auto &shard : ws_shards_) {
      std::vector<exchange::StreamRequest> shard_requests;
      for (const auto &request : batch_requests) {
        const auto symbol_index =
            metadata_refresh_symbol_index(request);
        if (symbol_index &&
            symbols_[*symbol_index].shard_id == shard.id) {
          shard_requests.push_back(request);
        }
      }
      if (shard_requests.empty()) {
        continue;
      }
      ignored.clear();
      std::string shard_error;
      if (!shard.adapter->upsert_metadata(
              json, shard_requests, ignored, shard_error)) {
        metadata_refresh_backoff(
            shard_error.empty()
                ? std::string_view(
                      "failed to update shard adapter metadata")
                : std::string_view(shard_error),
            Clock::now());
        return;
      }
    }

    for (const auto &request : batch_requests) {
      const auto symbol_index =
          *metadata_refresh_symbol_index(request);
      auto &symbol = symbols_[symbol_index];
      const auto found = std::find_if(
          parsed.begin(), parsed.end(),
          [&request](
              const exchange::InstrumentMetadata &entry) {
            return entry.venue_symbol == request.venue_symbol ||
                   entry.canonical_symbol ==
                       request.canonical_symbol;
          });
      if (!open_symbol(symbol, *found, false)) {
        metadata_refresh_backoff(
            error_.empty()
                ? std::string_view(
                      "failed to apply refreshed symbol metadata")
                : std::string_view(error_),
            Clock::now());
        return;
      }
      symbol.metadata_refresh_pending = false;
      symbol.metadata_refresh_quarantined = false;
      symbol.consecutive_metadata_refresh_failures = 0;
      symbol.metadata_refresh_not_before = {};
      symbol.scale_mismatch_seen = false;
      ++metrics_.metadata_refresh_successes;
      auto &shard = shard_for(symbol);
      if (!resync_symbol(
              shard, symbol, ResyncReason::ScaleMismatch,
              "symbol metadata scale refreshed")) {
        return;
      }
    }

    ++metadata_refresh_batch_index_;
    if (metadata_refresh_batch_index_ <
        metadata_refresh_batches_.size()) {
      if (!start_symbol_metadata_batch(Clock::now())) {
        metadata_refresh_backoff(
            metadata_http_.error_message().empty()
                ? std::string_view(
                      "failed to start next symbol metadata batch")
                : metadata_http_.error_message(),
            Clock::now());
      }
      return;
    }
    clear_active_metadata_refresh();
  }

  void reset_metadata_pagination() noexcept {
    metadata_page_count_ = 0;
    seen_metadata_cursors_.clear();
    metadata_bootstrap_started_ = {};
    metadata_bootstrap_paginated_ = false;
  }

  [[nodiscard]] bool paginated_bootstrap_active() const noexcept {
    return metadata_http_purpose_ != MetadataHttpPurpose::SymbolRefresh &&
           metadata_bootstrap_paginated_;
  }

  void current_pagination_cursor(bool &present,
                                 std::string_view &cursor) const noexcept {
    present = false;
    cursor = {};
    if (metadata_batch_index_ >= metadata_batches_.size()) {
      return;
    }
    const auto &batch = metadata_batches_[metadata_batch_index_];
    present = !batch.page_cursor.empty();
    cursor = batch.page_cursor;
  }

  void log_metadata_bootstrap_failure(std::string_view stage,
                                      std::string_view reason,
                                      bool cursor_present,
                                      std::string_view cursor) const {
    if (!metadata_bootstrap_paginated_) {
      return;
    }
    std::cerr << utc_timestamp() << ' '
              << exchange::venue_name(options_.venue)
              << " metadata bootstrap failed product="
              << exchange::product_name(options_.product)
              << " page=" << metadata_page_count_
              << " cursor_present=" << (cursor_present ? 1 : 0);
    if (cursor_present) {
      std::cerr << " cursor_hash=" << truncated_cursor_hash(cursor);
    }
    std::cerr << " requested_symbols=" << symbols_.size()
              << " matched_symbols=" << metadata_.size()
              << " stage=" << stage
              << " reason=" << escape_diagnostic_bytes(reason) << '\n';
  }

  void log_metadata_bootstrap_success() const {
    if (!metadata_bootstrap_paginated_) {
      return;
    }
    const auto elapsed =
        std::chrono::duration_cast<std::chrono::milliseconds>(
            Clock::now() - metadata_bootstrap_started_)
            .count();
    std::cerr << utc_timestamp() << ' '
              << exchange::venue_name(options_.venue)
              << " metadata bootstrap completed product="
              << exchange::product_name(options_.product)
              << " mode=bulk_paginated"
              << " requested_symbols=" << symbols_.size()
              << " pages=" << metadata_page_count_
              << " matched_symbols=" << metadata_.size()
              << " elapsed_ms=" << elapsed << '\n';
  }

  void fail_paginated_metadata(std::string_view stage, std::string reason,
                               bool cursor_present, std::string_view cursor) {
    log_metadata_bootstrap_failure(stage, reason, cursor_present, cursor);
    fail(reason);
  }

  void fail_metadata_response(std::string_view stage, std::string reason) {
    if (!paginated_bootstrap_active()) {
      fail(reason);
      return;
    }
    bool cursor_present = false;
    std::string_view cursor;
    current_pagination_cursor(cursor_present, cursor);
    fail_paginated_metadata(stage, std::move(reason), cursor_present, cursor);
  }

  bool accept_paginated_metadata_page(
      const std::vector<exchange::InstrumentMetadata> &parsed) {
    for (std::size_t index = 0; index < parsed.size(); ++index) {
      const auto &symbol = parsed[index].venue_symbol;
      for (std::size_t prior = 0; prior < index; ++prior) {
        if (parsed[prior].venue_symbol == symbol) {
          fail_paginated_metadata(
              "duplicate_symbol",
              "Bybit metadata pagination duplicated symbol: " + symbol,
              false, {});
          return false;
        }
      }
      for (const auto &existing : metadata_) {
        if (existing.venue_symbol == symbol) {
          fail_paginated_metadata(
              "duplicate_symbol",
              "Bybit metadata pagination duplicated symbol: " + symbol,
              false, {});
          return false;
        }
      }
    }
    return true;
  }

  bool finish_or_continue_metadata_pagination(std::string_view json) {
    auto &batch = metadata_batches_[metadata_batch_index_];
    std::string cursor;
    std::string cursor_error;
    if (!metadata_adapter().discovery_metadata_next_cursor(
            json, cursor, cursor_error)) {
      fail_paginated_metadata(
          "cursor",
          cursor_error.empty() ? std::string("failed to parse nextPageCursor")
                               : std::move(cursor_error),
          false, {});
      return false;
    }
    bool list_empty = false;
    std::string list_error;
    if (!metadata_adapter().metadata_page_list_empty(json, list_empty,
                                                     list_error)) {
      fail_paginated_metadata(
          "page_list",
          list_error.empty() ? std::string("failed to inspect metadata page")
                             : std::move(list_error),
          !cursor.empty(), cursor);
      return false;
    }
    if (list_empty && !cursor.empty()) {
      fail_paginated_metadata(
          "empty_page",
          "Bybit metadata pagination returned an empty page before "
          "pagination completed",
          true, cursor);
      return false;
    }
    if (cursor.empty()) {
      return true;
    }
    if (!seen_metadata_cursors_.insert(cursor).second) {
      fail_paginated_metadata(
          "cursor", "Bybit metadata pagination cursor repeated", true, cursor);
      return false;
    }
    if (!metadata_adapter().apply_metadata_page_cursor(batch, cursor,
                                                       cursor_error)) {
      fail_paginated_metadata(
          "cursor",
          cursor_error.empty()
              ? std::string("failed to apply metadata page cursor")
              : std::move(cursor_error),
          true, cursor);
      return false;
    }
    if (!start_metadata_batch(Clock::now())) {
      fail_paginated_metadata(
          "page_limit",
          error_.empty() ? std::string("failed to start next metadata page")
                         : error_,
          true, cursor);
      return false;
    }
    return false;
  }

  void handle_metadata(std::string_view json) {
    if (metadata_http_purpose_ ==
        MetadataHttpPurpose::SymbolRefresh) {
      handle_symbol_metadata(json);
      return;
    }
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
    if (metadata_http_.response().status_class() !=
        net::HttpStatusClass::Success) {
      std::string reason{"HTTP "};
      reason.append(
          std::to_string(metadata_http_.response().status_code()));
      fail_metadata_response("http", std::string("metadata: ") + reason);
      return;
    }
    auto outcome = metadata_adapter().parse_metadata_response(
        json, batch_requests, parsed);
    if (outcome.kind ==
        exchange::MetadataResponseKind::ConnectionFailure) {
      fail_metadata_response(
          "parse",
          std::string("metadata: ") +
              (outcome.reason.empty()
                   ? "failed to parse venue metadata"
                   : outcome.reason));
      return;
    }
    const bool symbol_unavailable =
        outcome.kind ==
        exchange::MetadataResponseKind::SymbolUnavailable;
    if (symbol_unavailable) {
      if (batch.request_count != 1) {
        fail_metadata_response(
            "parse",
            "metadata adapter returned symbol unavailability for "
            "a multi-symbol batch");
        return;
      }
      auto &symbol = symbols_[batch.request_offset];
      if (!symbol.quarantined) {
        symbol.quarantined = true;
        ++metrics_.metadata_symbol_quarantines;
      }
      std::cerr
          << utc_timestamp() << ' '
          << exchange::venue_name(options_.venue)
          << " metadata symbol quarantined product="
          << exchange::product_name(options_.product)
          << " symbol="
          << escape_diagnostic_bytes(symbol.options.symbol)
          << " reason="
          << escape_diagnostic_bytes(
                 outcome.reason.empty()
                     ? std::string_view("symbol unavailable")
                     : std::string_view(outcome.reason))
          << '\n';
    } else {
      if (batch.cursor_paginated) {
        std::string repeated;
        std::string repeated_error;
        if (!metadata_adapter().metadata_page_repeated_venue_symbol(
                json, repeated, repeated_error)) {
          fail_paginated_metadata(
              "duplicate_symbol",
              repeated_error.empty()
                  ? std::string("failed to inspect metadata page symbols")
                  : std::move(repeated_error),
              false, {});
          return;
        }
        if (!repeated.empty()) {
          fail_paginated_metadata(
              "duplicate_symbol",
              "Bybit metadata pagination duplicated symbol: " + repeated,
              false, {});
          return;
        }
        if (!accept_paginated_metadata_page(parsed)) {
          return;
        }
      }
      for (auto &entry : parsed) {
        metadata_.push_back(std::move(entry));
      }
    }
    if (!symbol_unavailable &&
        options_.venue != Venue::Polymarket) {
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
        if (!shard.adapter->upsert_metadata(
                json, shard_batch_requests, shard_metadata, shard_error)) {
          fail_metadata_response(
              "upsert",
              "ws shard " + std::to_string(shard.id) +
                  ": failed to initialize adapter metadata: " +
                  shard_error);
          return;
        }
      }
    }
    if (!symbol_unavailable && batch.cursor_paginated) {
      if (!finish_or_continue_metadata_pagination(json)) {
        return;
      }
    }
    ++metadata_batch_index_;
    if (metadata_batch_index_ < metadata_batches_.size()) {
      if (!start_metadata_batch(Clock::now())) {
        fail_metadata_response("request", error_);
      }
      return;
    }
    std::vector<std::string_view> quarantine_examples;
    quarantine_examples.reserve(3);
    for (auto &symbol : symbols_) {
      const auto found = std::find_if(
          metadata_.begin(), metadata_.end(),
          [&symbol](const exchange::InstrumentMetadata &entry) {
            return entry.canonical_symbol == symbol.options.symbol ||
                   entry.venue_symbol == symbol.venue_symbol;
          });
      if (found == metadata_.end() || found->tick_size <= 0 ||
          found->venue_symbol.empty()) {
        if (!symbol.quarantined) {
          symbol.quarantined = true;
          ++metrics_.metadata_symbol_quarantines;
          if (quarantine_examples.size() < 3U) {
            quarantine_examples.push_back(symbol.options.symbol);
          }
        }
        continue;
      }
      symbol.venue_symbol = found->venue_symbol;
      if ((options_.venue == Venue::Hyperliquid ||
           options_.venue == Venue::Lighter) &&
          !found->canonical_symbol.empty()) {
        symbol.options.symbol = found->canonical_symbol;
      }
      std::string capacity_error;
      if (!metadata_fits_fixed_fields(symbol, *found, capacity_error)) {
        symbol.quarantined = true;
        ++metrics_.metadata_symbol_quarantines;
        if (quarantine_examples.size() < 3U) {
          quarantine_examples.push_back(symbol.options.symbol);
        }
        continue;
      }
      if (!open_symbol(symbol, *found)) {
        fail_metadata_response("open_symbol", error_);
        return;
      }
      symbol.metadata_refresh_pending = false;
      symbol.metadata_refresh_quarantined = false;
      symbol.consecutive_metadata_refresh_failures = 0;
      symbol.metadata_refresh_not_before = {};
      symbol.scale_mismatch_seen = false;
    }
    if (metrics_.metadata_symbol_quarantines != 0) {
      std::cerr << utc_timestamp() << ' '
                << exchange::venue_name(options_.venue)
                << " metadata symbols quarantined product="
                << exchange::product_name(options_.product)
                << " count=" << metrics_.metadata_symbol_quarantines;
      for (const auto example : quarantine_examples) {
        std::cerr << " example=" << escape_diagnostic_bytes(example);
      }
      std::cerr << '\n';
    }
    if (std::none_of(symbols_.begin(), symbols_.end(),
                     [](const SymbolRuntime &symbol) {
                       return !symbol.quarantined && symbol.metadata_ready;
                     })) {
      fail_metadata_response(
          "complete", "venue metadata produced no usable symbols");
      return;
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
      std::erase_if(shard.symbol_indices, [this](std::size_t symbol_index) {
        return symbols_[symbol_index].quarantined;
      });
      shard.requests.clear();
      shard.requests.reserve(shard.symbol_indices.size());
      if (shard.symbol_indices.empty()) {
        shard.batches.clear();
        shard.next_batch = 0;
        shard.next_batch_to_send = 0;
        shard.pending_acknowledgements = 0;
        shard.awaiting_ack = false;
        shard.all_subscribed = true;
        shard.state = MarketDataState::Live;
        shard.deadlines.mark_live();
        continue;
      }
      for (const auto symbol_index : shard.symbol_indices) {
        shard.requests.push_back(requests_[symbol_index]);
      }
      if (!rebuild_subscription_batches(shard, false, error_)) {
        fail_metadata_response(
            "subscribe",
            error_.empty()
                ? "venue produced no subscription batches for shard " +
                      std::to_string(shard.id)
                : "ws shard " + std::to_string(shard.id) + ": " + error_);
        return;
      }
      shard.next_batch = 0;
      shard.next_batch_to_send = 0;
      shard.pending_acknowledgements = 0;
      shard.awaiting_ack = false;
      shard.all_subscribed = false;
      shard.subscription_isolation_mode = false;
    }
    log_metadata_bootstrap_success();
    metadata_batches_.clear();
    metadata_.clear();
    metadata_batch_index_ = 0;
    reset_metadata_pagination();
    metadata_refresh_queue_.clear();
    metadata_refresh_active_symbols_.clear();
    metadata_refresh_requests_.clear();
    metadata_refresh_batches_.clear();
    metadata_refresh_batch_index_ = 0;
    metadata_http_purpose_ = MetadataHttpPurpose::Idle;
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

  bool metadata_fits_fixed_fields(
      const SymbolRuntime &symbol,
      const exchange::InstrumentMetadata &metadata,
      std::string &reason) const {
    const auto fits = [](std::size_t capacity, std::string_view value) {
      return value.size() < capacity;
    };
    if (!exchange::valid_utf8_symbol(symbol.canonical_identity.empty()
                                         ? symbol.options.symbol
                                         : symbol.canonical_identity) ||
        !exchange::valid_utf8_symbol(symbol.venue_symbol)) {
      reason = "symbol is invalid UTF-8 or exceeds 31-byte wire capacity";
      return false;
    }
    if (!fits(symbol.instrument.base_asset.size(), metadata.base_asset) ||
        !fits(symbol.instrument.quote_asset.size(), metadata.quote_asset) ||
        !fits(symbol.instrument.settle_asset.size(), metadata.settle_asset)) {
      reason = "asset exceeds 15-byte fixed capacity";
      return false;
    }
    const auto key = instrument::InstrumentManager::canonical_key(
        options_.venue, options_.product, symbol.options.symbol);
    if (!fits(symbol.instrument.instrument_key.size(), key)) {
      reason = "instrument key exceeds fixed capacity";
      return false;
    }
    return true;
  }

  bool open_symbol(SymbolRuntime &symbol,
                   const exchange::InstrumentMetadata &metadata,
                   bool publish_catalog = true) {
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
    const auto key = instrument::InstrumentManager::canonical_key(
        options_.venue, options_.product, symbol.canonical_identity);
    if (!copy_text(symbol.instrument.base_asset, metadata.base_asset) ||
        !copy_text(symbol.instrument.quote_asset, metadata.quote_asset) ||
        !copy_text(symbol.instrument.settle_asset, metadata.settle_asset) ||
        !copy_text(symbol.instrument.canonical_symbol,
                   symbol.canonical_identity) ||
        !copy_text(symbol.instrument.venue_symbol, symbol.venue_symbol) ||
        !copy_text(symbol.instrument.instrument_key, key)) {
      error_ = "instrument metadata exceeds fixed wire capacity";
      return false;
    }

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
    if (!copy_text(catalog.base_asset, metadata.base_asset) ||
        !copy_text(catalog.quote_asset, metadata.quote_asset) ||
        !copy_text(catalog.settle_asset, metadata.settle_asset) ||
        !copy_text(catalog.canonical_symbol, symbol.canonical_identity) ||
        !copy_text(catalog.venue_symbol, symbol.venue_symbol)) {
      error_ = "instrument catalog metadata exceeds fixed wire capacity";
      return false;
    }
    if (auto *poly = polymarket()) {
      if (const auto *market = poly->resolved_market(
              exchange::polymarket::ResolveSlot::Current)) {
        const auto outcome =
            symbol.options.polymarket_outcome == "DOWN"
                ? exchange::polymarket::Outcome::Down
                : exchange::polymarket::Outcome::Up;
        if (!copy_text(catalog.venue_symbol, market->token(outcome)) ||
            !copy_text(catalog.market_slug, market->slug.view()) ||
            !copy_text(catalog.condition_id, market->condition_id.view()) ||
            !copy_text(catalog.outcome, symbol.options.polymarket_outcome)) {
          error_ = "Polymarket catalog metadata exceeds fixed wire capacity";
          return false;
        }
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
    if (publish_catalog) {
      (void)publish_catalog_barrier(symbol);
    }
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
    shard.next_batch_to_send = 0;
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
      if (metadata_http_purpose_ ==
          MetadataHttpPurpose::SymbolRefresh) {
        metadata_refresh_backoff(
            metadata_http_.error_message(), now);
      } else {
        fail_metadata_response(
            "http",
            std::string("metadata HTTP: ") +
                std::string(metadata_http_.error_message()));
        return;
      }
    }
    if (snapshot_active_ && terminal(snapshot_http_.state())) {
      snapshot_backoff(snapshot_http_.error_message(), now);
    }
    if (discovery_active_ && terminal(discovery_http_.state())) {
      discovery_failed(discovery_http_.error_message());
    }
    drive_symbol_metadata_refresh(now);
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
        schedule_terminal_reconnect(shard, now);
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
        drive_heartbeat(shard, now);
        drive_outbound(shard, now);
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

  bool allow_client_message(WsShard &shard,
                            Clock::time_point now) {
    if (!options_.client_message_budget) {
      return true;
    }
    if (options_.client_message_budget->allow(
            now, options_.client_message_limit_per_minute)) {
      return true;
    }
    ++metrics_.global_budget_deferrals;
    shard.next_budget_retry =
        options_.client_message_budget->retry_at(
            now, options_.client_message_limit_per_minute);
    return false;
  }

  void record_client_message(Clock::time_point now) noexcept {
    if (options_.client_message_budget) {
      options_.client_message_budget->record(now);
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
        outbound_send_spacing(options_.venue, options_.product);
    if (shard.last_data_send != Clock::time_point{} &&
        now - shard.last_data_send < spacing) {
      return;
    }
    std::string_view send_error;
    if (shard.requires_login && !shard.login_sent) {
      if (!allow_client_message(shard, now)) {
        return;
      }
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
      record_client_message(now);
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
    const auto send_window =
        shard.subscription_isolation_mode
            ? std::size_t{1}
            : std::max<std::size_t>(
                  1, shard.adapter->subscription_send_window());
    if (shard.batch_pending_acknowledgements.size() !=
        shard.batches.size()) {
      shard.batch_pending_acknowledgements.assign(
          shard.batches.size(), 0);
    } else if (shard.next_batch_to_send == 0 &&
               shard.pending_acknowledgements == 0) {
      std::fill(shard.batch_pending_acknowledgements.begin(),
                shard.batch_pending_acknowledgements.end(), 0);
    }
    if (shard.next_batch_to_send < shard.batches.size() &&
        shard.next_batch_to_send - shard.next_batch <
            send_window) {
      if (!allow_client_message(shard, now)) {
        return;
      }
      if (!shard.budget.allow(now, subscription_limit())) {
        ++metrics_.budget_deferrals;
        shard.next_budget_retry =
            shard.budget.retry_at(now, subscription_limit());
        return;
      }
      const auto &batch =
          shard.batches[shard.next_batch_to_send];
      const auto bytes = std::span<const std::byte>(
          reinterpret_cast<const std::byte *>(batch.data()), batch.size());
      if (!shard.websocket->send(
              net::WsOpcode::Text, bytes, send_error)) {
        schedule_reconnect(shard, send_error, now);
        return;
      }
      shard.budget.record(now);
      record_client_message(now);
      shard.next_budget_retry = {};
      const auto acknowledgements =
          shard.adapter->expected_subscription_acks(batch);
      const bool was_awaiting = shard.awaiting_ack;
      shard.pending_acknowledgements += acknowledgements;
      shard.batch_pending_acknowledgements
          [shard.next_batch_to_send] = acknowledgements;
      shard.awaiting_ack = shard.pending_acknowledgements != 0;
      ++shard.next_batch_to_send;
      if (acknowledgements == 0) {
        while (shard.next_batch < shard.next_batch_to_send &&
               shard.batch_pending_acknowledgements[shard.next_batch] ==
                   0) {
          ++shard.next_batch;
        }
      } else if (!was_awaiting) {
        shard.acknowledgement_deadline =
            now + options_.request_timeout;
      }
      if (!shard.awaiting_ack) {
        shard.acknowledgement_deadline = {};
      }
      shard.last_data_send = now;
      ++metrics_.subscription_requests;
      complete_subscriptions_if_ready(shard, now);
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
        now - shard.last_data_send <
            outbound_send_spacing(options_.venue, options_.product)) {
      return;
    }
    if (!shard.websocket->can_send_data() &&
        shard.heartbeat.kind != exchange::HeartbeatKind::Rfc6455Ping) {
      return;
    }
    if (shard.heartbeat.kind !=
            exchange::HeartbeatKind::Rfc6455Ping &&
        !allow_client_message(shard, now)) {
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
      record_client_message(now);
      shard.last_data_send = now;
    }
    shard.last_heartbeat = now;
  }

  bool symbol_ready(const SymbolRuntime &symbol) const noexcept {
    return RecoveryStreamReady(
        symbol.options.ticker, symbol.ticker_live,
        symbol.options.ticker_requires_first_data,
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
                          Clock::time_point now = Clock::now(),
                          ReconnectKind kind = ReconnectKind::Failure) {
    if (!started_ || state_ == MarketDataState::Failed) {
      return;
    }
    if (shard.state == MarketDataState::ReconnectWait) {
      return;
    }
    error_ = "ws shard " + std::to_string(shard.id) + ": ";
    error_.append(reason);
    shard.pending_reconnect_reason.clear();
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
    shard.next_batch_to_send = 0;
    shard.pending_acknowledgements = 0;
    shard.awaiting_ack = false;
    shard.all_subscribed = false;
    shard.deadlines.reset_connection();
    shard.acknowledgement_deadline = {};
    shard.last_data_send = {};
    std::string build_error;
    if (!rebuild_subscription_batches(
            shard, shard.subscription_isolation_mode, build_error)) {
      fail(build_error.empty() ? "failed to rebuild subscriptions"
                               : build_error);
      return;
    }
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
      symbol.rate_limit_recovery_active = false;
      symbol.subscription_rate_limit_retries = 0;
      symbol.resubscribe_ticker = false;
      symbol.resubscribe_orderbook = false;
      symbol.resubscribe_mode =
          SymbolResubscribeMode::UnsubscribeThenSubscribe;
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
    std::chrono::milliseconds delay{};
    if (kind == ReconnectKind::ServerExpiration) {
      ++metrics_.server_expirations;
      const auto mixed =
          (static_cast<std::uint64_t>(shard.id + 1) *
               0x9e3779b97f4a7c15ULL) ^
          (static_cast<std::uint64_t>(metrics_.server_expirations) *
               0xbf58476d1ce4e5b9ULL);
      delay = std::chrono::milliseconds(
          200 + static_cast<std::int64_t>(mixed % 601U));
    } else {
      const auto exponent =
          std::min<std::uint32_t>(shard.reconnect_attempt++, 10);
      delay = options_.reconnect_base * (1U << exponent);
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
            static_cast<std::int64_t>(
                mixed % static_cast<std::uint64_t>(range)));
        delay = std::min(options_.reconnect_max, delay + jitter);
      }
    }
    shard.reconnect_at = now + delay;
    shard.state = MarketDataState::ReconnectWait;
    std::cerr << utc_timestamp() << ' '
              << exchange::venue_name(options_.venue)
              << " websocket reconnect scheduled product="
              << exchange::product_name(options_.product)
              << " shard=" << shard.id
              << " attempt=" << shard.reconnect_attempt
              << " reconnect_kind="
              << (kind == ReconnectKind::ServerExpiration
                      ? "server_expiration"
                      : "failure")
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
      shard.next_batch_to_send = 0;
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
    reset_metadata_pagination();
    metadata_refresh_queue_.clear();
    metadata_refresh_active_symbols_.clear();
    metadata_refresh_requests_.clear();
    metadata_refresh_batches_.clear();
    metadata_refresh_batch_index_ = 0;
    metadata_http_purpose_ = MetadataHttpPurpose::Idle;
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
        symbol.resubscribe_pending = false;
        symbol.resubscribe_not_before = {};
        symbol.rate_limit_recovery_active = false;
        symbol.subscription_rate_limit_retries = 0;
        symbol.resubscribe_ticker = false;
        symbol.resubscribe_orderbook = false;
        symbol.resubscribe_mode =
            SymbolResubscribeMode::UnsubscribeThenSubscribe;
        symbol.metadata_refresh_pending = false;
        symbol.metadata_refresh_quarantined = false;
        symbol.consecutive_metadata_refresh_failures = 0;
        symbol.metadata_refresh_not_before = {};
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
    metadata_http_purpose_ = MetadataHttpPurpose::Startup;
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
    reset_metadata_pagination();
    metadata_refresh_queue_.clear();
    metadata_refresh_active_symbols_.clear();
    metadata_refresh_requests_.clear();
    metadata_refresh_batches_.clear();
    metadata_refresh_batch_index_ = 0;
    metadata_http_purpose_ = MetadataHttpPurpose::Idle;
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
  std::set<std::string> seen_metadata_cursors_;
  std::vector<std::size_t> metadata_refresh_queue_;
  std::vector<std::size_t> metadata_refresh_active_symbols_;
  std::vector<exchange::StreamRequest> metadata_refresh_requests_;
  std::vector<exchange::MetadataRequestBatch>
      metadata_refresh_batches_;
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
  Clock::time_point metadata_bootstrap_started_{};
  std::size_t metadata_batch_index_{};
  std::size_t metadata_page_count_{};
  std::size_t metadata_refresh_batch_index_{};
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
  MetadataHttpPurpose metadata_http_purpose_{
      MetadataHttpPurpose::Idle};
  bool metadata_ready_{};
  bool metadata_bootstrap_paginated_{};
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

void VenueConnection::adopt_multiplex_publishers(
    VenueConnection &source) noexcept {
  impl_->adopt_multiplex_publishers(*source.impl_);
}

VenueConnectionManager::VenueConnectionManager() {
  std::string error;
  tls_ = net::make_client_ssl_context(error);
}

VenueConnectionManager::VenueConnectionManager(
    net::SharedSslContext tls)
    : tls_(std::move(tls)) {}

api::Result<void> VenueConnectionManager::prepare_options(
    VenueConnectionOptions &options) {
  if (!tls_) {
    return {.error = api::ErrorCode::InternalError,
            .message = "TLS context is unavailable"};
  }
  if (!options.instrument_manager) {
    options.instrument_manager = instrument_manager_;
  }
  if (options.venue == Venue::Lighter) {
    const auto limit = options.client_message_limit_per_minute;
    if (limit == 0 ||
        limit > ClientMessageBudget::kMaximumLimit) {
      return {.error = api::ErrorCode::InvalidConfig,
              .message = "invalid Lighter client message limit"};
    }
    if (lighter_message_limit_ != 0 &&
        lighter_message_limit_ != limit) {
      return {.error = api::ErrorCode::InvalidConfig,
              .message =
                  "Lighter connections must share one client message limit"};
    }
    if (!lighter_message_budget_) {
      lighter_message_budget_ =
          std::make_shared<ClientMessageBudget>();
      lighter_message_limit_ = limit;
    }
    options.client_message_budget = lighter_message_budget_;
    if (!lighter_connection_budget_) {
      lighter_connection_budget_ =
          std::make_shared<ConnectionAttemptBudget>();
    }
    options.connection_attempt_budget = lighter_connection_budget_;
  }
  return {};
}

api::Result<VenueConnection *> VenueConnectionManager::create(
    VenueConnectionOptions options, bool start_immediately) {
  const auto prepared = prepare_options(options);
  if (!prepared) {
    return {.value = nullptr, .error = prepared.error,
            .message = prepared.message};
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

api::Result<VenueConnection *> VenueConnectionManager::replace(
    VenueConnection *current, VenueConnectionOptions options,
    bool start_immediately) {
  const auto found = std::find_if(
      connections_.begin(), connections_.end(),
      [current](const auto &entry) { return entry.get() == current; });
  if (found == connections_.end()) {
    return {.value = nullptr,
            .error = api::ErrorCode::InvalidHandle,
            .message = "venue connection replacement target is unavailable"};
  }
  const auto prepared = prepare_options(options);
  if (!prepared) {
    return {.value = nullptr, .error = prepared.error,
            .message = prepared.message};
  }
  std::unique_ptr<VenueConnection> replacement;
  try {
    replacement =
        std::make_unique<VenueConnection>(loop_, tls_, std::move(options));
  } catch (const std::exception &exception) {
    return {.value = nullptr,
            .error = api::ErrorCode::InternalError,
            .message = std::string("failed to construct replacement: ") +
                       exception.what()};
  } catch (...) {
    return {.value = nullptr,
            .error = api::ErrorCode::InternalError,
            .message = "failed to construct replacement"};
  }
  auto *pointer = replacement.get();
  current->stop();
  replacement->adopt_multiplex_publishers(*current);
  *found = std::move(replacement);
  if (start_immediately) {
    const auto started = pointer->start();
    if (!started) {
      return {.value = pointer, .error = started.error,
              .message = started.message};
    }
  }
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
