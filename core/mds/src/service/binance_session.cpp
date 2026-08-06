#include "mds/service/binance_session.h"

#include <algorithm>
#include <array>
#include <cctype>
#include <cstring>
#include <limits>
#include <random>
#include <span>
#include <utility>

namespace mds::service {
namespace {

using Profile = exchange::binance::Profile;
using SessionState = BinanceSessionState;

std::uint64_t clock_ticks() noexcept {
  return static_cast<std::uint64_t>(
      BinanceSession::Clock::now().time_since_epoch().count());
}

std::string profile_name(Profile profile) {
  return profile == Profile::Spot ? "spot" : "usdm";
}

std::string lowercase(std::string value) {
  std::transform(value.begin(), value.end(), value.begin(),
                 [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
  return value;
}

bool same_symbol(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t index = 0; index < left.size(); ++index) {
    if (std::toupper(static_cast<unsigned char>(left[index])) !=
        std::toupper(static_cast<unsigned char>(right[index]))) {
      return false;
    }
  }
  return true;
}

BinanceSession::Endpoint parse_endpoint(std::string_view configured,
                                        std::string_view fallback_host) {
  BinanceSession::Endpoint result;
  std::string_view authority = configured;
  if (const auto scheme = authority.find("://"); scheme != std::string_view::npos) {
    authority.remove_prefix(scheme + 3U);
  }
  if (const auto slash = authority.find('/'); slash != std::string_view::npos) {
    authority = authority.substr(0, slash);
  }
  if (authority.empty()) {
    authority = fallback_host;
  }
  if (const auto colon = authority.rfind(':'); colon != std::string_view::npos &&
      authority.find(':') == colon) {
    result.host.assign(authority.substr(0, colon));
    result.service.assign(authority.substr(colon + 1U));
  } else {
    result.host.assign(authority);
  }
  return result;
}

template <std::size_t N>
void copy_text(std::array<char, N> &destination, std::string_view source) {
  const auto count = std::min(source.size(), N - 1U);
  std::memcpy(destination.data(), source.data(), count);
  destination[count] = '\0';
}

utils::md::EventHeader make_header(std::uint32_t generation,
                                   std::uint64_t source_sequence,
                                   std::uint64_t bus_sequence,
                                   std::uint64_t exchange_time_ms,
                                   utils::md::BookState state) noexcept {
  utils::md::EventHeader header{};
  header.instrument_id = 1;
  header.book_generation = generation;
  header.source_seq = source_sequence;
  header.bus_seq = bus_sequence;
  header.exchange_ts_ns =
      exchange_time_ms > std::numeric_limits<std::uint64_t>::max() / 1'000'000ULL
          ? 0
          : exchange_time_ms * 1'000'000ULL;
  header.receive_tsc = clock_ticks();
  header.publish_tsc = header.receive_tsc;
  header.state = state;
  header.source_id = 1;
  return header;
}

std::vector<utils::md::Level>
convert_levels(const std::vector<exchange::binance::PriceLevel> &levels) {
  std::vector<utils::md::Level> converted;
  converted.reserve(levels.size());
  for (const auto &level : levels) {
    converted.push_back({level.price, level.quantity});
  }
  return converted;
}

bool terminal(network::WebSocketClientState state) noexcept {
  return state == network::WebSocketClientState::Closed ||
         state == network::WebSocketClientState::TimedOut ||
         state == network::WebSocketClientState::Failed;
}

bool terminal(network::HttpClientState state) noexcept {
  return state == network::HttpClientState::TimedOut ||
         state == network::HttpClientState::Failed;
}

bool connecting(SessionState state) noexcept {
  return state == SessionState::Resolving || state == SessionState::Tcp ||
         state == SessionState::Tls || state == SessionState::Upgrade;
}

} // namespace

std::string_view to_string(BinanceSessionState state) noexcept {
  switch (state) {
  case SessionState::Resolving:
    return "Resolving";
  case SessionState::Tcp:
    return "TCP";
  case SessionState::Tls:
    return "TLS";
  case SessionState::Upgrade:
    return "Upgrade";
  case SessionState::Buffering:
    return "Buffering";
  case SessionState::Metadata:
    return "Metadata";
  case SessionState::Snapshot:
    return "Snapshot";
  case SessionState::Bridging:
    return "Bridging";
  case SessionState::Live:
    return "Live";
  case SessionState::Resync:
    return "Resync";
  case SessionState::ReconnectWait:
    return "ReconnectWait";
  case SessionState::Stopped:
    return "Stopped";
  case SessionState::Failed:
    return "Failed";
  }
  return "Unknown";
}

BinanceSession::BinanceSession(network::EpollLoop &loop,
                               network::SharedSslContext tls,
                               BinanceSessionOptions options)
    : loop_(loop), tls_(std::move(tls)), options_(std::move(options)),
      websocket_(tls_), metadata_http_(tls_), snapshot_http_(tls_),
      combined_parser_(), rest_parser_(),
      synchronizer_(options_.profile, options_.max_buffered_updates),
      order_book_(std::make_unique<utils::md::OrderBook>(
          options_.ladder_levels_per_side)),
      overlay_(options_.profile == Profile::Spot
                   ? book::SequenceDomain::Shared
                   : book::SequenceDomain::Independent) {
  websocket_.set_frame_callback([this](const network::WsFrameView &frame) {
    if (frame.opcode != network::WsOpcode::Text) {
      return true;
    }
    last_message_ = Clock::now();
    const std::string_view message(
        reinterpret_cast<const char *>(frame.payload.data()), frame.payload.size());
    std::string parse_error;
    if (!ingest_websocket_message(message, parse_error)) {
      ++metrics_.parse_errors;
      error_ = std::move(parse_error);
      return false;
    }
    return true;
  });
}

BinanceSession::~BinanceSession() { stop(); }

api::Result<void> BinanceSession::start(Clock::time_point now) {
  if (started_) {
    return {.error = api::ErrorCode::AlreadyInitialized,
            .message = "Binance session already started"};
  }
  if (options_.symbol.empty() || options_.ladder_levels_per_side == 0 ||
      options_.ladder_levels_per_side > options_.max_ladder_levels_per_side ||
      options_.max_ladder_levels_per_side > utils::md::kMaxLadderLevels) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = "invalid Binance symbol or ladder bounds"};
  }
  options_.symbol = lowercase(options_.symbol);
  std::transform(options_.symbol.begin(), options_.symbol.end(),
                 options_.symbol.begin(), [](unsigned char c) {
                   return static_cast<char>(std::toupper(c));
                 });
  std::string start_error;
  if (!exchange::binance::build_combined_stream_path(
          options_.profile, options_.symbol, true, true, websocket_target_,
          start_error, "100ms") ||
      !open_publishers(start_error)) {
    state_ = SessionState::Failed;
    error_ = std::move(start_error);
    return {.error = api::ErrorCode::InvalidConfig, .message = error_};
  }
  started_ = true;
  generation_.store(1, std::memory_order_release);
  last_message_ = now;
  if (!begin_connection(now, start_error)) {
    started_ = false;
    state_ = SessionState::Failed;
    error_ = std::move(start_error);
    return {.error = api::ErrorCode::InternalError, .message = error_};
  }
  return {};
}

