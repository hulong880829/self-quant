#include "mds/publish/wire_publisher.h"
#include "mds/service/venue_connection.h"
#include "mds/transport/shared_ring.h"
#include "net/websocket_codec.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <arpa/inet.h>
#include <array>
#include <atomic>
#include <cassert>
#include <chrono>
#include <csignal>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <initializer_list>
#include <iostream>
#include <mutex>
#include <openssl/ssl.h>
#include <openssl/x509v3.h>
#include <sstream>
#include <set>
#include <span>
#include <string>
#include <string_view>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace {

using namespace std::chrono_literals;

std::string bybit_linear_instrument(
    std::string_view symbol, std::string_view base,
    std::string_view contract = "LinearPerpetual",
    std::string_view status = "Trading",
    std::string_view tick = "0.01",
    std::string_view qty = "0.001") {
  return std::string(R"({"symbol":")") + std::string(symbol) +
         R"(","contractType":")" + std::string(contract) +
         R"(","status":")" + std::string(status) +
         R"(","baseCoin":")" + std::string(base) +
         R"(","quoteCoin":"USDT","settleCoin":"USDT","priceFilter":{"tickSize":")" +
         std::string(tick) +
         R"("},"lotSizeFilter":{"qtyStep":")" + std::string(qty) + R"("}})";
}

std::string bybit_instruments_info(const std::string &list,
                                   std::string_view cursor) {
  return std::string(
             R"({"retCode":0,"result":{"category":"linear","nextPageCursor":")") +
         std::string(cursor) + R"(","list":[)" + list + "]}}";
}

std::string bybit_instrument_list(std::size_t count, std::string_view extra) {
  std::string list;
  list.reserve(count * 220U + extra.size());
  for (std::size_t index = 0; index < count; ++index) {
    if (!list.empty()) {
      list.push_back(',');
    }
    const auto symbol = "C" + std::to_string(index) + "USDT";
    list += bybit_linear_instrument(symbol, "C" + std::to_string(index));
  }
  if (!extra.empty()) {
    if (!list.empty()) {
      list.push_back(',');
    }
    list += extra;
  }
  return list;
}

std::string metadata_request_target(const std::string &request) {
  const auto method_end = request.find(' ');
  if (method_end == std::string::npos) {
    return {};
  }
  const auto target_end = request.find(' ', method_end + 1);
  if (target_end == std::string::npos) {
    return {};
  }
  return request.substr(method_end + 1, target_end - method_end - 1);
}

struct Certificate {
  EVP_PKEY *key{};
  X509 *certificate{};

  Certificate() {
    auto *context = EVP_PKEY_CTX_new_id(EVP_PKEY_RSA, nullptr);
    assert(context != nullptr);
    assert(EVP_PKEY_keygen_init(context) == 1);
    assert(EVP_PKEY_CTX_set_rsa_keygen_bits(context, 2048) == 1);
    assert(EVP_PKEY_keygen(context, &key) == 1);
    EVP_PKEY_CTX_free(context);
    certificate = X509_new();
    assert(certificate != nullptr);
    assert(X509_set_version(certificate, 2) == 1);
    assert(ASN1_INTEGER_set(X509_get_serialNumber(certificate), 1) == 1);
    assert(X509_gmtime_adj(X509_getm_notBefore(certificate), -60) != nullptr);
    assert(X509_gmtime_adj(X509_getm_notAfter(certificate), 3600) != nullptr);
    assert(X509_set_pubkey(certificate, key) == 1);
    auto *name = X509_get_subject_name(certificate);
    constexpr unsigned char common_name[] = "127.0.0.1";
    assert(X509_NAME_add_entry_by_txt(
               name, "CN", MBSTRING_ASC, common_name, -1, -1, 0) == 1);
    assert(X509_set_issuer_name(certificate, name) == 1);
    X509V3_CTX extension_context{};
    X509V3_set_ctx(&extension_context, certificate, certificate, nullptr,
                   nullptr, 0);
    auto *san = X509V3_EXT_conf_nid(
        nullptr, &extension_context, NID_subject_alt_name,
        const_cast<char *>("IP:127.0.0.1"));
    assert(san != nullptr);
    assert(X509_add_ext(certificate, san, -1) == 1);
    X509_EXTENSION_free(san);
    assert(X509_sign(certificate, key, EVP_sha256()) > 0);
  }

  ~Certificate() {
    X509_free(certificate);
    EVP_PKEY_free(key);
  }
};

net::SharedSslContext client_context(const Certificate &identity) {
  auto *raw = SSL_CTX_new(TLS_client_method());
  assert(raw != nullptr);
  net::SharedSslContext context(raw, net::SslCtxDeleter{});
  assert(SSL_CTX_set_min_proto_version(raw, TLS1_2_VERSION) == 1);
  SSL_CTX_set_verify(raw, SSL_VERIFY_PEER, nullptr);
  assert(X509_STORE_add_cert(SSL_CTX_get_cert_store(raw),
                             identity.certificate) == 1);
  return context;
}

bool write_all(SSL *ssl, std::string_view text) {
  std::size_t offset{};
  while (offset < text.size()) {
    const int written =
        SSL_write(ssl, text.data() + offset,
                  static_cast<int>(text.size() - offset));
    if (written <= 0) return false;
    offset += static_cast<std::size_t>(written);
  }
  return true;
}

bool read_exact(SSL *ssl, std::span<std::byte> bytes) {
  std::size_t offset{};
  while (offset < bytes.size()) {
    const int received =
        SSL_read(ssl, bytes.data() + offset,
                 static_cast<int>(bytes.size() - offset));
    if (received <= 0) return false;
    offset += static_cast<std::size_t>(received);
  }
  return true;
}

std::string read_headers(SSL *ssl) {
  std::string result;
  char byte{};
  while (result.find("\r\n\r\n") == std::string::npos &&
         result.size() < 16384) {
    if (SSL_read(ssl, &byte, 1) != 1) break;
    result.push_back(byte);
  }
  return result;
}

std::string read_masked_text(SSL *ssl) {
  std::array<std::byte, 2> header{};
  if (!read_exact(ssl, header)) return {};
  std::size_t size = std::to_integer<unsigned>(header[1] & std::byte{0x7f});
  if (size == 126) {
    std::array<std::byte, 2> extended{};
    if (!read_exact(ssl, extended)) return {};
    size = (std::to_integer<unsigned>(extended[0]) << 8U) |
           std::to_integer<unsigned>(extended[1]);
  }
  std::array<std::byte, 4> mask{};
  if (!read_exact(ssl, mask)) return {};
  std::vector<std::byte> payload(size);
  if (!read_exact(ssl, payload)) return {};
  std::string result(size, '\0');
  for (std::size_t index = 0; index < size; ++index) {
    result[index] = static_cast<char>(
        std::to_integer<unsigned>(payload[index] ^ mask[index & 3U]));
  }
  return result;
}

bool send_text(SSL *ssl, std::string_view payload) {
  std::string frame;
  frame.push_back(static_cast<char>(0x81));
  if (payload.size() < 126) {
    frame.push_back(static_cast<char>(payload.size()));
  } else {
    assert(payload.size() <= 0xffff);
    frame.push_back(static_cast<char>(126));
    frame.push_back(static_cast<char>((payload.size() >> 8U) & 0xffU));
    frame.push_back(static_cast<char>(payload.size() & 0xffU));
  }
  frame.append(payload);
  return write_all(ssl, frame);
}

std::string subscription_coin(std::string_view payload) {
  constexpr std::string_view marker{"\"coin\":\""};
  const auto begin = payload.find(marker);
  if (begin == std::string_view::npos) {
    return {};
  }
  const auto value_begin = begin + marker.size();
  const auto value_end = payload.find('"', value_begin);
  if (value_end == std::string_view::npos) {
    return {};
  }
  return std::string(
      payload.substr(value_begin, value_end - value_begin));
}

bool send_close(SSL *ssl, std::uint16_t code,
                std::string_view reason) {
  assert(reason.size() <= 123);
  std::string frame;
  frame.push_back(static_cast<char>(0x88));
  frame.push_back(static_cast<char>(reason.size() + 2));
  frame.push_back(static_cast<char>((code >> 8U) & 0xffU));
  frame.push_back(static_cast<char>(code & 0xffU));
  frame.append(reason);
  return write_all(ssl, frame);
}

enum class Protocol {
  Binance,
  BinanceAckOnly,
  BinanceMalformed,
  BinanceClose,
  BinanceClose1000,
  BinanceMetadataFailOnce,
  BinanceScaleRefresh,
  BinanceScaleRefresh503,
  BinanceScaleRefreshInvalid,
  BinancePartialMetadata,
  BinanceRejectBtc,
  BybitSymbolUnavailable,
  BybitAllSymbolsUnavailable,
  BybitMetadata429Once,
  BybitMetadataMalformedOnce,
  BybitMetadataParameterOnce,
  BybitBulkSinglePage,
  BybitBulkTwoPage,
  BybitBulkCursorRepeat,
  BybitBulkEmptyPageCursor,
  BybitBulkPageLimit,
  BybitBulkPageTwoTimeout,
  BybitBulkPageTwoRetCode,
  BybitBulkPageTwoMalformed,
  BybitBulkIntraPageDuplicate,
  BybitBulkCrossPageDuplicate,
  BybitBulkMissingSymbol,
  BybitScaleRefreshBoth,
  Bitget,
  BitgetRateLimit,
  BitgetRateLimitMissingArg,
  GateDirtyQuantity,
  GateDecimalPerpetual,
  HyperliquidWindow,
  HyperliquidRejectFirst,
  HyperliquidExpired,
  HyperliquidMalformedThenExpired,
  AsterSpot,
  AsterPerpetual,
  LighterSpot,
  LighterPerpetual
};

bool is_bybit_protocol(Protocol protocol) {
  switch (protocol) {
    case Protocol::BybitSymbolUnavailable:
    case Protocol::BybitAllSymbolsUnavailable:
    case Protocol::BybitMetadata429Once:
    case Protocol::BybitMetadataMalformedOnce:
    case Protocol::BybitMetadataParameterOnce:
    case Protocol::BybitBulkSinglePage:
    case Protocol::BybitBulkTwoPage:
    case Protocol::BybitBulkCursorRepeat:
    case Protocol::BybitBulkEmptyPageCursor:
    case Protocol::BybitBulkPageLimit:
    case Protocol::BybitBulkPageTwoTimeout:
    case Protocol::BybitBulkPageTwoRetCode:
    case Protocol::BybitBulkPageTwoMalformed:
    case Protocol::BybitBulkIntraPageDuplicate:
    case Protocol::BybitBulkCrossPageDuplicate:
    case Protocol::BybitBulkMissingSymbol:
    case Protocol::BybitScaleRefreshBoth:
      return true;
    default:
      return false;
  }
}

std::string bybit_bulk_metadata_body(Protocol protocol,
                                     unsigned metadata_request) {
  const auto candidate = bybit_linear_instrument("BTCUSDT", "BTC") + "," +
                         bybit_linear_instrument("ETHUSDT", "ETH") + "," +
                         bybit_linear_instrument("TUSDT", "T");
  switch (protocol) {
    case Protocol::BybitBulkSinglePage:
      return bybit_instruments_info(
          bybit_instrument_list(852, candidate), "");
    case Protocol::BybitBulkTwoPage:
      if (metadata_request == 1) {
        return bybit_instruments_info(
            bybit_linear_instrument("BTCUSDT", "BTC") + "," +
                bybit_linear_instrument("ETHUSDT", "ETH") + "," +
                bybit_instrument_list(10, {}),
            "page2");
      }
      return bybit_instruments_info(bybit_linear_instrument("TUSDT", "T"), "");
    case Protocol::BybitBulkCursorRepeat:
      if (metadata_request == 1) {
        return bybit_instruments_info(
            bybit_linear_instrument("BTCUSDT", "BTC"), "page2");
      }
      return bybit_instruments_info(
          bybit_linear_instrument("ETHUSDT", "ETH"), "page2");
    case Protocol::BybitBulkEmptyPageCursor:
      return bybit_instruments_info({}, "page2");
    case Protocol::BybitBulkPageLimit: {
      char cursor[8];
      std::snprintf(cursor, sizeof(cursor), "p%02u", metadata_request);
      const auto filler = "F" + std::to_string(metadata_request) + "USDT";
      return bybit_instruments_info(
          bybit_linear_instrument(filler, "F"), cursor);
    }
    case Protocol::BybitBulkPageTwoTimeout:
    case Protocol::BybitBulkPageTwoRetCode:
    case Protocol::BybitBulkPageTwoMalformed:
      if (metadata_request == 1) {
        return bybit_instruments_info(
            bybit_linear_instrument("BTCUSDT", "BTC"), "page2");
      }
      if (protocol == Protocol::BybitBulkPageTwoRetCode) {
        return R"({"retCode":10001,"retMsg":"Request parameter error","result":{}})";
      }
      if (protocol == Protocol::BybitBulkPageTwoMalformed) {
        return R"({"retCode":0,"result":)";
      }
      return bybit_instruments_info(
          bybit_linear_instrument("ETHUSDT", "ETH"), "");
    case Protocol::BybitBulkIntraPageDuplicate:
      return bybit_instruments_info(
          bybit_linear_instrument("BTCUSDT", "BTC") + "," +
              bybit_linear_instrument("BTCUSDT", "BTC") + "," +
              bybit_linear_instrument("ETHUSDT", "ETH"),
          "");
    case Protocol::BybitBulkCrossPageDuplicate:
      if (metadata_request == 1) {
        return bybit_instruments_info(
            bybit_linear_instrument("BTCUSDT", "BTC"), "page2");
      }
      return bybit_instruments_info(
          bybit_linear_instrument("BTCUSDT", "BTC") + "," +
              bybit_linear_instrument("ETHUSDT", "ETH"),
          "");
    case Protocol::BybitBulkMissingSymbol:
      return bybit_instruments_info(
          bybit_linear_instrument("BTCUSDT", "BTC") + "," +
              bybit_linear_instrument("ETHUSDT", "ETH"),
          "");
    default:
      return bybit_instruments_info(candidate, "");
  }
}

