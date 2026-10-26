#include "oms/exchange/hyperliquid/trade_adapter.h"

#include "oms/exchange/hyperliquid/protocol.h"
#include "oms/exchange/live_transport.h"

#include <algorithm>
#include <chrono>
#include <cstring>
#include <limits>
#include <span>
#include <string>

namespace oms::exchange::hyperliquid {
namespace {

AdapterEvent MakeEvent(AdapterEventKind kind) noexcept {
  AdapterEvent event;
  event.kind = kind;
  event.source = {AdapterKind::Hyperliquid, 0, utils::md::Venue::Hyperliquid,
                  utils::md::ProductType::Unknown, {}};
  return event;
}

bool supported_order(const api::NewOrderRequest& request) noexcept {
  if (request.type != api::OrderType::Limit) return false;
  if (request.time_in_force == api::TimeInForce::GTD ||
      request.time_in_force == api::TimeInForce::FOK ||
      request.time_in_force == api::TimeInForce::FAK)
    return false;
  const bool post_only = (request.flags & api::PostOnly) != 0U;
  if (post_only && request.time_in_force != api::TimeInForce::GTC) return false;
  if (request.time_in_force != api::TimeInForce::GTC &&
      request.time_in_force != api::TimeInForce::IOC)
    return false;
  return true;
}

const char* tif_name(const api::NewOrderRequest& request) noexcept {
  if ((request.flags & api::PostOnly) != 0U) return "Alo";
  if (request.time_in_force == api::TimeInForce::IOC) return "Ioc";
  return "Gtc";
}

std::string decimal(std::int64_t value, std::uint8_t scale) {
  const bool negative = value < 0;
  std::uint64_t magnitude =
      static_cast<std::uint64_t>(negative ? -value : value);
  std::string digits = std::to_string(magnitude);
  if (scale == 0) return negative ? "-" + digits : digits;
  if (digits.size() <= scale) digits.insert(0, scale - digits.size() + 1, '0');
  digits.insert(digits.size() - scale, 1, '.');
  return negative ? "-" + digits : digits;
}

std::string hex_bytes(const std::uint8_t* data, std::size_t size) {
  static constexpr char kHex[] = "0123456789abcdef";
  std::string out = "0x";
  out.resize(2 + size * 2);
  for (std::size_t index = 0; index < size; ++index) {
    out[2 + index * 2] = kHex[data[index] >> 4U];
    out[3 + index * 2] = kHex[data[index] & 0x0fU];
  }
  return out;
}

std::uint64_t wall_clock_ms() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::milliseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

template <typename Id>
bool copy_id(std::string_view source, Id& destination) noexcept {
  if (source.empty() || source.size() > destination.value.size()) return false;
  std::memcpy(destination.value.data(), source.data(), source.size());
  destination.length = static_cast<std::uint16_t>(source.size());
  return true;
}

template <typename Id>
std::string_view id_view(const Id& value) noexcept {
  if (value.length > value.value.size()) return {};
  return {value.value.data(), value.length};
}

std::string_view dex_from_query(const api::QueryRequest& request) noexcept {
  if (request.scope != api::QueryScope::SingleInstrument ||
      request.instrument.kind != api::ExecutionRouteKind::Crypto)
    return {};
  const std::string_view symbol =
      id_view(request.instrument.crypto.venue_symbol);
  const std::size_t colon = symbol.find(':');
  return colon == std::string_view::npos ? std::string_view{}
                                         : symbol.substr(0, colon);
}

}  // namespace

HyperliquidTradeAdapter::HyperliquidTradeAdapter(std::string_view private_key) {
  process_start_ms_ = wall_clock_ms();
  (void)session_.arm_signer(private_key);
}

HyperliquidTradeAdapter::HyperliquidTradeAdapter(const WireConfig& config)
    : transport_(config.transport),
      account_address_(config.account_address),
      vault_address_(config.vault_address),
      strategy_instance_hash_(
          config.strategy_instance.empty()
              ? 0
              : stable_strategy_hash(config.strategy_instance)),
      mainnet_(config.mainnet) {
  process_start_ms_ = wall_clock_ms();
  session_.set_strategy_instance(config.strategy_instance);
  (void)session_.arm_signer(config.private_key);
}

AdapterIdentity HyperliquidTradeAdapter::identity() const noexcept {
  return {AdapterKind::Hyperliquid, 0, utils::md::Venue::Hyperliquid,
          utils::md::ProductType::Unknown, {}};
}

AdapterStatus HyperliquidTradeAdapter::status() const noexcept {
  return session_.status();
}

AdapterCapabilities HyperliquidTradeAdapter::capabilities() const noexcept {
  AdapterCapabilities capabilities;
  capabilities.bits =
      static_cast<std::uint64_t>(AdapterCapability::Limit) |
      static_cast<std::uint64_t>(AdapterCapability::TifGtc) |
      static_cast<std::uint64_t>(AdapterCapability::TifIoc) |
      static_cast<std::uint64_t>(AdapterCapability::PostOnly) |
      static_cast<std::uint64_t>(AdapterCapability::ReduceOnly);
  return capabilities;
}

AdapterResult HyperliquidTradeAdapter::reserve_command(
    AdapterCommandKind, AdapterReservation& reservation) noexcept {
  if (reserved_) return AdapterResult::NotReady;
  reserved_ = true;
  reservation.adapter = AdapterKind::Hyperliquid;
  reservation.slot = 0;
  reservation.generation = generation_;
  return AdapterResult::Ok;
}

void HyperliquidTradeAdapter::cancel_reservation(
    AdapterReservation reservation) noexcept {
  if (reservation.generation == generation_) reserved_ = false;
}

bool HyperliquidTradeAdapter::queue_reject(const AdapterPlaceCommand& command,
                                           std::string_view) noexcept {
  if (pending_count_ >= pending_.size()) return false;
  AdapterEvent event = MakeEvent(AdapterEventKind::Venue);
  event.venue.type = api::VenueEventType::NewReject;
  event.venue.handle = command.handle;
  event.venue.token = command.request.token;
  event.venue.client_order_id = command.request.client_order_id;
  event.venue.reconciled_status = api::OrderStatus::Rejected;
  pending_[pending_count_++] = event;
  return true;
}

AdapterResult HyperliquidTradeAdapter::commit_place(
    AdapterReservation reservation, const AdapterPlaceCommand& command) noexcept {
  if (reservation.generation != generation_ || !reserved_)
    return AdapterResult::StaleReservation;
  reserved_ = false;
  ++generation_;
  if (!supported_order(command.request)) {
    return queue_reject(command, "unsupported order") ? AdapterResult::Ok
                                                      : AdapterResult::WouldBlock;
  }
  if (!session_.ready() || !rest_healthy_ || !websocket_healthy_ ||
      !metadata_healthy_ || transport_ == nullptr)
    return AdapterResult::NotReady;
  if (command.routing.kind != api::ExecutionRouteKind::Crypto)
    return queue_reject(command, "route") ? AdapterResult::Ok
                                          : AdapterResult::WouldBlock;
  const auto& symbol = command.routing.crypto.venue_symbol;
  const std::string coin(symbol.value.data(), symbol.length);
  Binding* binding = nullptr;
  for (Binding& candidate : bindings_) {
    if (!candidate.used) {
      binding = &candidate;
      break;
    }
  }
  if (binding == nullptr) return AdapterResult::WouldBlock;
  AssetId asset;
  if (!session_.assets().find(coin, asset))
    return queue_reject(command, "asset") ? AdapterResult::Ok
                                          : AdapterResult::WouldBlock;
  const bool buy = command.request.side == api::Side::Buy;
  const bool reduce_only = (command.request.flags & api::ReduceOnly) != 0U;
  const std::string price =
      decimal(command.request.price.value, command.request.price.scale);
  const std::string size =
      decimal(command.request.quantity.value, command.request.quantity.scale);
  const std::string_view client_order_id(
      command.request.client_order_id.value.data(),
      command.request.client_order_id.length);
  const auto signed_order = session_.place_limit(
      command.request.token.sequence, coin, buy, price, size, reduce_only,
      tif_name(command.request), wall_clock_ms(), mainnet_, client_order_id);
  if (!signed_order.accepted)
    return queue_reject(command, "sign") ? AdapterResult::Ok
                                         : AdapterResult::WouldBlock;
  const std::string r = hex_bytes(signed_order.signature.data(), 32);
  const std::string s = hex_bytes(signed_order.signature.data() + 32, 32);
  body_ = std::string("{\"action\":{\"type\":\"order\",\"orders\":[{\"a\":") +
          std::to_string(asset.value) + ",\"b\":" + (buy ? "true" : "false") +
          ",\"p\":\"" + price + "\",\"s\":\"" + size + "\",\"r\":" +
          (reduce_only ? "true" : "false") + ",\"t\":{\"limit\":{\"tif\":\"" +
          tif_name(command.request) + "\"}},\"c\":\"" + signed_order.cloid +
          "\"}],\"grouping\":\"na\"},\"nonce\":" +
          std::to_string(signed_order.nonce) + ",\"signature\":{\"r\":\"" + r +
          "\",\"s\":\"" + s + "\",\"v\":" +
          std::to_string(signed_order.signature[64]) + "}}";
  live::Request request;
  request.id = next_request_id_++;
  request.method = net::HttpMethod::Post;
  request.target = "/exchange";
  request.content_type = "application/json";
  request.body = {reinterpret_cast<const std::byte*>(body_.data()), body_.size()};
  request.deadline = live::LiveTransport::Clock::now() + std::chrono::seconds(2);
  const std::uint64_t request_id = request.id;
  const auto submitted = transport_->submit(request);
  if (submitted != live::SubmitResult::Accepted) return AdapterResult::NotReady;
  *binding = {};
  binding->used = true;
  binding->cloid = signed_order.cloid;
  binding->coin = coin;
  binding->instrument_id = command.request.instrument_id;
  binding->handle = command.handle;
  binding->token = command.request.token;
  binding->client_order_id = command.request.client_order_id;
  if (inflight_count_ < inflight_.size()) {
    inflight_[inflight_count_++] = Inflight{
        request_id, command.handle, command.request.token,
        command.request.client_order_id, signed_order.cloid};
  }
  return AdapterResult::Ok;
}

AdapterResult HyperliquidTradeAdapter::commit_cancel(
    AdapterReservation reservation, const AdapterCancelCommand& command) noexcept {
  if (reservation.generation != generation_ || !reserved_)
    return AdapterResult::StaleReservation;
  reserved_ = false;
  ++generation_;
  if (transport_ == nullptr) {
    return session_.cancel(command.request.target_token.sequence)
               ? AdapterResult::Ok
               : AdapterResult::NotReady;
  }
  if (!rest_healthy_ || !session_.ready()) return AdapterResult::NotReady;
  const auto signed_cancel = session_.cancel_by_cloid(
      command.request.target_token.sequence, wall_clock_ms(), mainnet_);
  if (!signed_cancel.accepted) return AdapterResult::NotReady;
  const std::string& cloid = signed_cancel.cloid;
  const std::string r = hex_bytes(signed_cancel.signature.data(), 32);
  const std::string s = hex_bytes(signed_cancel.signature.data() + 32, 32);
  body_ = std::string("{\"action\":{\"type\":\"cancelByCloid\",\"cancels\":[{\"asset\":") +
          std::to_string(signed_cancel.asset) + ",\"cloid\":\"" + cloid +
          "\"}]},\"nonce\":" + std::to_string(signed_cancel.nonce) +
          ",\"signature\":{\"r\":\"" + r + "\",\"s\":\"" + s + "\",\"v\":" +
          std::to_string(signed_cancel.signature[64]) + "}}";
  live::Request request;
  request.id = next_request_id_++;
  request.method = net::HttpMethod::Post;
  request.target = "/exchange";
  request.content_type = "application/json";
  request.body = {reinterpret_cast<const std::byte*>(body_.data()), body_.size()};
  request.deadline = live::LiveTransport::Clock::now() + std::chrono::seconds(2);
  const std::uint64_t request_id = request.id;
  if (transport_->submit(request) != live::SubmitResult::Accepted)
    return AdapterResult::NotReady;
  if (inflight_count_ < inflight_.size()) {
    Inflight item;
    item.request_id = request_id;
    item.handle = command.request.handle;
    item.token = command.request.target_token;
    item.cancel = true;
    inflight_[inflight_count_++] = item;
  }
  return AdapterResult::Ok;
}

AdapterResult HyperliquidTradeAdapter::commit_cancel_venue(
    AdapterReservation reservation,
    const AdapterCancelVenueCommand& command) noexcept {
  if (reservation.generation != generation_ || !reserved_)
    return AdapterResult::StaleReservation;
  reserved_ = false;
  ++generation_;
  if (transport_ == nullptr || !rest_healthy_ || !session_.ready())
    return AdapterResult::NotReady;
  if (command.request.routing.kind != api::ExecutionRouteKind::Crypto)
    return AdapterResult::InvalidArgument;
  const std::string_view cloid = id_view(command.request.client_order_id);
  const std::string_view coin =
      id_view(command.request.routing.crypto.venue_symbol);
  if (strategy_instance_hash_ == 0 ||
      !cloid_owned_by(cloid, strategy_instance_hash_))
    return AdapterResult::InvalidArgument;
  bool registered = false;
  for (const RequiredInstrument& required : required_instruments_) {
    if (required.instrument_id == command.request.instrument_id &&
        id_view(required.symbol) == coin) {
      registered = true;
      break;
    }
  }
  if (!registered) return AdapterResult::InvalidArgument;
  const auto signed_cancel =
      session_.cancel_by_cloid(coin, cloid, wall_clock_ms(), mainnet_);
  if (!signed_cancel.accepted) return AdapterResult::NotReady;
  const std::string r = hex_bytes(signed_cancel.signature.data(), 32);
  const std::string s =
      hex_bytes(signed_cancel.signature.data() + 32, 32);
  body_ =
      std::string(
          "{\"action\":{\"type\":\"cancelByCloid\",\"cancels\":[{\"asset\":") +
      std::to_string(signed_cancel.asset) + ",\"cloid\":\"" +
      signed_cancel.cloid + "\"}]},\"nonce\":" +
      std::to_string(signed_cancel.nonce) +
      ",\"signature\":{\"r\":\"" + r + "\",\"s\":\"" + s +
      "\",\"v\":" + std::to_string(signed_cancel.signature[64]) + "}}";
  live::Request request;
  request.id = next_request_id_++;
  request.method = net::HttpMethod::Post;
  request.target = "/exchange";
  request.content_type = "application/json";
  request.body = {reinterpret_cast<const std::byte*>(body_.data()),
                  body_.size()};
  request.deadline =
      live::LiveTransport::Clock::now() + std::chrono::seconds(2);
  return transport_->submit(request) == live::SubmitResult::Accepted
             ? AdapterResult::Ok
             : AdapterResult::NotReady;
}

AdapterResult HyperliquidTradeAdapter::emit(const AdapterEventSink& sink) noexcept {
  std::size_t index = 0;
  while (index < pending_count_) {
    if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
    const AdapterResult result = sink.on_event(sink.context, pending_[index]);
    if (result != AdapterResult::Ok) {
      for (std::size_t next = index; next < pending_count_; ++next) {
        pending_[next - index] = pending_[next];
      }
      pending_count_ -= index;
      return result;
    }
    ++index;
  }
  pending_count_ = 0;
  return AdapterResult::Ok;
}

AdapterServiceResult HyperliquidTradeAdapter::service_io(
    std::uint64_t now_ns, std::uint32_t event_budget,
    const AdapterEventSink& sink) noexcept {
  AdapterServiceResult result;
  result.result = emit(sink);
  if (result.result != AdapterResult::Ok) return result;
  if (transport_ == nullptr) return result;

  if (!ws_started_) {
    const auto deadline =
        live::LiveTransport::Clock::now() + std::chrono::seconds(2);
    ws_started_ = transport_->start_websocket("/ws", deadline);
  }

  if (metadata_request_id_ == 0 &&
      metadata_phase_ != MetadataPhase::Complete) {
    if (metadata_phase_ == MetadataPhase::PerpDexs) {
      body_ = "{\"type\":\"perpDexs\"}";
    } else {
      if (metadata_dex_cursor_ >= perp_dexs_.size()) {
        metadata_phase_ = MetadataPhase::Complete;
        metadata_healthy_ = required_instrument_count_ != 0;
        for (std::size_t index = 0;
             metadata_healthy_ && index < required_instrument_count_;
             ++index) {
          AssetId id;
          metadata_healthy_ = session_.assets().find(
              id_view(required_instruments_[index].symbol), id);
        }
        if (metadata_healthy_) session_.seal_metadata();
      } else {
        const PerpDex& dex = perp_dexs_[metadata_dex_cursor_];
        body_ = dex.name.empty()
                    ? "{\"type\":\"metaAndAssetCtxs\"}"
                    : std::string("{\"type\":\"metaAndAssetCtxs\",\"dex\":\"") +
                          dex.name + "\"}";
      }
    }
  }
  if (metadata_request_id_ == 0 &&
      metadata_phase_ != MetadataPhase::Complete) {
    live::Request request;
    request.id = next_request_id_++;
    request.method = net::HttpMethod::Post;
    request.target = "/info";
    request.content_type = "application/json";
    request.body = {reinterpret_cast<const std::byte*>(body_.data()), body_.size()};
    request.deadline = live::LiveTransport::Clock::now() + std::chrono::seconds(2);
    if (transport_->submit(request) == live::SubmitResult::Accepted)
      metadata_request_id_ = request.id;
  }
  const std::string_view state_user = state_address();
  if (orders_queried_ && positions_queried_ && !fill_catchup_complete_ &&
      fill_catchup_request_id_ == 0 && !state_user.empty()) {
    constexpr std::uint64_t kOverlapMs = 10 * 60'000;
    const std::uint64_t cursor =
        last_fill_time_ms_ != 0 ? last_fill_time_ms_ : process_start_ms_;
    const std::uint64_t start =
        cursor > kOverlapMs ? cursor - kOverlapMs : 0;
    body_ = std::string("{\"type\":\"userFillsByTime\",\"user\":\"") +
            std::string(state_user) + "\",\"startTime\":" +
            std::to_string(start) +
            ",\"aggregateByTime\":false}";
    live::Request request;
    request.id = next_request_id_++;
    request.method = net::HttpMethod::Post;
    request.target = "/info";
    request.content_type = "application/json";
    request.body = {reinterpret_cast<const std::byte*>(body_.data()),
                    body_.size()};
    request.deadline =
        live::LiveTransport::Clock::now() + std::chrono::seconds(2);
    if (transport_->submit(request) == live::SubmitResult::Accepted)
      fill_catchup_request_id_ = request.id;
  }

  live::Event event;
  while (result.events_processed < event_budget && transport_->poll(event)) {
    if (event.kind == live::EventKind::WebSocketOpen) {
      websocket_healthy_ = true;
      if (!ws_subscribed_ && !state_user.empty()) {
        const std::string orders =
            std::string("{\"method\":\"subscribe\",\"subscription\":{\"type\":\"orderUpdates\",\"user\":\"") +
            std::string(state_user) + "\"}}";
        const std::string fills =
            std::string("{\"method\":\"subscribe\",\"subscription\":{\"type\":\"userFills\",\"user\":\"") +
            std::string(state_user) + "\"}}";
        const auto orders_bytes = std::span<const std::byte>(
            reinterpret_cast<const std::byte*>(orders.data()), orders.size());
        const auto fills_bytes = std::span<const std::byte>(
            reinterpret_cast<const std::byte*>(fills.data()), fills.size());
        if (transport_->send_websocket(net::WsOpcode::Text, orders_bytes) ==
                live::SubmitResult::Accepted &&
            transport_->send_websocket(net::WsOpcode::Text, fills_bytes) ==
                live::SubmitResult::Accepted) {
          ws_subscribed_ = true;
          session_.note_private_open();
        }
      }
    }
    if (event.kind == live::EventKind::WebSocketClosed) {
      websocket_healthy_ = false;
      ws_subscribed_ = false;
      session_.note_private_lost();
    }
    if (event.kind == live::EventKind::WebSocketMessage) {
      std::vector<UserEvent> decoded;
      const JsonParseResult parsed =
          parse_user_events(event.payload, decoded);
      if (parsed == JsonParseResult::Ok) {
        for (const UserEvent& update : decoded) {
          Binding* binding = nullptr;
          for (Binding& candidate : bindings_) {
            if (!candidate.used) continue;
            if ((!update.cloid.empty() && candidate.cloid == update.cloid) ||
                (!update.oid.empty() && candidate.oid == update.oid)) {
              binding = &candidate;
              break;
            }
          }
          if (binding == nullptr) {
            session_.note_unmapped();
            if (pending_count_ < pending_.size()) {
              AdapterEvent status = MakeEvent(AdapterEventKind::Status);
              status.status.identity = identity();
              status.status.status = AdapterStatus::Reconciling;
              status.status.reason = AdapterResult::Failed;
              status.status.event_time_ns = now_ns;
              pending_[pending_count_++] = status;
            }
            continue;
          }
          if (pending_count_ >= pending_.size()) break;
          AdapterEvent venue = MakeEvent(AdapterEventKind::Venue);
          venue.venue.handle = binding->handle;
          venue.venue.token = binding->token;
          venue.venue.client_order_id = binding->client_order_id;
          (void)copy_id(update.oid, venue.venue.venue_order_id);
          venue.venue.event_time_ns =
              update.time_ms <=
                      std::numeric_limits<std::uint64_t>::max() / 1000000ULL
                  ? update.time_ms * 1000000ULL
                  : 0;
          if (update.kind == UserEventKind::Fill) {
            const auto applied = session_.apply_fill(
                update.time_ms, update.coin, update.tid,
                binding->token.sequence, update.size);
            if (applied != Session::FillResult::Applied) continue;
            venue.venue.type = api::VenueEventType::Fill;
            venue.venue.fill_quantity = update.size;
            venue.venue.fill_price = update.price;
            (void)copy_id(update.tid, venue.venue.trade_id);
          } else {
            venue.venue.type = update.status == "open"
                                   ? api::VenueEventType::NewAck
                                   : api::VenueEventType::ReconcileTerminal;
            venue.venue.reconciled_status =
                update.status == "open"
                    ? api::OrderStatus::Open
                    : (update.status == "filled"
                           ? api::OrderStatus::Filled
                           : (update.status == "canceled"
                                  ? api::OrderStatus::Canceled
                                  : api::OrderStatus::Unknown));
          }
          pending_[pending_count_++] = venue;
        }
      }
    }
    if (event.kind == live::EventKind::HttpFailure) {
      rest_healthy_ = false;
      if (query_.active && event.request_id == query_.request_id) {
        query_ = {};
        session_.note_unmapped();
      }
      if (event.request_id == metadata_request_id_) metadata_request_id_ = 0;
      if (event.request_id == fill_catchup_request_id_) {
        fill_catchup_request_id_ = 0;
        session_.note_unmapped();
      }
    }
    if (event.kind == live::EventKind::HttpResponse && event.status_code < 500) {
      rest_healthy_ = true;
      if (event.request_id == metadata_request_id_) {
        if (metadata_phase_ == MetadataPhase::PerpDexs) {
          if (parse_perp_dexs(event.payload, perp_dexs_) !=
              JsonParseResult::Ok) {
            result.result = AdapterResult::Failed;
            return result;
          }
          metadata_phase_ = MetadataPhase::DexMetadata;
          metadata_dex_cursor_ = 0;
        } else {
          std::vector<PerpAsset> assets;
          const PerpDex& dex = perp_dexs_[metadata_dex_cursor_];
          if (parse_perp_meta(event.payload, dex.index, dex.name, assets) !=
              JsonParseResult::Ok) {
            result.result = AdapterResult::Failed;
            return result;
          }
          for (const PerpAsset& asset : assets) {
            (void)session_.add_perp(asset.symbol, asset.asset_index,
                                    asset.dex_index, dex.name,
                                    asset.size_decimals);
          }
          ++metadata_dex_cursor_;
        }
        metadata_request_id_ = 0;
        ++result.events_processed;
        continue;
      }
      if (event.request_id == fill_catchup_request_id_) {
        std::vector<UserEvent> fills;
        if (parse_user_events(event.payload, fills) != JsonParseResult::Ok) {
          fill_catchup_request_id_ = 0;
          session_.note_unmapped();
          result.result = AdapterResult::Failed;
          return result;
        }
        bool fills_mapped = true;
        for (const UserEvent& fill : fills) {
          Binding* binding = nullptr;
          for (Binding& candidate : bindings_) {
            if (!candidate.used) continue;
            if ((!fill.cloid.empty() && candidate.cloid == fill.cloid) ||
                (!fill.oid.empty() && candidate.oid == fill.oid)) {
              binding = &candidate;
              break;
            }
          }
          if (binding == nullptr) {
            // Cold-start history is audit-only. Without an in-memory binding
            // we cannot prove whether a prior process already hedged it.
            last_fill_time_ms_ =
                std::max(last_fill_time_ms_, fill.time_ms);
            continue;
          }
          const auto applied =
              session_.apply_fill(fill.time_ms, fill.coin, fill.tid,
                                  binding->token.sequence, fill.size);
          if (applied == Session::FillResult::Unknown) {
            fills_mapped = false;
            continue;
          }
          if (applied == Session::FillResult::Duplicate) continue;
          if (pending_count_ >= pending_.size()) {
            fills_mapped = false;
            break;
          }
          last_fill_time_ms_ =
              std::max(last_fill_time_ms_, fill.time_ms);
          AdapterEvent venue = MakeEvent(AdapterEventKind::Venue);
          venue.venue.type = api::VenueEventType::Fill;
          venue.venue.handle = binding->handle;
          venue.venue.token = binding->token;
          venue.venue.client_order_id = binding->client_order_id;
          venue.venue.fill_quantity = fill.size;
          venue.venue.fill_price = fill.price;
          venue.venue.event_time_ns =
              fill.time_ms <=
                      std::numeric_limits<std::uint64_t>::max() / 1000000ULL
                  ? fill.time_ms * 1000000ULL
                  : 0;
          (void)copy_id(fill.tid, venue.venue.trade_id);
          (void)copy_id(fill.oid, venue.venue.venue_order_id);
          pending_[pending_count_++] = venue;
        }
        fill_catchup_request_id_ = 0;
        fill_catchup_complete_ = fills_mapped;
        if (!session_.complete_reconcile(
                orders_mapped_ && fills_mapped, positions_mapped_)) {
          session_.note_unmapped();
        } else if (pending_count_ < pending_.size()) {
          AdapterEvent complete =
              MakeEvent(AdapterEventKind::ReconcileComplete);
          complete.reconcile.generation = generation_;
          complete.reconcile.result = AdapterResult::Ok;
          pending_[pending_count_++] = complete;
        }
        ++result.events_processed;
        continue;
      }
      if (query_.active && event.request_id == query_.request_id) {
        const std::size_t available = pending_.size() - pending_count_;
        bool mapped = true;
        std::size_t item_count = 0;
        if (query_.kind == api::QueryKind::OpenOrders) {
          std::vector<OpenOrder> orders;
          if (parse_open_orders(event.payload, orders) != JsonParseResult::Ok ||
              orders.size() + 1 > available) {
            result.result = AdapterResult::Failed;
            session_.note_unmapped();
            query_ = {};
            return result;
          }
          item_count = orders.size();
          for (const OpenOrder& order : orders) {
            const bool single =
                query_.request.request.scope ==
                api::QueryScope::SingleInstrument;
            const std::string_view requested =
                id_view(query_.request.request.instrument.crypto.venue_symbol);
            if (single && order.coin != requested) {
              --item_count;
              continue;
            }
            AdapterEvent snapshot =
                MakeEvent(AdapterEventKind::OpenOrderSnapshot);
            snapshot.open_order.query_token = query_.request.token;
            snapshot.open_order.account_id =
                query_.request.request.account_id;
            snapshot.open_order.side = order.side;
            snapshot.open_order.status = api::OrderStatus::Open;
            snapshot.open_order.quantity = order.size;
            snapshot.open_order.remaining_quantity = order.size;
            snapshot.open_order.price = order.price;
            (void)copy_id(order.oid, snapshot.open_order.venue_order_id);
            (void)copy_id(order.cloid, snapshot.open_order.client_order_id);
            const Binding* binding = nullptr;
            for (const Binding& candidate : bindings_) {
              if (!candidate.used) continue;
              if ((!order.cloid.empty() && candidate.cloid == order.cloid) ||
                  (!order.oid.empty() && candidate.oid == order.oid)) {
                binding = &candidate;
                break;
              }
            }
            if (binding == nullptr && !single) {
              const bool owned =
                  strategy_instance_hash_ != 0 &&
                  cloid_owned_by(order.cloid, strategy_instance_hash_);
              if (owned) {
                for (const RequiredInstrument& required :
                     required_instruments_) {
                  if (required.instrument_id != 0 &&
                      id_view(required.symbol) == order.coin) {
                    snapshot.open_order.instrument_id =
                        required.instrument_id;
                    break;
                  }
                }
                if (snapshot.open_order.instrument_id != 0)
                  snapshot.open_order.reserved |=
                      api::SnapshotOwnedExternal;
              }
              if (snapshot.open_order.instrument_id == 0) {
                snapshot.open_order.reserved |= api::SnapshotUnmapped;
                mapped = false;
              }
            } else if (binding == nullptr) {
              if (strategy_instance_hash_ != 0 &&
                  cloid_owned_by(order.cloid,
                                 strategy_instance_hash_)) {
                snapshot.open_order.instrument_id =
                    query_.request.request.instrument_id;
                snapshot.open_order.reserved |=
                    api::SnapshotOwnedExternal;
              } else {
                snapshot.open_order.reserved |= api::SnapshotUnmapped;
                mapped = false;
              }
            } else {
              snapshot.open_order.instrument_id = binding->instrument_id;
              snapshot.open_order.tracked_token = binding->token;
              snapshot.open_order.client_order_id =
                  binding->client_order_id;
              snapshot.open_order.reserved |= api::SnapshotTracked;
            }
            pending_[pending_count_++] = snapshot;
          }
          orders_queried_ = true;
          orders_mapped_ = mapped;
        } else {
          std::vector<Position> positions;
          if (parse_positions(event.payload, positions) != JsonParseResult::Ok ||
              positions.size() + 1 > available) {
            result.result = AdapterResult::Failed;
            session_.note_unmapped();
            query_ = {};
            return result;
          }
          item_count = positions.size();
          for (const Position& position : positions) {
            const bool single =
                query_.request.request.scope ==
                api::QueryScope::SingleInstrument;
            const std::string_view requested =
                id_view(query_.request.request.instrument.crypto.venue_symbol);
            if (single && position.coin != requested) {
              --item_count;
              continue;
            }
            AdapterEvent snapshot =
                MakeEvent(AdapterEventKind::PositionSnapshot);
            snapshot.position.query_token = query_.request.token;
            snapshot.position.account_id =
                query_.request.request.account_id;
            snapshot.position.quantity = position.size;
            const Binding* binding = nullptr;
            for (const Binding& candidate : bindings_) {
              if (candidate.used && candidate.coin == position.coin) {
                binding = &candidate;
                break;
              }
            }
            if (binding == nullptr && !single) {
              snapshot.position.reserved |= api::SnapshotUnmapped;
              mapped = false;
            } else if (binding == nullptr) {
              snapshot.position.instrument_id =
                  query_.request.request.instrument_id;
            } else {
              snapshot.position.instrument_id = binding->instrument_id;
            }
            pending_[pending_count_++] = snapshot;
          }
          positions_queried_ = true;
          positions_mapped_ = mapped;
        }
        AdapterEvent complete = MakeEvent(AdapterEventKind::QueryComplete);
        complete.query_complete.query_token = query_.request.token;
        complete.query_complete.account_id =
            query_.request.request.account_id;
        complete.query_complete.kind = query_.kind;
        pending_[pending_count_++] = complete;
        query_ = {};
        result.events_processed +=
            static_cast<std::uint32_t>(item_count + 1);
        continue;
      }
      for (std::size_t index = 0; index < inflight_count_; ++index) {
        if (inflight_[index].request_id != event.request_id) continue;
        AdapterEvent venue = MakeEvent(AdapterEventKind::Venue);
        ExchangeResponse response;
        const JsonParseResult parsed =
            parse_exchange_response(event.payload, response);
        const bool rejected = event.status_code >= 400 ||
                              parsed != JsonParseResult::Ok ||
                              response.rejected;
        if (inflight_[index].cancel) {
          venue.venue.type = rejected ? api::VenueEventType::CancelReject
                                      : api::VenueEventType::CancelAck;
          venue.venue.reconciled_status = rejected ? api::OrderStatus::Open
                                                   : api::OrderStatus::Canceled;
        } else {
          venue.venue.type = rejected ? api::VenueEventType::NewReject
                                      : api::VenueEventType::NewAck;
          venue.venue.reconciled_status = rejected ? api::OrderStatus::Rejected
                                                   : api::OrderStatus::Open;
        }
        venue.venue.handle = inflight_[index].handle;
        venue.venue.token = inflight_[index].token;
        venue.venue.client_order_id = inflight_[index].client_order_id;
        if (response.accepted) {
          (void)copy_id(response.oid, venue.venue.venue_order_id);
          for (Binding& binding : bindings_) {
            if (binding.used && binding.cloid == inflight_[index].cloid) {
              binding.oid = response.oid;
              break;
            }
          }
        }
        if (pending_count_ < pending_.size()) pending_[pending_count_++] = venue;
        inflight_[index] = inflight_[--inflight_count_];
        break;
      }
    }
    ++result.events_processed;
  }
  if (result.result == AdapterResult::Ok) result.result = emit(sink);
  return result;
}

AdapterResult HyperliquidTradeAdapter::on_deadline(
    const AdapterDeadline&, std::uint64_t, const AdapterEventSink&) noexcept {
  return AdapterResult::Ok;
}

AdapterResult HyperliquidTradeAdapter::query_open_orders(
    const AdapterQueryRequest& request, const AdapterEventSink& sink) noexcept {
  (void)sink;
  const std::string_view user = state_address();
  if (transport_ == nullptr || user.empty())
    return AdapterResult::Unsupported;
  if (query_.active) return AdapterResult::WouldBlock;
  const std::string_view dex = dex_from_query(request.request);
  body_ = std::string("{\"type\":\"openOrders\",\"user\":\"") +
          std::string(user) + "\"";
  if (!dex.empty()) body_ += ",\"dex\":\"" + std::string(dex) + "\"";
  body_ += "}";
  live::Request wire;
  wire.id = next_request_id_++;
  wire.method = net::HttpMethod::Post;
  wire.target = "/info";
  wire.content_type = "application/json";
  wire.body = {reinterpret_cast<const std::byte*>(body_.data()), body_.size()};
  wire.deadline =
      live::LiveTransport::Clock::now() + std::chrono::seconds(2);
  if (transport_->submit(wire) != live::SubmitResult::Accepted)
    return AdapterResult::WouldBlock;
  query_.active = true;
  query_.request_id = wire.id;
  query_.kind = api::QueryKind::OpenOrders;
  query_.request = request;
  return AdapterResult::Ok;
}

AdapterResult HyperliquidTradeAdapter::query_positions(
    const AdapterQueryRequest& request, const AdapterEventSink& sink) noexcept {
  (void)sink;
  const std::string_view user = state_address();
  if (transport_ == nullptr || user.empty())
    return AdapterResult::Unsupported;
  if (query_.active) return AdapterResult::WouldBlock;
  const std::string_view dex = dex_from_query(request.request);
  body_ = std::string("{\"type\":\"clearinghouseState\",\"user\":\"") +
          std::string(user) + "\"";
  if (!dex.empty()) body_ += ",\"dex\":\"" + std::string(dex) + "\"";
  body_ += "}";
  live::Request wire;
  wire.id = next_request_id_++;
  wire.method = net::HttpMethod::Post;
  wire.target = "/info";
  wire.content_type = "application/json";
  wire.body = {reinterpret_cast<const std::byte*>(body_.data()), body_.size()};
  wire.deadline =
      live::LiveTransport::Clock::now() + std::chrono::seconds(2);
  if (transport_->submit(wire) != live::SubmitResult::Accepted)
    return AdapterResult::WouldBlock;
  query_.active = true;
  query_.request_id = wire.id;
  query_.kind = api::QueryKind::Positions;
  query_.request = request;
  return AdapterResult::Ok;
}

AdapterResult HyperliquidTradeAdapter::register_instrument(
    api::InstrumentId instrument_id,
    const api::ResolvedInstrument& routing) noexcept {
  if (instrument_id == 0 ||
      routing.kind != api::ExecutionRouteKind::Crypto ||
      routing.crypto.venue_symbol.length == 0)
    return AdapterResult::InvalidArgument;
  for (std::size_t index = 0; index < required_instrument_count_; ++index) {
    if (required_instruments_[index].instrument_id == instrument_id)
      return required_instruments_[index].symbol ==
                     routing.crypto.venue_symbol
                 ? AdapterResult::Ok
                 : AdapterResult::InvalidArgument;
  }
  if (required_instrument_count_ >= required_instruments_.size())
    return AdapterResult::WouldBlock;
  required_instruments_[required_instrument_count_++] = {
      instrument_id, routing.crypto.venue_symbol};
  return AdapterResult::Ok;
}

bool HyperliquidTradeAdapter::instrument_ready(
    const api::ResolvedInstrument& routing) const noexcept {
  if (routing.kind != api::ExecutionRouteKind::Crypto ||
      status() != AdapterStatus::Ready)
    return false;
  AssetId id;
  return session_.assets().find(id_view(routing.crypto.venue_symbol), id);
}

AdapterResult HyperliquidTradeAdapter::begin_reconcile(
    std::uint64_t generation, std::uint64_t,
    const AdapterEventSink&) noexcept {
  generation_ = generation;
  ++connection_generation_;
  session_.note_private_lost();
  websocket_healthy_ = false;
  orders_queried_ = false;
  positions_queried_ = false;
  orders_mapped_ = false;
  positions_mapped_ = false;
  fill_catchup_complete_ = false;
  fill_catchup_request_id_ = 0;
  return AdapterResult::Ok;
}

AdapterResult HyperliquidTradeAdapter::shutdown(
    std::uint64_t, const AdapterEventSink&) noexcept {
  session_.note_private_lost();
  if (transport_ != nullptr) transport_->close_websocket();
  return AdapterResult::Ok;
}

}  // namespace oms::exchange::hyperliquid