bool BinanceSession::open_publishers(std::string &error) {
  if (!options_.publish) {
    return true;
  }
  auto ring = options_.ring;
  auto ticker_options = ring;
  ticker_options.name = publish::make_publisher_segment_name(
      options_.shm_prefix, profile_name(options_.profile), options_.symbol,
      "ticker");
  auto orderbook_options = ring;
  orderbook_options.name = publish::make_publisher_segment_name(
      options_.shm_prefix, profile_name(options_.profile), options_.symbol,
      "orderbook");
  if (ticker_options.name.empty() || orderbook_options.name.empty()) {
    error = "shared-memory prefix, profile, or symbol produces an invalid "
            "segment name";
    return false;
  }
  auto ticker_ring = transport::SharedRing::open(ticker_options);
  if (!ticker_ring) {
    error = ticker_ring.message;
    return false;
  }
  auto orderbook_ring = transport::SharedRing::open(orderbook_options);
  if (!orderbook_ring) {
    error = orderbook_ring.message;
    return false;
  }
  publishers_ = std::make_unique<publish::TickerOrderBookPublishers>(
      publish::WirePublisher(std::move(ticker_ring.value)),
      publish::WirePublisher(std::move(orderbook_ring.value)));
  return true;
}

bool BinanceSession::begin_connection(Clock::time_point now,
                                      std::string &error) {
  remove_registration(ClientKind::WebSocket);
  remove_registration(ClientKind::Metadata);
  remove_registration(ClientKind::Snapshot);
  websocket_.reset();
  metadata_http_.reset();
  snapshot_http_.reset();
  synchronizer_.reset();
  raw_depth_buffer_.clear();
  metadata_ready_ = false;
  snapshot_deferred_ = false;
  snapshot_request_active_ = false;
  json_parser_.reset();
  bridge_.reset();
  overlay_.Clear();
  instrument_ready_ = false;
  state_ = SessionState::Resolving;
  const auto endpoint = websocket_endpoint();
  if (!websocket_.start(endpoint.host, endpoint.service, websocket_target_,
                        now + options_.connect_timeout)) {
    error.assign(websocket_.error_message());
    return false;
  }
  sync_registration(ClientKind::WebSocket);
  if (!begin_metadata(now, error)) {
    return false;
  }
  opened_at_ = now;
  handle_ws_state(now);
  return true;
}

bool BinanceSession::begin_metadata(Clock::time_point now, std::string &error) {
  const auto endpoint = rest_endpoint();
  const auto capability = exchange::binance::capability(options_.profile);
  std::string target(capability.exchange_info_path);
  target += "?symbol=";
  target += options_.symbol;
  if (!metadata_http_.start_get(endpoint.host, endpoint.service, target,
                                now + options_.request_timeout)) {
    error.assign(metadata_http_.error_message());
    return false;
  }
  sync_registration(ClientKind::Metadata);
  return true;
}

