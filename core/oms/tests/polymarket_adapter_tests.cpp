#include <algorithm>
#include <array>
#include <charconv>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <limits>
#include <stdexcept>
#include <string>
#include <string_view>
#include <tuple>
#include <type_traits>

#include "oms/exchange/polymarket/crypto.h"
#include "oms/exchange/polymarket/protocol.h"
#include "oms/exchange/polymarket/trade_adapter.h"

#ifndef OMS_TEST_FIXTURE_DIR
#define OMS_TEST_FIXTURE_DIR "core/oms/tests/fixtures"
#endif

namespace {

#define REQUIRE(condition)                                                     \
  do {                                                                         \
    if (!(condition))                                                          \
      throw std::runtime_error(std::string("require failed: ") + #condition +  \
                               " at line " + std::to_string(__LINE__));        \
  } while (false)

using namespace oms;
using namespace oms::exchange;
using namespace oms::exchange::polymarket;

constexpr std::string_view kPrivateKey =
    "4f3edf983ac63ad7c7a0f4a1c2e8b7f5f6f0f4f0a3a5b6c7d8e9f00112233445";
constexpr std::string_view kSigner =
    "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1";
constexpr std::string_view kToken =
    "102936000000000000000000000000000000000000000000000000000000000000000000";
constexpr std::string_view kClobAuthPrivateKey =
    "0000000000000000000000000000000000000000000000000000000000000001";
constexpr std::string_view kClobAuthTimestamp = "1786186200";
constexpr std::string_view kClobAuthAddress =
    "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf";
constexpr std::string_view kClobAuthSignature =
    "0x25c2c4ae6e0cf49450d82e78426929f4ddc2ef8b159b43a3c0229ba7d02121e57503ff9e543acbe45d6ae20fd7d607c1f76660e49f09d2b5c38c857439b1cef51c";
constexpr std::string_view kDeriveCredentialsJson =
    R"({"apiKey":"key","secret":"c2VjcmV0LWtleQ","passphrase":"passphrase"})";
constexpr std::string_view kEmptyOpenOrders =
    R"({"next_cursor":"LTE=","data":[]})";

static_assert(std::tuple_size_v<decltype(TransportRequest{}.poly_signature.value)> ==
              45);
static_assert(std::tuple_size_v<decltype(ClobAuthRequest{}.poly_signature.value)> ==
              132);

static_assert(!std::is_copy_constructible_v<OrderSigningContext>);
static_assert(!std::is_move_constructible_v<OrderSigningContext>);

std::string LoadFixture(std::string_view relative) {
  const std::string path =
      std::string(OMS_TEST_FIXTURE_DIR) + "/" + std::string(relative);
  std::ifstream input(path);
  REQUIRE(input.good());
  return {std::istreambuf_iterator<char>(input),
          std::istreambuf_iterator<char>()};
}

std::string_view Vector(std::string_view json, std::string_view id) {
  const std::string needle = "\"id\": \"" + std::string(id) + "\"";
  const std::size_t begin = json.find(needle);
  REQUIRE(begin != std::string_view::npos);
  const std::size_t next = json.find("\"id\": \"", begin + needle.size());
  return json.substr(begin, next == std::string_view::npos ? next
                                                           : next - begin);
}

std::string_view StringField(std::string_view json, std::string_view key) {
  const std::string needle = "\"" + std::string(key) + "\"";
  std::size_t position = json.find(needle);
  REQUIRE(position != std::string_view::npos);
  position = json.find(':', position + needle.size());
  REQUIRE(position != std::string_view::npos);
  position = json.find('"', position + 1);
  REQUIRE(position != std::string_view::npos);
  const std::size_t end = json.find('"', position + 1);
  REQUIRE(end != std::string_view::npos);
  return json.substr(position + 1, end - position - 1);
}

std::uint64_t UintField(std::string_view json, std::string_view key) {
  const std::string_view field = StringField(json, key);
  std::uint64_t output = 0;
  const auto parsed =
      std::from_chars(field.data(), field.data() + field.size(), output);
  REQUIRE(parsed.ec == std::errc{});
  REQUIRE(parsed.ptr == field.data() + field.size());
  return output;
}

std::string Hex(const std::uint8_t* bytes, std::size_t size) {
  constexpr char digits[] = "0123456789abcdef";
  std::string output = "0x";
  output.reserve(size * 2 + 2);
  for (std::size_t index = 0; index < size; ++index) {
    output.push_back(digits[bytes[index] >> 4U]);
    output.push_back(digits[bytes[index] & 0x0fU]);
  }
  return output;
}

Order VectorOrder(std::uint8_t signature_type, std::string_view maker) {
  Order order{};
  order.salt = 479249096354ULL;
  REQUIRE(DecodeAddress(maker, order.maker) == CryptoResult::Ok);
  REQUIRE(DecodeAddress(maker, order.signer) == CryptoResult::Ok);
  REQUIRE(TokenIdFromDecimal(kToken, order.token_id) == CryptoResult::Ok);
  order.maker_amount = 5200000;
  order.taker_amount = 10000000;
  order.side = 0;
  order.signature_type = signature_type;
  order.timestamp_ms = 1786186200000ULL;
  return order;
}

void TestSigningVectors() {
  const std::string fixture = LoadFixture("polymarket/signing_vectors.json");
  const std::string_view l2_vector =
      Vector(fixture, "polymarket.signing.l2_hmac.v1");
  std::array<char, 45> l2{};
  std::size_t l2_size = 0;
  REQUIRE(L2Signature(StringField(l2_vector, "secret_base64url"),
                      StringField(l2_vector, "timestamp"),
                      StringField(l2_vector, "method"),
                      StringField(l2_vector, "path"),
                      StringField(l2_vector, "body"), l2.data(), l2.size(),
                      l2_size) == CryptoResult::Ok);
  REQUIRE(std::string_view(l2.data(), l2_size) ==
          StringField(l2_vector, "signature_base64url"));
  REQUIRE(L2Signature("c2VjcmV0LWtleQ===", "1786186200", "GET",
                      "/balance-allowance", "", l2.data(), l2.size(),
                      l2_size) == CryptoResult::InvalidArgument);

  Signature signature{};
  const std::string_view eoa_vector =
      Vector(fixture, "polymarket.signing.v2_eip712.v1");
  Order order{};
  order.salt = UintField(eoa_vector, "salt");
  REQUIRE(DecodeAddress(StringField(eoa_vector, "maker"), order.maker) ==
          CryptoResult::Ok);
  REQUIRE(DecodeAddress(StringField(eoa_vector, "signer"), order.signer) ==
          CryptoResult::Ok);
  REQUIRE(TokenIdFromDecimal(StringField(eoa_vector, "tokenId"),
                             order.token_id) == CryptoResult::Ok);
  order.maker_amount = UintField(eoa_vector, "makerAmount");
  order.taker_amount = UintField(eoa_vector, "takerAmount");
  order.side = static_cast<std::uint8_t>(UintField(eoa_vector, "side"));
  order.signature_type =
      static_cast<std::uint8_t>(UintField(eoa_vector, "signatureType"));
  order.timestamp_ms = UintField(eoa_vector, "timestamp");
  REQUIRE(DecodeHex32(StringField(eoa_vector, "metadata"), order.metadata) ==
          CryptoResult::Ok);
  REQUIRE(DecodeHex32(StringField(eoa_vector, "builder"), order.builder) ==
          CryptoResult::Ok);
  REQUIRE(SignOrder(order, StringField(eoa_vector, "private_key_hex"),
                    signature) == CryptoResult::Ok);
  REQUIRE(signature.size == 65);
  REQUIRE(Hex(signature.bytes.data(), signature.size) ==
          StringField(eoa_vector, "signature_hex"));
  OrderSigningContext signing_context(
      StringField(eoa_vector, "private_key_hex"));
  REQUIRE(signing_context.status() == CryptoResult::Ok);
  Signature context_signature{};
  REQUIRE(SignOrder(order, signing_context, context_signature) ==
          CryptoResult::Ok);
  REQUIRE(context_signature.size == signature.size);
  REQUIRE(context_signature.bytes == signature.bytes);
  context_signature = {};
  REQUIRE(SignOrder(order, signing_context, context_signature) ==
          CryptoResult::Ok);
  REQUIRE(Hex(context_signature.bytes.data(), context_signature.size) ==
          StringField(eoa_vector, "signature_hex"));

  const std::string_view deposit_vector =
      Vector(fixture, "polymarket.signing.deposit_wallet_erc7739.v1");
  order.signature_type =
      static_cast<std::uint8_t>(UintField(deposit_vector, "signatureType"));
  REQUIRE(DecodeAddress(StringField(deposit_vector, "maker"), order.maker) ==
          CryptoResult::Ok);
  REQUIRE(DecodeAddress(StringField(deposit_vector, "signer"), order.signer) ==
          CryptoResult::Ok);
  REQUIRE(SignOrder(order, StringField(deposit_vector, "private_key_hex"),
                    signature) == CryptoResult::Ok);
  REQUIRE(Hex(signature.bytes.data(), signature.size) ==
          StringField(deposit_vector, "signature_hex"));
  REQUIRE(SignOrder(order, signing_context, context_signature) ==
          CryptoResult::Ok);
  REQUIRE(context_signature.size == signature.size);
  REQUIRE(context_signature.bytes == signature.bytes);

  const std::string_view proxy_vector =
      Vector(fixture, "polymarket.signing.poly_proxy.v1");
  order.signature_type =
      static_cast<std::uint8_t>(UintField(proxy_vector, "signatureType"));
  REQUIRE(DecodeAddress(StringField(proxy_vector, "maker"), order.maker) ==
          CryptoResult::Ok);
  REQUIRE(DecodeAddress(StringField(proxy_vector, "signer"), order.signer) ==
          CryptoResult::Ok);
  REQUIRE(SignOrder(order, StringField(proxy_vector, "private_key_hex"),
                    signature) == CryptoResult::Ok);
  REQUIRE(signature.size == 65);
  REQUIRE(Hex(signature.bytes.data(), signature.size) ==
          StringField(proxy_vector, "signature_hex"));
  REQUIRE(SignOrder(order, signing_context, context_signature) ==
          CryptoResult::Ok);
  REQUIRE(context_signature.size == signature.size);
  REQUIRE(context_signature.bytes == signature.bytes);

  order = VectorOrder(2, kSigner);
  signature.size = 99;
  REQUIRE(SignOrder(order, kPrivateKey, signature) ==
          CryptoResult::InvalidArgument);
  REQUIRE(signature.size == 0);
  REQUIRE(OrderSigningHash(order) == Bytes32{});
  REQUIRE(SignOrder(order, signing_context, signature) ==
          CryptoResult::InvalidArgument);
  REQUIRE(signature.size == 0);
  REQUIRE(ValidatePrivateKey(std::string(64, '0')) ==
          CryptoResult::InvalidArgument);
  OrderSigningContext invalid_context(std::string(64, '0'));
  REQUIRE(invalid_context.status() == CryptoResult::InvalidArgument);
  order.signature_type = 0;
  signature.size = 99;
  REQUIRE(SignOrder(order, invalid_context, signature) ==
          CryptoResult::InvalidArgument);
  REQUIRE(signature.size == 0);

  OrderSigningContext derived_context(kPrivateKey);
  REQUIRE(derived_context.status() == CryptoResult::Ok);
  const Address derived = derived_context.signer_address();
  REQUIRE(derived != Address{});
  OrderSigningContext prefixed("0x" + std::string(kPrivateKey));
  REQUIRE(prefixed.status() == CryptoResult::Ok);
  REQUIRE(prefixed.signer_address() == derived);
  OrderSigningContext upper_prefixed("0X" + std::string(kPrivateKey));
  REQUIRE(upper_prefixed.status() == CryptoResult::Ok);
  REQUIRE(upper_prefixed.signer_address() == derived);
  REQUIRE(ValidatePrivateKey("not-hex") == CryptoResult::InvalidArgument);
  REQUIRE(ValidatePrivateKey(std::string(63, 'a')) ==
          CryptoResult::InvalidArgument);
  REQUIRE(ValidatePrivateKey(std::string(64, 'f')) ==
          CryptoResult::InvalidArgument);

  OrderSigningContext clob_context(kClobAuthPrivateKey);
  REQUIRE(clob_context.status() == CryptoResult::Ok);
  std::array<char, 42> eip55{};
  std::size_t eip55_size = 0;
  REQUIRE(EncodeEip55Address(clob_context.signer_address(), eip55.data(),
                             eip55.size(), eip55_size) == CryptoResult::Ok);
  REQUIRE(std::string_view(eip55.data(), eip55_size) == kClobAuthAddress);
  std::array<char, 132> clob_signature{};
  std::size_t clob_signature_size = 0;
  REQUIRE(clob_context.SignClobAuth(kClobAuthTimestamp, 0, clob_signature.data(),
                                    clob_signature.size(),
                                    clob_signature_size) == CryptoResult::Ok);
  REQUIRE(clob_signature_size == 132);
  REQUIRE(std::string_view(clob_signature.data(), clob_signature_size) ==
          kClobAuthSignature);
}

void TestProtocolFixtures() {
  Order order = VectorOrder(0, kSigner);
  Signature signature{};
  REQUIRE(SignOrder(order, kPrivateKey, signature) == CryptoResult::Ok);
  WireRequest request{};
  REQUIRE(BuildPlaceOrder({order, signature, "key", api::TimeInForce::GTC,
                           false},
                          request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.method.data(), request.method_size) == "POST");
  REQUIRE(std::string_view(request.path.data(), request.path_size) == "/order");
  const std::string_view body(request.body.data(), request.body_size);
  REQUIRE(body.find("\"tokenId\":\"" + std::string(kToken) + "\"") !=
          std::string_view::npos);
  REQUIRE(body.find("\"signatureType\":0") != std::string_view::npos);
  REQUIRE(body.find("\"orderType\":\"GTC\"") != std::string_view::npos);
  PlaceOrderInput gtd{order, signature, "key", api::TimeInForce::GTD, false,
                      1786189800};
  REQUIRE(BuildPlaceOrder(gtd, request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.body.data(), request.body_size)
              .find("\"expiration\":\"1786189800\"") !=
          std::string_view::npos);

  REQUIRE(BuildCancelOrder("order-1", request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.method.data(), request.method_size) ==
          "DELETE");
  REQUIRE(std::string_view(request.body.data(), request.body_size) ==
          "{\"orderID\":\"order-1\"}");

  api::VenueEvent event{};
  REQUIRE(ParsePlaceResponse(
              R"({"success":true,"orderID":"order-2","status":"live"})",
              {1, 0, 2}, {3, 4, 5}, event) == ProtocolResult::Ok);
  REQUIRE(event.type == api::VenueEventType::NewAck);
  REQUIRE(std::string_view(event.venue_order_id.value.data(),
                           event.venue_order_id.length) == "order-2");
  REQUIRE(ParseCancelResponse(
              R"({"canceled":["order-10"],"not_canceled":{}})", "order-1",
              {1, 0, 2}, {3, 4, 5}, event) == ProtocolResult::Ok);
  REQUIRE(event.type == api::VenueEventType::CancelReject);
  REQUIRE(ParseCancelResponse(
              R"({"canceled":["order-1"],"not_canceled":{}})", "order-1",
              {1, 0, 2}, {3, 4, 5}, event) == ProtocolResult::Ok);
  REQUIRE(event.type == api::VenueEventType::CancelAck);

  Pagination pagination{};
  std::array<api::VenueEvent, 2> events{};
  std::size_t count = 0;
  REQUIRE(ParseOpenOrdersPage(
              R"({"next_cursor":"MTAw","data":[{"id":"live","status":"ORDER_STATUS_LIVE","original_size":"5","size_matched":"1.25","price":"0.55"}]})",
              pagination, events.data(), events.size(), count) ==
          ProtocolResult::Ok);
  REQUIRE(count == 1);
  REQUIRE(!pagination.complete);
  REQUIRE(events[0].type == api::VenueEventType::ReconcileOpen);
  REQUIRE(events[0].reconciled_status == api::OrderStatus::PartiallyFilled);
  REQUIRE(BuildOpenOrdersPage(pagination, request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.path.data(), request.path_size) ==
          "/data/orders?next_cursor=MTAw");
  Pagination encoded_cursor{};
  constexpr std::string_view cursor = "a+/=";
  std::copy(cursor.begin(), cursor.end(), encoded_cursor.cursor.begin());
  encoded_cursor.cursor_size = static_cast<std::uint16_t>(cursor.size());
  REQUIRE(BuildOpenOrdersPage(encoded_cursor, request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.path.data(), request.path_size) ==
          "/data/orders?next_cursor=a%2B%2F%3D");
  count = 0;
  REQUIRE(ParseOpenOrdersPage(
              R"({"next_cursor":"LTE=","data":[]})", pagination, events.data(),
              events.size(), count) == ProtocolResult::Ok);
  REQUIRE(pagination.complete);
  Pagination decimal_zero{};
  count = 0;
  REQUIRE(ParseOpenOrdersPage(
              R"([{"id":"zero","status":"LIVE","original_size":"5.0","size_matched":"0.0","price":"0.55"}])",
              decimal_zero, events.data(), events.size(), count) ==
          ProtocolResult::Ok);
  REQUIRE(count == 1);
  REQUIRE(events[0].reconciled_status == api::OrderStatus::Open);
  Pagination malformed{};
  count = 0;
  REQUIRE(ParseOpenOrdersPage(
              R"({"next_cursor":"x","data":[{"id":"broken"})", malformed,
              events.data(), events.size(), count) ==
          ProtocolResult::Malformed);

  REQUIRE(ParseUserMessage(
              R"({"event_type":"order","type":"UPDATE","id":"order-1","status":"LIVE","created_at":"1786186200"})",
              event) == ProtocolResult::Ok);
  REQUIRE(event.type == api::VenueEventType::NewAck);
  REQUIRE(event.event_time_ns == 1786186200000000000ULL);
  REQUIRE(ParseUserMessage(
              R"({"event_type":"trade","type":"TRADE","id":"trade-1","order_id":"order-1","price":"0.54","size_matched":"2.5","status":"MATCHED","created_at":"1786186201"})",
              event) == ProtocolResult::Ok);
  REQUIRE(event.type == api::VenueEventType::Fill);
  REQUIRE(event.event_time_ns == 1786186201000000000ULL);
  const api::FixedPoint expected_price{54, 2, {}};
  const api::FixedPoint expected_quantity{25, 1, {}};
  REQUIRE(event.fill_price == expected_price);
  REQUIRE(event.fill_quantity == expected_quantity);
  REQUIRE(ParseUserMessage("PONG", event) == ProtocolResult::Unsupported);
  REQUIRE(ParseUserMessage(
              R"({"event_type":"trade","id":"trade-2","taker_order_id":"order-9","price":"0.5","size_matched":"1","created_at":"1786186201.25"})",
              event) == ProtocolResult::Ok);
  REQUIRE(event.event_time_ns == 1786186201250000000ULL);
  REQUIRE(std::string_view(event.venue_order_id.value.data(),
                           event.venue_order_id.length) == "order-9");
  REQUIRE(ParseUserMessage(
              R"({"event_type":"trade","id":"trade-3","price":"0.5","size_matched":"1","created_at":"1786186201"})",
              event) == ProtocolResult::Malformed);

  std::array<api::VenueEvent, 3> trade_events{};
  std::size_t trade_count = 0;
  constexpr std::string_view multi_maker =
      R"({"event_type":"trade","type":"TRADE","id":"trade-multi","taker_order_id":"taker-1","price":"0.54","size":"3.0","maker_orders":[{"order_id":"maker-1","price":"0.53","matched_amount":"1.0"},{"order_id":"maker-2","price":"0.55","matched_amount":"2.0"}],"created_at":"1786186201.5"})";
  REQUIRE(ParseUserMessageEvents(multi_maker, trade_events.data(),
                                 trade_events.size(), trade_count) ==
          ProtocolResult::Ok);
  REQUIRE(trade_count == 3);
  REQUIRE(std::string_view(trade_events[0].venue_order_id.value.data(),
                           trade_events[0].venue_order_id.length) == "taker-1");
  REQUIRE(std::string_view(trade_events[1].venue_order_id.value.data(),
                           trade_events[1].venue_order_id.length) == "maker-1");
  REQUIRE(std::string_view(trade_events[2].venue_order_id.value.data(),
                           trade_events[2].venue_order_id.length) == "maker-2");
  const api::FixedPoint maker_price{53, 2, {}};
  const api::FixedPoint maker_quantity{20, 1, {}};
  REQUIRE(trade_events[1].fill_price == maker_price);
  REQUIRE(trade_events[2].fill_quantity == maker_quantity);
  REQUIRE(ParseUserMessage(multi_maker, event) ==
          ProtocolResult::BoundsExceeded);

  std::string too_many =
      R"({"event_type":"trade","id":"bounded","price":"0.5","size":"1","maker_orders":[)";
  for (std::size_t index = 0; index < kMaximumTradeEvents + 1; ++index) {
    if (index != 0) too_many += ',';
    too_many +=
        R"({"order_id":"maker","price":"0.5","matched_amount":"1"})";
  }
  too_many += R"(],"created_at":"1786186201"})";
  std::array<api::VenueEvent, kMaximumTradeEvents> bounded_events{};
  trade_count = 0;
  REQUIRE(ParseUserMessageEvents(too_many, bounded_events.data(),
                                 bounded_events.size(), trade_count) ==
          ProtocolResult::BoundsExceeded);

  Pagination empty_middle{};
  count = 0;
  REQUIRE(ParseOpenOrdersPage(R"({"next_cursor":"page-2","data":[]})",
                              empty_middle, events.data(), events.size(),
                              count) == ProtocolResult::Ok);
  REQUIRE(!empty_middle.complete);
  REQUIRE(empty_middle.page_count == 1);
  REQUIRE(ParseOpenOrdersPage(R"({"next_cursor":"LTE=","data":[]})",
                              empty_middle, events.data(), events.size(),
                              count) == ProtocolResult::Ok);
  REQUIRE(empty_middle.complete);

  REQUIRE(BuildDeriveApiKey(request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.method.data(), request.method_size) == "GET");
  REQUIRE(std::string_view(request.path.data(), request.path_size) ==
          "/auth/derive-api-key");
  REQUIRE(request.body_size == 0);
  REQUIRE(BuildCreateApiKey(request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.method.data(), request.method_size) ==
          "POST");
  REQUIRE(std::string_view(request.path.data(), request.path_size) ==
          "/auth/api-key");
  REQUIRE(request.body_size == 0);
  REQUIRE(CredentialHttpSuccess(200));
  REQUIRE(CredentialHttpSuccess(201));
  REQUIRE(!CredentialHttpSuccess(199));
  REQUIRE(CredentialHttpTemporary(0));
  REQUIRE(CredentialHttpTemporary(408));
  REQUIRE(CredentialHttpTemporary(429));
  REQUIRE(CredentialHttpTemporary(503));
  REQUIRE(!CredentialHttpTemporary(400));
  REQUIRE(DeriveExplicitlyMissing(404, {}));
  REQUIRE(DeriveExplicitlyMissing(400, "Could not derive api key"));
  REQUIRE(!DeriveExplicitlyMissing(400, "bad request"));
  REQUIRE(!DeriveExplicitlyMissing(401, "could not derive api key"));
  REQUIRE(CreateConflictStatus(400));
  REQUIRE(CreateConflictStatus(409));
  REQUIRE(!CreateConflictStatus(401));
  REQUIRE(!CreateConflictStatus(422));
  order = VectorOrder(1, kSigner);
  REQUIRE(SignOrder(order, kPrivateKey, signature) == CryptoResult::Ok);
  REQUIRE(BuildPlaceOrder({order, signature, "key", api::TimeInForce::GTC,
                           false},
                          request) == ProtocolResult::Ok);
  REQUIRE(std::string_view(request.body.data(), request.body_size)
              .find("\"signatureType\":1") != std::string_view::npos);
  ApiKeyCredentials credentials{};
  REQUIRE(ParseApiKeyCredentials(kDeriveCredentialsJson, credentials) ==
          ProtocolResult::Ok);
  REQUIRE(credentials.api_key == "key");
  REQUIRE(credentials.secret == "c2VjcmV0LWtleQ");
  REQUIRE(credentials.passphrase == "passphrase");
  REQUIRE(ParseApiKeyCredentials(R"({"apiKey":"key"})", credentials) ==
          ProtocolResult::Malformed);
  REQUIRE(ParseApiKeyCredentials(
              R"({"apiKey":"","secret":"c2VjcmV0LWtleQ","passphrase":"p"})",
              credentials) == ProtocolResult::Malformed);
  const std::string too_long_key(129, 'a');
  const std::string long_json =
      std::string(R"({"apiKey":")") + too_long_key +
      R"(","secret":"c2VjcmV0LWtleQ","passphrase":"p"})";
  REQUIRE(ParseApiKeyCredentials(long_json, credentials) ==
          ProtocolResult::Malformed);
  std::array<char, 256> subscribe{};
  std::size_t subscribe_size = 0;
  REQUIRE(BuildUserSubscribe("test-key", "test-secret", "test-pass",
                             subscribe.data(), subscribe.size(),
                             subscribe_size) == ProtocolResult::Ok);
  REQUIRE(std::string_view(subscribe.data(), subscribe_size) ==
          "{\"auth\":{\"apiKey\":\"test-key\",\"secret\":\"test-secret\","
          "\"passphrase\":\"test-pass\"},\"type\":\"user\"}");
}