class Server {
 public:
  explicit Server(const Certificate &identity,
                  Protocol protocol = Protocol::Binance)
      : protocol_(protocol) {
    context_ = SSL_CTX_new(TLS_server_method());
    assert(context_ != nullptr);
    assert(SSL_CTX_set_min_proto_version(context_, TLS1_2_VERSION) == 1);
    assert(SSL_CTX_use_certificate(context_, identity.certificate) == 1);
    assert(SSL_CTX_use_PrivateKey(context_, identity.key) == 1);
    listener_ = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    assert(listener_ >= 0);
    int reuse = 1;
    assert(::setsockopt(listener_, SOL_SOCKET, SO_REUSEADDR, &reuse,
                       sizeof(reuse)) == 0);
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    assert(::bind(listener_, reinterpret_cast<sockaddr *>(&address),
                  sizeof(address)) == 0);
    assert(::listen(listener_, 16) == 0);
    socklen_t size = sizeof(address);
    assert(::getsockname(listener_, reinterpret_cast<sockaddr *>(&address),
                         &size) == 0);
    port_ = ntohs(address.sin_port);
    acceptor_ = std::thread([this] { accept_loop(); });
  }

  ~Server() {
    stopping_.store(true);
    ::shutdown(listener_, SHUT_RDWR);
    ::close(listener_);
    if (acceptor_.joinable()) acceptor_.join();
    for (auto &worker : workers_) {
      if (worker.joinable()) worker.join();
    }
    SSL_CTX_free(context_);
  }

  std::string endpoint(std::string_view scheme,
                       std::string_view path = {}) const {
    return std::string(scheme) + "://127.0.0.1:" +
           std::to_string(port_) + std::string(path);
  }

  unsigned btc_connections() const { return btc_connections_.load(); }
  unsigned eth_connections() const { return eth_connections_.load(); }
  unsigned btc_closures() const { return btc_closures_.load(); }
  unsigned btc_updates() const { return btc_updates_.load(); }
  unsigned gate_btc_updates() const { return gate_btc_updates_.load(); }
  unsigned gate_eth_updates() const { return gate_eth_updates_.load(); }
  unsigned gate_sol_updates() const { return gate_sol_updates_.load(); }
  unsigned gate_connections() const { return gate_connections_.load(); }
  unsigned websocket_requests() const {
    return websocket_requests_.load();
  }
  unsigned websocket_decimal_headers() const {
    return websocket_decimal_headers_.load();
  }
  unsigned metadata_decimal_headers() const {
    return metadata_decimal_headers_.load();
  }
  unsigned gate_snapshot_requests() const {
    return gate_snapshot_requests_.load();
  }
  unsigned metadata_requests() const {
    return metadata_requests_.load();
  }
  unsigned subscribe_metadata_requests() const {
    return subscribe_metadata_requests_.load();
  }
  std::vector<std::string> metadata_targets() const {
    std::lock_guard<std::mutex> lock(metadata_targets_mutex_);
    return metadata_targets_;
  }
  std::int64_t metadata_retry_delay_ms() const {
    const auto second = metadata_second_request_ms_.load();
    const auto third = metadata_third_request_ms_.load();
    return second == 0 || third == 0 ? 0 : third - second;
  }
  unsigned scale_btc_updates() const {
    return scale_btc_updates_.load();
  }
  unsigned scale_eth_updates() const {
    return scale_eth_updates_.load();
  }
  unsigned scale_sol_updates() const {
    return scale_sol_updates_.load();
  }
  unsigned hyperliquid_subscriptions_before_ack() const {
    return hyperliquid_subscriptions_before_ack_.load();
  }
  unsigned hyperliquid_subscriptions() const {
    return hyperliquid_subscriptions_.load();
  }
  unsigned hyperliquid_reject_connections() const {
    return hyperliquid_reject_connections_.load();
  }
  unsigned bitget_rate_limit_retries() const {
    return bitget_rate_limit_retries_.load();
  }
  unsigned hyperliquid_expired_connections() const {
    return hyperliquid_expired_connections_.load();
  }
  void close_btc_connection() { ++btc_close_requests_; }

