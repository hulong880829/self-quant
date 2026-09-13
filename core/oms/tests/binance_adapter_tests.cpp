#include <algorithm>
#include <array>
#include <cstdint>
#include <cstring>
#include <deque>
#include <fstream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

#include "oms/exchange/binance/trade_adapter.h"

namespace {

using namespace oms;
using namespace oms::exchange;
using namespace oms::exchange::binance;

#define REQUIRE(condition)                                                     \
  do {                                                                         \
    if (!(condition))                                                          \
      throw std::runtime_error(std::string("require failed: ") + #condition +  \
                               " at line " + std::to_string(__LINE__));        \
  } while (false)

#ifndef OMS_TEST_FIXTURE_DIR
#define OMS_TEST_FIXTURE_DIR "core/oms/tests/fixtures"
#endif

std::string Load(std::string_view relative) {
  const std::string path =
      std::string(OMS_TEST_FIXTURE_DIR) + "/" + std::string(relative);
  std::ifstream input(path);
  REQUIRE(input.good());
  return {std::istreambuf_iterator<char>(input),
          std::istreambuf_iterator<char>()};
}

std::string_view ObjectAfter(const std::string& text, std::string_view marker,
                             std::size_t occurrence = 0) {
  std::size_t marker_position = 0;
  for (std::size_t index = 0; index <= occurrence; ++index) {
    marker_position = text.find(marker, marker_position);
    REQUIRE(marker_position != std::string::npos);
    marker_position += marker.size();
  }
  const std::size_t start = text.find('{', marker_position);
  REQUIRE(start != std::string::npos);
  std::size_t depth = 0;
  bool string = false;
  bool escaped = false;
  for (std::size_t position = start; position < text.size(); ++position) {
    const char value = text[position];
    if (string) {
      if (escaped)
        escaped = false;
      else if (value == '\\')
        escaped = true;
      else if (value == '"')
        string = false;
      continue;
    }
    if (value == '"') {
      string = true;
    } else if (value == '{') {
      ++depth;
    } else if (value == '}' && --depth == 0) {
      return {text.data() + start, position - start + 1};
    }
  }
  throw std::runtime_error("unterminated fixture object");
}

template <typename Id>
Id IdFrom(std::string_view value) {
  REQUIRE(!value.empty());
  REQUIRE(value.size() <= Id{}.value.size());
  Id result{};
  std::memcpy(result.value.data(), value.data(), value.size());
  result.length = static_cast<std::uint16_t>(value.size());
  return result;
}

void TestProfilesAndSigning() {
  REQUIRE(profile(Product::Spot).place_path == "/api/v3/order");
  REQUIRE(profile(Product::Usdm).place_path == "/fapi/v1/order");
  REQUIRE(profile(Product::Spot).capabilities.supports(
      AdapterCapability::QuoteQuantity));
  REQUIRE(profile(Product::Usdm).capabilities.supports(
      AdapterCapability::ReduceOnly));
  REQUIRE(!profile(Product::Usdm).capabilities.supports(
      AdapterCapability::ClosePosition));
  api::NewOrderRequest preflight{};
  preflight.type = api::OrderType::Limit;
  preflight.time_in_force = api::TimeInForce::IOC;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Ok);
  preflight.time_in_force = api::TimeInForce::GTD;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Ok);
  preflight.type = api::OrderType::Market;
  preflight.time_in_force = api::TimeInForce::FOK;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  preflight.type = api::OrderType::Limit;
  preflight.time_in_force = api::TimeInForce::IOC;
  preflight.flags = api::PostOnly;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Unsupported);
  preflight.time_in_force = api::TimeInForce::FOK;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Unsupported);
  preflight.flags = 0;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Ok);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Ok);
  preflight.time_in_force = api::TimeInForce::GTC;
  preflight.flags = api::QuoteQuantity;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  preflight.type = api::OrderType::Market;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Ok);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Unsupported);
  preflight.flags = api::ReduceOnly;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Ok);
  preflight.type = api::OrderType::Limit;
  preflight.flags = 0;
  preflight.time_in_force = api::TimeInForce::FAK;
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::Unsupported);
  REQUIRE(preflight_place(profile(Product::Usdm).capabilities, preflight) ==
          AdapterResult::Unsupported);
  preflight.time_in_force = static_cast<api::TimeInForce>(255);
  REQUIRE(preflight_place(profile(Product::Spot).capabilities, preflight) ==
          AdapterResult::InvalidArgument);

  const std::string key(20, '\x0b');
  std::array<char, 64> digest{};
  REQUIRE(hmac_sha256_hex(key, "Hi There", digest));
  REQUIRE(std::string_view(digest.data(), digest.size()) ==
          "b0344c61d8db38535ca8afceaf0bf12b"
          "881dc200c9833da726e9376c2e32cff7");
}

PlaceParameters LimitOrder(std::uint16_t flags = 0) {
  PlaceParameters result{};
  result.symbol = "BTCUSDT";
  result.side = api::Side::Buy;
  result.type = api::OrderType::Limit;
  result.time_in_force = api::TimeInForce::GTC;
  result.flags = flags;
  result.quantity = {10000000, 8, {}};
  result.price = {1000000000000, 8, {}};
  result.client_order_id = "client-1";
  return result;
}