bool BinanceSession::begin_snapshot(Clock::time_point now, std::string &error) {
  if (snapshot_request_active_) {
    return true;
  }
  remove_registration(ClientKind::Snapshot);
  snapshot_http_.reset();
  const auto endpoint = rest_endpoint();
  const auto capability = exchange::binance::capability(options_.profile);
  std::string target(capability.depth_path);
  target += "?symbol=";
  target += options_.symbol;
  target += "&limit=";
  target += options_.profile == Profile::Spot ? "5000" : "1000";
  if (!snapshot_http_.start_get(endpoint.host, endpoint.service, target,
                                now + options_.request_timeout)) {
    error.assign(snapshot_http_.error_message());
    return false;
  }
  snapshot_deferred_ = false;
  snapshot_request_active_ = true;
  state_ = SessionState::Snapshot;
  sync_registration(ClientKind::Snapshot);
  return true;
}

bool BinanceSession::request_snapshot(Clock::time_point now,
                                      std::string &error) {
  if (snapshot_request_active_) {
    return true;
  }
  if (!snapshot_allowed(websocket_.state())) {
    snapshot_deferred_ = true;
    return true;
  }
  return begin_snapshot(now, error);
}

void BinanceSession::on_client_event(ClientKind kind,
                                     std::uint32_t events) noexcept {
  const auto now = Clock::now();
  if (kind == ClientKind::WebSocket) {
    (void)websocket_.on_event(events, now);
  } else if (kind == ClientKind::Metadata) {
    (void)metadata_http_.on_event(events, now);
  } else {
    (void)snapshot_http_.on_event(events, now);
  }
}

void BinanceSession::sync_registration(ClientKind kind) noexcept {
  int *registered = kind == ClientKind::WebSocket
                        ? &websocket_fd_
                        : (kind == ClientKind::Metadata ? &metadata_fd_
                                                        : &snapshot_fd_);
  std::uint64_t *registered_generation =
      kind == ClientKind::WebSocket
          ? &websocket_socket_generation_
          : (kind == ClientKind::Metadata
                 ? &metadata_socket_generation_
                 : &snapshot_socket_generation_);
  const int current = client_fd(kind);
  const std::uint64_t current_generation = client_socket_generation(kind);
  const auto events = client_events(kind);
  if (*registered >= 0 &&
      (*registered != current ||
       *registered_generation != current_generation || events == 0)) {
    (void)loop_.remove(*registered);
    *registered = -1;
    *registered_generation = 0;
  }
  if (current < 0 || events == 0) {
    return;
  }
  if (*registered < 0) {
    if (loop_.add(current, events, [this, kind](std::uint32_t ready) {
          on_client_event(kind, ready);
        })) {
      *registered = current;
      *registered_generation = current_generation;
    } else {
      error_ = "failed to add client fd to epoll";
      state_ = SessionState::Failed;
    }
  } else if (!loop_.modify(current, events)) {
    error_ = "failed to modify client fd in epoll";
    state_ = SessionState::Failed;
  }
}

void BinanceSession::remove_registration(ClientKind kind) noexcept {
  int *registered = kind == ClientKind::WebSocket
                        ? &websocket_fd_
                        : (kind == ClientKind::Metadata ? &metadata_fd_
                                                        : &snapshot_fd_);
  std::uint64_t *registered_generation =
      kind == ClientKind::WebSocket
          ? &websocket_socket_generation_
          : (kind == ClientKind::Metadata
                 ? &metadata_socket_generation_
                 : &snapshot_socket_generation_);
  if (*registered >= 0) {
    (void)loop_.remove(*registered);
    *registered = -1;
  }
  *registered_generation = 0;
}

void BinanceSession::handle_ws_state(Clock::time_point now) noexcept {
  (void)now;
  const auto current = state();
  switch (websocket_.state()) {
  case network::WebSocketClientState::TcpConnecting:
    if (connecting(current)) {
      state_ = SessionState::Tcp;
    }
    break;
  case network::WebSocketClientState::TlsHandshaking:
    if (connecting(current)) {
      state_ = SessionState::Tls;
    }
    break;
  case network::WebSocketClientState::SendingUpgrade:
  case network::WebSocketClientState::ReadingUpgrade:
    if (connecting(current)) {
      state_ = SessionState::Upgrade;
    }
    break;
  case network::WebSocketClientState::Open:
    if (connecting(current) || current == SessionState::Metadata) {
      state_ = SessionState::Buffering;
    }
    break;
  default:
    break;
  }
}

void BinanceSession::handle_http_state(ClientKind kind,
                                       Clock::time_point now) noexcept {
  auto &client =
      kind == ClientKind::Metadata ? metadata_http_ : snapshot_http_;
  if (terminal(client.state())) {
    schedule_reconnect(client.error_message(), now);
    return;
  }
  if (client.state() != network::HttpClientState::Complete) {
    return;
  }
  const auto completed_generation = client.socket_generation();
  const auto body = client.response().body();
  const std::string_view json(reinterpret_cast<const char *>(body.data()),
                              body.size());
  std::string parse_error;
  bool ok = false;
  if (client.response().status_class() != network::HttpStatusClass::Success) {
    parse_error = "Binance REST returned HTTP " +
                  std::to_string(client.response().status_code());
  } else if (kind == ClientKind::Metadata) {
    if (connecting(state()) || state() == SessionState::Buffering) {
      state_ = SessionState::Metadata;
    }
    ok = ingest_exchange_info(json, parse_error);
    if (ok) {
      ok = request_snapshot(now, parse_error);
    }
  } else {
    snapshot_request_active_ = false;
    ok = ingest_depth_snapshot(json, parse_error);
  }
  if (client.socket_generation() == completed_generation) {
    remove_registration(kind);
    client.reset();
  }
  if (!ok) {
    ++metrics_.parse_errors;
    schedule_reconnect(parse_error, now);
  }
}