 private:
  void accept_loop() {
    while (!stopping_.load()) {
      const int fd = ::accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (fd < 0) continue;
      const timeval timeout{1, 0};
      (void)::setsockopt(
          fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
      (void)::setsockopt(
          fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
      workers_.emplace_back([this, fd] { serve(fd); });
    }
  }

  void serve(int fd) {
    auto *ssl = SSL_new(context_);
    SSL_set_fd(ssl, fd);
    if (SSL_accept(ssl) == 1) {
      const auto request = read_headers(ssl);
      if (request.find("Upgrade: websocket") != std::string::npos) {
        ++websocket_requests_;
        if (request.find("\r\nX-Gate-Size-Decimal: 1\r\n") !=
            std::string::npos) {
          ++websocket_decimal_headers_;
        }
        websocket(ssl, request);
      } else {
        if (request.find("\r\nX-Gate-Size-Decimal: 1\r\n") !=
            std::string::npos) {
          ++metadata_decimal_headers_;
        }
        metadata(ssl, request);
      }
    }
    SSL_shutdown(ssl);
    SSL_free(ssl);
    ::close(fd);
  }

  void metadata(SSL *ssl, const std::string &request) {
    constexpr std::string_view content_length_header{"Content-Length:"};
    const auto length_offset = request.find(content_length_header);
    if (length_offset != std::string::npos) {
      std::size_t cursor =
          length_offset + content_length_header.size();
      while (cursor < request.size() && request[cursor] == ' ') {
        ++cursor;
      }
      std::size_t content_length = 0;
      while (cursor < request.size() && request[cursor] >= '0' &&
             request[cursor] <= '9') {
        content_length =
            content_length * 10U +
            static_cast<std::size_t>(request[cursor] - '0');
        ++cursor;
      }
      std::vector<std::byte> body(content_length);
      if (!body.empty() && !read_exact(ssl, body)) {
        return;
      }
    }
    if ((protocol_ == Protocol::AsterSpot ||
         protocol_ == Protocol::AsterPerpetual) &&
        request.find("/depth?") != std::string::npos) {
      static constexpr std::string_view body =
          R"({"lastUpdateId":100,"bids":[["100.00","1.000"],["99.99","1.000"],["99.98","1.000"],["99.97","1.000"],["99.96","1.000"],["99.95","1.000"],["99.94","1.000"],["99.93","1.000"],["99.92","1.000"],["99.91","1.000"]],"asks":[["100.01","2.000"],["100.02","2.000"],["100.03","2.000"],["100.04","2.000"],["100.05","2.000"],["100.06","2.000"],["100.07","2.000"],["100.08","2.000"],["100.09","2.000"],["100.10","2.000"]]})";
      const auto response =
          "HTTP/1.1 200 OK\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    const auto metadata_request = ++metadata_requests_;
    {
      std::lock_guard<std::mutex> lock(metadata_targets_mutex_);
      metadata_targets_.push_back(metadata_request_target(request));
    }
    const auto request_time_ms =
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now().time_since_epoch())
            .count();
    if (metadata_request == 2) {
      metadata_second_request_ms_.store(request_time_ms);
    } else if (metadata_request == 3) {
      metadata_third_request_ms_.store(request_time_ms);
    }
    if (protocol_ == Protocol::BinanceMetadataFailOnce &&
        metadata_request == 1) {
      static constexpr std::string_view body = "{}";
      const auto response =
          "HTTP/1.1 503 Service Unavailable\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    if (protocol_ == Protocol::BinanceScaleRefresh503 &&
        metadata_request == 2) {
      static constexpr std::string_view body = "{}";
      const auto response =
          "HTTP/1.1 503 Service Unavailable\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    if (protocol_ == Protocol::BybitMetadata429Once &&
        metadata_request == 1) {
      static constexpr std::string_view body =
          R"({"retCode":10006,"retMsg":"Too many visits","result":{}})";
      const auto response =
          "HTTP/1.1 429 Too Many Requests\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    if (protocol_ == Protocol::BybitMetadataMalformedOnce &&
        metadata_request == 1) {
      static constexpr std::string_view body =
          R"({"retCode":0,"result":)";
      const auto response =
          "HTTP/1.1 200 OK\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    if (protocol_ == Protocol::BybitMetadataParameterOnce &&
        metadata_request == 1) {
      static constexpr std::string_view body =
          R"({"retCode":10001,"retMsg":"Request parameter error","result":{}})";
      const auto response =
          "HTTP/1.1 200 OK\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    if (protocol_ == Protocol::BybitBulkPageTwoTimeout &&
        metadata_request == 2) {
      std::this_thread::sleep_for(1500ms);
      return;
    }
    static constexpr std::string_view binance_body =
        R"({"timezone":"UTC","symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]},{"symbol":"ETHUSDT","status":"TRADING","baseAsset":"ETH","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
    static constexpr std::string_view binance_eth_only =
        R"({"timezone":"UTC","symbols":[{"symbol":"ETHUSDT","status":"TRADING","baseAsset":"ETH","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
    static constexpr std::string_view binance_scale_initial =
        R"({"timezone":"UTC","symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]},{"symbol":"ETHUSDT","status":"TRADING","baseAsset":"ETH","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]},{"symbol":"SOLUSDT","status":"TRADING","baseAsset":"SOL","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
    static constexpr std::string_view binance_scale_refreshed =
        R"({"timezone":"UTC","symbols":[{"symbol":"SOLUSDT","status":"TRADING","baseAsset":"SOL","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":3,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.001","minPrice":"0.001","maxPrice":"1000000.000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
    static constexpr std::string_view binance_scale_invalid =
        R"({"timezone":"UTC","symbols":[{"symbol":"SOLUSDT","status":"TRADING","baseAsset":"SOL_ASSET_NAME_THAT_EXCEEDS_THE_FIXED_WIRE_FIELD_CAPACITY","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":3,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.001","minPrice":"0.001","maxPrice":"1000000.000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
    static constexpr std::string_view bitget_body =
        R"({"code":"00000","data":[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","pricePlace":"1","volumePlace":"3","priceEndStep":"1","sizeMultiplier":"0.001"},{"symbol":"ETHUSDT","baseCoin":"ETH","quoteCoin":"USDT","pricePlace":"1","volumePlace":"3","priceEndStep":"1","sizeMultiplier":"0.001"}]})";
    static constexpr std::string_view gate_metadata =
        R"([{"id":"BTC_USDT","base":"BTC","quote":"USDT","precision":2,"amount_precision":3},{"id":"ETH_USDT","base":"ETH","quote":"USDT","precision":2,"amount_precision":3},{"id":"SOL_USDT","base":"SOL","quote":"USDT","precision":3,"amount_precision":8}])";
    static constexpr std::string_view gate_perpetual_metadata =
        R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true}])";
    static constexpr std::string_view gate_snapshot =
        R"({"id":10,"current":10,"bids":[["150.000","1.00000000"]],"asks":[["150.001","2.00000000"]]})";
    static constexpr std::string_view aster_spot_metadata =
        R"({"symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","baseAssetPrecision":8,"quotePrecision":8,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000"}]}]})";
    static constexpr std::string_view aster_perpetual_metadata =
        R"({"symbols":[{"symbol":"BTCUSDT","status":"TRADING","contractType":"PERPETUAL","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000"}]}]})";
    static constexpr std::string_view bybit_btc_metadata =
        R"({"retCode":0,"result":{"category":"linear","list":[{"symbol":"BTCUSDT","contractType":"LinearPerpetual","status":"Trading","baseCoin":"BTC","quoteCoin":"USDT","settleCoin":"USDT","priceFilter":{"tickSize":"0.01"},"lotSizeFilter":{"qtyStep":"0.001"}}]}})";
    static constexpr std::string_view lighter_spot_metadata =
        R"({"code":200,"spot_order_book_details":[{"symbol":"BTC/USDC","market_id":101,"market_type":"spot","status":"active","supported_price_decimals":2,"supported_size_decimals":3,"daily_quote_token_volume":2000000}]})";
    static constexpr std::string_view lighter_perpetual_metadata =
        R"({"code":200,"order_book_details":[{"symbol":"BTC","market_id":1,"market_type":"perp","status":"active","multiplier":"1","supported_price_decimals":2,"supported_size_decimals":3,"daily_quote_token_volume":2000000}]})";
    std::string hyperliquid_metadata;
    std::string bybit_generated;
    std::string_view body = binance_body;
    if (protocol_ == Protocol::BinanceScaleRefresh ||
        protocol_ == Protocol::BinanceScaleRefresh503 ||
        protocol_ == Protocol::BinanceScaleRefreshInvalid) {
      body = metadata_request == 1
                 ? binance_scale_initial
                 : protocol_ == Protocol::BinanceScaleRefreshInvalid
                       ? binance_scale_invalid
                       : binance_scale_refreshed;
    } else if (protocol_ == Protocol::BinancePartialMetadata) {
      body = binance_eth_only;
    } else if (protocol_ == Protocol::Bitget ||
               protocol_ == Protocol::BitgetRateLimit ||
               protocol_ == Protocol::BitgetRateLimitMissingArg) {
      body = bitget_body;
    } else if (protocol_ == Protocol::GateDirtyQuantity) {
      if (request.find("/api/v4/spot/order_book?") != std::string::npos) {
        ++gate_snapshot_requests_;
        body = gate_snapshot;
      } else {
        body = gate_metadata;
      }
    } else if (protocol_ == Protocol::GateDecimalPerpetual) {
      body = gate_perpetual_metadata;
    } else if (protocol_ == Protocol::AsterSpot) {
      body = aster_spot_metadata;
    } else if (protocol_ == Protocol::AsterPerpetual) {
      body = aster_perpetual_metadata;
    } else if (protocol_ == Protocol::BybitSymbolUnavailable) {
      bybit_generated = bybit_instruments_info(
          bybit_linear_instrument("BTCUSDT", "BTC", "LinearFutures") + "," +
              bybit_linear_instrument("ETHUSDT", "ETH"),
          "");
      body = bybit_generated;
    } else if (protocol_ == Protocol::BybitAllSymbolsUnavailable) {
      bybit_generated = bybit_instruments_info(
          bybit_linear_instrument("BTCUSDT", "BTC", "LinearFutures") + "," +
              bybit_linear_instrument("ETHUSDT", "ETH", "LinearPerpetual",
                                      "PreLaunch"),
          "");
      body = bybit_generated;
    } else if (protocol_ == Protocol::BybitMetadata429Once ||
               protocol_ == Protocol::BybitMetadataMalformedOnce ||
               protocol_ == Protocol::BybitMetadataParameterOnce) {
      body = bybit_btc_metadata;
    } else if (protocol_ == Protocol::BybitScaleRefreshBoth) {
      if (request.find("symbol=") != std::string::npos) {
        const auto symbol =
            request.find("symbol=ETHUSDT") != std::string::npos ? "ETHUSDT"
                                                                : "BTCUSDT";
        const auto base = std::string_view(symbol).substr(0, 3);
        bybit_generated = bybit_instruments_info(
            bybit_linear_instrument(symbol, base, "LinearPerpetual", "Trading",
                                    "0.001"),
            "");
      } else {
        bybit_generated = bybit_instruments_info(
            bybit_linear_instrument("BTCUSDT", "BTC") + "," +
                bybit_linear_instrument("ETHUSDT", "ETH"),
            "");
      }
      body = bybit_generated;
    } else if (is_bybit_protocol(protocol_)) {
      bybit_generated = bybit_bulk_metadata_body(protocol_, metadata_request);
      body = bybit_generated;
    } else if (protocol_ == Protocol::LighterSpot) {
      body = lighter_spot_metadata;
    } else if (protocol_ == Protocol::LighterPerpetual) {
      body = lighter_perpetual_metadata;
    } else if (protocol_ == Protocol::HyperliquidWindow ||
               protocol_ == Protocol::HyperliquidRejectFirst ||
               protocol_ == Protocol::HyperliquidExpired ||
               protocol_ == Protocol::HyperliquidMalformedThenExpired) {
      const unsigned count =
          protocol_ == Protocol::HyperliquidWindow
              ? 17
              : (protocol_ == Protocol::HyperliquidExpired ||
                 protocol_ ==
                     Protocol::HyperliquidMalformedThenExpired)
                    ? 1
                    : 3;
      hyperliquid_metadata = R"({"universe":[)";
      for (unsigned index = 0; index < count; ++index) {
        if (index != 0) {
          hyperliquid_metadata.push_back(',');
        }
        hyperliquid_metadata += R"({"name":"COIN)" +
                                std::to_string(index) +
                                R"(","szDecimals":3})";
      }
      hyperliquid_metadata += "]}";
      body = hyperliquid_metadata;
    }
    const auto response =
        "HTTP/1.1 200 OK\r\nContent-Length: " +
        std::to_string(body.size()) +
        "\r\nConnection: close\r\n\r\n" + std::string(body);
    (void)write_all(ssl, response);
  }

  void websocket(SSL *ssl, const std::string &request) {
    const auto key_begin =
        request.find("Sec-WebSocket-Key: ") + std::strlen("Sec-WebSocket-Key: ");
    const auto key_end = request.find("\r\n", key_begin);
    std::array<char, 29> accept{};
    assert(net::websocket_accept_value(
        std::string_view(request).substr(key_begin, key_end - key_begin),
        accept));
    const auto response =
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
        "Connection: Upgrade\r\nSec-WebSocket-Accept: " +
        std::string(accept.data()) + "\r\n\r\n";
    if (!write_all(ssl, response)) return;
    const auto subscription = read_masked_text(ssl);
    if (is_bybit_protocol(protocol_)) {
      if (subscription.empty()) {
        return;
      }
      unsigned zero = 0;
      subscribe_metadata_requests_.compare_exchange_strong(
          zero, metadata_requests_.load());
      if (!send_text(
              ssl,
              R"({"success":true,"ret_msg":"subscribe","op":"subscribe","conn_id":"loopback"})")) {
        return;
      }
      std::vector<std::string> symbols;
      for (const char *name : {"BTCUSDT", "ETHUSDT", "TUSDT"}) {
        if (subscription.find(name) != std::string::npos) {
          symbols.emplace_back(name);
        }
      }
      if (symbols.empty()) {
        return;
      }
      ++eth_connections_;
      while (!stopping_.load()) {
        const auto sequence = ++eth_updates_;
        const bool mismatch =
            protocol_ == Protocol::BybitScaleRefreshBoth && sequence > 3;
        const char *price = mismatch ? "100.001" : "100.00";
        const char *ask = mismatch ? "100.002" : "100.01";
        for (const auto &symbol : symbols) {
          if (!send_text(
                  ssl,
                  std::string(R"({"topic":"orderbook.1.)") + symbol +
                      R"(","type":"snapshot","ts":10,"data":{"s":")" + symbol +
                      R"(","b":[[")" + price + R"(","1.000"]],"a":[[")" + ask +
                      R"(","2.000"]],"u":)" + std::to_string(sequence) +
                      R"(,"seq":)" + std::to_string(sequence) + "}}")) {
            return;
          }
        }
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::AsterSpot ||
        protocol_ == Protocol::AsterPerpetual) {
      if (subscription.find("btcusdt@bookTicker") == std::string::npos ||
          subscription.find("btcusdt@depth@100ms") == std::string::npos ||
          !send_text(ssl, R"({"result":null,"id":1})")) {
        return;
      }
      std::this_thread::sleep_for(50ms);
      if (!send_text(
              ssl,
              R"({"u":101,"e":"bookTicker","s":"BTCUSDT","b":"100.00","B":"1.000","a":"100.01","A":"2.000","E":10,"T":10})") ||
          !send_text(
              ssl,
              R"({"e":"depthUpdate","E":10,"T":10,"s":"BTCUSDT","U":101,"u":101,"pu":100,"b":[["100.00","1.000"]],"a":[["100.01","2.000"]]})") ||
          !send_text(
              ssl,
              R"({"u":102,"e":"bookTicker","s":"BTCUSDT","b":"100.00","B":"1.500","a":"100.01","A":"2.500","E":11,"T":11})") ||
          !send_text(
              ssl,
              R"({"e":"depthUpdate","E":11,"T":11,"s":"BTCUSDT","U":102,"u":102,"pu":101,"b":[["100.00","1.500"]],"a":[["100.01","2.500"]]})")) {
        return;
      }
      std::this_thread::sleep_for(200ms);
      if (!send_text(
              ssl,
              R"({"e":"depthUpdate","E":12,"T":12,"s":"BTCUSDT","U":103,"u":103,"pu":102,"b":[["100.00","1.750"]],"a":[["100.01","2.750"]]})")) {
        return;
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::LighterSpot ||
        protocol_ == Protocol::LighterPerpetual) {
      const auto market_id =
          protocol_ == Protocol::LighterSpot ? "101" : "1";
      const auto venue_symbol =
          protocol_ == Protocol::LighterSpot ? "BTC/USDC" : "BTC";
      if (subscription !=
          R"({"type":"subscribe","channel":"ticker/)" +
              std::string(market_id) + R"("})") {
        return;
      }
      if (!send_text(
              ssl,
              R"({"channel":"ticker:)" + std::string(market_id) +
                  R"(","nonce":10,"ticker":{"s":")" +
                  std::string(venue_symbol) +
                  R"(","a":{"price":"100.01","size":"2.000"},"b":{"price":"100.00","size":"1.000"}},"timestamp":10,"type":"subscribed/ticker"})")) {
        return;
      }
      const auto book_subscription = read_masked_text(ssl);
      if (book_subscription !=
          R"({"type":"subscribe","channel":"order_book/)" +
              std::string(market_id) + R"("})") {
        return;
      }
      if (!send_text(
              ssl,
              R"({"channel":"order_book:)" + std::string(market_id) +
                  R"(","order_book":{"asks":[{"price":"100.01","size":"2.000"},{"price":"100.02","size":"2.000"},{"price":"100.03","size":"2.000"},{"price":"100.04","size":"2.000"},{"price":"100.05","size":"2.000"},{"price":"100.06","size":"2.000"},{"price":"100.07","size":"2.000"},{"price":"100.08","size":"2.000"},{"price":"100.09","size":"2.000"},{"price":"100.10","size":"2.000"}],"bids":[{"price":"100.00","size":"1.000"},{"price":"99.99","size":"1.000"},{"price":"99.98","size":"1.000"},{"price":"99.97","size":"1.000"},{"price":"99.96","size":"1.000"},{"price":"99.95","size":"1.000"},{"price":"99.94","size":"1.000"},{"price":"99.93","size":"1.000"},{"price":"99.92","size":"1.000"},{"price":"99.91","size":"1.000"}],"nonce":20,"begin_nonce":0,"offset":100},"timestamp":10,"type":"subscribed/order_book"})") ||
          !send_text(
              ssl,
              R"({"channel":"order_book:)" + std::string(market_id) +
                  R"(","order_book":{"asks":[{"price":"100.01","size":"2.500"}],"bids":[{"price":"100.00","size":"1.500"}],"nonce":21,"begin_nonce":20,"offset":999},"timestamp":11,"type":"update/order_book"})")) {
        return;
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::HyperliquidExpired ||
        protocol_ == Protocol::HyperliquidMalformedThenExpired) {
      const auto coin = subscription_coin(subscription);
      if (coin.empty() ||
          !send_text(
              ssl,
              R"({"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":{"type":"bbo","coin":")" +
                  coin + R"("}}})")) {
        return;
      }
      const auto connection = ++hyperliquid_expired_connections_;
      if (connection == 1) {
        if (protocol_ ==
            Protocol::HyperliquidMalformedThenExpired) {
          (void)send_text(ssl, R"({"channel":"bbo","data":)");
        }
        (void)send_close(ssl, 1000, "Expired");
        return;
      }
      if (!send_text(
              ssl,
              R"({"channel":"bbo","data":{"coin":")" + coin +
                  R"(","time":10,"bbo":[{"px":"100.000","sz":"1.000"},{"px":"100.001","sz":"2.000"}]}})")) {
        return;
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::HyperliquidWindow) {
      std::vector<std::string> subscriptions;
      subscriptions.push_back(subscription);
      while (subscriptions.size() < 16) {
        auto next = read_masked_text(ssl);
        if (next.empty()) {
          return;
        }
        subscriptions.push_back(std::move(next));
      }
      hyperliquid_subscriptions_before_ack_.store(
          static_cast<unsigned>(subscriptions.size()));
      for (std::size_t index = subscriptions.size(); index != 0;
           --index) {
        const auto coin = subscription_coin(subscriptions[index - 1]);
        if (coin.empty()) {
          return;
        }
        std::this_thread::sleep_for(70ms);
        if (!send_text(
                ssl,
                R"({"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":{"type":"bbo","coin":")" +
                    coin + R"("}}})")) {
          return;
        }
      }
      auto final_subscription = read_masked_text(ssl);
      const auto final_coin =
          subscription_coin(final_subscription);
      if (final_coin.empty()) {
        return;
      }
      hyperliquid_subscriptions_.store(17);
      if (!send_text(
              ssl,
              R"({"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":{"type":"bbo","coin":")" +
                  final_coin + R"("}}})")) {
        return;
      }
      for (unsigned index = 0; index < 17; ++index) {
        const auto coin = "COIN" + std::to_string(index);
        if (!send_text(
                ssl,
                R"({"channel":"bbo","data":{"coin":")" + coin +
                    R"(","time":10,"bbo":[{"px":"100.0","sz":"1.000"},{"px":"100.1","sz":"2.000"}]}})")) {
          return;
        }
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::HyperliquidRejectFirst) {
      const auto connection = ++hyperliquid_reject_connections_;
      if (connection == 1) {
        for (unsigned index = 1; index < 3; ++index) {
          if (read_masked_text(ssl).empty()) {
            return;
          }
        }
        (void)send_text(
            ssl,
            R"({"channel":"error","data":"invalid subscription"})");
        return;
      }
      if (subscription.find("COIN1") == std::string::npos ||
          !send_text(
              ssl,
              R"({"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":{"type":"bbo","coin":"COIN1"}}})")) {
        return;
      }
      const auto second = read_masked_text(ssl);
      if (second.find("COIN2") == std::string::npos ||
          !send_text(
              ssl,
              R"({"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":{"type":"bbo","coin":"COIN2"}}})")) {
        return;
      }
      for (unsigned index = 1; index < 3; ++index) {
        const auto coin = "COIN" + std::to_string(index);
        if (!send_text(
                ssl,
                R"({"channel":"bbo","data":{"coin":")" + coin +
                    R"(","time":10,"bbo":[{"px":"100.0","sz":"1.000"},{"px":"100.1","sz":"2.000"}]}})")) {
          return;
        }
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::BinanceScaleRefresh ||
        protocol_ == Protocol::BinanceScaleRefresh503 ||
        protocol_ == Protocol::BinanceScaleRefreshInvalid) {
      if (!send_text(ssl, R"({"result":null,"id":1})")) {
        return;
      }
      const auto update = [ssl](std::string_view symbol,
                                std::string_view bid,
                                unsigned sequence) {
        return send_text(
            ssl,
            R"({"u":)" + std::to_string(sequence) + R"(,"s":")" +
                std::string(symbol) + R"(","b":")" +
                std::string(bid) +
                R"(","B":"1.000","a":")" + std::string(bid) +
                R"(","A":"2.000","E":1})");
      };
      if (!update("BTCUSDT", "100.00", 1) ||
          !update("ETHUSDT", "200.00", 1) ||
          !update("SOLUSDT", "150.00", 1)) {
        return;
      }
      ++scale_btc_updates_;
      ++scale_eth_updates_;
      ++scale_sol_updates_;
      for (unsigned sequence = 2; sequence <= 4; ++sequence) {
        if (!update("SOLUSDT", "150.001", sequence)) {
          return;
        }
        ++scale_sol_updates_;
      }
      for (unsigned sequence = 2; sequence <= 80; ++sequence) {
        if (!update("BTCUSDT", "100.00", sequence) ||
            !update("ETHUSDT", "200.00", sequence)) {
          return;
        }
        ++scale_btc_updates_;
        ++scale_eth_updates_;
        std::this_thread::sleep_for(5ms);
      }
      const auto unsubscribe = read_masked_text(ssl);
      if (unsubscribe.find("solusdt") == std::string::npos ||
          !send_text(ssl, R"({"result":null,"id":1})")) {
        return;
      }
      const auto resubscribe = read_masked_text(ssl);
      if (resubscribe.find("solusdt") == std::string::npos ||
          !send_text(ssl, R"({"result":null,"id":1})")) {
        return;
      }
      for (unsigned sequence = 5;
           sequence <= 20 && !stopping_.load(); ++sequence) {
        if (!update("BTCUSDT", "100.00", sequence) ||
            !update("ETHUSDT", "200.00", sequence) ||
            !update("SOLUSDT", "150.001", sequence)) {
          return;
        }
        ++scale_btc_updates_;
        ++scale_eth_updates_;
        ++scale_sol_updates_;
        std::this_thread::sleep_for(5ms);
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    if (protocol_ == Protocol::BinanceRejectBtc) {
      const auto handle = [this, ssl](const std::string &message) {
        if (message.empty()) {
          return true;
        }
        if (message.find("btcusdt") != std::string::npos) {
          return send_text(
              ssl, R"({"code":2,"msg":"Invalid symbol","id":1})");
        }
        if (message.find("ethusdt") == std::string::npos) {
          return true;
        }
        if (!send_text(ssl, R"({"result":null,"id":1})")) {
          return false;
        }
        ++eth_connections_;
        while (!stopping_.load()) {
          const auto sequence = ++eth_updates_;
          if (!send_text(
                  ssl,
                  R"({"u":)" + std::to_string(sequence) +
                      R"(,"s":"ETHUSDT","b":"100.00","B":"1.000","a":"100.01","A":"2.000","E":1})")) {
            return false;
          }
          std::this_thread::sleep_for(10ms);
        }
        return true;
      };
      if (!handle(subscription)) {
        return;
      }
      while (!stopping_.load()) {
        const auto next = read_masked_text(ssl);
        if (!handle(next)) {
          return;
        }
      }
      return;
    }
    if (protocol_ == Protocol::GateDirtyQuantity) {
      ++gate_connections_;
      const auto acknowledge = [ssl] {
        return send_text(
            ssl,
            R"({"event":"subscribe","result":{"status":"success"}})");
      };
      if (subscription.find("BTC_USDT") == std::string::npos ||
          !acknowledge()) {
        return;
      }
      const auto eth_subscription = read_masked_text(ssl);
      if (eth_subscription.find("ETH_USDT") == std::string::npos ||
          !acknowledge()) {
        return;
      }
      const auto sol_subscription = read_masked_text(ssl);
      if (sol_subscription.find("SOL_USDT") == std::string::npos ||
          !acknowledge()) {
        return;
      }
      const auto update = [ssl](std::string_view symbol,
                                std::string_view price,
                                std::string_view quantity,
                                unsigned sequence) {
        return send_text(
            ssl,
            R"({"channel":"spot.book_ticker","event":"update","time_ms":)" +
                std::to_string(sequence) + R"(,"result":{"s":")" +
                std::string(symbol) + R"(","b":")" +
                std::string(price) + R"(","B":")" +
                std::string(quantity) + R"(","a":")" +
                std::string(price) + R"(","A":")" +
                std::string(quantity) + R"(","u":)" +
                std::to_string(sequence) + R"(,"t":)" +
                std::to_string(sequence) + "}}");
      };
      if (!update("BTC_USDT", "100.00", "1.000", 1) ||
          !update("ETH_USDT", "200.00", "1.000", 1) ||
          !update("SOL_USDT", "150.000", "1.00000000", 1)) {
        return;
      }
      ++gate_btc_updates_;
      ++gate_eth_updates_;
      ++gate_sol_updates_;
      if (!update("SOL_USDT", "150.000", "0.123456789", 2)) {
        return;
      }
      for (unsigned sequence = 2; sequence <= 5; ++sequence) {
        if (!update("BTC_USDT", "100.00", "1.000", sequence) ||
            !update("ETH_USDT", "200.00", "1.000", sequence)) {
          return;
        }
        ++gate_btc_updates_;
        ++gate_eth_updates_;
        std::this_thread::sleep_for(10ms);
      }
      const auto sol_unsubscribe = read_masked_text(ssl);
      if (sol_unsubscribe.find("SOL_USDT") == std::string::npos ||
          !acknowledge()) {
        return;
      }
      const auto sol_resubscribe = read_masked_text(ssl);
      if (sol_resubscribe.find("SOL_USDT") == std::string::npos ||
          !acknowledge()) {
        return;
      }
      if (!stopping_.load() &&
          update("SOL_USDT", "150.000", "1.00000000", 3)) {
        ++gate_sol_updates_;
      }
      while (!stopping_.load()) std::this_thread::sleep_for(10ms);
      return;
    }
    if (protocol_ == Protocol::GateDecimalPerpetual) {
      const auto connection = ++gate_connections_;
      if (subscription.find("BTC_USDT") == std::string::npos ||
          !send_text(
              ssl,
              R"({"event":"subscribe","result":{"status":"success"}})") ||
          !send_text(
              ssl,
              R"({"channel":"futures.book_ticker","event":"update","time_ms":10,"result":{"s":"BTC_USDT","b":"100.0","B":"0.5","a":"100.1","A":"1.7","u":7,"t":10}})")) {
        return;
      }
      if (connection == 1) {
        (void)send_close(ssl, 1001, "reconnect");
        return;
      }
      while (!stopping_.load()) {
        std::this_thread::sleep_for(10ms);
      }
      return;
    }
    const bool btc = subscription.find("btcusdt") != std::string::npos ||
                     subscription.find("BTCUSDT") != std::string::npos;
    const auto number = btc ? ++btc_connections_ : ++eth_connections_;
    const std::string symbol = btc ? "BTCUSDT" : "ETHUSDT";
    if (protocol_ == Protocol::BitgetRateLimitMissingArg) {
      (void)send_text(
          ssl,
          R"({"event":"error","code":"30006","msg":"request too many"})");
      return;
    }
    if (protocol_ == Protocol::BitgetRateLimit) {
      if (!send_text(
              ssl,
              R"({"event":"error","arg":{"instType":"USDT-FUTURES","channel":"books1","instId":")" +
                  symbol +
                  R"("},"code":"30006","msg":"request too many"})")) {
        return;
      }
      const auto retry = read_masked_text(ssl);
      if (retry.find("\"op\":\"subscribe\"") == std::string::npos ||
          retry.find("\"unsubscribe\"") != std::string::npos ||
          retry.find(symbol) == std::string::npos) {
        return;
      }
      ++bitget_rate_limit_retries_;
      if (!send_text(
              ssl,
              R"({"event":"subscribe","arg":{"instType":"USDT-FUTURES","channel":"books1","instId":")" +
                  symbol + R"("}})") ||
          !send_text(
              ssl,
              R"({"arg":{"channel":"books1","instId":")" + symbol +
                  R"("},"action":"snapshot","data":[{"bids":[],"asks":[["100.1","2.000"]],"ts":"10","seq":1}]})") ||
          !send_text(
              ssl,
              R"({"arg":{"channel":"books1","instId":")" + symbol +
                  R"("},"action":"snapshot","data":[{"bids":[["100.0","1.000"]],"asks":[["100.1","2.000"]],"ts":"11","seq":2}]})")) {
        return;
      }
      while (!stopping_.load()) std::this_thread::sleep_for(10ms);
      return;
    }
    if (protocol_ == Protocol::Bitget) {
      if (!send_text(
              ssl,
              R"({"event":"subscribe","arg":{"instType":"USDT-FUTURES","channel":"books1","instId":")" +
                  symbol + R"("}})")) {
        return;
      }
      if (btc && number == 1) {
        (void)send_text(ssl, "{\"arg\":\n");
        return;
      }
      if (!send_text(
              ssl,
              R"({"arg":{"channel":"books1","instId":")" + symbol +
                  R"("},"action":"snapshot","data":[{"bids":[["100.0","1.000"]],"asks":[["100.1","2.000"]],"ts":"10","seq":1}]})")) {
        return;
      }
      while (!stopping_.load()) std::this_thread::sleep_for(10ms);
      return;
    }
    if (!send_text(ssl, R"({"result":null,"id":1})")) {
      return;
    }
    if (protocol_ == Protocol::BinanceAckOnly) {
      while (!stopping_.load()) std::this_thread::sleep_for(10ms);
      return;
    }
    if (protocol_ == Protocol::BinanceMalformed && btc && number == 1) {
      (void)send_text(ssl, "{\"s\":\"BTCUSDT\",");
      return;
    }
    if ((protocol_ == Protocol::BinanceClose ||
         protocol_ == Protocol::BinanceClose1000) &&
        btc && number == 1) {
      (void)send_close(
          ssl, protocol_ == Protocol::BinanceClose ? 1001 : 1000,
          protocol_ == Protocol::BinanceClose ? "rotate" : "Expired");
      ++btc_closures_;
      return;
    }
    const auto close_request = btc_close_requests_.load();
    auto &updates = btc ? btc_updates_ : eth_updates_;
    while (!stopping_.load() &&
           (!btc || btc_close_requests_.load() == close_request)) {
      const auto sequence = ++updates;
      if (!send_text(
              ssl,
              R"({"u":)" + std::to_string(sequence) + R"(,"s":")" +
                  symbol +
                  R"(","b":"100.00","B":"1.000","a":"100.01","A":"2.000","E":1})")) {
        return;
      }
      std::this_thread::sleep_for(10ms);
    }
    if (btc && !stopping_.load()) {
      ++btc_closures_;
    }
  }

  SSL_CTX *context_{};
  int listener_{-1};
  std::uint16_t port_{};
  std::atomic<bool> stopping_{};
  std::atomic<unsigned> btc_connections_{};
  std::atomic<unsigned> eth_connections_{};
  std::atomic<unsigned> btc_close_requests_{};
  std::atomic<unsigned> btc_closures_{};
  std::atomic<unsigned> btc_updates_{};
  std::atomic<unsigned> eth_updates_{};
  std::atomic<unsigned> gate_btc_updates_{};
  std::atomic<unsigned> gate_eth_updates_{};
  std::atomic<unsigned> gate_sol_updates_{};
  std::atomic<unsigned> gate_connections_{};
  std::atomic<unsigned> websocket_requests_{};
  std::atomic<unsigned> websocket_decimal_headers_{};
  std::atomic<unsigned> metadata_decimal_headers_{};
  std::atomic<unsigned> gate_snapshot_requests_{};
  std::atomic<unsigned> metadata_requests_{};
  std::atomic<unsigned> subscribe_metadata_requests_{};
  std::atomic<std::int64_t> metadata_second_request_ms_{};
  std::atomic<std::int64_t> metadata_third_request_ms_{};
  std::atomic<unsigned> scale_btc_updates_{};
  std::atomic<unsigned> scale_eth_updates_{};
  std::atomic<unsigned> scale_sol_updates_{};
  std::atomic<unsigned> hyperliquid_subscriptions_before_ack_{};
  std::atomic<unsigned> hyperliquid_subscriptions_{};
  std::atomic<unsigned> hyperliquid_reject_connections_{};
  std::atomic<unsigned> bitget_rate_limit_retries_{};
  std::atomic<unsigned> hyperliquid_expired_connections_{};
  Protocol protocol_{};
  mutable std::mutex metadata_targets_mutex_;
  std::vector<std::string> metadata_targets_;
  std::thread acceptor_;
  std::vector<std::thread> workers_;
};