void TestRequestBuilders() {
  constexpr CredentialsView credentials{"api-key", "secret-key"};
  HttpRequest request{};
  RequestBuilder spot(Product::Spot);
  auto place = LimitOrder();
  REQUIRE(spot.place(place, 1499827319559ULL, 5000, credentials, request));
  REQUIRE(request.method == HttpMethod::Post);
  REQUIRE(request.api_key_header() == "X-MBX-APIKEY");
  REQUIRE(request.api_key_value == "api-key");
  REQUIRE(request.target_view().starts_with(
      "/api/v3/order?symbol=BTCUSDT&side=BUY&type=LIMIT&timeInForce=GTC"
      "&quantity=0.10000000&price=10000.00000000"
      "&newClientOrderId=client-1&timestamp=1499827319559"
      "&recvWindow=5000&signature="));
  REQUIRE(request.target_view().size() -
              request.target_view().find("&signature=") ==
          75);

  CancelParameters cancel{"BTCUSDT", {}, "28"};
  REQUIRE(spot.cancel(cancel, 1499827319559ULL, 5000, credentials, request));
  REQUIRE(request.method == HttpMethod::Delete);
  REQUIRE(request.target_view().starts_with(
      "/api/v3/order?symbol=BTCUSDT&orderId=28&timestamp="));
  REQUIRE(spot.open_orders("LTCBTC", 1499827319559ULL, 5000, credentials,
                           request));
  REQUIRE(request.method == HttpMethod::Get);
  REQUIRE(request.target_view().starts_with(
      "/api/v3/openOrders?symbol=LTCBTC&timestamp="));
  REQUIRE(spot.open_orders({}, 1499827319559ULL, 5000, credentials, request));
  REQUIRE(request.target_view().starts_with(
      "/api/v3/openOrders?timestamp=1499827319559&recvWindow=5000"));
  REQUIRE(spot.query_order(
      {"BTCUSDT", "client-1", {}}, 1499827319559ULL, 5000, credentials,
      request));
  REQUIRE(request.method == HttpMethod::Get);
  REQUIRE(request.target_view().starts_with(
      "/api/v3/order?symbol=BTCUSDT&origClientOrderId=client-1&timestamp="));

  REQUIRE(spot.create_listen_key(credentials, request));
  REQUIRE(request.method == HttpMethod::Post);
  REQUIRE(request.target_view() == "/api/v3/userDataStream");
  REQUIRE(spot.keepalive_listen_key(credentials, "spot-listen-key", request));
  REQUIRE(request.method == HttpMethod::Put);
  REQUIRE(request.target_view() ==
          "/api/v3/userDataStream?listenKey=spot-listen-key");
  REQUIRE(spot.close_listen_key(credentials, "spot-listen-key", request));
  REQUIRE(request.method == HttpMethod::Delete);
  REQUIRE(request.target_view() ==
          "/api/v3/userDataStream?listenKey=spot-listen-key");

  RequestBuilder usdm(Product::Usdm);
  REQUIRE(usdm.keepalive_listen_key(credentials, {}, request));
  REQUIRE(request.target_view() == "/fapi/v1/listenKey");
  REQUIRE(usdm.close_listen_key(credentials, {}, request));
  REQUIRE(request.target_view() == "/fapi/v1/listenKey");
  place = LimitOrder(api::OrderFlag::PostOnly | api::OrderFlag::ReduceOnly);
  REQUIRE(usdm.place(place, 1591702613943ULL, 5000, credentials, request));
  REQUIRE(request.target_view().find("&reduceOnly=true") !=
          std::string_view::npos);
  REQUIRE(request.target_view().find("&timeInForce=GTX") !=
          std::string_view::npos);
  place = LimitOrder(api::OrderFlag::ClosePosition);
  REQUIRE(!usdm.place(place, 1591702613943ULL, 5000, credentials, request));
  place = LimitOrder(api::OrderFlag::QuoteQuantity);
  REQUIRE(!spot.place(place, 1591702613943ULL, 5000, credentials, request));
  place = LimitOrder(api::OrderFlag::ReduceOnly);
  REQUIRE(!spot.place(place, 1591702613943ULL, 5000, credentials, request));
  place = LimitOrder();
  place.time_in_force = api::TimeInForce::GTD;
  place.expire_time_ns = (1591702613ULL + 601ULL) * 1000000000ULL;
  REQUIRE(usdm.place(place, 1591702613943ULL, 5000, credentials, request));
  REQUIRE(request.target_view().find("&goodTillDate=1591703214") !=
          std::string_view::npos);

  TradingRequest trading{};
  place = LimitOrder();
  place.time_in_force = api::TimeInForce::IOC;
  REQUIRE(spot.trading_place(place, 77, 1499827319559ULL, 5000,
                             credentials, trading));
  REQUIRE(trading.payload_view().starts_with(
      R"({"id":77,"method":"order.place","params":{)"));
  REQUIRE(trading.payload_view().find(R"("timeInForce":"IOC")") !=
          std::string_view::npos);
  REQUIRE(trading.payload_view().find(R"("signature":")") !=
          std::string_view::npos);
  REQUIRE(trading.payload_view().find(
              R"("signature":"6d56a04dd8f5e22277ffe5de5e91e61985eed75cea791ff60483b4dd8280e643")") !=
          std::string_view::npos);
  REQUIRE(spot.trading_cancel(cancel, 78, 1499827319559ULL, 5000,
                              credentials, trading));
  REQUIRE(trading.payload_view().find(R"("method":"order.cancel")") !=
          std::string_view::npos);

  std::uint32_t response_id = 0;
  std::uint32_t response_status = 0;
  std::string_view response_payload;
  REQUIRE(parse_trading_response(
              R"({"id":77,"status":200,"result":{"orderId":28,"clientOrderId":"client-1"}})",
              response_id, response_status, response_payload) ==
          ParseResult::Ok);
  REQUIRE(response_id == 77 && response_status == 200);
  REQUIRE(response_payload ==
          R"({"orderId":28,"clientOrderId":"client-1"})");
}

ParseContext Context() {
  ParseContext result{};
  result.handle = {3, 0, 9};
  result.token = {2, 4, 6};
  result.price_scale = 8;
  result.quantity_scale = 8;
  return result;
}