struct MockTransport final : Transport {
  std::array<TransportRequest, 24> submitted{};
  std::size_t submitted_count{};
  std::array<ClobAuthRequest, 16> clob_auths{};
  std::size_t clob_auth_count{};
  std::array<TransportEvent, 24> events{};
  std::size_t event_begin{};
  std::size_t event_count{};
  std::string subscription;
  bool closed{};
  std::size_t reconnects{};
  std::size_t heartbeats{};
  AdapterResult reconnect_result{AdapterResult::Ok};
  AdapterResult clob_auth_result{AdapterResult::Ok};

  AdapterResult submit(const TransportRequest& request) noexcept override {
    if (submitted_count == submitted.size()) return AdapterResult::WouldBlock;
    submitted[submitted_count++] = request;
    return AdapterResult::Ok;
  }

  AdapterResult submit_clob_auth(const ClobAuthRequest& request) noexcept override {
    if (clob_auth_count == clob_auths.size()) return AdapterResult::WouldBlock;
    clob_auths[clob_auth_count++] = request;
    return clob_auth_result;
  }

  AdapterResult set_user_subscription(std::string_view message) noexcept override {
    subscription.assign(message.begin(), message.end());
    return AdapterResult::Ok;
  }

  AdapterResult poll(TransportEvent& event) noexcept override {
    if (event_count == 0) return AdapterResult::WouldBlock;
    event = events[event_begin];
    event_begin = (event_begin + 1) % events.size();
    --event_count;
    return AdapterResult::Ok;
  }