struct Audit final : utils::md::wire::RecordVisitor {
  bool OnInstrument(
      const utils::md::wire::InstrumentUpdateRecord &record) noexcept override {
    instruments.insert(record.header.instrument_id);
    observe(record.header.bus_seq);
    return true;
  }
  bool OnInstrumentCatalog(
      const utils::md::wire::InstrumentCatalogRecord &record) noexcept override {
    catalogs.insert(record.header.instrument_id);
    ++catalog_records;
    observe(record.header.bus_seq);
    return true;
  }
  bool OnBbo(const utils::md::wire::BboRecord &record) noexcept override {
    bbos.insert(record.header.instrument_id);
    ++bbo_records;
    last_bid_price = record.bid_price;
    last_ask_price = record.ask_price;
    observe(record.header.bus_seq);
    return true;
  }
  bool OnTicker(const utils::md::wire::TickerRecord &) noexcept override {
    return true;
  }
  bool OnDelta(const utils::md::wire::DeltaRecord &) noexcept override {
    ++delta_records;
    return true;
  }
  bool OnSnapshotBegin(
      const utils::md::wire::SnapshotBeginRecord &) noexcept override {
    ++snapshot_begin_records;
    return true;
  }
  bool OnSnapshotChunk(
      const utils::md::wire::SnapshotChunkRecord &) noexcept override {
    return true;
  }
  bool OnSnapshotEnd(
      const utils::md::wire::SnapshotEndRecord &) noexcept override {
    return true;
  }
  void observe(std::uint64_t sequence) {
    monotonic = monotonic && sequence > last_bus_seq;
    last_bus_seq = sequence;
  }