bool BinanceSession::ingest_exchange_info(std::string_view json,
                                          std::string &error) {
  if (!rest_parser_.parse_exchange_info(options_.profile, json, options_.symbol,
                                        metadata_, error)) {
    return false;
  }
  metadata_ready_ = true;
  json_parser_ = std::make_unique<exchange::binance::JsonParser>(
      metadata_.price_scale, metadata_.quantity_scale);
  bridge_ = std::make_unique<book::BookBridge>(*order_book_,
                                               metadata_.price_filter.tick_size);

  if (publishers_) {
    instrument_ = {};
    instrument_.instrument_id = 1;
    instrument_.venue = utils::md::Venue::Binance;
    instrument_.product_type =
        options_.profile == Profile::Spot ? utils::md::ProductType::Spot
                                          : utils::md::ProductType::Perpetual;
    instrument_.price_scale = static_cast<std::uint8_t>(metadata_.price_scale);
    instrument_.quantity_scale =
        static_cast<std::uint8_t>(metadata_.quantity_scale);
    instrument_.tick_size = metadata_.price_filter.tick_size;
    instrument_.lot_size = metadata_.lot_size.step_size;
    instrument_.contract_multiplier = metadata_.contract_size;
    copy_text(instrument_.base_asset, metadata_.base_asset);
    copy_text(instrument_.quote_asset, metadata_.quote_asset);
    copy_text(instrument_.settle_asset, metadata_.settle_asset);
    copy_text(instrument_.canonical_symbol, options_.symbol);
    copy_text(instrument_.venue_symbol, metadata_.venue_symbol);
    const auto key = std::string("binance:") + profile_name(options_.profile) +
                     ":" + options_.symbol;
    copy_text(instrument_.instrument_key, key);
    instrument_ready_ = true;
    auto header = make_header(generation(), 0, bus_sequence_++, 0,
                              utils::md::BookState::Building);
    const auto ticker_result =
        publishers_->ticker().publish_instrument(header, instrument_);
    const auto book_result =
        publishers_->order_book().publish_instrument(header, instrument_);
    if (!ticker_result || !book_result) {
      ++metrics_.publish_errors;
      error = !ticker_result ? ticker_result.message : book_result.message;
      handle_publish_failure(error, Clock::now());
      return false;
    }
  } else {
    instrument_ready_ = true;
  }

  while (!raw_depth_buffer_.empty()) {
    exchange::binance::CombinedMessageView view;
    if (!combined_parser_.unpack(raw_depth_buffer_.front(), view, error) ||
        view.route.kind != exchange::binance::StreamKind::Depth ||
        !handle_depth(view.data, error)) {
      return false;
    }
    raw_depth_buffer_.pop_front();
  }
  return true;
}

bool BinanceSession::ingest_depth_snapshot(std::string_view json,
                                           std::string &error) {
  if (!metadata_ready_ || !bridge_) {
    error = "depth snapshot arrived before instrument metadata";
    return false;
  }
  exchange::binance::DepthSnapshot parsed;
  if (!rest_parser_.parse_depth(options_.profile, json, metadata_, parsed,
                                error)) {
    return false;
  }
  book::DepthSnapshot snapshot;
  snapshot.last_update_id = parsed.last_update_id;
  snapshot.bids = convert_levels(parsed.bids);
  snapshot.asks = convert_levels(parsed.asks);
  const auto loaded = bridge_->LoadSnapshot(snapshot, generation());
  if (loaded.action != book::BridgeAction::Applied) {
    error = "depth snapshot does not fit the configured ladder window";
    return false;
  }
  state_ = SessionState::Bridging;
  synchronizer_.inject_snapshot(parsed.last_update_id);
  const auto action =
      synchronizer_.drain_buffered(this, &BinanceSession::apply_buffered);
  if (action == exchange::binance::SyncAction::Resnapshot) {
    error = "buffered depth updates do not bridge the REST snapshot";
    return false;
  }
  if (action == exchange::binance::SyncAction::BecameLive) {
    bridge_->SetLive();
    state_ = SessionState::Live;
    if (!publish_live_image(synchronizer_.last_update_id(), 0)) {
      return true;
    }
  }
  return true;
}