  AdapterResult request_reconnect() noexcept override {
    ++reconnects;
    return reconnect_result;
  }

  AdapterResult send_heartbeat() noexcept override {
    ++heartbeats;
    return AdapterResult::Ok;
  }

  void push(TransportEvent event) {
    REQUIRE(event_count != events.size());
    events[(event_begin + event_count) % events.size()] = event;
    ++event_count;
  }

  void close() noexcept override { closed = true; }
};

struct EventLog {
  std::array<AdapterEvent, 96> events{};
  std::size_t size{};

  static AdapterResult OnEvent(void* context,
                               const AdapterEvent& event) noexcept {
    EventLog& self = *static_cast<EventLog*>(context);
    if (self.size == self.events.size()) return AdapterResult::WouldBlock;
    self.events[self.size++] = event;
    return AdapterResult::Ok;
  }
};

std::uint64_t Now(void*) noexcept { return 1786186200000ULL; }

struct TestDirectory {
  api::InstrumentId instrument_id{7};
  std::array<std::uint8_t, 32> token_id{};
};

api::InstrumentId ResolveToken(
    void* context,
    const std::array<std::uint8_t, 32>& token_id) noexcept {
  const auto& directory = *static_cast<TestDirectory*>(context);
  return directory.token_id == token_id ? directory.instrument_id : 0;
}

AdapterConfig Config(TestDirectory& registry, MockTransport& transport,
                     api::PolymarketSignatureType signature_type =
                         api::PolymarketSignatureType::Eoa,
                     std::string_view funder = {}) {
  AdapterConfig config{};
  config.instrument_context = &registry;
  config.resolve_polymarket_token = &ResolveToken;
  config.transport = &transport;
  config.signature_type = signature_type;
  if (!funder.empty()) {
    REQUIRE(config.credentials.funder_address.assign(funder));
  }
  REQUIRE(config.credentials.private_key.assign(kPrivateKey));
  REQUIRE(config.credentials.api_key.assign("key"));
  REQUIRE(config.credentials.api_secret.assign("c2VjcmV0LWtleQ"));
  REQUIRE(config.credentials.passphrase.assign("passphrase"));
  config.now_ms = &Now;
  return config;
}

std::string DerivedSignerHex() {
  OrderSigningContext context(kPrivateKey);
  REQUIRE(context.status() == CryptoResult::Ok);
  return Hex(context.signer_address().data(), context.signer_address().size());
}

TestDirectory Registry(std::uint8_t signature_type = 0) {
  (void)signature_type;
  TestDirectory registry;
  REQUIRE(TokenIdFromDecimal(kToken, registry.token_id) ==
          CryptoResult::Ok);
  return registry;
}