  std::set<utils::md::InstrumentId> catalogs;
  std::set<utils::md::InstrumentId> instruments;
  std::set<utils::md::InstrumentId> bbos;
  std::size_t catalog_records{};
  std::size_t bbo_records{};
  std::size_t delta_records{};
  std::size_t snapshot_begin_records{};
  std::int64_t last_bid_price{};
  std::int64_t last_ask_price{};
  std::uint64_t last_bus_seq{};
  bool monotonic{true};
};

}  // namespace

void test_binance_shard_reconnect() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 300ms;
  options.reconnect_max = 300ms;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.sharding.loopback." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  assert(connection->websocket_shard_count() == 2);

  mds::transport::RingOptions attach;
  attach.create = false;
  attach.name = mds::publish::make_multiplex_segment_name(
      "/mds.sharding.loopback." + std::to_string(::getpid()),
      "binance", "perpetual", "ticker", 0);
  auto opened = mds::transport::SharedRing::open(attach);
  assert(opened);
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      mds::transport::process_start_marker(::getpid()),
      static_cast<std::uint64_t>(
          std::chrono::steady_clock::now().time_since_epoch().count()));
  assert(registered);
  auto reader = registered.value;
  Audit audit;
  const auto drain = [&] {
    (void)manager.run_once(5);
    for (;;) {
      auto record = ring.read(reader);
      if (!record) break;
      assert(utils::md::wire::Decode(record.value->payload, audit) ==
             utils::md::wire::CodecError::Ok);
      assert(record.value.commit());
    }
  };
  const auto wait_until = [&](auto predicate,
                              std::chrono::milliseconds timeout = 5s) {
    const auto deadline = std::chrono::steady_clock::now() + timeout;
    while (std::chrono::steady_clock::now() < deadline) {
      drain();
      if (predicate()) return true;
    }
    return false;
  };

  const bool initially_live = wait_until([&] {
    return connection->state() == mds::service::MarketDataState::Live &&
           server.btc_connections() == 1 &&
           server.eth_connections() == 1 &&
           audit.catalogs.size() == 2 &&
           audit.instruments.size() == 2 && audit.bbos.size() == 2;
  });
  if (!initially_live) {
    std::cerr << "initial Binance shards did not become live state="
              << mds::service::to_string(connection->state())
              << " error=\"" << connection->error_message()
              << "\" btc_connections=" << server.btc_connections()
              << " eth_connections=" << server.eth_connections()
              << " catalogs=" << audit.catalogs.size()
              << " instruments=" << audit.instruments.size()
              << " bbos=" << audit.bbos.size() << '\n';
  }
  assert(initially_live);

  std::ostringstream diagnostics;
  auto *original = std::cerr.rdbuf(diagnostics.rdbuf());
  for (unsigned disconnect = 1; disconnect <= 2; ++disconnect) {
    const auto prior_btc_connections = server.btc_connections();
    const auto prior_btc_updates = server.btc_updates();
    server.close_btc_connection();
    assert(wait_until([&] {
      return server.btc_closures() >= disconnect;
    }));

    const auto healthy_baseline = audit.bbo_records;
    assert(wait_until([&] {
      return audit.bbo_records >= healthy_baseline + 3 &&
             server.btc_connections() == prior_btc_connections;
    }, 200ms));

    const bool restored = wait_until([&] {
      return connection->state() == mds::service::MarketDataState::Live &&
             connection->metrics().ws_shards_live == 2 &&
             server.btc_connections() == prior_btc_connections + 1 &&
             server.btc_updates() > prior_btc_updates &&
             connection->metrics().reconnects >= disconnect;
    });
    if (!restored) {
      std::fprintf(
          stderr,
          "Binance shard did not recover disconnect=%u state=%.*s "
          "error=\"%.*s\" live_shards=%llu reconnecting_shards=%llu "
          "reconnects=%llu btc_connections=%u eth_connections=%u\n",
          disconnect,
          static_cast<int>(mds::service::to_string(connection->state()).size()),
          mds::service::to_string(connection->state()).data(),
          static_cast<int>(connection->error_message().size()),
          connection->error_message().data(),
          static_cast<unsigned long long>(
              connection->metrics().ws_shards_live),
          static_cast<unsigned long long>(
              connection->metrics().ws_shards_reconnecting),
          static_cast<unsigned long long>(connection->metrics().reconnects),
          server.btc_connections(), server.eth_connections());
      std::fprintf(stderr, "Reconnect diagnostics:\n%s",
                   diagnostics.str().c_str());
    }
    assert(restored);
  }
  std::cerr.rdbuf(original);

  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().ws_shards == 2);
  assert(connection->metrics().ws_shards_live == 2);
  assert(connection->metrics().reconnects == 2);
  assert(server.btc_connections() == 3);
  assert(server.eth_connections() == 1);
  assert(server.websocket_decimal_headers() == 0);
  assert(server.metadata_decimal_headers() == 0);
  assert(audit.catalogs.size() == 2);
  assert(audit.instruments.size() == 2);
  assert(audit.bbos.size() == 2);
  assert(audit.monotonic);
  const auto log = diagnostics.str();
  const auto count = [&](std::string_view marker) {
    std::size_t result{};
    for (std::size_t offset{}; (offset = log.find(marker, offset)) !=
                               std::string::npos;
         offset += marker.size()) {
      ++result;
    }
    return result;
  };
  assert(count("binance websocket reconnect scheduled") == 2);
  assert(count("binance websocket reconnect restored") == 2);
  assert(count("binance websocket reconnect live") == 2);
  assert(count("product=perpetual shard=0") == 6);
  assert(count("attempt=1") == 6);

  assert(ring.unregister_reader(reader));
  auto late_registered = ring.register_reader(
      mds::transport::process_start_marker(::getpid()),
      static_cast<std::uint64_t>(
          std::chrono::steady_clock::now().time_since_epoch().count()));
  assert(late_registered);
  auto late_reader = late_registered.value;
  Audit late_audit;
  const auto late_deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < late_deadline) {
    (void)manager.run_once(5);
    for (;;) {
      auto record = ring.read(late_reader);
      if (!record) break;
      assert(utils::md::wire::Decode(record.value->payload, late_audit) ==
             utils::md::wire::CodecError::Ok);
      assert(record.value.commit());
    }
    if (late_audit.catalogs.size() == 2 &&
        late_audit.instruments.size() == 2 &&
        late_audit.bbos.size() == 2) {
      break;
    }
  }
  assert(late_audit.catalogs.size() == 2);
  assert(late_audit.instruments.size() == 2);
  assert(late_audit.bbos.size() == 2);
  assert(late_audit.monotonic);
  manager.stop();
}

void test_multiplex_publishers_survive_compatible_replace() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity);
  mds::service::VenueConnectionManager manager(tls);
  const auto prefix =
      "/mds.replace.loopback." + std::to_string(::getpid());
  const auto make_options = [&] {
    mds::service::VenueConnectionOptions options;
    options.venue = utils::md::Venue::Binance;
    options.product = utils::md::ProductType::Perpetual;
    options.websocket_endpoint = server.endpoint("wss", "/ws");
    options.rest_endpoint = server.endpoint("https");
    options.connect_timeout = 1s;
    options.request_timeout = 1s;
    options.idle_timeout = 5s;
    mds::service::SymbolStreamOptions stream;
    stream.symbol = "BTCUSDT";
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix = prefix;
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
    return options;
  };

  auto replacement_options = make_options();
  auto created = manager.create(make_options());
  assert(created);
  auto *connection = created.value;

  mds::transport::RingOptions attach;
  attach.create = false;
  attach.name = mds::publish::make_multiplex_segment_name(
      prefix, "binance", "perpetual", "ticker", 0);
  auto opened = mds::transport::SharedRing::open(attach);
  assert(opened);
  auto ring = std::move(opened.value);
  const auto segment_name = std::string(ring.name());
  const auto producer_epoch = ring.epoch();
  auto registered = ring.register_reader(
      mds::transport::process_start_marker(::getpid()),
      static_cast<std::uint64_t>(
          std::chrono::steady_clock::now().time_since_epoch().count()));
  assert(registered);
  auto reader = registered.value;
  Audit audit;
  const auto drain = [&] {
    (void)manager.run_once(5);
    for (;;) {
      auto record = ring.read(reader);
      if (!record) break;
      assert(utils::md::wire::Decode(record.value->payload, audit) ==
             utils::md::wire::CodecError::Ok);
      assert(record.value.commit());
    }
  };
  const auto wait_until = [&](auto predicate) {
    const auto deadline = std::chrono::steady_clock::now() + 5s;
    while (std::chrono::steady_clock::now() < deadline) {
      drain();
      if (predicate()) return true;
    }
    return false;
  };

  assert(wait_until([&] {
    return connection->state() == mds::service::MarketDataState::Live &&
           audit.bbo_records != 0;
  }));
  const auto bus_seq_before_replace = audit.last_bus_seq;
  const auto records_before_replace = audit.bbo_records;

  auto replaced =
      manager.replace(connection, std::move(replacement_options));
  assert(replaced);
  connection = replaced.value;
  assert(ring.name() == segment_name);
  assert(ring.epoch() == producer_epoch);
  assert(wait_until([&] {
    return connection->state() == mds::service::MarketDataState::Live &&
           audit.bbo_records > records_before_replace;
  }));
  assert(audit.last_bus_seq > bus_seq_before_replace);
  assert(audit.monotonic);

  auto incompatible = make_options();
  incompatible.streams.front().shard_count = 2;
  replaced = manager.replace(connection, std::move(incompatible));
  assert(replaced);
  connection = replaced.value;
  auto reopened = mds::transport::SharedRing::open(attach);
  assert(reopened);
  assert(reopened.value.name() == segment_name);
  assert(reopened.value.epoch() != producer_epoch);

  assert(ring.unregister_reader(reader));
  manager.stop();
}