void TestFixtureParsing() {
  const auto context = Context();
  api::VenueEvent event{};

  const std::string spot_trading = Load("binance/spot_trading.json");
  REQUIRE(parse_rest_place(ObjectAfter(spot_trading, "\"response\":", 0),
                           context, event) == ParseResult::Ok);
  REQUIRE(event.type == api::VenueEventType::NewAck);
  REQUIRE(std::string_view(event.venue_order_id.value.data(),
                           event.venue_order_id.length) == "28");
  REQUIRE(event.event_time_ns == 1507725176595000000ULL);
  REQUIRE(parse_rest_cancel(ObjectAfter(spot_trading, "\"response\":", 1),
                            context, event) == ParseResult::Ok);
  REQUIRE(event.type == api::VenueEventType::CancelAck);

  const std::string usdm_trading = Load("binance/usdm_trading.json");
  REQUIRE(parse_rest_place(ObjectAfter(usdm_trading, "\"response\":", 0),
                           context, event) == ParseResult::Ok);
  REQUIRE(event.event_time_ns == 1566818724722000000ULL);
  REQUIRE(parse_rest_cancel(ObjectAfter(usdm_trading, "\"response\":", 1),
                            context, event) == ParseResult::Ok);

  const std::string spot_stream =
      Load("binance/spot_execution_report.json");
  REQUIRE(parse_spot_execution_report(
              ObjectAfter(spot_stream, "\"payload\":"), context, event) ==
          ParseResult::Ok);
  REQUIRE(event.type == api::VenueEventType::Fill);
  REQUIRE(event.reconciled_status == api::OrderStatus::PartiallyFilled);
  REQUIRE((event.fill_quantity == api::FixedPoint{10000000, 8, {}}));
  REQUIRE((event.fill_price == api::FixedPoint{10264410, 8, {}}));
  REQUIRE(std::string_view(event.trade_id.value.data(), event.trade_id.length) ==
          "114168");

  const std::string usdm_stream =
      Load("binance/usdm_order_trade_update.json");
  REQUIRE(parse_usdm_order_trade_update(
              ObjectAfter(usdm_stream, "\"payload\":"), context, event) ==
          ParseResult::Ok);
  REQUIRE(event.type == api::VenueEventType::NewAck);
  REQUIRE(event.reconciled_status == api::OrderStatus::Open);
  REQUIRE(event.event_time_ns == 1568879465651000000ULL);

  const std::string lifecycle =
      Load("binance/usdm_listen_key_lifecycle.json");
  std::array<char, kMaxListenKeyBytes> key{};
  std::uint16_t length = 0;
  REQUIRE(parse_listen_key(ObjectAfter(lifecycle, "\"response\":", 0), key,
                           length) == ParseResult::Ok);
  REQUIRE(std::string_view(key.data(), length) == "listen-key-fixture");

  const std::string queries = Load("binance/spot_queries.json");
  REQUIRE(queries.find("\"/api/v3/openOrders\"") != std::string::npos);
  REQUIRE(queries.find("\"clientOrderId\": \"myOrder1\"") !=
          std::string::npos);

  for (const std::string_view fixture :
       {"binance/spot_open_orders_snapshot.json",
        "binance/usdm_open_orders_snapshot.json"}) {
    const std::string snapshot = Load(fixture);
    OpenOrdersCursor cursor{};
    std::array<api::VenueEvent, 1> page{};
    std::size_t count = 0;
    REQUIRE(parse_open_orders_page(snapshot, cursor, page.data(), page.size(),
                                   count) == ParseResult::Ok);
    REQUIRE(count == 1);
    REQUIRE(page[0].type == api::VenueEventType::ReconcileOpen);
    REQUIRE(page[0].handle.generation == 0);
    REQUIRE(parse_open_orders_page(snapshot, cursor, page.data(), page.size(),
                                   count) == ParseResult::Ok);
    REQUIRE(count == 1);
    REQUIRE(page[0].type == api::VenueEventType::ReconcileTerminal);
    REQUIRE(page[0].reconciled_status == (fixture.starts_with("binance/spot")
                                              ? api::OrderStatus::Filled
                                              : api::OrderStatus::Expired));
    REQUIRE(parse_open_orders_page(snapshot, cursor, page.data(), page.size(),
                                   count) == ParseResult::Ok);
    REQUIRE(count == 0);
    REQUIRE(cursor.complete);
  }

  OpenOrdersCursor invalid_cursor{};
  std::array<api::VenueEvent, 1> invalid_page{};
  std::size_t invalid_count = 0;
  REQUIRE(parse_open_orders_page(
              R"([{"orderId":1,"clientOrderId":"x","status":"MYSTERY"}])",
              invalid_cursor, invalid_page.data(), invalid_page.size(),
              invalid_count) == ParseResult::Unsupported);
  api::VenueEvent queried{};
  REQUIRE(parse_order_query(
              R"({"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"client-42","price":"10000.00000000","origQty":"0.10000000","executedQty":"0.00000000","status":"NEW","updateTime":1500})",
              queried) == ParseResult::Ok);
  REQUIRE(queried.type == api::VenueEventType::ReconcileOpen);
  REQUIRE(queried.reconciled_status == api::OrderStatus::Open);
}