bool BinanceSession::ingest_websocket_message(std::string_view json,
                                              std::string &error) {
  exchange::binance::CombinedMessageView view;
  if (!combined_parser_.unpack(json, view, error)) {
    return false;
  }
  if (view.route.kind == exchange::binance::StreamKind::Depth &&
      !metadata_ready_) {
    if (raw_depth_buffer_.size() >= options_.max_buffered_updates) {
      error = "raw depth buffer limit exceeded before metadata";
      return false;
    }
    raw_depth_buffer_.emplace_back(json);
    return true;
  }
  if (!json_parser_) {
    return true;
  }
  return view.route.kind == exchange::binance::StreamKind::BookTicker
             ? handle_ticker(view.data, error)
             : handle_depth(view.data, error);
}

bool BinanceSession::handle_ticker(std::string_view data, std::string &error) {
  exchange::binance::BookTicker ticker;
  if (!json_parser_->parse_book_ticker(data, ticker, error)) {
    return false;
  }
  ++metrics_.ticker_updates;
  utils::md::BboEvent event{};
  event.header =
      make_header(generation(), ticker.update_id, bus_sequence_++,
                  ticker.transaction_time_ms != 0 ? ticker.transaction_time_ms
                                                  : ticker.event_time_ms,
                  utils::md::BookState::Live);
  event.bid = {ticker.bid_price, ticker.bid_quantity};
  event.ask = {ticker.ask_price, ticker.ask_quantity};
  auto canonical = order_book_->Bbo(1).value_or(utils::md::BboEvent{});
  canonical.header.instrument_id = 1;
  canonical.header.book_generation = generation();
  canonical.header.source_seq =
      bridge_ ? bridge_->last_update_id() : std::uint64_t{};
  if (overlay_.OnTicker(event, canonical) == book::OverlayAction::Divergence) {
    request_resync("ticker/canonical BBO divergence", Clock::now());
    return true;
  }
  std::optional<utils::md::BboEvent> output =
      options_.profile == Profile::Spot ? overlay_.Effective(canonical)
                                        : overlay_.ticker();
  if (publishers_ && output && !publishers_->ticker().publish_bbo(*output)) {
    ++metrics_.publish_errors;
    handle_publish_failure("ticker BBO publish failed", Clock::now());
  }
  return true;
}

bool BinanceSession::handle_depth(std::string_view data, std::string &error) {
  exchange::binance::DepthUpdate update;
  if (!json_parser_->parse_depth(data, update, error)) {
    return false;
  }
  const auto action = synchronizer_.on_update(update);
  if (action == exchange::binance::SyncAction::Resnapshot) {
    const std::string reason =
        "Binance depth sequence gap: U=" +
        std::to_string(update.first_update_id) + " u=" +
        std::to_string(update.final_update_id) + " pu=" +
        std::to_string(update.previous_final_update_id) + " expected_after=" +
        std::to_string(synchronizer_.last_update_id());
    request_resync(reason, Clock::now());
    return true;
  }
  if (action == exchange::binance::SyncAction::Apply ||
      action == exchange::binance::SyncAction::BecameLive) {
    const bool was_live = state() == SessionState::Live;
    const auto generation_before_apply = generation();
    if (!apply_depth(update, was_live &&
                                action == exchange::binance::SyncAction::Apply)) {
      // apply_depth can itself resync on overlay divergence or publisher
      // failure. Only classify this as a ladder-window failure when recovery
      // has not already advanced the generation.
      if (generation() == generation_before_apply &&
          state() != SessionState::Failed) {
        request_resync("depth update moved outside ladder window", Clock::now());
      }
      return true;
    }
    if (action == exchange::binance::SyncAction::BecameLive) {
      bridge_->SetLive();
      state_ = SessionState::Live;
      (void)publish_live_image(
          update.final_update_id,
          update.transaction_time_ms != 0 ? update.transaction_time_ms
                                          : update.event_time_ms);
    }
  }
  return true;
}

bool BinanceSession::apply_buffered(
    void *context, const exchange::binance::DepthUpdate &update) noexcept {
  return static_cast<BinanceSession *>(context)->apply_depth(update, false);
}