void test_binance_ticker_only_ack_is_live() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceAckOnly);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.binance.ack.loopback." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         connection->state() != mds::service::MarketDataState::Live) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().ws_shards_live == 1);
  assert(connection->metrics().ticker_updates == 0);
  manager.stop();
}

void test_continuous_recovery_timeout_isolates_connection() {
  Certificate identity;
  auto tls = client_context(identity);
  Server stalled_server(identity, Protocol::BinanceAckOnly);
  Server healthy_server(identity);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = stalled_server.endpoint("wss", "/ws");
  options.rest_endpoint = stalled_server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.max_continuous_recovery_duration = 100ms;
  options.reconnect_base = 20ms;
  options.reconnect_max = 40ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.orderbook = true;
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.binance.recovery.timeout." + std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));

  auto created = manager.create(std::move(options));
  assert(created);
  auto *stalled = created.value;

  mds::service::VenueConnectionOptions healthy_options;
  healthy_options.venue = utils::md::Venue::Binance;
  healthy_options.product = utils::md::ProductType::Spot;
  healthy_options.websocket_endpoint =
      healthy_server.endpoint("wss", "/ws");
  healthy_options.rest_endpoint = healthy_server.endpoint("https");
  healthy_options.connect_timeout = 1s;
  healthy_options.request_timeout = 1s;
  healthy_options.idle_timeout = 5s;
  mds::service::SymbolStreamOptions healthy_stream;
  healthy_stream.symbol = "ETHUSDT";
  healthy_stream.ticker = true;
  healthy_stream.ticker_channel = "bookTicker";
  healthy_stream.ring_layout = mds::publish::RingLayout::Multiplex;
  healthy_stream.shard_count = 1;
  healthy_stream.shm_prefix =
      "/mds.binance.recovery.healthy." + std::to_string(::getpid());
  healthy_stream.multiplex_ring.ring_bytes = 64U << 10U;
  healthy_stream.multiplex_ring.max_record_bytes = 4096;
  healthy_stream.multiplex_ring.max_readers = 4;
  healthy_stream.multiplex_ring.unlink_on_close = true;
  healthy_options.streams.push_back(std::move(healthy_stream));
  auto healthy_created = manager.create(std::move(healthy_options));
  assert(healthy_created);
  auto *healthy = healthy_created.value;

  const auto deadline = std::chrono::steady_clock::now() + 2s;
  while (std::chrono::steady_clock::now() < deadline &&
         (stalled->metrics().connection_rebuild_attempts < 1 ||
          healthy->state() != mds::service::MarketDataState::Live ||
          healthy->metrics().ticker_updates < 3)) {
    (void)manager.run_once(5);
  }
  assert(stalled->state() != mds::service::MarketDataState::Failed);
  assert(stalled->metrics().connection_rebuild_attempts >= 1);
  assert(stalled->metrics().connection_rebuild_successes == 0);
  assert(healthy->state() == mds::service::MarketDataState::Live);
  assert(healthy->metrics().ticker_updates >= 3);
  assert(healthy->metrics().connection_rebuild_attempts == 0);
  manager.stop();
}

void test_metadata_failure_rebuilds_connection() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceMetadataFailOnce);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 20ms;
  options.reconnect_max = 40ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.ticker = true;
  stream.ticker_channel = "bookTicker";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.binance.metadata.rebuild." + std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));

  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().connection_rebuild_successes == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(server.metadata_requests() >= 2);
  assert(connection->metrics().connection_rebuild_attempts >= 1);
  assert(connection->metrics().connection_rebuild_successes == 1);
  assert(connection->metrics().connection_rebuild_failures == 0);
  manager.stop();
}

void test_symbol_scale_refresh(Protocol protocol,
                               bool expect_retry) {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, protocol);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 3;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.snapshot_failure_backoff_ms = 20;
  options.snapshot_failure_backoff_max_ms = 40;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT", "SOLUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.binance.scale.refresh." +
        std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->metrics().metadata_refresh_successes < 1 ||
          server.scale_sol_updates() < 5 ||
          connection->state() !=
              mds::service::MarketDataState::Live)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() ==
         mds::service::MarketDataState::Live);
  assert(connection->metrics().scale_mismatch_frames >= 1);
  assert(connection->metrics().scale_mismatch_symbols == 1);
  assert(connection->metrics().metadata_refresh_coalesced >= 2);
  assert(connection->metrics().metadata_refresh_successes == 1);
  assert(connection->metrics().resyncs == 1);
  assert(connection->metrics().last_resync_reason ==
         mds::service::ResyncReason::ScaleMismatch);
  assert(std::string_view(
             connection->metrics().last_resync_symbol.data()) ==
         "SOLUSDT");
  assert(connection->metrics().reconnects == 0);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(server.scale_btc_updates() >= 80);
  assert(server.scale_eth_updates() >= 80);
  assert(server.scale_sol_updates() >= 5);
  if (expect_retry) {
    assert(connection->metrics().metadata_refresh_failures >= 1);
    assert(server.metadata_requests() >= 3);
    assert(server.metadata_retry_delay_ms() >= 900);
  } else {
    assert(connection->metrics().metadata_refresh_failures == 0);
    assert(server.metadata_requests() == 2);
  }
  manager.stop();
}

void test_invalid_symbol_scale_refresh_does_not_publish_catalog() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceScaleRefreshInvalid);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 3;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.snapshot_max_consecutive_failures = 1;
  const auto prefix =
      "/mds.binance.scale.invalid." +
      std::to_string(::getpid());
  for (const auto symbol : {"BTCUSDT", "ETHUSDT", "SOLUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix = prefix;
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;

  mds::transport::RingOptions attach;
  attach.create = false;
  attach.name = mds::publish::make_multiplex_segment_name(
      prefix, "binance", "perpetual", "ticker", 0);
  auto opened = mds::transport::SharedRing::open(attach);
  assert(opened);
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      mds::transport::process_start_marker(::getpid()),
      static_cast<std::uint64_t>(
          std::chrono::steady_clock::now().time_since_epoch().count()));
  assert(registered);
  auto reader = registered.value;
  Audit audit;
  const auto deadline = std::chrono::steady_clock::now() + 4s;
  while (std::chrono::steady_clock::now() < deadline &&
         connection->metrics().metadata_refresh_failures < 1) {
    (void)manager.run_once(5);
    for (;;) {
      auto record = ring.read(reader);
      if (!record) {
        break;
      }
      assert(utils::md::wire::Decode(record.value->payload, audit) ==
             utils::md::wire::CodecError::Ok);
      assert(record.value.commit());
    }
  }
  assert(connection->state() ==
         mds::service::MarketDataState::Live);
  assert(connection->metrics().metadata_refresh_failures == 1);
  assert(connection->metrics().metadata_refresh_successes == 0);
  assert(connection->metrics().metadata_symbol_quarantines == 1);
  assert(connection->metrics().resyncs == 0);
  assert(connection->metrics().reconnects == 0);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(server.metadata_requests() == 2);
  assert(audit.catalog_records == 3);
  assert(ring.unregister_reader(reader));
  manager.stop();
}

void test_binance_malformed_json_reconnect() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceMalformed);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.binance.parse.loopback." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;

  std::ostringstream diagnostics;
  auto *original = std::cerr.rdbuf(diagnostics.rdbuf());
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    if (connection->state() == mds::service::MarketDataState::Live &&
        connection->metrics().reconnects >= 1 &&
        server.btc_connections() >= 2) {
      break;
    }
  }
  std::cerr.rdbuf(original);

  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().parse_errors == 1);
  assert(connection->metrics().reconnects >= 1);
  const auto log = diagnostics.str();
  assert(log.find("binance websocket parse failed") != std::string::npos);
  assert(log.find("product=perpetual") != std::string::npos);
  assert(log.find("shard=0") != std::string::npos);
  assert(log.find("connection_generation=") != std::string::npos);
  assert(log.find("payload=\"{\\\"s\\\":\\\"BTCUSDT\\\",\"") !=
         std::string::npos);
  assert(log.find("frame callback rejected frame") == std::string::npos);
  manager.stop();
}

void test_binance_close_detail_reconnect() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceClose);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.binance.close.loopback." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;

  std::ostringstream diagnostics;
  auto *original = std::cerr.rdbuf(diagnostics.rdbuf());
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    if (connection->state() == mds::service::MarketDataState::Live &&
        connection->metrics().reconnects >= 1 &&
        server.btc_connections() >= 2) {
      break;
    }
  }
  std::cerr.rdbuf(original);

  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().reconnects >= 1);
  const auto log = diagnostics.str();
  assert(log.find("code=1001") != std::string::npos);
  assert(log.find("reason=rotate") != std::string::npos);
  manager.stop();
}

void test_non_hyperliquid_expired_close_is_failure() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceClose1000);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.ticker = true;
  stream.ticker_channel = "bookTicker";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.binance.close1000.loopback." + std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().reconnects == 1);
  assert(connection->metrics().server_expirations == 0);
  assert(server.btc_connections() >= 2);
  manager.stop();
}

void test_bitget_malformed_json_reconnect() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::Bitget);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Bitget;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v2/ws/public");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "books1";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.bitget.recovery.loopback." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;

  std::ostringstream diagnostics;
  auto *original = std::cerr.rdbuf(diagnostics.rdbuf());
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    if (connection->state() == mds::service::MarketDataState::Live &&
        connection->metrics().reconnects >= 1 &&
        server.btc_connections() >= 2 && server.eth_connections() == 1) {
      break;
    }
  }
  std::cerr.rdbuf(original);

  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().parse_errors == 1);
  assert(connection->metrics().reconnects >= 1);
  assert(server.btc_connections() >= 2);
  assert(server.eth_connections() == 1);
  const auto log = diagnostics.str();
  const std::string_view marker = "bitget websocket parse failed";
  assert(log.find(marker) != std::string::npos);
  assert(log.find("product=perpetual") != std::string::npos);
  assert(log.find("shard=0") != std::string::npos);
  assert(log.find("connection_generation=") != std::string::npos);
  assert(log.find("payload_bytes=8") != std::string::npos);
  assert(log.find("error=\"") != std::string::npos);
  assert(log.find("payload=\"{\\\"arg\\\":\\n\"") != std::string::npos);
  assert(log.find(marker, log.find(marker) + marker.size()) ==
         std::string::npos);
  manager.stop();
}

void test_bitget_rate_limit_resubscribes_without_false_live() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BitgetRateLimit);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Bitget;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v2/ws/public");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 3s;
  options.recovery_deadline = 5s;
  options.idle_timeout = 5s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.ticker = true;
  stream.ticker_channel = "books1";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.bitget.rate-limit.loopback." + std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;

  const auto cooldown_check = std::chrono::steady_clock::now() + 500ms;
  while (std::chrono::steady_clock::now() < cooldown_check) {
    (void)manager.run_once(5);
  }
  assert(connection->state() != mds::service::MarketDataState::Live);

  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().subscription_rejections == 1);
  assert(connection->metrics().subscription_rate_limit_deferrals == 1);
  assert(connection->metrics().subscription_symbol_quarantines == 0);
  assert(connection->metrics().reconnects == 0);
  assert(connection->metrics().one_sided_book_frames == 1);
  assert(connection->metrics().parse_errors == 0);
  assert(connection->metrics().ticker_updates == 1);
  assert(server.bitget_rate_limit_retries() == 1);
  manager.stop();
}

void test_bitget_rate_limit_without_arg_reconnects_safely() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BitgetRateLimitMissingArg);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Bitget;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v2/ws/public");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.ticker = true;
  stream.ticker_channel = "books1";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.bitget.rate-limit-missing.loopback." +
      std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         connection->metrics().reconnects == 0) {
    (void)manager.run_once(5);
  }
  assert(connection->metrics().reconnects >= 1);
  assert(connection->metrics().subscription_symbol_quarantines == 0);
  assert(connection->metrics().subscription_rate_limit_deferrals == 0);
  manager.stop();
}