void TestErrorsClockAndSession() {
  VenueError error{};
  api::VenueEvent event{};
  RateLimitMetadata metadata{};
  metadata.retry_after_ms = 2500;
  metadata.used_weight_1m = 1200;
  metadata.has_retry_after = true;
  metadata.has_used_weight = true;
  REQUIRE(parse_error(R"({"code":-1021,"msg":"outside recvWindow"})", 400,
                      metadata, error) == ParseResult::Ok);
  REQUIRE(error.classification == ErrorClass::TimestampSkew);
  REQUIRE(error.rate_limit.retry_after_ms == 2500);
  REQUIRE(parse_error(R"({"code":-1003,"msg":"too many requests"})", 429,
                      metadata, error) == ParseResult::Ok);
  REQUIRE(error.classification == ErrorClass::RateLimited);
  REQUIRE(parse_error(R"({"code":-1003,"code":-1004,"msg":"duplicate"})",
                      429, metadata, error) ==
          ParseResult::DuplicateField);
  REQUIRE(parse_spot_execution_report(R"({"e":"balanceUpdate"})", Context(),
                                      event) == ParseResult::Unsupported);
  REQUIRE(classify_error(-1003, 418) == ErrorClass::Banned);
  REQUIRE(retry_disposition(AdapterCommandKind::Place,
                            ErrorClass::TimestampSkew) ==
          RetryDisposition::ReconcileBeforeRetry);
  REQUIRE(retry_disposition(AdapterCommandKind::Cancel,
                            ErrorClass::Transport) ==
          RetryDisposition::Safe);

  ServerClock clock;
  clock.observe(1000, 1020, 1110);
  REQUIRE(clock.synchronized());
  REQUIRE(clock.offset_ms() == 100);
  REQUIRE(clock.venue_time_ms(2000) == 2100);
  clock.observe(2000, 2020, 1910);
  REQUIRE(clock.offset_ms() == 50);

  ListenKeySession session;
  REQUIRE(session.start() == ListenKeyAction::Create);
  REQUIRE(session.start() == ListenKeyAction::None);
  REQUIRE(session.activated("key", 100) == true);
  REQUIRE(session.on_deadline(100 + ListenKeySession::kKeepaliveIntervalNs -
                              1) == ListenKeyAction::None);
  REQUIRE(session.on_deadline(100 + ListenKeySession::kKeepaliveIntervalNs) ==
          ListenKeyAction::Keepalive);
  session.keepalive_succeeded(200);
  REQUIRE(session.state() == ListenKeyState::Active);
  REQUIRE(session.shutdown() == ListenKeyAction::Close);
  REQUIRE(session.shutdown() == ListenKeyAction::None);

  ListenKeySession expiring;
  REQUIRE(expiring.start() == ListenKeyAction::Create);
  REQUIRE(expiring.activated("key", 10));
  REQUIRE(expiring.on_deadline(10 + ListenKeySession::kExpiryNs) ==
          ListenKeyAction::ReconnectStream);
  REQUIRE(expiring.state() == ListenKeyState::RecreateRequired);
}

struct OfflineVenue final : Transport {
  struct Pending {
    TransportEvent event{};
    std::string body{};
  };

  std::vector<std::string> targets;
  std::vector<HttpMethod> methods;
  std::vector<std::uint32_t> request_ids;
  std::vector<std::string> trading_payloads;
  std::deque<Pending> responses;
  std::string polled_body;
  std::string open_orders_body{"[]"};
  std::uint32_t next_status{200};
  std::uint32_t trading_start_calls{};
  AdapterResult next_submit{AdapterResult::Ok};
  bool auto_respond{true};
  bool emit_trading_ready{true};
  bool closed{};

  static bool ResolveCancel(void*, api::OrderHandle handle,
                            ResolvedCancel& output) noexcept {
    if (handle.generation == 0) return false;
    std::memcpy(output.symbol.data(), "BTCUSDT", 7);
    output.symbol_length = 7;
    output.client_order_id = IdFrom<api::ClientOrderId>("client-42");
    output.venue_order_id = IdFrom<api::VenueOrderId>("12345");
    return true;
  }

  std::string Body(const HttpRequest& request, std::uint32_t status) const {
    std::string_view body = "{}";
    if (request.target_view().starts_with("/api/v3/userDataStream") ||
        request.target_view() == "/fapi/v1/listenKey")
      body = request.method == HttpMethod::Post
                 ? R"({"listenKey":"offline-listen-key"})"
                 : "{}";
    else if (request.target_view() == "/api/v3/time" ||
             request.target_view() == "/fapi/v1/time")
      body = R"({"serverTime":1000})";
    else if (request.target_view().starts_with("/api/v3/openOrders") ||
             request.target_view().starts_with("/fapi/v1/openOrders"))
      body = open_orders_body;
    else if (request.method == HttpMethod::Get &&
             (request.target_view().starts_with("/api/v3/order?") ||
              request.target_view().starts_with("/fapi/v1/order?")))
      body =
          R"({"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"client-42","price":"10000.00000000","origQty":"0.10000000","executedQty":"0.00000000","status":"NEW","updateTime":1500})";
    else if (status >= 400)
      body = status == 429
                 ? R"({"code":-1003,"msg":"too many requests"})"
                 : R"({"code":-1021,"msg":"outside recvWindow"})";
    else if (request.method == HttpMethod::Delete)
      body =
          R"({"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"client-42","transactTime":1500})";
    else
      body =
          R"({"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"client-42","transactTime":1500})";
    return std::string(body);
  }

  void Respond(std::uint32_t request_id, std::uint32_t status = 200) {
    REQUIRE(!request_ids.empty());
    const std::size_t index = static_cast<std::size_t>(
        std::find(request_ids.begin(), request_ids.end(), request_id) -
        request_ids.begin());
    REQUIRE(index < request_ids.size());
    HttpRequest request{};
    request.method = methods[index];
    const std::string& target = targets[index];
    REQUIRE(target.size() <= request.target.size());
    std::memcpy(request.target.data(), target.data(), target.size());
    request.target_length = static_cast<std::uint16_t>(target.size());
    Pending pending{};
    pending.event.kind = TransportEventKind::Response;
    pending.event.request_id = request_id;
    pending.event.result = AdapterResult::Ok;
    pending.event.metadata.http_status = status;
    pending.body = Body(request, status);
    responses.push_back(std::move(pending));
  }

  void Fail(std::uint32_t request_id,
            AdapterResult result = AdapterResult::Failed) {
    Pending pending{};
    pending.event.kind = TransportEventKind::Failure;
    pending.event.request_id = request_id;
    pending.event.result = result;
    responses.push_back(std::move(pending));
  }

  AdapterResult submit(const TransportRequest& request) noexcept override {
    const AdapterResult result = next_submit;
    next_submit = AdapterResult::Ok;
    if (result != AdapterResult::Ok) return result;
    if (request.use_trading_websocket) {
      trading_payloads.emplace_back(request.trading.payload_view());
      targets.emplace_back("TRADING_WEBSOCKET");
      methods.push_back(
          request.trading.payload_view().find("\"order.cancel\"") !=
                  std::string_view::npos
              ? HttpMethod::Delete
              : HttpMethod::Post);
    } else {
      targets.emplace_back(request.wire.target_view());
      methods.push_back(request.wire.method);
    }
    request_ids.push_back(request.id);
    if (auto_respond) {
      try {
        if (request.use_trading_websocket) {
          Pending pending{};
          pending.event.kind = TransportEventKind::Response;
          pending.event.request_id = request.id;
          pending.event.result = AdapterResult::Ok;
          pending.event.metadata.http_status = next_status;
          pending.body =
              next_status >= 400
                  ? (next_status == 429
                         ? R"({"code":-1003,"msg":"too many requests"})"
                         : R"({"code":-1021,"msg":"outside recvWindow"})")
                  : R"({"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"client-42","transactTime":1500})";
          responses.push_back(std::move(pending));
        } else {
          Respond(request.id, next_status);
        }
      } catch (...) {
        return AdapterResult::Failed;
      }
      next_status = 200;
    }
    return AdapterResult::Ok;
  }

  AdapterResult poll(TransportEvent& event) noexcept override {
    if (responses.empty()) return AdapterResult::WouldBlock;
    Pending pending = std::move(responses.front());
    responses.pop_front();
    polled_body = std::move(pending.body);
    event = pending.event;
    event.payload = polled_body;
    return AdapterResult::Ok;
  }

  AdapterResult start_user_stream(std::string_view listen_key) noexcept override {
    if (listen_key.empty()) return AdapterResult::InvalidArgument;
    Pending pending{};
    pending.event.kind = TransportEventKind::SessionReady;
    pending.event.result = AdapterResult::Ok;
    responses.push_back(std::move(pending));
    return AdapterResult::Ok;
  }

  AdapterResult start_trading_stream() noexcept override {
    ++trading_start_calls;
    if (!emit_trading_ready) return AdapterResult::Ok;
    Pending pending{};
    pending.event.kind = TransportEventKind::TradingSessionReady;
    pending.event.result = AdapterResult::Ok;
    responses.push_back(std::move(pending));
    return AdapterResult::Ok;
  }

  void close() noexcept override { closed = true; }
};