bool BinanceSession::apply_depth(
    const exchange::binance::DepthUpdate &update, bool publish) noexcept {
  if (!bridge_) {
    return false;
  }
  const auto bids = convert_levels(update.bids);
  const auto asks = convert_levels(update.asks);
  std::vector<utils::md::Level> maintained_bids;
  std::vector<utils::md::Level> maintained_asks;
  maintained_bids.reserve(bids.size());
  maintained_asks.reserve(asks.size());
  for (const auto &level : bids) {
    if (bridge_->Maintains(utils::md::Side::Bid, level.price)) {
      maintained_bids.push_back(level);
    }
  }
  for (const auto &level : asks) {
    if (bridge_->Maintains(utils::md::Side::Ask, level.price)) {
      maintained_asks.push_back(level);
    }
  }
  const auto ignored_before = bridge_->outside_updates_ignored();
  if (bridge_->Apply({update.first_update_id, update.final_update_id, bids,
                      asks}) != book::BridgeAction::Applied) {
    metrics_.outside_depth_levels_ignored +=
        bridge_->outside_updates_ignored() - ignored_before;
    return false;
  }
  metrics_.outside_depth_levels_ignored +=
      bridge_->outside_updates_ignored() - ignored_before;
  ++metrics_.depth_updates;
  if (publish && publishers_) {
    for (const auto &level : maintained_bids) {
      utils::md::BookDelta delta{};
      delta.header = make_header(
          generation(), update.final_update_id, bus_sequence_++,
          update.transaction_time_ms != 0 ? update.transaction_time_ms
                                          : update.event_time_ms,
          state() == SessionState::Live ? utils::md::BookState::Live
                                        : utils::md::BookState::Building);
      delta.side = utils::md::Side::Bid;
      delta.level = level;
      if (!publishers_->order_book().publish_delta(delta)) {
        ++metrics_.publish_errors;
        handle_publish_failure("order-book delta publish failed", Clock::now());
        return false;
      }
    }
    for (const auto &level : maintained_asks) {
      utils::md::BookDelta delta{};
      delta.header = make_header(
          generation(), update.final_update_id, bus_sequence_++,
          update.transaction_time_ms != 0 ? update.transaction_time_ms
                                          : update.event_time_ms,
          state() == SessionState::Live ? utils::md::BookState::Live
                                        : utils::md::BookState::Building);
      delta.side = utils::md::Side::Ask;
      delta.level = level;
      if (!publishers_->order_book().publish_delta(delta)) {
        ++metrics_.publish_errors;
        handle_publish_failure("order-book delta publish failed", Clock::now());
        return false;
      }
    }
  }
  if (publish &&
      !publish_canonical(update.final_update_id,
                         update.transaction_time_ms != 0
                             ? update.transaction_time_ms
                             : update.event_time_ms)) {
    return false;
  }
  return true;
}

bool BinanceSession::publish_canonical(std::uint64_t sequence,
                                       std::uint64_t exchange_time_ms) noexcept {
  auto canonical = order_book_->Bbo(1);
  if (!canonical) {
    return true;
  }
  canonical->header =
      make_header(generation(), sequence, bus_sequence_++, exchange_time_ms,
                  state() == SessionState::Live ? utils::md::BookState::Live
                                                : utils::md::BookState::Building);
  if (overlay_.OnCanonical(*canonical) == book::OverlayAction::Divergence) {
    request_resync("ticker/canonical BBO divergence", Clock::now());
    return false;
  }
  if (publishers_ &&
      !publishers_->order_book().publish_bbo(*canonical)) {
    ++metrics_.publish_errors;
    handle_publish_failure("canonical BBO publish failed", Clock::now());
    return false;
  }
  return true;
}

bool BinanceSession::publish_live_image(
    std::uint64_t sequence, std::uint64_t exchange_time_ms) noexcept {
  if (publishers_) {
    const auto header =
        make_header(generation(), sequence, bus_sequence_++, exchange_time_ms,
                    utils::md::BookState::Live);
    const auto result =
        publishers_->order_book().publish_snapshot(header, *order_book_);
    if (!result) {
      ++metrics_.publish_errors;
      handle_publish_failure(result.message, Clock::now());
      return false;
    }
  }
  return publish_canonical(sequence, exchange_time_ms);
}

bool BinanceSession::republish_ticker() noexcept {
  if (!publishers_ || !instrument_ready_) {
    return true;
  }
  auto header = make_header(generation(), 0, bus_sequence_++, 0,
                            state() == SessionState::Live
                                ? utils::md::BookState::Live
                                : utils::md::BookState::Building);
  auto result = publishers_->ticker().publish_instrument(header, instrument_);
  if (!result) {
    ++metrics_.publish_errors;
    handle_publish_failure(result.message, Clock::now());
    return false;
  }
  auto canonical = order_book_->Bbo(1).value_or(utils::md::BboEvent{});
  canonical.header.instrument_id = 1;
  canonical.header.book_generation = generation();
  canonical.header.source_seq =
      bridge_ ? bridge_->last_update_id() : std::uint64_t{};
  const auto output =
      options_.profile == Profile::Spot ? overlay_.Effective(canonical)
                                        : overlay_.ticker();
  if (output) {
    result = publishers_->ticker().publish_bbo(*output);
    if (!result) {
      ++metrics_.publish_errors;
      handle_publish_failure(result.message, Clock::now());
      return false;
    }
  }
  return true;
}

bool BinanceSession::republish_order_book() noexcept {
  if (!publishers_ || !instrument_ready_) {
    return true;
  }
  auto header = make_header(generation(), 0, bus_sequence_++, 0,
                            state() == SessionState::Live
                                ? utils::md::BookState::Live
                                : utils::md::BookState::Building);
  auto result =
      publishers_->order_book().publish_instrument(header, instrument_);
  if (!result) {
    ++metrics_.publish_errors;
    handle_publish_failure(result.message, Clock::now());
    return false;
  }
  if (state() == SessionState::Live) {
    return publish_live_image(bridge_ ? bridge_->last_update_id() : 0, 0);
  }
  return true;
}

void BinanceSession::handle_publish_failure(
    std::string_view reason, Clock::time_point now) noexcept {
  if (handling_publish_failure_ || state() == SessionState::Failed ||
      state() == SessionState::Stopped) {
    return;
  }
  handling_publish_failure_ = true;
  request_resync(reason.empty() ? "market-data publish failed" : reason, now);
  handling_publish_failure_ = false;
}