void test_hyperliquid_server_expiration_reconnects_quickly() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::HyperliquidExpired);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Hyperliquid;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 5s;
  options.reconnect_max = 5s;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "COIN0USDC";
  stream.ticker = true;
  stream.ticker_channel = "bbo";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.hyperliquid.expired.loopback." + std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 4s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().reconnects == 1);
  assert(connection->metrics().server_expirations == 1);
  assert(server.hyperliquid_expired_connections() >= 2);
  assert(connection->metrics().ticker_updates == 1);
  manager.stop();
}

void test_hyperliquid_parse_failure_precedes_expired_close() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::HyperliquidMalformedThenExpired);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Hyperliquid;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "COIN0USDC";
  stream.ticker = true;
  stream.ticker_channel = "bbo";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.hyperliquid.expired-failure.loopback." +
      std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().parse_errors == 1);
  assert(connection->metrics().reconnects == 1);
  assert(connection->metrics().server_expirations == 0);
  assert(server.hyperliquid_expired_connections() >= 2);
  manager.stop();
}

void test_gate_dirty_quantity_resyncs_only_sol() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::GateDirtyQuantity);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Gate;
  options.product = utils::md::ProductType::Spot;
  options.websocket_endpoint = server.endpoint("wss", "/v4/ws/spot");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 3;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT", "SOLUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "spot.book_ticker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.gate.dirty.loopback." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  assert(connection->websocket_shard_count() == 1);

  mds::transport::RingOptions attach;
  attach.create = false;
  attach.name = mds::publish::make_multiplex_segment_name(
      "/mds.gate.dirty.loopback." + std::to_string(::getpid()),
      "gate", "spot", "ticker", 0);
  auto opened = mds::transport::SharedRing::open(attach);
  assert(opened);
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      mds::transport::process_start_marker(::getpid()),
      static_cast<std::uint64_t>(
          std::chrono::steady_clock::now().time_since_epoch().count()));
  assert(registered);
  auto reader = registered.value;
  Audit audit;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    for (;;) {
      auto record = ring.read(reader);
      if (!record) break;
      assert(utils::md::wire::Decode(record.value->payload, audit) ==
             utils::md::wire::CodecError::Ok);
      assert(record.value.commit());
    }
    if (connection->state() == mds::service::MarketDataState::Live &&
        connection->metrics().dirty_data_resyncs == 1 &&
        server.gate_sol_updates() == 2 &&
        server.gate_btc_updates() >= 5 &&
        server.gate_eth_updates() >= 5 &&
        audit.catalog_records >= 4) {
      break;
    }
  }

  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().parse_errors == 1);
  assert(connection->metrics().resyncs == 1);
  assert(connection->metrics().dirty_data_resyncs == 1);
  assert(connection->metrics().reconnects == 0);
  assert(connection->metrics().budget_reconnects == 0);
  assert(connection->metrics().ws_shards_live == 1);
  assert(std::string_view(connection->metrics().last_resync_symbol.data()) ==
         "SOLUSDT");
  assert(connection->metrics().last_resync_reason ==
         mds::service::ResyncReason::DirtyData);
  assert(server.gate_connections() == 1);
  assert(server.websocket_decimal_headers() == 0);
  assert(server.metadata_decimal_headers() == 0);
  assert(server.gate_snapshot_requests() == 0);
  assert(server.gate_btc_updates() >= 5);
  assert(server.gate_eth_updates() >= 5);
  assert(server.gate_sol_updates() == 2);
  assert(audit.catalogs.size() == 3);
  assert(audit.instruments.size() == 3);
  assert(audit.bbos.size() == 3);
  assert(audit.catalog_records >= 4);
  assert(audit.monotonic);
  assert(ring.unregister_reader(reader));
  manager.stop();
}

void test_gate_perpetual_decimal_headers_survive_reconnect() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::GateDecimalPerpetual);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Gate;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v4/ws/usdt");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.ticker = true;
  stream.ticker_channel = "futures.book_ticker";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.gate.decimal.loopback." + std::to_string(::getpid());
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));

  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().reconnects < 1 ||
          connection->metrics().ticker_updates < 2)) {
    (void)manager.run_once(5);
  }

  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().reconnects >= 1);
  assert(connection->metrics().ticker_updates >= 2);
  assert(server.gate_connections() >= 2);
  assert(server.websocket_requests() >= 2);
  assert(server.websocket_decimal_headers() ==
         server.websocket_requests());
  assert(server.metadata_requests() >= 1);
  assert(server.metadata_decimal_headers() ==
         server.metadata_requests());
  manager.stop();
}

void test_metadata_missing_symbol_is_quarantined() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinancePartialMetadata);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.binance.metadata.quarantine." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().metadata_symbol_quarantines >= 1);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(connection->metrics().ticker_updates > 0);
  manager.stop();
}

void test_bybit_symbol_metadata_unavailability(bool all_unavailable) {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(
      identity,
      all_unavailable ? Protocol::BybitAllSymbolsUnavailable
                      : Protocol::BybitSymbolUnavailable);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Bybit;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v5/public/linear");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 20ms;
  options.reconnect_max = 40ms;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "orderbook.1";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.bybit.metadata.quarantine." +
        std::to_string(::getpid()) +
        (all_unavailable ? ".all" : ".partial");
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }

  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline =
      std::chrono::steady_clock::now() +
      (all_unavailable ? 1500ms : 3s);
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    if (all_unavailable) {
      if (connection->metrics().connection_rebuild_attempts >= 1) {
        break;
      }
    } else if (
        connection->state() == mds::service::MarketDataState::Live &&
        connection->metrics().ticker_updates != 0) {
      break;
    }
  }

  if (all_unavailable) {
    assert(connection->state() !=
           mds::service::MarketDataState::Live);
    assert(connection->metrics().metadata_symbol_quarantines == 2);
    assert(connection->metrics().connection_rebuild_attempts >= 1);
    assert(connection->metrics().ticker_updates == 0);
  } else {
    assert(connection->state() ==
           mds::service::MarketDataState::Live);
    assert(connection->metrics().metadata_symbol_quarantines == 1);
    assert(connection->metrics().connection_rebuild_attempts == 0);
    assert(connection->metrics().ticker_updates != 0);
    assert(server.metadata_requests() == 1);
  }
  manager.stop();
}

void test_bybit_connection_scoped_metadata_failure(
    Protocol protocol) {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, protocol);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Bybit;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v5/public/linear");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 20ms;
  options.reconnect_max = 40ms;
  mds::service::SymbolStreamOptions stream;
  stream.symbol = "BTCUSDT";
  stream.ticker = true;
  stream.ticker_channel = "orderbook.1";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix =
      "/mds.bybit.metadata.failure." + std::to_string(::getpid()) +
      "." + std::to_string(static_cast<unsigned>(protocol));
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(std::move(stream));

  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() !=
              mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0 ||
          connection->metrics().connection_rebuild_successes == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().metadata_symbol_quarantines == 0);
  assert(connection->metrics().connection_rebuild_attempts >= 1);
  assert(connection->metrics().connection_rebuild_successes == 1);
  assert(connection->metrics().ticker_updates != 0);
  assert(server.metadata_requests() >= 2);
  manager.stop();
}

mds::service::SymbolStreamOptions bybit_bulk_stream(
    std::string_view symbol, std::string_view suffix) {
  mds::service::SymbolStreamOptions stream;
  stream.symbol = std::string(symbol);
  stream.ticker = true;
  stream.ticker_channel = "orderbook.1";
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  stream.shm_prefix = "/mds.bybit.bulk." + std::to_string(::getpid()) + "." +
                      std::string(suffix);
  stream.multiplex_ring.ring_bytes = 64U << 10U;
  stream.multiplex_ring.max_record_bytes = 4096;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  return stream;
}

mds::service::VenueConnectionOptions bybit_bulk_options(
    Server &server, std::initializer_list<const char *> symbols,
    std::string_view suffix) {
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Bybit;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/v5/public/linear");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 20ms;
  options.reconnect_max = 40ms;
  for (const auto *symbol : symbols) {
    options.streams.push_back(bybit_bulk_stream(symbol, suffix));
  }
  return options;
}

void test_bybit_bulk_single_page_includes_tusdt() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BybitBulkSinglePage);
  mds::service::VenueConnectionManager manager(tls);
  auto created = manager.create(bybit_bulk_options(
      server, {"BTCUSDT", "ETHUSDT", "TUSDT"}, "single"));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(server.metadata_requests() == 1);
  assert(server.subscribe_metadata_requests() == 1);
  const auto targets = server.metadata_targets();
  assert(targets.size() == 1);
  assert(targets[0].find("status=Trading") != std::string::npos);
  assert(targets[0].find("limit=1000") != std::string::npos);
  assert(targets[0].find("symbol=") == std::string::npos);
  assert(connection->websocket_shard("TUSDT").has_value());
  manager.stop();
}

void test_bybit_bulk_two_page_waits_for_second_symbol() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BybitBulkTwoPage);
  mds::service::VenueConnectionManager manager(tls);
  auto created = manager.create(bybit_bulk_options(
      server, {"BTCUSDT", "ETHUSDT", "TUSDT"}, "twopage"));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().metadata_symbol_quarantines == 0);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(server.metadata_requests() == 2);
  assert(server.subscribe_metadata_requests() == 2);
  assert(connection->websocket_shard("TUSDT").has_value());
  manager.stop();
}

void test_bybit_bulk_pagination_failure(Protocol protocol,
                                        unsigned expected_requests) {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, protocol);
  mds::service::VenueConnectionManager manager(tls);
  auto created = manager.create(bybit_bulk_options(
      server, {"BTCUSDT", "ETHUSDT"},
      "fail" + std::to_string(static_cast<unsigned>(protocol))));
  assert(created);
  auto *connection = created.value;
  const auto deadline =
      std::chrono::steady_clock::now() +
      (protocol == Protocol::BybitBulkPageLimit ? 20s : 4s);
  while (std::chrono::steady_clock::now() < deadline &&
         connection->metrics().connection_rebuild_attempts == 0) {
    (void)manager.run_once(5);
  }
  assert(connection->state() != mds::service::MarketDataState::Live ||
         connection->metrics().connection_rebuild_attempts >= 1);
  assert(connection->metrics().connection_rebuild_attempts >= 1);
  assert(server.metadata_requests() >= expected_requests);
  manager.stop();
}

void test_bybit_bulk_missing_symbol_is_quarantined() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BybitBulkMissingSymbol);
  mds::service::VenueConnectionManager manager(tls);
  auto created = manager.create(bybit_bulk_options(
      server, {"BTCUSDT", "ETHUSDT", "TUSDT"}, "missing"));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().metadata_symbol_quarantines == 1);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(server.metadata_requests() == 1);
  assert(connection->websocket_shard("BTCUSDT").has_value());
  manager.stop();
}

void test_bybit_multi_symbol_scale_refresh_stays_exact() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BybitScaleRefreshBoth);
  mds::service::VenueConnectionManager manager(tls);
  auto created = manager.create(
      bybit_bulk_options(server, {"BTCUSDT", "ETHUSDT"}, "refresh"));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 6s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->metrics().metadata_refresh_successes < 2 ||
          connection->state() != mds::service::MarketDataState::Live)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().metadata_refresh_successes >= 2);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  const auto targets = server.metadata_targets();
  assert(!targets.empty());
  assert(targets.front().find("symbol=") == std::string::npos);
  unsigned exact = 0;
  for (std::size_t index = 1; index < targets.size(); ++index) {
    if (targets[index].find("symbol=") != std::string::npos) {
      ++exact;
    }
  }
  assert(exact >= 2);
  manager.stop();
}

void test_bybit_candidate_like_replace_adds_tusdt() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BybitBulkSinglePage);
  mds::service::VenueConnectionManager manager(tls);
  auto created = manager.create(
      bybit_bulk_options(server, {"BTCUSDT", "ETHUSDT"}, "replace-old"));
  assert(created);
  auto *connection = created.value;
  const auto live_deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < live_deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(server.metadata_requests() == 1);
  auto replaced = manager.replace(
      connection,
      bybit_bulk_options(server, {"BTCUSDT", "ETHUSDT", "TUSDT"},
                         "replace-new"));
  assert(replaced);
  connection = replaced.value;
  const auto replace_deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < replace_deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().connection_rebuild_attempts == 0);
  assert(server.metadata_requests() == 2);
  assert(connection->websocket_shard("TUSDT").has_value());
  manager.stop();
}

void test_subscription_reject_isolates_one_symbol() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::BinanceRejectBtc);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  for (const auto symbol : {"BTCUSDT", "ETHUSDT"}) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = symbol;
    stream.ticker = true;
    stream.ticker_channel = "bookTicker";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.binance.reject.quarantine." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }
  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 4s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() != mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates == 0)) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().subscription_rejections >= 1);
  assert(connection->metrics().subscription_symbol_quarantines >= 1);
  assert(connection->metrics().reconnects == 0);
  assert(connection->metrics().ticker_updates > 0);
  manager.stop();
}