api::ResolvedInstrument Route(std::uint8_t signature_type = 0) {
  api::ResolvedInstrument routing{};
  routing.kind = api::ExecutionRouteKind::Polymarket;
  routing.venue = static_cast<std::uint8_t>(utils::md::Venue::Polymarket);
  routing.product_type =
      static_cast<std::uint8_t>(utils::md::ProductType::BinaryOption);
  routing.price_scale = 2;
  routing.quantity_scale = 1;
  routing.catalog_revision = 1;
  routing.tick_size = 1;
  routing.lot_size = 1;
  routing.minimum_order_size = 1;
  routing.signature_type = signature_type;
  routing.outcome = api::PolymarketOutcome::Yes;
  routing.polymarket.condition_id[0] = 1;
  REQUIRE(TokenIdFromDecimal(kToken, routing.polymarket.token_id) ==
          CryptoResult::Ok);
  return routing;
}

void Drain(PolymarketTradeAdapter& adapter, EventLog& log,
           std::uint64_t now_ns = 1786186200000000000ULL) {
  const AdapterEventSink sink{&log, &EventLog::OnEvent};
  for (unsigned attempt = 0; attempt < 32; ++attempt) {
    const AdapterServiceResult result =
        adapter.service_io(now_ns, 96, sink);
    REQUIRE(result.result == AdapterResult::Ok ||
            ((adapter.status() == AdapterStatus::Failed ||
              adapter.status() == AdapterStatus::Stopped) &&
             result.result == AdapterResult::NotReady));
    if (result.events_processed == 0) return;
  }
  REQUIRE(false);
}

void CompleteDerive(PolymarketTradeAdapter& adapter, MockTransport& transport,
                    EventLog& log,
                    std::uint64_t now_ns = 1786186200000000000ULL) {
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
  REQUIRE(transport.clob_auth_count >= 1);
  REQUIRE(transport.reconnects == 0);
  const ClobAuthRequest& auth = transport.clob_auths[transport.clob_auth_count - 1];
  REQUIRE(std::string_view(auth.wire.method.data(), auth.wire.method_size) ==
          "GET");
  REQUIRE(std::string_view(auth.wire.path.data(), auth.wire.path_size) ==
          "/auth/derive-api-key");
  REQUIRE(auth.poly_signature.view().size() == 132);
  REQUIRE(auth.poly_nonce.view() == "0");
  transport.push({TransportEventKind::HttpResponse, auth.id, 200,
                  kDeriveCredentialsJson});
  Drain(adapter, log, now_ns);
  REQUIRE(adapter.status() == AdapterStatus::Connecting);
  REQUIRE(transport.reconnects == 1);
  REQUIRE(transport.subscription.find("\"type\":\"user\"") != std::string::npos);
}

void CompleteStartup(PolymarketTradeAdapter& adapter, MockTransport& transport,
                     EventLog& log,
                     std::uint64_t now_ns = 1786186200000000000ULL) {
  CompleteDerive(adapter, transport, log, now_ns);
  transport.push({TransportEventKind::SessionReady, 0, 0, {}});
  Drain(adapter, log, now_ns);
  REQUIRE(adapter.status() == AdapterStatus::Reconciling);
  REQUIRE(transport.submitted_count >= 1);
  transport.push({TransportEventKind::HttpResponse,
                  transport.submitted[transport.submitted_count - 1].id, 200,
                  kEmptyOpenOrders});
  Drain(adapter, log, now_ns);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
}

void TestAdapterSessionAndReconcile() {
  TestDirectory registry = Registry();
  MockTransport transport;
  PolymarketTradeAdapter adapter(Config(registry, transport));
  EventLog log;
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  CompleteStartup(adapter, transport, log);
  REQUIRE(log.events[0].status.event_time_ns == 1786186200000000000ULL);
  const std::size_t after_startup = transport.submitted_count;

  std::array<AdapterReservation, PolymarketTradeAdapter::kCommandCapacity>
      reservations{};
  for (AdapterReservation& reservation : reservations)
    REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
            AdapterResult::Ok);
  AdapterReservation overflow{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, overflow) ==
          AdapterResult::WouldBlock);
  for (const AdapterReservation reservation : reservations)
    adapter.cancel_reservation(reservation);
  REQUIRE(adapter.status() == AdapterStatus::Ready);

  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  AdapterPlaceCommand place{};
  place.routing = Route();
  place.command_id = 91;
  place.handle = {2, 0, 3};
  place.request.token = {4, 5, 6};
  place.request.instrument_id = 7;
  place.request.side = api::Side::Buy;
  place.request.type = api::OrderType::Limit;
  place.request.time_in_force = api::TimeInForce::GTC;
  place.request.quantity = {100, 1, {}};
  place.request.price = {52, 2, {}};
  REQUIRE(adapter.commit_place(reservation, place) == AdapterResult::Ok);
  REQUIRE(transport.submitted_count == after_startup + 1);
  REQUIRE(transport.submitted[after_startup].poly_address.view() ==
          DerivedSignerHex());
  REQUIRE(transport.submitted[after_startup].poly_signature.view().size() == 44);
  REQUIRE(transport.submitted[after_startup].poly_signature.view().back() == '=');
  const std::string_view first_body(
      transport.submitted[after_startup].wire.body.data(),
      transport.submitted[after_startup].wire.body_size);
  const std::string derived = DerivedSignerHex();
  REQUIRE(first_body.find("\"maker\":\"" + derived + "\"") !=
          std::string_view::npos);
  REQUIRE(first_body.find("\"signer\":\"" + derived + "\"") !=
          std::string_view::npos);
  REQUIRE(first_body.find("\"signatureType\":0") != std::string_view::npos);
  const std::uint32_t place_id = transport.submitted[after_startup].id;
  transport.push({TransportEventKind::HttpResponse, place_id, 200,
                  R"({"success":true,"orderID":"order-2","status":"live"})"});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 2].kind == AdapterEventKind::CommandResult);
  REQUIRE(log.events[log.size - 2].command_result.command_id == 91);
  REQUIRE(log.events[log.size - 1].venue.type ==
          api::VenueEventType::NewAck);
  transport.push(
      {TransportEventKind::UserMessage, 0, 0,
       R"({"event_type":"trade","type":"TRADE","id":"trade-2","order_id":"order-2","price":"0.53","size_matched":"1.0","status":"MATCHED","created_at":"1786186201.5"})"});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 1].kind == AdapterEventKind::Venue);
  REQUIRE(log.events[log.size - 1].venue.type == api::VenueEventType::Fill);
  REQUIRE(log.events[log.size - 1].venue.handle == place.handle);
  REQUIRE(log.events[log.size - 1].venue.event_time_ns ==
          1786186201500000000ULL);
  const std::size_t before_multi = log.size;
  transport.push(
      {TransportEventKind::UserMessage, 0, 0,
       R"({"event_type":"trade","id":"trade-multi","taker_order_id":"order-2","price":"0.54","size":"3","maker_orders":[{"order_id":"maker-1","price":"0.53","matched_amount":"1"},{"order_id":"maker-2","price":"0.55","matched_amount":"2"}],"created_at":"1786186202"})"});
  Drain(adapter, log);
  REQUIRE(log.size == before_multi + 3);
  REQUIRE(log.events[before_multi].venue.handle == place.handle);
  REQUIRE(log.events[before_multi + 1].venue.handle == api::OrderHandle{});
  REQUIRE(log.events[before_multi + 2].venue.handle == api::OrderHandle{});

  REQUIRE(adapter.reserve_command(AdapterCommandKind::Cancel, reservation) ==
          AdapterResult::Ok);
  AdapterCancelCommand cancel{};
  cancel.command_id = 92;
  cancel.request.request_token = {4, 5, 7};
  cancel.request.target_token = place.request.token;
  cancel.request.handle = place.handle;
  REQUIRE(adapter.commit_cancel(reservation, cancel) == AdapterResult::Ok);
  const TransportRequest& cancel_wire =
      transport.submitted[transport.submitted_count - 1];
  REQUIRE(std::string_view(cancel_wire.wire.body.data(),
                           cancel_wire.wire.body_size) ==
          "{\"orderID\":\"order-2\"}");
  transport.push({TransportEventKind::HttpResponse, cancel_wire.id, 200,
                  R"({"canceled":["order-2"],"not_canceled":{}})"});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 1].venue.type ==
          api::VenueEventType::CancelAck);

  REQUIRE(adapter.begin_reconcile(44, 1786186200000000000ULL,
                                  {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  const TransportRequest first_page =
      transport.submitted[transport.submitted_count - 1];
  transport.push({TransportEventKind::HttpResponse, first_page.id, 200,
                  R"({"next_cursor":"MTAw","data":[{"id":"open-1","status":"LIVE","original_size":"5","size_matched":"0","price":"0.55"}]})"});
  Drain(adapter, log);
  REQUIRE(transport.submitted_count == after_startup + 4);
  const TransportRequest second_page =
      transport.submitted[transport.submitted_count - 1];
  REQUIRE(std::string_view(second_page.wire.path.data(),
                           second_page.wire.path_size) ==
          "/data/orders?next_cursor=MTAw");
  std::array<char, 45> expected_signature{};
  std::size_t expected_signature_size = 0;
  REQUIRE(L2Signature("c2VjcmV0LWtleQ", "1786186200", "GET",
                      "/data/orders", "", expected_signature.data(),
                      expected_signature.size(), expected_signature_size) ==
          CryptoResult::Ok);
  REQUIRE(second_page.poly_signature.view() ==
          std::string_view(expected_signature.data(), expected_signature_size));
  transport.push({TransportEventKind::HttpResponse, second_page.id, 200,
                  R"({"next_cursor":"LTE=","data":[]})"});
  Drain(adapter, log);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(log.events[log.size - 1].kind ==
          AdapterEventKind::ReconcileComplete);
  REQUIRE(log.events[log.size - 1].reconcile.generation == 44);
  REQUIRE(log.events[log.size - 1].reconcile.result == AdapterResult::Failed);

  transport.push({TransportEventKind::SessionLost, 0, 0, {}});
  Drain(adapter, log);
  REQUIRE(adapter.status() == AdapterStatus::Reconnecting);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::NotReady);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Cancel, reservation) ==
          AdapterResult::Ok);
  adapter.cancel_reservation(reservation);
  transport.push({TransportEventKind::SessionReady, 0, 0, {}});
  Drain(adapter, log);
  REQUIRE(adapter.status() == AdapterStatus::Reconciling);
  REQUIRE(transport.submitted_count == after_startup + 5);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::NotReady);
  const TransportRequest reconnect_page =
      transport.submitted[transport.submitted_count - 1];
  transport.push({TransportEventKind::HttpResponse, reconnect_page.id, 200,
                  R"({"next_cursor":"LTE=","data":[]})"});
  Drain(adapter, log);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(log.events[log.size - 1].kind ==
          AdapterEventKind::ReconcileComplete);
  REQUIRE(log.events[log.size - 1].reconcile.generation != 0);
  REQUIRE(log.events[log.size - 1].reconcile.generation != 44);

  REQUIRE(adapter.shutdown(1786186200000000000ULL,
                           {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  REQUIRE(transport.closed);
}

void TestDepositWalletSignerAndValidation() {
  constexpr std::string_view funder =
      "0x1111111111111111111111111111111111111111";
  TestDirectory registry = Registry(3);
  MockTransport transport;
  PolymarketTradeAdapter adapter(Config(
      registry, transport, api::PolymarketSignatureType::Poly1271, funder));
  EventLog log;
  CompleteStartup(adapter, transport, log);

  AdapterPlaceCommand place{};
  place.routing = Route(3);
  place.command_id = 101;
  place.handle = {3, 0, 4};
  place.request.token = {5, 6, 7};
  place.request.instrument_id = 7;
  place.request.side = api::Side::Buy;
  place.request.type = api::OrderType::Limit;
  place.request.time_in_force = api::TimeInForce::GTC;
  place.request.quantity = {100, 1, {}};
  place.request.price = {52, 2, {}};
  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) == AdapterResult::Ok);
  REQUIRE(transport.submitted[transport.submitted_count - 1].poly_address.view() ==
          DerivedSignerHex());
  const std::string_view body(
      transport.submitted[transport.submitted_count - 1].wire.body.data(),
      transport.submitted[transport.submitted_count - 1].wire.body_size);
  REQUIRE(body.find("\"maker\":\"0x1111111111111111111111111111111111111111\"") !=
          std::string_view::npos);
  REQUIRE(body.find("\"signer\":\"0x1111111111111111111111111111111111111111\"") !=
          std::string_view::npos);
  REQUIRE(body.find("\"signatureType\":3") != std::string_view::npos);
  transport.push({TransportEventKind::HttpResponse,
                  transport.submitted[transport.submitted_count - 1].id, 400,
                  R"({"success":false,"errorMsg":"invalid order"})"});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 2].kind ==
          AdapterEventKind::CommandResult);
  REQUIRE(log.events[log.size - 2].command_result.result ==
          AdapterResult::Failed);
  REQUIRE(log.events[log.size - 1].venue.type ==
          api::VenueEventType::NewReject);

  place.request.price = {100, 2, {}};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) ==
          AdapterResult::InvalidArgument);
  place.request.price = {525, 3, {}};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) ==
          AdapterResult::InvalidArgument);
  place.request.price = {52, 2, {}};
  place.request.time_in_force = api::TimeInForce::FOK;
  place.request.flags = static_cast<std::uint16_t>(api::OrderFlag::PostOnly);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) ==
          AdapterResult::InvalidArgument);

  place.request.time_in_force = api::TimeInForce::GTC;
  place.request.flags = 0;
  place.routing.signature_type = 3;
  REQUIRE(TokenIdFromDecimal("42", place.routing.polymarket.token_id) ==
          CryptoResult::Ok);
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) == AdapterResult::Ok);
  const std::string_view frozen_route_body(
      transport.submitted[transport.submitted_count - 1].wire.body.data(),
      transport.submitted[transport.submitted_count - 1].wire.body_size);
  REQUIRE(frozen_route_body.find("\"tokenId\":\"42\"") !=
          std::string_view::npos);
  REQUIRE(frozen_route_body.find("\"signatureType\":3") !=
          std::string_view::npos);

  PolymarketTradeAdapter invalid_key([&] {
    AdapterConfig config = Config(registry, transport);
    REQUIRE(config.credentials.private_key.assign(std::string(64, '0')));
    return config;
  }());
  REQUIRE(invalid_key.status() == AdapterStatus::Failed);
}