void BinanceSession::schedule_reconnect(std::string_view reason,
                                        Clock::time_point now,
                                        bool advance_generation) noexcept {
  error_.assign(reason);
  remove_registration(ClientKind::WebSocket);
  remove_registration(ClientKind::Metadata);
  remove_registration(ClientKind::Snapshot);
  websocket_.reset();
  metadata_http_.reset();
  snapshot_http_.reset();
  snapshot_deferred_ = false;
  snapshot_request_active_ = false;
  const auto current_generation = generation();
  if (advance_generation) {
    if (current_generation == std::numeric_limits<std::uint32_t>::max()) {
      error_ = "book generation exhausted during reconnect";
      state_ = SessionState::Failed;
      started_ = false;
      return;
    }
    generation_.store(current_generation + 1U, std::memory_order_release);
  }
  ++metrics_.reconnects;
  const auto shift = std::min<std::uint32_t>(reconnect_attempt_, 16U);
  const auto multiplier = std::uint64_t{1} << shift;
  const auto capped = std::min<std::uint64_t>(
      static_cast<std::uint64_t>(options_.reconnect_max.count()),
      static_cast<std::uint64_t>(options_.reconnect_base.count()) * multiplier);
  std::minstd_rand generator(
      static_cast<unsigned>(clock_ticks() ^ generation()));
  std::uniform_int_distribution<std::uint64_t> jitter(0, capped / 4U + 1U);
  const auto delay = static_cast<std::chrono::milliseconds::rep>(
      capped + jitter(generator));
  reconnect_at_ = now + std::chrono::milliseconds(delay);
  ++reconnect_attempt_;
  state_ = SessionState::ReconnectWait;
}

void BinanceSession::request_resync(std::string_view reason,
                                    Clock::time_point now) noexcept {
  error_.assign(reason);
  ++metrics_.resyncs;
  const auto current_generation = generation();
  if (current_generation == std::numeric_limits<std::uint32_t>::max()) {
    remove_registration(ClientKind::WebSocket);
    remove_registration(ClientKind::Metadata);
    remove_registration(ClientKind::Snapshot);
    websocket_.reset();
    metadata_http_.reset();
    snapshot_http_.reset();
    error_ = "book generation exhausted during resync";
    state_ = SessionState::Failed;
    started_ = false;
    return;
  }
  generation_.store(current_generation + 1U, std::memory_order_release);
  state_ = SessionState::Resync;
  synchronizer_.reset();
  overlay_.Clear();
  remove_registration(ClientKind::Snapshot);
  snapshot_http_.reset();
  snapshot_deferred_ = false;
  snapshot_request_active_ = false;
  std::string request_error;
  if (!request_snapshot(now, request_error)) {
    // request_resync already advanced the generation. Falling back to a full
    // reconnect is the same recovery attempt and must not advance it twice.
    schedule_reconnect(request_error, now, false);
  }
}

void BinanceSession::tick(Clock::time_point now) noexcept {
  if (!started_ || state() == SessionState::Stopped ||
      state() == SessionState::Failed) {
    return;
  }
  if (state() == SessionState::ReconnectWait) {
    if (now >= reconnect_at_) {
      std::string reconnect_error;
      if (!begin_connection(now, reconnect_error)) {
        schedule_reconnect(reconnect_error, now);
      }
    }
    return;
  }
  (void)websocket_.check_timeout(now);
  (void)metadata_http_.check_timeout(now);
  (void)snapshot_http_.check_timeout(now);
  handle_ws_state(now);
  if (terminal(websocket_.state())) {
    schedule_reconnect(websocket_.error_message(), now);
    return;
  }
  if (snapshot_deferred_ && snapshot_allowed(websocket_.state())) {
    std::string snapshot_error;
    if (!request_snapshot(now, snapshot_error)) {
      schedule_reconnect(snapshot_error, now);
      return;
    }
  }
  handle_http_state(ClientKind::Metadata, now);
  if (state() == SessionState::ReconnectWait) {
    return;
  }
  handle_http_state(ClientKind::Snapshot, now);
  if (state() == SessionState::ReconnectWait) {
    return;
  }
  sync_registration(ClientKind::WebSocket);
  sync_registration(ClientKind::Metadata);
  sync_registration(ClientKind::Snapshot);
  if (publishers_ &&
      (last_reader_reclaim_ == Clock::time_point{} ||
       now - last_reader_reclaim_ >= std::chrono::seconds(1))) {
    const auto now_ns = static_cast<std::uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            now.time_since_epoch())
            .count());
    const auto timeout_ns = static_cast<std::uint64_t>(
        std::max<std::int64_t>(1, options_.reader_lease_timeout.count()));
    (void)publishers_->ticker().reclaim_stale_readers(now_ns, timeout_ns);
    (void)publishers_->order_book().reclaim_stale_readers(now_ns, timeout_ns);
    last_reader_reclaim_ = now;
  }
  if (!poll_reader_changes()) {
    return;
  }

  if (websocket_.state() == network::WebSocketClientState::Open &&
      now - last_message_ >= options_.idle_timeout) {
    schedule_reconnect("Binance WebSocket idle timeout", now);
    return;
  }
  constexpr auto rotation = std::chrono::hours(23) + std::chrono::minutes(50);
  if (websocket_.state() == network::WebSocketClientState::Open &&
      now - opened_at_ >= rotation) {
    ++metrics_.rotations;
    metrics_.rotation_is_seamless = false;
    schedule_reconnect("scheduled 23h50 connection rotation", now);
  }
}