struct Events {
  std::vector<AdapterEvent> values;
  bool block{};

  static AdapterResult OnEvent(void* raw,
                               const AdapterEvent& event) noexcept {
    auto& self = *static_cast<Events*>(raw);
    if (self.block) return AdapterResult::WouldBlock;
    self.values.push_back(event);
    return AdapterResult::Ok;
  }

  AdapterEventSink sink() noexcept { return {this, &Events::OnEvent}; }
};

BinanceAdapterConfig Config(OfflineVenue& venue,
                            Product product = Product::Spot) {
  BinanceAdapterConfig result{};
  result.product = product;
  result.credentials = {"offline-key", "offline-secret"};
  result.callbacks = {&venue, &OfflineVenue::ResolveCancel, nullptr};
  result.transport = &venue;
  return result;
}

AdapterPlaceCommand PlaceCommand() {
  AdapterPlaceCommand command{};
  command.command_id = 42;
  command.handle = {4, 0, 8};
  command.request.token = {1, 2, 42};
  command.request.client_order_id =
      IdFrom<api::ClientOrderId>("client-42");
  command.request.instrument_id = 7;
  command.request.side = api::Side::Buy;
  command.request.type = api::OrderType::Limit;
  command.request.time_in_force = api::TimeInForce::GTC;
  command.request.quantity = {10000000, 8, {}};
  command.request.price = {1000000000000, 8, {}};
  command.routing.kind = api::ExecutionRouteKind::Crypto;
  command.routing.venue =
      static_cast<std::uint8_t>(utils::md::Venue::Binance);
  command.routing.product_type =
      static_cast<std::uint8_t>(utils::md::ProductType::Spot);
  command.routing.catalog_revision = 1;
  command.routing.crypto.venue_symbol =
      IdFrom<api::VenueSymbol>("BTCUSDT");
  return command;
}