void test_hyperliquid_subscription_window() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::HyperliquidWindow);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Hyperliquid;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 100;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  for (unsigned index = 0; index < 17; ++index) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = "COIN" + std::to_string(index) +
                    (index == 0 ? "USDT" : "USDC");
    stream.ticker = true;
    stream.ticker_channel = "bbo";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.hyperliquid.window." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }

  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() !=
              mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates < 17)) {
    (void)manager.run_once(5);
  }
  assert(server.hyperliquid_subscriptions_before_ack() == 16);
  assert(server.hyperliquid_subscriptions() == 17);
  assert(connection->metrics().subscription_requests == 17);
  assert(connection->metrics().budget_deferrals == 0);
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().ticker_updates >= 17);
  manager.stop();
}

void test_hyperliquid_window_rejection_isolates_symbol() {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, Protocol::HyperliquidRejectFirst);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Hyperliquid;
  options.product = utils::md::ProductType::Perpetual;
  options.websocket_endpoint = server.endpoint("wss", "/ws");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 100;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 5s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  for (unsigned index = 0; index < 3; ++index) {
    mds::service::SymbolStreamOptions stream;
    stream.symbol = "COIN" + std::to_string(index) + "USDC";
    stream.ticker = true;
    stream.ticker_channel = "bbo";
    stream.ring_layout = mds::publish::RingLayout::Multiplex;
    stream.shard_count = 1;
    stream.shm_prefix =
        "/mds.hyperliquid.reject." + std::to_string(::getpid());
    stream.multiplex_ring.ring_bytes = 64U << 10U;
    stream.multiplex_ring.max_record_bytes = 4096;
    stream.multiplex_ring.max_readers = 4;
    stream.multiplex_ring.unlink_on_close = true;
    options.streams.push_back(std::move(stream));
  }

  auto created = manager.create(std::move(options));
  assert(created);
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         (connection->state() !=
              mds::service::MarketDataState::Live ||
          connection->metrics().ticker_updates < 2)) {
    (void)manager.run_once(5);
  }
  assert(server.hyperliquid_reject_connections() >= 2);
  assert(connection->metrics().subscription_rejections >= 1);
  assert(connection->metrics().subscription_symbol_quarantines == 1);
  assert(connection->metrics().reconnects >= 1);
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().ticker_updates >= 2);
  manager.stop();
}

void test_aster_lighter_full_pipeline(
    Protocol protocol, utils::md::Venue venue,
    utils::md::ProductType product) {
  Certificate identity;
  auto tls = client_context(identity);
  Server server(identity, protocol);
  mds::service::VenueConnectionManager manager(tls);
  mds::service::VenueConnectionOptions options;
  options.venue = venue;
  options.product = product;
  options.websocket_endpoint = server.endpoint("wss", "/stream");
  options.rest_endpoint = server.endpoint("https");
  options.max_symbols_per_ws = 1;
  options.connect_timeout = 1s;
  options.request_timeout = 2s;
  options.idle_timeout = 5s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  if (venue == utils::md::Venue::Lighter) {
    options.client_message_limit_per_minute = 150;
  }

  mds::service::SymbolStreamOptions stream;
  stream.symbol =
      venue == utils::md::Venue::Aster ? "BTCUSDT" : "BTCUSDC";
  stream.ticker = true;
  stream.orderbook = true;
  stream.ticker_channel =
      venue == utils::md::Venue::Aster ? "bookTicker" : "ticker";
  stream.ticker_requires_first_data =
      venue == utils::md::Venue::Lighter;
  stream.orderbook_channel =
      venue == utils::md::Venue::Aster ? "depth" : "order_book";
  stream.orderbook_bootstrap =
      venue == utils::md::Venue::Aster
          ? mds::exchange::BookBootstrap::RestSnapshotThenDelta
          : mds::exchange::BookBootstrap::WsSnapshotThenDelta;
  stream.snapshot_depth =
      product == utils::md::ProductType::Spot ? 5000 : 1000;
  stream.max_levels_per_message = 5000;
  stream.update_interval_ms =
      venue == utils::md::Venue::Aster ? 100 : 50;
  stream.ladder_ticks_per_side = 256;
  stream.ladder_price_band_bps = 100;
  stream.ring_layout = mds::publish::RingLayout::Multiplex;
  stream.shard_count = 1;
  const auto suffix =
      std::string(mds::exchange::venue_name(venue)) + "." +
      std::string(mds::exchange::product_name(product));
  stream.shm_prefix =
      "/mds.aster-lighter.loopback." + std::to_string(::getpid()) + "." +
      suffix;
  stream.multiplex_ring.ring_bytes = 8U << 20U;
  stream.multiplex_ring.max_record_bytes = 1U << 20U;
  stream.multiplex_ring.max_readers = 4;
  stream.multiplex_ring.unlink_on_close = true;
  options.streams.push_back(stream);

  auto created = manager.create(std::move(options));
  if (!created) {
    std::cerr << "Aster/Lighter loopback create failed venue="
              << mds::exchange::venue_name(venue)
              << " product=" << mds::exchange::product_name(product)
              << " error=\"" << created.message << "\"\n";
  }
  assert(created);
  auto *connection = created.value;
  const auto open_ring = [&](std::string_view kind) {
    mds::transport::RingOptions attach;
    attach.create = false;
    attach.name = mds::publish::make_multiplex_segment_name(
        stream.shm_prefix, mds::exchange::venue_name(venue),
        mds::exchange::product_name(product), kind, 0);
    auto opened = mds::transport::SharedRing::open(attach);
    assert(opened);
    return std::move(opened.value);
  };
  auto ticker_ring = open_ring("ticker");
  auto book_ring = open_ring("orderbook");
  const auto marker = mds::transport::process_start_marker(::getpid());
  const auto registered_at = static_cast<std::uint64_t>(
      std::chrono::steady_clock::now().time_since_epoch().count());
  auto ticker_registered =
      ticker_ring.register_reader(marker, registered_at);
  auto book_registered = book_ring.register_reader(marker, registered_at);
  assert(ticker_registered && book_registered);
  auto ticker_reader = ticker_registered.value;
  auto book_reader = book_registered.value;
  Audit ticker_audit;
  Audit book_audit;
  const auto drain = [](mds::transport::SharedRing &ring,
                        mds::transport::ReaderHandle &reader,
                        Audit &audit) {
    for (;;) {
      auto record = ring.read(reader);
      if (!record) {
        return;
      }
      assert(utils::md::wire::Decode(record.value->payload, audit) ==
             utils::md::wire::CodecError::Ok);
      assert(record.value.commit());
    }
  };

  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    drain(ticker_ring, ticker_reader, ticker_audit);
    drain(book_ring, book_reader, book_audit);
    if (connection->state() == mds::service::MarketDataState::Live &&
        ticker_audit.bbo_records != 0 &&
        book_audit.snapshot_begin_records != 0 &&
        book_audit.delta_records != 0) {
      break;
    }
  }
  assert(connection->state() == mds::service::MarketDataState::Live);
  assert(connection->metrics().ticker_updates != 0);
  assert(connection->metrics().depth_updates != 0);
  assert(ticker_audit.instruments.size() == 1);
  assert(book_audit.instruments.size() == 1);
  assert(book_audit.snapshot_begin_records != 0);
  assert(book_audit.delta_records != 0);
  assert(ticker_audit.last_bid_price == book_audit.last_bid_price);
  assert(ticker_audit.last_ask_price == book_audit.last_ask_price);
  manager.stop();
}

void test_lighter_budget_is_shared_across_products() {
  Certificate identity;
  auto tls = client_context(identity);
  Server spot_server(identity, Protocol::LighterSpot);
  Server perpetual_server(identity, Protocol::LighterPerpetual);
  mds::service::VenueConnectionManager manager(tls);
  const auto make_options =
      [&](utils::md::ProductType product, const Server &server) {
        mds::service::VenueConnectionOptions options;
        options.venue = utils::md::Venue::Lighter;
        options.product = product;
        options.websocket_endpoint = server.endpoint("wss", "/stream");
        options.rest_endpoint = server.endpoint("https");
        options.max_symbols_per_ws = 1;
        options.client_message_limit_per_minute = 1;
        options.connect_timeout = 1s;
        options.request_timeout = 2s;
        options.idle_timeout = 5s;
        mds::service::SymbolStreamOptions stream;
        stream.symbol = "BTCUSDC";
        stream.ticker = true;
        stream.ticker_requires_first_data = true;
        stream.ticker_channel = "ticker";
        stream.ring_layout = mds::publish::RingLayout::Multiplex;
        stream.shard_count = 1;
        stream.shm_prefix =
            "/mds.lighter.shared-budget." + std::to_string(::getpid()) +
            (product == utils::md::ProductType::Spot ? ".spot" : ".perp");
        stream.multiplex_ring.ring_bytes = 64U << 10U;
        stream.multiplex_ring.max_record_bytes = 4096;
        stream.multiplex_ring.max_readers = 4;
        stream.multiplex_ring.unlink_on_close = true;
        options.streams.push_back(std::move(stream));
        return options;
      };
  auto spot = manager.create(
      make_options(utils::md::ProductType::Spot, spot_server));
  auto perpetual = manager.create(
      make_options(utils::md::ProductType::Perpetual, perpetual_server));
  assert(spot && perpetual);

  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    const auto requests =
        spot.value->metrics().subscription_requests +
        perpetual.value->metrics().subscription_requests;
    const auto deferrals =
        spot.value->metrics().global_budget_deferrals +
        perpetual.value->metrics().global_budget_deferrals;
    if (requests == 1 && deferrals != 0) {
      break;
    }
  }
  assert(spot.value->metrics().subscription_requests +
             perpetual.value->metrics().subscription_requests ==
         1);
  assert(spot.value->metrics().global_budget_deferrals +
             perpetual.value->metrics().global_budget_deferrals !=
         0);
  manager.stop();
}

int main() {
  std::signal(SIGPIPE, SIG_IGN);
  test_aster_lighter_full_pipeline(
      Protocol::AsterSpot, utils::md::Venue::Aster,
      utils::md::ProductType::Spot);
  test_aster_lighter_full_pipeline(
      Protocol::AsterPerpetual, utils::md::Venue::Aster,
      utils::md::ProductType::Perpetual);
  test_aster_lighter_full_pipeline(
      Protocol::LighterSpot, utils::md::Venue::Lighter,
      utils::md::ProductType::Spot);
  test_aster_lighter_full_pipeline(
      Protocol::LighterPerpetual, utils::md::Venue::Lighter,
      utils::md::ProductType::Perpetual);
  test_lighter_budget_is_shared_across_products();
  if (std::getenv("MDS_ASTER_LIGHTER_LOOPBACK_ONLY") != nullptr) {
    return 0;
  }
  test_multiplex_publishers_survive_compatible_replace();
  test_binance_shard_reconnect();
  test_binance_ticker_only_ack_is_live();
  test_continuous_recovery_timeout_isolates_connection();
  test_metadata_failure_rebuilds_connection();
  test_symbol_scale_refresh(Protocol::BinanceScaleRefresh, false);
  test_symbol_scale_refresh(Protocol::BinanceScaleRefresh503, true);
  test_invalid_symbol_scale_refresh_does_not_publish_catalog();
  test_metadata_missing_symbol_is_quarantined();
  test_bybit_symbol_metadata_unavailability(false);
  test_bybit_symbol_metadata_unavailability(true);
  test_bybit_connection_scoped_metadata_failure(
      Protocol::BybitMetadata429Once);
  test_bybit_connection_scoped_metadata_failure(
      Protocol::BybitMetadataMalformedOnce);
  test_bybit_connection_scoped_metadata_failure(
      Protocol::BybitMetadataParameterOnce);
  test_bybit_bulk_single_page_includes_tusdt();
  test_bybit_bulk_two_page_waits_for_second_symbol();
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkCursorRepeat, 2);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkEmptyPageCursor, 1);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkPageLimit, 64);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkPageTwoTimeout, 2);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkPageTwoRetCode, 2);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkPageTwoMalformed, 2);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkIntraPageDuplicate, 1);
  test_bybit_bulk_pagination_failure(Protocol::BybitBulkCrossPageDuplicate, 2);
  test_bybit_bulk_missing_symbol_is_quarantined();
  test_bybit_multi_symbol_scale_refresh_stays_exact();
  test_bybit_candidate_like_replace_adds_tusdt();
  test_subscription_reject_isolates_one_symbol();
  test_hyperliquid_subscription_window();
  test_hyperliquid_window_rejection_isolates_symbol();
  test_binance_malformed_json_reconnect();
  test_binance_close_detail_reconnect();
  test_non_hyperliquid_expired_close_is_failure();
  test_bitget_malformed_json_reconnect();
  test_bitget_rate_limit_resubscribes_without_false_live();
  test_bitget_rate_limit_without_arg_reconnects_safely();
  test_hyperliquid_server_expiration_reconnects_quickly();
  test_hyperliquid_parse_failure_precedes_expired_close();
  test_gate_perpetual_decimal_headers_survive_reconnect();
  test_gate_dirty_quantity_resyncs_only_sol();
  return 0;
}