void TestInvalidAdapterIsSafe() {
  PolymarketTradeAdapter adapter(AdapterConfig{});
  EventLog log;
  REQUIRE(adapter.status() == AdapterStatus::Failed);
  REQUIRE(adapter.service_io(1, 1, {&log, &EventLog::OnEvent}).result ==
          AdapterResult::NotReady);
  REQUIRE(adapter.shutdown(1, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Stopped);
}

void TestRequestDeadline() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig config = Config(registry, transport);
  config.request_timeout_ns = 100;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  CompleteStartup(adapter, transport, log);

  AdapterPlaceCommand place{};
  place.routing = Route();
  place.command_id = 501;
  place.handle = {7, 0, 8};
  place.request.token = {9, 10, 11};
  place.request.instrument_id = 7;
  place.request.side = api::Side::Buy;
  place.request.type = api::OrderType::Limit;
  place.request.time_in_force = api::TimeInForce::GTC;
  place.request.quantity = {100, 1, {}};
  place.request.price = {52, 2, {}};
  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) == AdapterResult::Ok);
  const AdapterEventSink sink{&log, &EventLog::OnEvent};
  const auto serviced =
      adapter.service_io(1'000, 8, sink);
  REQUIRE(serviced.next_deadline_ns == 1'100);
  AdapterDeadline deadline{};
  deadline.kind = AdapterDeadlineKind::Request;
  deadline.due_time_ns = serviced.next_deadline_ns;
  REQUIRE(adapter.on_deadline(deadline, 1'100, sink) == AdapterResult::Ok);
  REQUIRE(log.events[log.size - 1].kind ==
          AdapterEventKind::CommandResult);
  REQUIRE(log.events[log.size - 1].command_result.command_id == 501);
  REQUIRE(log.events[log.size - 1].command_result.result ==
          AdapterResult::Failed);
}

void TestReconcileMissingLocalIsAuditable() {
  TestDirectory registry = Registry();
  MockTransport transport;
  PolymarketTradeAdapter adapter(Config(registry, transport));
  EventLog log;
  CompleteStartup(adapter, transport, log);

  AdapterPlaceCommand place{};
  place.routing = Route();
  place.command_id = 601;
  place.handle = {8, 0, 9};
  place.request.token = {10, 11, 12};
  place.request.instrument_id = 7;
  place.request.side = api::Side::Buy;
  place.request.type = api::OrderType::Limit;
  place.request.time_in_force = api::TimeInForce::GTC;
  place.request.quantity = {100, 1, {}};
  place.request.price = {52, 2, {}};
  AdapterReservation reservation{};
  REQUIRE(adapter.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(adapter.commit_place(reservation, place) == AdapterResult::Ok);
  const std::uint32_t place_request =
      transport.submitted[transport.submitted_count - 1].id;
  transport.push(
      {TransportEventKind::HttpResponse, place_request, 200,
       R"({"success":true,"orderID":"local-open","status":"live"})"});
  Drain(adapter, log);

  const std::size_t before_reconcile = log.size;
  REQUIRE(adapter.begin_reconcile(602, 1'000, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  const std::uint32_t reconcile_request =
      transport.submitted[transport.submitted_count - 1].id;
  transport.push({TransportEventKind::HttpResponse, reconcile_request, 200,
                  R"({"next_cursor":"LTE=","data":[]})"});
  Drain(adapter, log);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(log.events[log.size - 1].kind ==
          AdapterEventKind::ReconcileComplete);
  REQUIRE(log.events[log.size - 1].reconcile.generation == 602);
  REQUIRE(log.events[log.size - 1].reconcile.result == AdapterResult::Failed);
  for (std::size_t index = before_reconcile; index + 1 < log.size; ++index) {
    REQUIRE(log.events[index].kind != AdapterEventKind::Venue ||
            log.events[index].venue.type !=
                api::VenueEventType::ReconcileTerminal);
  }

  REQUIRE(adapter.reserve_command(AdapterCommandKind::Cancel, reservation) ==
          AdapterResult::Ok);
  AdapterCancelCommand cancel{};
  cancel.command_id = 603;
  cancel.request.request_token = {10, 11, 13};
  cancel.request.target_token = place.request.token;
  cancel.request.handle = place.handle;
  REQUIRE(adapter.commit_cancel(reservation, cancel) == AdapterResult::Ok);
  const TransportRequest& cancel_wire =
      transport.submitted[transport.submitted_count - 1];
  REQUIRE(std::string_view(cancel_wire.wire.body.data(),
                           cancel_wire.wire.body_size) ==
          R"({"orderID":"local-open"})");
}

void TestSessionDeadlineActions() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig config = Config(registry, transport);
  config.reconnect_initial_ns = 100;
  config.reconnect_max_ns = 400;
  config.heartbeat_interval_ns = 100;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  constexpr std::uint64_t now = 1786186200000000000ULL;
  CompleteStartup(adapter, transport, log, now);
  const std::size_t base_reconnects = transport.reconnects;
  const AdapterEventSink sink{&log, &EventLog::OnEvent};

  AdapterDeadline heartbeat{};
  heartbeat.kind = AdapterDeadlineKind::Heartbeat;
  heartbeat.due_time_ns = now + 100;
  REQUIRE(adapter.on_deadline(heartbeat, now + 100, sink) ==
          AdapterResult::Ok);
  REQUIRE(transport.heartbeats == 1);
  REQUIRE(adapter.on_deadline(heartbeat, now + 100, sink) ==
          AdapterResult::StaleReservation);

  transport.push({TransportEventKind::SessionLost, 0, 0, {}});
  Drain(adapter, log);
  AdapterDeadline reconnect{};
  reconnect.kind = AdapterDeadlineKind::Reconnect;
  reconnect.due_time_ns = now;
  REQUIRE(adapter.on_deadline(reconnect, now, sink) ==
          AdapterResult::WouldBlock);
  reconnect.due_time_ns = now + 100;
  REQUIRE(adapter.on_deadline(reconnect, now + 100, sink) ==
          AdapterResult::Ok);
  REQUIRE(transport.reconnects == base_reconnects + 1);
  reconnect.due_time_ns = now + 299;
  REQUIRE(adapter.on_deadline(reconnect, now + 299, sink) ==
          AdapterResult::WouldBlock);
  reconnect.due_time_ns = now + 300;
  REQUIRE(adapter.on_deadline(reconnect, now + 300, sink) ==
          AdapterResult::Ok);
  REQUIRE(transport.reconnects == base_reconnects + 2);
  reconnect.due_time_ns = now + 700;
  REQUIRE(adapter.on_deadline(reconnect, now + 700, sink) ==
          AdapterResult::Ok);
  reconnect.due_time_ns = now + 1'100;
  REQUIRE(adapter.on_deadline(reconnect, now + 1'100, sink) ==
          AdapterResult::Ok);
  REQUIRE(transport.reconnects == base_reconnects + 4);
}

void TestReconcileRequestDeadline() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig config = Config(registry, transport);
  config.request_timeout_ns = 100;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  CompleteStartup(adapter, transport, log);
  REQUIRE(adapter.begin_reconcile(701, 1'000, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  const AdapterEventSink sink{&log, &EventLog::OnEvent};
  const AdapterServiceResult serviced = adapter.service_io(1'000, 8, sink);
  REQUIRE(serviced.next_deadline_ns == 1'100);
  AdapterDeadline deadline{};
  deadline.kind = AdapterDeadlineKind::Request;
  deadline.id = transport.submitted[transport.submitted_count - 1].id;
  deadline.generation = 701;
  deadline.due_time_ns = 1'100;
  REQUIRE(adapter.on_deadline(deadline, 1'100, sink) == AdapterResult::Ok);
  REQUIRE(log.events[log.size - 1].kind ==
          AdapterEventKind::ReconcileComplete);
  REQUIRE(log.events[log.size - 1].reconcile.generation == 701);
  REQUIRE(log.events[log.size - 1].reconcile.result == AdapterResult::Failed);
  REQUIRE(adapter.status() == AdapterStatus::Failed);
}

void TestSessionLivenessTimeout() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig config = Config(registry, transport);
  config.heartbeat_interval_ns = 100;
  config.liveness_timeout_ns = 250;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  constexpr std::uint64_t now = 1786186200000000000ULL;
  CompleteStartup(adapter, transport, log, now);
  const AdapterServiceResult result =
      adapter.service_io(now + 250, 8, {&log, &EventLog::OnEvent});
  REQUIRE(result.result == AdapterResult::Ok);
  REQUIRE(transport.closed);
  REQUIRE(adapter.status() == AdapterStatus::Reconnecting);
  REQUIRE(log.events[log.size - 1].kind == AdapterEventKind::Status);
  REQUIRE(log.events[log.size - 1].status.status ==
          AdapterStatus::Reconnecting);
}

void TestAuthoritativeQueriesStaySeparateFromReconcile() {
  TestDirectory registry = Registry();
  MockTransport clob;
  MockTransport data;
  AdapterConfig config = Config(registry, clob);
  config.data_transport = &data;
  PolymarketTradeAdapter adapter(config);
  EventLog log;

  AdapterQueryRequest open_request{};
  open_request.token = {4, 9, 1};
  open_request.request.account_id = 17;
  REQUIRE(adapter.query_open_orders(
              open_request, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  REQUIRE(clob.submitted_count == 1);
  REQUIRE(std::string_view(clob.submitted[0].wire.path.data(),
                           clob.submitted[0].wire.path_size) ==
          "/data/orders");
  const std::string open_json =
      std::string(R"({"next_cursor":"LTE=","data":[{"id":"query-order",)") +
      R"("asset_id":")" + std::string(kToken) +
      R"(","side":"BUY","status":"LIVE","original_size":"10.0",)"
      R"("size_matched":"2.5","price":"0.52"}]})";
  clob.push({TransportEventKind::HttpResponse, clob.submitted[0].id, 200,
             open_json});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 2].kind ==
          AdapterEventKind::OpenOrderSnapshot);
  REQUIRE(log.events[log.size - 2].open_order.account_id == 17);
  REQUIRE(log.events[log.size - 2].open_order.instrument_id == 7);
  REQUIRE(log.events[log.size - 2].open_order.remaining_quantity.value == 75);
  REQUIRE(log.events[log.size - 1].kind == AdapterEventKind::QueryComplete);
  REQUIRE(log.events[log.size - 1].query_complete.error == api::Error::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);

  AdapterQueryRequest positions_request{};
  positions_request.token = {4, 9, 2};
  positions_request.request.account_id = 17;
  REQUIRE(adapter.query_positions(
              positions_request, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  REQUIRE(data.submitted_count == 1);
  const auto& positions_wire = data.submitted[0];
  const std::string_view path(positions_wire.wire.path.data(),
                              positions_wire.wire.path_size);
  REQUIRE(path.starts_with("/positions?user="));
  REQUIRE(path.find(DerivedSignerHex()) != std::string_view::npos);
  REQUIRE(path.find("sizeThreshold=0.0001&limit=500") !=
          std::string_view::npos);
  REQUIRE(positions_wire.poly_signature.size == 0);
  const std::string positions_json =
      std::string(R"([{"asset":")") + std::string(kToken) +
      R"(","size":3.25}])";
  data.push({TransportEventKind::HttpResponse, positions_wire.id, 200,
             positions_json});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 2].kind ==
          AdapterEventKind::PositionSnapshot);
  REQUIRE(log.events[log.size - 2].position.account_id == 17);
  REQUIRE(log.events[log.size - 2].position.instrument_id == 7);
  REQUIRE(log.events[log.size - 2].position.quantity.value == 325);
  REQUIRE(log.events[log.size - 1].kind == AdapterEventKind::QueryComplete);
}

void TestUnknownQueryIdentityIsQuarantinable() {
  TestDirectory directory{};
  MockTransport transport;
  PolymarketTradeAdapter adapter(Config(directory, transport));
  EventLog log;
  AdapterQueryRequest request{};
  request.token = {4, 9, 3};
  request.request.account_id = 17;
  REQUIRE(adapter.query_open_orders(
              request, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  const std::string json =
      std::string(R"({"next_cursor":"LTE=","data":[{"id":"unknown",)") +
      R"("asset_id":")" + std::string(kToken) +
      R"(","side":"BUY","status":"LIVE","original_size":"1.0",)"
      R"("size_matched":"0","price":"0.50"}]})";
  transport.push({TransportEventKind::HttpResponse,
                  transport.submitted[0].id, 200, json});
  Drain(adapter, log);
  REQUIRE(log.events[log.size - 2].kind ==
          AdapterEventKind::OpenOrderSnapshot);
  REQUIRE(log.events[log.size - 2].open_order.instrument_id == 0);
  REQUIRE((log.events[log.size - 2].open_order.reserved &
           api::SnapshotUnmapped) != 0);
  REQUIRE(log.events[log.size - 1].kind ==
          AdapterEventKind::QueryComplete);
  REQUIRE(log.events[log.size - 1].query_complete.error == api::Error::Ok);
}

void TestScopedQueriesCarryNativeReference() {
  TestDirectory directory = Registry();
  MockTransport clob;
  MockTransport data;
  AdapterConfig config = Config(directory, clob);
  config.data_transport = &data;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  const auto route = Route();
  AdapterQueryRequest request{};
  request.token = {4, 9, 4};
  request.request.account_id = 17;
  request.request.scope = api::QueryScope::SingleInstrument;
  request.request.instrument.kind = route.kind;
  request.request.instrument.venue = route.venue;
  request.request.instrument.product_type = route.product_type;
  request.request.instrument.outcome = route.outcome;
  request.request.instrument.polymarket.condition_id =
      route.polymarket.condition_id;
  request.request.instrument.polymarket.token_id =
      route.polymarket.token_id;
  REQUIRE(adapter.query_open_orders(
              request, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  const std::string_view open_path(clob.submitted[0].wire.path.data(),
                                  clob.submitted[0].wire.path_size);
  REQUIRE(open_path ==
          std::string("/data/orders?asset_id=") + std::string(kToken));
  clob.push({TransportEventKind::HttpResponse, clob.submitted[0].id, 200,
             R"({"next_cursor":"LTE=","data":[]})"});
  Drain(adapter, log);

  request.token.sequence = 5;
  REQUIRE(adapter.query_positions(
              request, {&log, &EventLog::OnEvent}) ==
          AdapterResult::Ok);
  const std::string_view position_path(data.submitted[0].wire.path.data(),
                                      data.submitted[0].wire.path_size);
  REQUIRE(position_path.find("&market=0x01") != std::string_view::npos);
}

void TestType0RejectsConfiguredFunder() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig with_funder = Config(registry, transport);
  REQUIRE(with_funder.credentials.funder_address.assign(
      "0x1111111111111111111111111111111111111111"));
  PolymarketTradeAdapter type0_with_funder(with_funder);
  REQUIRE(type0_with_funder.status() == AdapterStatus::Failed);

  AdapterConfig unspecified = Config(registry, transport);
  unspecified.signature_type = api::PolymarketSignatureType::Unspecified;
  PolymarketTradeAdapter missing_type(unspecified);
  REQUIRE(missing_type.status() == AdapterStatus::Failed);

  PolymarketTradeAdapter type3_without_funder(Config(
      registry, transport, api::PolymarketSignatureType::Poly1271));
  REQUIRE(type3_without_funder.status() == AdapterStatus::Failed);

  PolymarketTradeAdapter type1_without_funder(Config(
      registry, transport, api::PolymarketSignatureType::PolyProxy));
  REQUIRE(type1_without_funder.status() == AdapterStatus::Failed);

  PolymarketTradeAdapter type1_with_funder(Config(
      registry, transport, api::PolymarketSignatureType::PolyProxy,
      "0x1111111111111111111111111111111111111111"));
  REQUIRE(type1_with_funder.status() == AdapterStatus::Authenticating);
}

void TestAccountSignatureTypeIgnoresRoute() {
  TestDirectory registry = Registry();
  MockTransport transport;
  EventLog log;
  const std::string derived = DerivedSignerHex();
  constexpr std::string_view funder =
      "0x1111111111111111111111111111111111111111";

  PolymarketTradeAdapter eoa(Config(registry, transport));
  CompleteStartup(eoa, transport, log);
  AdapterPlaceCommand place{};
  place.routing = Route(3);
  place.command_id = 801;
  place.handle = {11, 0, 12};
  place.request.token = {13, 14, 15};
  place.request.instrument_id = 7;
  place.request.side = api::Side::Buy;
  place.request.type = api::OrderType::Limit;
  place.request.time_in_force = api::TimeInForce::GTC;
  place.request.quantity = {100, 1, {}};
  place.request.price = {52, 2, {}};
  AdapterReservation reservation{};
  REQUIRE(eoa.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(eoa.commit_place(reservation, place) == AdapterResult::Ok);
  const std::string_view eoa_body(
      transport.submitted[transport.submitted_count - 1].wire.body.data(),
      transport.submitted[transport.submitted_count - 1].wire.body_size);
  REQUIRE(eoa_body.find("\"maker\":\"" + derived + "\"") !=
          std::string_view::npos);
  REQUIRE(eoa_body.find("\"signer\":\"" + derived + "\"") !=
          std::string_view::npos);
  REQUIRE(eoa_body.find("\"signatureType\":0") != std::string_view::npos);
  REQUIRE(eoa_body.find("\"signatureType\":3") == std::string_view::npos);

  MockTransport type3_transport;
  PolymarketTradeAdapter type3(Config(
      registry, type3_transport, api::PolymarketSignatureType::Poly1271,
      funder));
  CompleteStartup(type3, type3_transport, log);
  place.routing = Route(0);
  place.command_id = 802;
  REQUIRE(type3.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(type3.commit_place(reservation, place) == AdapterResult::Ok);
  REQUIRE(type3_transport.submitted[type3_transport.submitted_count - 1]
              .poly_address.view() == derived);
  const std::string_view type3_body(
      type3_transport.submitted[type3_transport.submitted_count - 1]
          .wire.body.data(),
      type3_transport.submitted[type3_transport.submitted_count - 1]
          .wire.body_size);
  REQUIRE(type3_body.find("\"maker\":\"0x1111111111111111111111111111111111111111\"") !=
          std::string_view::npos);
  REQUIRE(type3_body.find("\"signatureType\":3") != std::string_view::npos);
  REQUIRE(type3_body.find("\"signatureType\":0") == std::string_view::npos);

  MockTransport type1_transport;
  PolymarketTradeAdapter type1(Config(
      registry, type1_transport, api::PolymarketSignatureType::PolyProxy,
      funder));
  CompleteStartup(type1, type1_transport, log);
  place.routing = Route(3);
  place.command_id = 803;
  REQUIRE(type1.reserve_command(AdapterCommandKind::Place, reservation) ==
          AdapterResult::Ok);
  REQUIRE(type1.commit_place(reservation, place) == AdapterResult::Ok);
  const std::string_view type1_body(
      type1_transport.submitted[type1_transport.submitted_count - 1]
          .wire.body.data(),
      type1_transport.submitted[type1_transport.submitted_count - 1]
          .wire.body_size);
  REQUIRE(type1_body.find("\"maker\":\"0x1111111111111111111111111111111111111111\"") !=
          std::string_view::npos);
  REQUIRE(type1_body.find("\"signer\":\"" + derived + "\"") !=
          std::string_view::npos);
  REQUIRE(type1_body.find("\"signatureType\":1") != std::string_view::npos);
}

void TestInitialSessionStartsOnce() {
  TestDirectory registry = Registry();
  MockTransport transport;
  PolymarketTradeAdapter adapter(Config(registry, transport));
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(transport.reconnects == 0);
  REQUIRE(transport.clob_auth_count == 1);
  REQUIRE(adapter.start_initial_session() == AdapterResult::InvalidArgument);
  REQUIRE(transport.clob_auth_count == 1);
  REQUIRE(transport.reconnects == 0);
}

void TestInitialSessionReadyReconciles() {
  TestDirectory registry = Registry();
  MockTransport transport;
  PolymarketTradeAdapter adapter(Config(registry, transport));
  EventLog log;
  CompleteStartup(adapter, transport, log);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
  REQUIRE(transport.submitted_count == 1);
  REQUIRE(transport.reconnects == 1);
}

void TestInitialSessionLostThenReconciles() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig config = Config(registry, transport);
  config.reconnect_initial_ns = 100;
  config.reconnect_max_ns = 400;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  constexpr std::uint64_t now = 1786186200000000000ULL;
  CompleteDerive(adapter, transport, log, now);
  transport.push({TransportEventKind::SessionLost, 0, 0, {}});
  Drain(adapter, log, now);
  REQUIRE(adapter.status() == AdapterStatus::Reconnecting);
  REQUIRE(log.events[log.size - 1].kind == AdapterEventKind::Status);
  REQUIRE(log.events[log.size - 1].status.status ==
          AdapterStatus::Reconnecting);
  REQUIRE(log.events[log.size - 1].status.reason == AdapterResult::NotReady);
  const AdapterEventSink sink{&log, &EventLog::OnEvent};
  AdapterDeadline reconnect{};
  reconnect.kind = AdapterDeadlineKind::Reconnect;
  reconnect.due_time_ns = now + 100;
  REQUIRE(adapter.on_deadline(reconnect, now + 100, sink) ==
          AdapterResult::Ok);
  REQUIRE(transport.reconnects == 2);
  transport.push({TransportEventKind::SessionReady, 0, 0, {}});
  Drain(adapter, log, now);
  REQUIRE(adapter.status() == AdapterStatus::Reconciling);
  REQUIRE(transport.submitted_count == 1);
  transport.push({TransportEventKind::HttpResponse,
                  transport.submitted[0].id, 200, kEmptyOpenOrders});
  Drain(adapter, log, now);
  REQUIRE(adapter.status() == AdapterStatus::Ready);
}

void TestInitialSessionImmediateFailure() {
  TestDirectory registry = Registry();
  MockTransport transport;
  transport.clob_auth_result = AdapterResult::Failed;
  PolymarketTradeAdapter adapter(Config(registry, transport));
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(adapter.start_initial_session() == AdapterResult::Failed);
  REQUIRE(adapter.status() == AdapterStatus::Failed);
  REQUIRE(transport.reconnects == 0);
  REQUIRE(transport.clob_auth_count == 1);
}

void TestSecureCredentialWipe() {
  FixedText<8> text{};
  REQUIRE(text.assign("abcdef"));
  REQUIRE(text.assign("xy"));
  REQUIRE(text.size == 2);
  REQUIRE(text.value[2] == 'c');
  REQUIRE(text.secure_assign("z"));
  REQUIRE(text.size == 1);
  REQUIRE(text.value[0] == 'z');
  for (std::size_t index = 1; index < text.value.size(); ++index)
    REQUIRE(text.value[index] == 0);
  text.secure_clear();
  REQUIRE(text.size == 0);
  for (char byte : text.value) REQUIRE(byte == 0);
}

void TestCredentialDeriveRetryAndLatch() {
  TestDirectory registry = Registry();
  MockTransport transport;
  AdapterConfig config = Config(registry, transport);
  config.reconnect_initial_ns = 100;
  config.reconnect_max_ns = 400;
  PolymarketTradeAdapter adapter(config);
  EventLog log;
  constexpr std::uint64_t now = 1786186200000000000ULL;
  REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
  REQUIRE(transport.clob_auth_count == 1);
  REQUIRE(transport.reconnects == 0);
  transport.push({TransportEventKind::HttpResponse,
                  transport.clob_auths[0].id, 429, {}});
  Drain(adapter, log, now);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(transport.reconnects == 0);
  const AdapterServiceResult retry =
      adapter.service_io(now + 100, 8, {&log, &EventLog::OnEvent});
  REQUIRE(retry.result == AdapterResult::Ok);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(transport.clob_auth_count == 2);
  REQUIRE(transport.reconnects == 0);
  transport.push({TransportEventKind::HttpResponse,
                  transport.clob_auths[1].id, 408, {}});
  Drain(adapter, log, now + 100);
  REQUIRE(adapter.status() == AdapterStatus::Authenticating);
  REQUIRE(transport.reconnects == 0);
  REQUIRE(adapter.service_io(now + 300, 8, {&log, &EventLog::OnEvent}).result ==
          AdapterResult::Ok);
  REQUIRE(transport.clob_auth_count == 3);
  transport.push({TransportEventKind::HttpResponse,
                  transport.clob_auths[2].id, 200, kDeriveCredentialsJson});
  Drain(adapter, log, now + 300);
  REQUIRE(adapter.status() == AdapterStatus::Connecting);
  transport.push({TransportEventKind::SessionReady, 0, 0, {}});
  Drain(adapter, log, now + 300);
  REQUIRE(adapter.status() == AdapterStatus::Reconciling);
  transport.push({TransportEventKind::HttpResponse,
                  transport.submitted[transport.submitted_count - 1].id, 200,
                  kEmptyOpenOrders});
  Drain(adapter, log, now + 300);
  REQUIRE(adapter.status() == AdapterStatus::Ready);

  MockTransport rejected;
  PolymarketTradeAdapter fail_401(Config(registry, rejected));
  EventLog rejected_log;
  REQUIRE(fail_401.start_initial_session() == AdapterResult::Ok);
  rejected.push({TransportEventKind::HttpResponse, rejected.clob_auths[0].id,
                 401, {}});
  Drain(fail_401, rejected_log);
  REQUIRE(fail_401.status() == AdapterStatus::Failed);
  REQUIRE(rejected.reconnects == 0);

  MockTransport missing;
  PolymarketTradeAdapter create_404(Config(registry, missing));
  EventLog missing_log;
  REQUIRE(create_404.start_initial_session() == AdapterResult::Ok);
  missing.push({TransportEventKind::HttpResponse, missing.clob_auths[0].id,
                404, {}});
  Drain(create_404, missing_log);
  REQUIRE(create_404.status() == AdapterStatus::Authenticating);
  REQUIRE(missing.clob_auth_count == 2);
  REQUIRE(std::string_view(missing.clob_auths[1].wire.method.data(),
                           missing.clob_auths[1].wire.method_size) == "POST");
  REQUIRE(std::string_view(missing.clob_auths[1].wire.path.data(),
                           missing.clob_auths[1].wire.path_size) ==
          "/auth/api-key");
  missing.push({TransportEventKind::HttpResponse, missing.clob_auths[1].id, 201,
                kDeriveCredentialsJson});
  Drain(create_404, missing_log);
  REQUIRE(create_404.status() == AdapterStatus::Connecting);
  REQUIRE(missing.reconnects == 1);

  MockTransport bad_json;
  PolymarketTradeAdapter fail_json(Config(registry, bad_json));
  EventLog json_log;
  REQUIRE(fail_json.start_initial_session() == AdapterResult::Ok);
  bad_json.push({TransportEventKind::HttpResponse, bad_json.clob_auths[0].id,
                 200, R"({"apiKey":"only"})"});
  Drain(fail_json, json_log);
  REQUIRE(fail_json.status() == AdapterStatus::Failed);
  REQUIRE(bad_json.reconnects == 0);

  MockTransport latch_transport;
  PolymarketTradeAdapter latch(Config(registry, latch_transport));
  EventLog latch_log;
  CompleteStartup(latch, latch_transport, latch_log, now);
  const std::size_t derives_after_ready = latch_transport.clob_auth_count;
  latch_transport.push({TransportEventKind::SessionLost, 0, 0, {}, true});
  Drain(latch, latch_log, now);
  REQUIRE(latch.status() == AdapterStatus::Authenticating);
  REQUIRE(latch_transport.clob_auth_count == derives_after_ready + 1);
  REQUIRE(latch_transport.closed);
  latch_transport.push(
      {TransportEventKind::HttpResponse,
       latch_transport.clob_auths[latch_transport.clob_auth_count - 1].id, 200,
       kDeriveCredentialsJson});
  Drain(latch, latch_log, now);
  REQUIRE(latch.status() == AdapterStatus::Connecting);
  latch_transport.push({TransportEventKind::SessionReady, 0, 0, {}});
  Drain(latch, latch_log, now);
  REQUIRE(latch.status() == AdapterStatus::Reconciling);
  latch_transport.push(
      {TransportEventKind::HttpResponse,
       latch_transport.submitted[latch_transport.submitted_count - 1].id, 200,
       kEmptyOpenOrders});
  Drain(latch, latch_log, now);
  REQUIRE(latch.status() == AdapterStatus::Ready);
  REQUIRE(latch_transport.clob_auth_count == derives_after_ready + 1);
  latch_transport.push({TransportEventKind::SessionLost, 0, 0, {}, true});
  Drain(latch, latch_log, now);
  REQUIRE(latch.status() == AdapterStatus::Failed);
  REQUIRE(latch_transport.clob_auth_count == derives_after_ready + 1);
}

std::string_view AuthMethod(const ClobAuthRequest& auth) {
  return {auth.wire.method.data(), auth.wire.method_size};
}

std::string_view AuthPath(const ClobAuthRequest& auth) {
  return {auth.wire.path.data(), auth.wire.path_size};
}

void RequireLastAuth(const MockTransport& transport, std::string_view method,
                     std::string_view path) {
  REQUIRE(transport.clob_auth_count > 0);
  const ClobAuthRequest& auth =
      transport.clob_auths[transport.clob_auth_count - 1];
  REQUIRE(AuthMethod(auth) == method);
  REQUIRE(AuthPath(auth) == path);
  REQUIRE(auth.wire.body_size == 0);
}

void TestCredentialCreateOrDeriveFlows() {
  TestDirectory registry = Registry();
  constexpr std::uint64_t now = 1786186200000000000ULL;
  constexpr std::string_view kCouldNotDerive =
      R"({"error":"Could not derive api key"})";

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 200, kDeriveCredentialsJson});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Connecting);
    REQUIRE(transport.clob_auth_count == 1);
    RequireLastAuth(transport, "GET", "/auth/derive-api-key");
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 400, kCouldNotDerive});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Authenticating);
    REQUIRE(transport.clob_auth_count == 2);
    RequireLastAuth(transport, "POST", "/auth/api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[1].id, 200, kDeriveCredentialsJson});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Connecting);
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 400, "generic bad request"});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Failed);
    REQUIRE(transport.clob_auth_count == 1);
    REQUIRE(log.events[log.size - 1].status.reason == AdapterResult::Failed);
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 422, {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Failed);
    REQUIRE(transport.clob_auth_count == 1);
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 404, {}});
    Drain(adapter, log, now);
    REQUIRE(transport.clob_auth_count == 2);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[1].id, 409, {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Authenticating);
    REQUIRE(transport.clob_auth_count == 3);
    RequireLastAuth(transport, "GET", "/auth/derive-api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[2].id, 200, kDeriveCredentialsJson});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Connecting);
  }

  {
    MockTransport transport;
    AdapterConfig config = Config(registry, transport);
    config.reconnect_initial_ns = 100;
    config.reconnect_max_ns = 400;
    PolymarketTradeAdapter adapter(config);
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 404, {}});
    Drain(adapter, log, now);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[1].id, 409, {}});
    Drain(adapter, log, now);
    REQUIRE(transport.clob_auth_count == 3);
    RequireLastAuth(transport, "GET", "/auth/derive-api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[2].id, 503, {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Authenticating);
    REQUIRE(adapter.service_io(now + 100, 8, {&log, &EventLog::OnEvent})
                .result == AdapterResult::Ok);
    REQUIRE(transport.clob_auth_count == 4);
    RequireLastAuth(transport, "GET", "/auth/derive-api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[3].id, 200, kDeriveCredentialsJson});
    Drain(adapter, log, now + 100);
    REQUIRE(adapter.status() == AdapterStatus::Connecting);
  }

  {
    MockTransport transport;
    AdapterConfig config = Config(registry, transport);
    config.request_timeout_ns = 1000;
    config.reconnect_initial_ns = 100;
    config.reconnect_max_ns = 400;
    PolymarketTradeAdapter adapter(config);
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 404, {}});
    Drain(adapter, log, now);
    REQUIRE(transport.clob_auth_count == 2);
    RequireLastAuth(transport, "POST", "/auth/api-key");
    REQUIRE(adapter.service_io(now + 1000, 8, {&log, &EventLog::OnEvent})
                .result == AdapterResult::Ok);
    REQUIRE(transport.clob_auth_count == 3);
    RequireLastAuth(transport, "GET", "/auth/derive-api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[2].id, 404, {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Authenticating);
    REQUIRE(adapter.service_io(now + 100, 8, {&log, &EventLog::OnEvent})
                .result == AdapterResult::Ok);
    REQUIRE(transport.clob_auth_count == 4);
    RequireLastAuth(transport, "POST", "/auth/api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[3].id, 200, kDeriveCredentialsJson});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Connecting);
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 404, {}});
    Drain(adapter, log, now);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[1].id, 429, {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Authenticating);
    REQUIRE(transport.clob_auth_count == 3);
    RequireLastAuth(transport, "GET", "/auth/derive-api-key");
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[2].id, 200, kDeriveCredentialsJson});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Connecting);
    std::size_t posts = 0;
    for (std::size_t index = 0; index < transport.clob_auth_count; ++index) {
      if (AuthMethod(transport.clob_auths[index]) == "POST") ++posts;
    }
    REQUIRE(posts == 1);
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    REQUIRE(adapter.start_initial_session() == AdapterResult::Ok);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[0].id, 404, {}});
    Drain(adapter, log, now);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[1].id, 422, {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Failed);
    REQUIRE(transport.clob_auth_count == 2);
    REQUIRE(log.events[log.size - 1].status.reason == AdapterResult::Failed);
  }

  {
    MockTransport transport;
    PolymarketTradeAdapter adapter(Config(registry, transport));
    EventLog log;
    CompleteStartup(adapter, transport, log, now);
    const std::size_t after_ready = transport.clob_auth_count;
    transport.push({TransportEventKind::SessionLost, 0, 0, {}, true});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Authenticating);
    transport.push({TransportEventKind::HttpResponse,
                    transport.clob_auths[transport.clob_auth_count - 1].id, 404,
                    {}});
    Drain(adapter, log, now);
    REQUIRE(adapter.status() == AdapterStatus::Failed);
    REQUIRE(transport.clob_auth_count == after_ready + 1);
  }
}

}  // namespace

int main() {
  TestSigningVectors();
  TestProtocolFixtures();
  TestAdapterSessionAndReconcile();
  TestDepositWalletSignerAndValidation();
  TestInvalidAdapterIsSafe();
  TestRequestDeadline();
  TestReconcileMissingLocalIsAuditable();
  TestSessionDeadlineActions();
  TestReconcileRequestDeadline();
  TestSessionLivenessTimeout();
  TestAuthoritativeQueriesStaySeparateFromReconcile();
  TestUnknownQueryIdentityIsQuarantinable();
  TestScopedQueriesCarryNativeReference();
  TestType0RejectsConfiguredFunder();
  TestAccountSignatureTypeIgnoresRoute();
  TestInitialSessionStartsOnce();
  TestInitialSessionReadyReconciles();
  TestInitialSessionLostThenReconciles();
  TestInitialSessionImmediateFailure();
  TestSecureCredentialWipe();
  TestCredentialDeriveRetryAndLatch();
  TestCredentialCreateOrDeriveFlows();
  return 0;
}