void TestTradeAdapterOffline() {
  OfflineVenue venue;
  Events events;
  BinanceTradeAdapter adapter(Config(venue));
  TradeAdapter& contract = adapter;
  REQUIRE(contract.status() == AdapterStatus::Authenticating);
  AdapterReservation reservation{};
  REQUIRE(contract.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::NotReady);
  REQUIRE(contract.identity().kind == AdapterKind::BinanceSpot);

  auto serviced = contract.service_io(1000000000ULL, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(adapter.listen_key_state() == ListenKeyState::Active);
  REQUIRE(contract.status() == AdapterStatus::Ready);
  REQUIRE(venue.targets.size() >= 2);
  REQUIRE(venue.targets[0] == "/api/v3/time");
  REQUIRE(venue.targets[1] == "/api/v3/userDataStream");

  REQUIRE(contract.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  const AdapterReservation stale = reservation;
  REQUIRE(contract.commit_place(reservation, PlaceCommand()) ==
          AdapterResult::Ok);
  REQUIRE(contract.commit_place(stale, PlaceCommand()) ==
          AdapterResult::StaleReservation);
  serviced = contract.service_io(2000000000ULL, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(events.values.size() == 2);
  REQUIRE(events.values[0].kind == AdapterEventKind::CommandResult);
  REQUIRE(events.values[0].command_result.result == AdapterResult::Ok);
  REQUIRE(events.values[1].kind == AdapterEventKind::Venue);
  REQUIRE(events.values[1].venue.type == api::VenueEventType::NewAck);
  REQUIRE(venue.targets.back() == "TRADING_WEBSOCKET");
  REQUIRE(venue.trading_payloads.back().find(
              R"("method":"order.place")") != std::string::npos);
  REQUIRE(std::none_of(venue.targets.begin(), venue.targets.end(),
                       [](const std::string& target) {
                         return target.starts_with("/api/v3/order?");
                       }));

  AdapterCancelCommand cancel{};
  cancel.command_id = 43;
  cancel.request.request_token = {1, 2, 43};
  cancel.request.target_token = {1, 2, 42};
  cancel.request.handle = {4, 0, 8};
  REQUIRE(contract.reserve_command(AdapterCommandKind::Cancel, reservation) ==
          AdapterResult::Ok);
  REQUIRE(contract.commit_cancel(reservation, cancel) == AdapterResult::Ok);
  serviced = contract.service_io(3000000000ULL, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(events.values.back().venue.type == api::VenueEventType::CancelAck);
  REQUIRE(venue.targets.back() == "TRADING_WEBSOCKET");
  REQUIRE(venue.trading_payloads.back().find(
              R"("method":"order.cancel")") != std::string::npos);

  const std::string stream = Load("binance/spot_execution_report.json");
  REQUIRE(adapter.ingest_user_stream(ObjectAfter(stream, "\"payload\":"),
                                     Context()) == AdapterResult::Ok);
  serviced = contract.service_io(4000000000ULL, 1, events.sink());
  REQUIRE(serviced.events_processed == 1);
  REQUIRE(events.values.back().venue.type == api::VenueEventType::Fill);

  const std::uint64_t keepalive =
      1000000000ULL + ListenKeySession::kKeepaliveIntervalNs;
  AdapterDeadline deadline{};
  deadline.kind = AdapterDeadlineKind::Keepalive;
  deadline.due_time_ns = keepalive;
  REQUIRE(contract.on_deadline(deadline, keepalive, events.sink()) ==
          AdapterResult::Ok);
  REQUIRE(venue.methods.back() == HttpMethod::Put);
  serviced = contract.service_io(keepalive, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);

  venue.open_orders_body = Load("binance/spot_open_orders_snapshot.json");
  const std::size_t before_reconcile = events.values.size();
  REQUIRE(contract.begin_reconcile(9, 5000000000ULL, events.sink()) ==
          AdapterResult::Ok);
  serviced = contract.service_io(5000000000ULL, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(events.values.size() == before_reconcile + 3);
  REQUIRE(events.values[before_reconcile].kind == AdapterEventKind::Venue);
  REQUIRE(events.values[before_reconcile].venue.type ==
          api::VenueEventType::ReconcileOpen);
  REQUIRE(events.values[before_reconcile + 1].venue.type ==
          api::VenueEventType::ReconcileTerminal);
  REQUIRE(events.values[before_reconcile + 1].venue.handle.generation == 0);
  REQUIRE(events.values.back().kind == AdapterEventKind::ReconcileComplete);
  REQUIRE(events.values.back().reconcile.generation == 9);
  REQUIRE(contract.status() == AdapterStatus::Ready);

  events.block = true;
  REQUIRE(adapter.ingest_user_stream(ObjectAfter(stream, "\"payload\":"),
                                     Context()) == AdapterResult::Ok);
  serviced = contract.service_io(6000000000ULL, 1, events.sink());
  REQUIRE(serviced.result == AdapterResult::WouldBlock);
  REQUIRE(adapter.pending_events() == 1);
  events.block = false;
  serviced = contract.service_io(6000000000ULL, 1, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(adapter.pending_events() == 0);

  venue.next_submit = AdapterResult::Failed;
  deadline.due_time_ns = keepalive + ListenKeySession::kKeepaliveIntervalNs;
  REQUIRE(contract.on_deadline(deadline, deadline.due_time_ns, events.sink()) ==
          AdapterResult::Failed);
  REQUIRE(contract.status() == AdapterStatus::Reconnecting);
  REQUIRE(contract.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::NotReady);
  REQUIRE(contract.reserve_command(AdapterCommandKind::Cancel, reservation) ==
          AdapterResult::NotReady);

  const std::size_t requests_before_reconnect = venue.targets.size();
  deadline.kind = AdapterDeadlineKind::Reconnect;
  deadline.due_time_ns += 1;
  REQUIRE(contract.on_deadline(deadline, deadline.due_time_ns, events.sink()) ==
          AdapterResult::NotReady);
  REQUIRE(venue.targets.size() == requests_before_reconnect);
  deadline.due_time_ns +=
      BinanceTradeAdapter::kReconnectInitialBackoffNs;
  REQUIRE(contract.on_deadline(deadline, deadline.due_time_ns, events.sink()) ==
          AdapterResult::Ok);
  serviced = contract.service_io(deadline.due_time_ns, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(contract.status() == AdapterStatus::Ready);
  REQUIRE(venue.methods[venue.methods.size() - 2] == HttpMethod::Post);
  REQUIRE(venue.targets.back().starts_with("/api/v3/openOrders"));

  REQUIRE(contract.shutdown(7000000000ULL, events.sink()) ==
          AdapterResult::Ok);
  REQUIRE(contract.status() == AdapterStatus::Stopped);
  REQUIRE(venue.closed);
}

void TestUsdmAdapterStream() {
  OfflineVenue venue;
  Events events;
  BinanceTradeAdapter adapter(Config(venue, Product::Usdm));
  REQUIRE(adapter.service_io(100, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.identity().kind == AdapterKind::BinanceUsdm);
  REQUIRE(venue.targets.size() >= 2);
  REQUIRE(venue.targets[0] == "/fapi/v1/time");
  REQUIRE(venue.targets[1] == "/fapi/v1/listenKey");
  const std::string stream =
      Load("binance/usdm_order_trade_update.json");
  REQUIRE(adapter.ingest_user_stream(ObjectAfter(stream, "\"payload\":"),
                                     Context()) == AdapterResult::Ok);
  REQUIRE(adapter.service_io(101, 1, events.sink()).events_processed == 1);
  REQUIRE(events.values.back().venue.type == api::VenueEventType::NewAck);
  REQUIRE(adapter.shutdown(102, events.sink()) == AdapterResult::Ok);
}

void TestTradingSessionRequired() {
  OfflineVenue venue;
  venue.emit_trading_ready = false;
  Events events;
  BinanceTradeAdapter adapter(Config(venue));
  REQUIRE(adapter.service_io(100, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::NotReady);
  REQUIRE(venue.trading_payloads.empty());
}

void TestTradingSessionReconnect() {
  OfflineVenue venue;
  Events events;
  BinanceTradeAdapter adapter(Config(venue));
  REQUIRE(adapter.service_io(100, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(venue.trading_start_calls == 1);

  OfflineVenue::Pending lost{};
  lost.event.kind = TransportEventKind::TradingSessionLost;
  lost.event.result = AdapterResult::NotReady;
  venue.responses.push_back(std::move(lost));
  REQUIRE(adapter.service_io(200, 8, events.sink()).result ==
          AdapterResult::NotReady);
  REQUIRE(adapter.status() == AdapterStatus::Reconnecting);
  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::NotReady);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Cancel, reservation) ==
          AdapterResult::NotReady);

  AdapterDeadline reconnect{};
  reconnect.kind = AdapterDeadlineKind::Reconnect;
  reconnect.due_time_ns = 200 + BinanceTradeAdapter::kReconnectInitialBackoffNs;
  REQUIRE(adapter.on_deadline(reconnect, reconnect.due_time_ns - 1,
                              events.sink()) == AdapterResult::InvalidArgument);
  reconnect.due_time_ns -= 1;
  REQUIRE(adapter.on_deadline(reconnect,
                              200 + BinanceTradeAdapter::kReconnectInitialBackoffNs,
                              events.sink()) == AdapterResult::Ok);
  REQUIRE(venue.trading_start_calls == 2);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(adapter.service_io(
              200 + BinanceTradeAdapter::kReconnectInitialBackoffNs, 8,
              events.sink())
              .result == AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
}

void TestOrderingAndUnknownOrderAudit() {
  OfflineVenue venue;
  Events events;
  BinanceTradeAdapter adapter(Config(venue));
  REQUIRE(adapter.service_io(100, 1, events.sink()).result ==
          AdapterResult::Ok);

  const std::string stream = Load("binance/spot_execution_report.json");
  ParseContext unknown{};
  unknown.price_scale = 8;
  unknown.quantity_scale = 8;
  REQUIRE(adapter.ingest_user_stream(ObjectAfter(stream, "\"payload\":"),
                                     unknown) == AdapterResult::Ok);
  venue.open_orders_body = Load("binance/spot_open_orders_snapshot.json");
  REQUIRE(adapter.begin_reconcile(77, 101, events.sink()) ==
          AdapterResult::WouldBlock);
  REQUIRE(adapter.service_io(102, 1, events.sink()).events_processed == 1);
  REQUIRE(events.values.back().kind == AdapterEventKind::Venue);
  REQUIRE(events.values.back().venue.type == api::VenueEventType::Fill);
  REQUIRE(events.values.back().venue.handle.generation == 0);

  REQUIRE(adapter.begin_reconcile(77, 103, events.sink()) ==
          AdapterResult::Ok);
  REQUIRE(adapter.service_io(103, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(events.values[events.values.size() - 3].venue.type ==
          api::VenueEventType::ReconcileOpen);
  REQUIRE(events.values[events.values.size() - 3].venue.handle.generation == 0);
  REQUIRE(events.values.back().kind == AdapterEventKind::ReconcileComplete);
}

void TestAsyncTransportContract() {
  OfflineVenue venue;
  venue.auto_respond = false;
  Events events;
  BinanceAdapterConfig config = Config(venue);
  config.request_timeout_ns = 1000;
  BinanceTradeAdapter adapter(config);

  auto serviced = adapter.service_io(10, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(venue.request_ids.size() == 1);
  venue.Respond(venue.request_ids.back());
  REQUIRE(adapter.service_io(11, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(venue.request_ids.size() == 2);
  venue.Respond(venue.request_ids.back());
  REQUIRE(adapter.service_io(12, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Ready);

  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, PlaceCommand()) ==
          AdapterResult::Ok);
  const std::size_t accepted_before = venue.request_ids.size();
  venue.next_submit = AdapterResult::WouldBlock;
  serviced = adapter.service_io(20, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::WouldBlock);
  REQUIRE(venue.request_ids.size() == accepted_before);
  REQUIRE(adapter.pending_commands() == 1);
  REQUIRE(events.values.empty());

  serviced = adapter.service_io(21, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(venue.request_ids.size() == accepted_before + 1);
  const std::uint32_t accepted_id = venue.request_ids.back();
  REQUIRE(adapter.pending_commands() == 1);
  REQUIRE(events.values.empty());

  venue.Respond(accepted_id);
  serviced = adapter.service_io(22, 8, events.sink());
  REQUIRE(serviced.result == AdapterResult::Ok);
  REQUIRE(adapter.pending_commands() == 0);
  REQUIRE(events.values.size() == 2);
  REQUIRE(events.values[0].kind == AdapterEventKind::CommandResult);
  REQUIRE(events.values[1].kind == AdapterEventKind::Venue);

  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  AdapterPlaceCommand timeout_place = PlaceCommand();
  timeout_place.command_id = 44;
  timeout_place.request.token = {1, 2, 44};
  REQUIRE(adapter.commit_place(reservation, timeout_place) ==
          AdapterResult::Ok);
  REQUIRE(adapter.service_io(30, 8, events.sink()).result ==
          AdapterResult::Ok);
  const std::uint32_t timed_out_id = venue.request_ids.back();
  AdapterDeadline timeout{};
  timeout.kind = AdapterDeadlineKind::Request;
  timeout.id = timed_out_id;
  timeout.generation = reservation.generation;
  timeout.due_time_ns = 1030;
  REQUIRE(adapter.on_deadline(timeout, 1030, events.sink()) ==
          AdapterResult::Failed);
  REQUIRE(adapter.pending_commands() == 0);
  REQUIRE(adapter.status() == AdapterStatus::Reconciling);
  REQUIRE(adapter.service_io(1030, 1, events.sink()).events_processed == 1);
  REQUIRE(events.values.back().command_result.command_id == 44);
  const std::size_t requests_before_reconcile = venue.request_ids.size();
  REQUIRE(adapter.service_io(1031, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(venue.request_ids.size() == requests_before_reconcile + 1);
  REQUIRE(venue.targets.back().starts_with("/api/v3/openOrders"));
  REQUIRE(adapter.status() == AdapterStatus::Reconciling);

  const std::size_t event_count = events.values.size();
  venue.Respond(timed_out_id);
  OfflineVenue::Pending unknown{};
  unknown.event.kind = TransportEventKind::Response;
  unknown.event.request_id = 0xf00d;
  unknown.event.result = AdapterResult::Ok;
  unknown.event.metadata.http_status = 200;
  unknown.body = "{}";
  venue.responses.push_back(std::move(unknown));
  REQUIRE(adapter.service_io(1031, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(events.values.size() == event_count);

  const std::uint32_t reconcile_id = venue.request_ids.back();
  venue.Respond(reconcile_id);
  REQUIRE(adapter.service_io(1032, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(venue.targets.back().starts_with(
      "/api/v3/order?symbol=BTCUSDT&origClientOrderId=client-42"));
  const std::uint32_t query_id = venue.request_ids.back();
  OfflineVenue::Pending queried{};
  queried.event.kind = TransportEventKind::Response;
  queried.event.request_id = query_id;
  queried.event.result = AdapterResult::Ok;
  queried.event.metadata.http_status = 200;
  queried.body =
      R"({"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"client-42","price":"10000.00000000","origQty":"0.10000000","executedQty":"0.00000000","status":"NEW","updateTime":1500})";
  venue.responses.push_back(std::move(queried));
  REQUIRE(adapter.service_io(1033, 8, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(std::any_of(events.values.begin(), events.values.end(),
                      [](const AdapterEvent& value) {
                        return value.kind == AdapterEventKind::Venue &&
                               value.venue.type ==
                                   api::VenueEventType::ReconcileOpen &&
                               value.venue.handle.generation != 0;
                      }));

  OfflineVenue failed_venue;
  failed_venue.auto_respond = false;
  Events failed_events;
  BinanceTradeAdapter failed_adapter(Config(failed_venue));
  REQUIRE(failed_adapter.service_io(1, 8, failed_events.sink()).result ==
          AdapterResult::Ok);
  failed_venue.Respond(failed_venue.request_ids.back());
  REQUIRE(failed_adapter.service_io(2, 8, failed_events.sink()).result ==
          AdapterResult::Ok);
  failed_venue.Respond(failed_venue.request_ids.back());
  REQUIRE(failed_adapter.service_io(2, 8, failed_events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(failed_adapter.reserve_command(AdapterCommandKind::Place,
                                         reservation) == AdapterResult::Ok);
  REQUIRE(failed_adapter.commit_place(reservation, PlaceCommand()) ==
          AdapterResult::Ok);
  REQUIRE(failed_adapter.service_io(3, 8, failed_events.sink()).result ==
          AdapterResult::Ok);
  failed_venue.Fail(failed_venue.request_ids.back());
  REQUIRE(failed_adapter.service_io(4, 8, failed_events.sink()).result ==
          AdapterResult::Failed);
  REQUIRE(failed_adapter.status() == AdapterStatus::Reconciling);
  REQUIRE(failed_adapter.pending_commands() == 0);
  REQUIRE(failed_events.values.size() == 1);
}

void TestAdapterErrors() {
  OfflineVenue venue;
  Events events;
  BinanceTradeAdapter adapter(Config(venue));
  REQUIRE(adapter.service_io(1, 1, events.sink()).result ==
          AdapterResult::Ok);
  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, PlaceCommand()) ==
          AdapterResult::Ok);
  venue.next_status = 429;
  const auto result = adapter.service_io(2, 4, events.sink());
  REQUIRE(result.result == AdapterResult::Failed);
  REQUIRE(events.values.size() == 1);
  REQUIRE(events.values[0].command_result.venue_code == -1003);
  REQUIRE(adapter.status() == AdapterStatus::Backpressured);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::WouldBlock);
  REQUIRE(adapter.service_io(1'000'000'002ULL, 4, events.sink()).result ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Ready);

  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, PlaceCommand()) ==
          AdapterResult::Ok);
  venue.next_status = 400;
  const auto skewed = adapter.service_io(1'000'000'003ULL, 4, events.sink());
  REQUIRE(skewed.result != AdapterResult::InvalidArgument);
  for (std::uint64_t step = 0;
       step < 8 && adapter.status() != AdapterStatus::Ready; ++step) {
    const auto recovered =
        adapter.service_io(1'000'000'004ULL + step, 8, events.sink());
    REQUIRE(recovered.result != AdapterResult::InvalidArgument);
  }
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(std::find(venue.targets.begin(), venue.targets.end(),
                    "/api/v3/time") != venue.targets.end());
  REQUIRE(std::any_of(venue.targets.begin(), venue.targets.end(),
                      [](const std::string& target) {
                        return target.starts_with("/api/v3/openOrders");
                      }));
  REQUIRE(venue.targets.back().starts_with("/api/v3/order?"));
}

}  // namespace

int main() {
  TestProfilesAndSigning();
  TestRequestBuilders();
  TestFixtureParsing();
  TestErrorsClockAndSession();
  TestTradeAdapterOffline();
  TestUsdmAdapterStream();
  TestTradingSessionRequired();
  TestTradingSessionReconnect();
  TestOrderingAndUnknownOrderAudit();
  TestAsyncTransportContract();
  TestAdapterErrors();
  return 0;
}