bool BinanceSession::poll_reader_changes() noexcept {
  if (!publishers_) {
    return true;
  }
  if (publishers_->ticker().poll_reader_change() && !republish_ticker()) {
    return false;
  }
  if (publishers_->order_book().poll_reader_change() &&
      !republish_order_book()) {
    return false;
  }
  return state() != SessionState::Resync &&
         state() != SessionState::ReconnectWait &&
         state() != SessionState::Failed;
}

void BinanceSession::stop() noexcept {
  remove_registration(ClientKind::WebSocket);
  remove_registration(ClientKind::Metadata);
  remove_registration(ClientKind::Snapshot);
  websocket_.reset();
  metadata_http_.reset();
  snapshot_http_.reset();
  snapshot_deferred_ = false;
  snapshot_request_active_ = false;
  state_ = SessionState::Stopped;
  started_ = false;
}

std::string_view BinanceSession::ticker_segment() const noexcept {
  return publishers_ ? publishers_->ticker().segment_name() : std::string_view{};
}

std::string_view BinanceSession::orderbook_segment() const noexcept {
  return publishers_ ? publishers_->order_book().segment_name()
                     : std::string_view{};
}

BinanceSession::Endpoint BinanceSession::websocket_endpoint() const {
  return parse_endpoint(options_.websocket_endpoint,
                        exchange::binance::capability(options_.profile)
                            .websocket_host);
}

BinanceSession::Endpoint BinanceSession::rest_endpoint() const {
  return parse_endpoint(options_.rest_endpoint,
                        exchange::binance::capability(options_.profile).rest_host);
}

int BinanceSession::client_fd(ClientKind kind) const noexcept {
  if (kind == ClientKind::WebSocket) {
    return websocket_.fd();
  }
  return kind == ClientKind::Metadata ? metadata_http_.fd()
                                      : snapshot_http_.fd();
}

std::uint64_t
BinanceSession::client_socket_generation(ClientKind kind) const noexcept {
  if (kind == ClientKind::WebSocket) {
    return websocket_.socket_generation();
  }
  return kind == ClientKind::Metadata ? metadata_http_.socket_generation()
                                      : snapshot_http_.socket_generation();
}

std::uint32_t BinanceSession::client_events(ClientKind kind) const noexcept {
  if (kind == ClientKind::WebSocket) {
    return websocket_.wanted_events();
  }
  return kind == ClientKind::Metadata ? metadata_http_.wanted_events()
                                      : snapshot_http_.wanted_events();
}

SessionManager::SessionManager() {
  std::string error;
  tls_ = network::make_client_ssl_context(error);
}

SessionManager::SessionManager(network::SharedSslContext tls)
    : tls_(std::move(tls)) {}

api::Result<BinanceSession *>
SessionManager::create(BinanceSessionOptions options, bool start_immediately) {
  std::lock_guard lock(mutex_);
  if (!tls_) {
    return {.value = nullptr,
            .error = api::ErrorCode::InternalError,
            .message = "TLS context is unavailable"};
  }
  std::transform(options.symbol.begin(), options.symbol.end(),
                 options.symbol.begin(), [](unsigned char c) {
                   return static_cast<char>(std::toupper(c));
                 });
  if (auto *existing = find_unlocked(options.profile, options.symbol)) {
    return {.value = existing};
  }
  auto session =
      std::make_unique<BinanceSession>(loop_, tls_, std::move(options));
  auto *result = session.get();
  if (start_immediately) {
    auto started = result->start();
    if (!started) {
      return {.value = nullptr,
              .error = started.error,
              .message = std::move(started.message)};
    }
  }
  sessions_.push_back(std::move(session));
  return {.value = result};
}

BinanceSession *SessionManager::find(Profile profile,
                                     std::string_view symbol) noexcept {
  std::lock_guard lock(mutex_);
  return find_unlocked(profile, symbol);
}

BinanceSession *SessionManager::find_unlocked(
    Profile profile, std::string_view symbol) noexcept {
  for (const auto &session : sessions_) {
    if (session->profile() == profile && same_symbol(session->symbol(), symbol)) {
      return session.get();
    }
  }
  return nullptr;
}

int SessionManager::run_once(int timeout_ms) {
  std::lock_guard lock(mutex_);
  const int count = loop_.run_once(timeout_ms);
  const auto now = BinanceSession::Clock::now();
  for (const auto &session : sessions_) {
    session->tick(now);
  }
  return count;
}

void SessionManager::stop() noexcept {
  std::lock_guard lock(mutex_);
  for (const auto &session : sessions_) {
    session->stop();
  }
  loop_.stop();
}

} // namespace mds::service
