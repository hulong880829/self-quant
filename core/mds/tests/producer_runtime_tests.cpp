#include "mds/producer/producer_runtime.h"
#include "net/websocket_codec.h"

#include <arpa/inet.h>
#include <array>
#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <dirent.h>
#include <cstddef>
#include <cstdint>
#include <fstream>
#include <functional>
#include <netinet/in.h>
#include <openssl/evp.h>
#include <openssl/pem.h>
#include <openssl/ssl.h>
#include <openssl/x509v3.h>
#include <span>
#include <sstream>
#include <string>
#include <string_view>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace {

void test_next_daily_discovery_utc() {
  using namespace std::chrono;
  const auto day = sys_days{year{2026} / August / 30};
  assert(mds::producer::next_daily_discovery_utc(day + minutes(4)) ==
         day + minutes(5));
  assert(mds::producer::next_daily_discovery_utc(
             day + minutes(5)) ==
         day + days(1) + minutes(5));
  assert(mds::producer::next_daily_discovery_utc(
             day + hours(23) + minutes(59)) ==
         day + days(1) + minutes(5));
}

struct TemporaryFile {
  std::string path;

  explicit TemporaryFile(std::string_view prefix) {
    std::string pattern = "/tmp/" + std::string(prefix) + ".XXXXXX";
    pattern.push_back('\0');
    const int descriptor = ::mkstemp(pattern.data());
    assert(descriptor >= 0);
    ::close(descriptor);
    path.assign(pattern.data());
  }

  ~TemporaryFile() { std::remove(path.c_str()); }
};

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

  void write_trust_file(const std::string &path) const {
    auto *file = std::fopen(path.c_str(), "w");
    assert(file != nullptr);
    assert(PEM_write_X509(file, certificate) == 1);
    std::fclose(file);
  }
};

bool write_all(SSL *ssl, std::string_view text) {
  std::size_t offset{};
  while (offset < text.size()) {
    const int written = SSL_write(
        ssl, text.data() + offset,
        static_cast<int>(text.size() - offset));
    if (written <= 0) {
      return false;
    }
    offset += static_cast<std::size_t>(written);
  }
  return true;
}

std::string read_headers(SSL *ssl) {
  std::string result;
  char byte{};
  while (result.find("\r\n\r\n") == std::string::npos &&
         result.size() < 16U * 1024U) {
    if (SSL_read(ssl, &byte, 1) != 1) {
      break;
    }
    result.push_back(byte);
  }
  return result;
}

class GateDiscoveryServer {
 public:
  explicit GateDiscoveryServer(const Certificate &identity) {
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
    assert(::listen(listener_, 4) == 0);
    socklen_t size = sizeof(address);
    assert(::getsockname(listener_, reinterpret_cast<sockaddr *>(&address),
                         &size) == 0);
    port_ = ntohs(address.sin_port);
    worker_ = std::thread([this] { serve(); });
  }

  ~GateDiscoveryServer() {
    stopping_.store(true);
    ::shutdown(listener_, SHUT_RDWR);
    ::close(listener_);
    if (worker_.joinable()) {
      worker_.join();
    }
    SSL_CTX_free(context_);
  }

  std::string endpoint() const {
    return "https://127.0.0.1:" + std::to_string(port_);
  }

  unsigned requests() const { return requests_.load(); }
  unsigned decimal_headers() const { return decimal_headers_.load(); }
  bool saw_contracts() const { return saw_contracts_.load(); }
  bool saw_tickers() const { return saw_tickers_.load(); }

 private:
  void serve() {
    while (!stopping_.load() && requests_.load() < 2) {
      const int descriptor =
          ::accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (descriptor < 0) {
        continue;
      }
      auto *ssl = SSL_new(context_);
      SSL_set_fd(ssl, descriptor);
      if (SSL_accept(ssl) == 1) {
        const auto request = read_headers(ssl);
        ++requests_;
        if (request.find("\r\nX-Gate-Size-Decimal: 1\r\n") !=
            std::string::npos) {
          ++decimal_headers_;
        }
        const bool tickers =
            request.find("/api/v4/futures/usdt/tickers") !=
            std::string::npos;
        const bool contracts =
            request.find("/api/v4/futures/usdt/contracts") !=
            std::string::npos;
        saw_tickers_.store(saw_tickers_.load() || tickers);
        saw_contracts_.store(saw_contracts_.load() || contracts);
        const std::string_view body =
            tickers
                ? R"([{"contract":"BTC_USDT","volume_24h_quote":"1000"}])"
                : R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true}])";
        const auto response =
            "HTTP/1.1 200 OK\r\nContent-Length: " +
            std::to_string(body.size()) +
            "\r\nConnection: close\r\n\r\n" + std::string(body);
        (void)write_all(ssl, response);
      }
      SSL_shutdown(ssl);
      SSL_free(ssl);
      ::close(descriptor);
    }
  }

  SSL_CTX *context_{};
  int listener_{-1};
  std::uint16_t port_{};
  std::atomic<bool> stopping_{};
  std::atomic<unsigned> requests_{};
  std::atomic<unsigned> decimal_headers_{};
  std::atomic<bool> saw_contracts_{};
  std::atomic<bool> saw_tickers_{};
  std::thread worker_;
};

class HyperliquidDiscoveryServer {
 public:
  explicit HyperliquidDiscoveryServer(const Certificate &identity) {
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
    assert(::listen(listener_, 4) == 0);
    socklen_t size = sizeof(address);
    assert(::getsockname(listener_, reinterpret_cast<sockaddr *>(&address),
                         &size) == 0);
    port_ = ntohs(address.sin_port);
    worker_ = std::thread([this] { serve(); });
  }

  ~HyperliquidDiscoveryServer() {
    stopping_.store(true);
    ::shutdown(listener_, SHUT_RDWR);
    ::close(listener_);
    if (worker_.joinable()) {
      worker_.join();
    }
    SSL_CTX_free(context_);
  }

  std::string endpoint() const {
    return "https://127.0.0.1:" + std::to_string(port_);
  }

  unsigned requests() const { return requests_.load(); }
  bool saw_default() const { return saw_default_.load(); }
  bool saw_xyz() const { return saw_xyz_.load(); }

 private:
  static std::string read_body(SSL *ssl, std::string_view headers) {
    constexpr std::string_view marker{"Content-Length:"};
    const auto marker_offset = headers.find(marker);
    if (marker_offset == std::string_view::npos) {
      return {};
    }
    const auto value_begin =
        headers.find_first_not_of(' ', marker_offset + marker.size());
    const auto value_end = headers.find("\r\n", value_begin);
    const auto length = static_cast<std::size_t>(
        std::stoul(std::string(headers.substr(
            value_begin, value_end - value_begin))));
    std::string body(length, '\0');
    std::size_t received = 0;
    while (received < body.size()) {
      const int count = SSL_read(
          ssl, body.data() + received,
          static_cast<int>(body.size() - received));
      if (count <= 0) {
        return {};
      }
      received += static_cast<std::size_t>(count);
    }
    return body;
  }

  void serve() {
    while (!stopping_.load() && requests_.load() < 2) {
      const int descriptor =
          ::accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (descriptor < 0) {
        continue;
      }
      auto *ssl = SSL_new(context_);
      SSL_set_fd(ssl, descriptor);
      if (SSL_accept(ssl) == 1) {
        const auto headers = read_headers(ssl);
        const auto body = read_body(ssl, headers);
        ++requests_;
        const bool xyz =
            body == R"({"type":"metaAndAssetCtxs","dex":"xyz"})";
        const bool default_dex =
            body == R"({"type":"metaAndAssetCtxs"})";
        saw_xyz_.store(saw_xyz_.load() || xyz);
        saw_default_.store(saw_default_.load() || default_dex);
        if (xyz) {
          constexpr std::string_view response_body{"{}"};
          const auto response =
              "HTTP/1.1 503 Service Unavailable\r\nContent-Length: " +
              std::to_string(response_body.size()) +
              "\r\nConnection: close\r\n\r\n" +
              std::string(response_body);
          (void)write_all(ssl, response);
        } else {
          constexpr std::string_view response_body =
              R"([{"universe":[{"name":"BTC","szDecimals":5}]},[{"dayNtlVlm":"1234567.89"}]])";
          const auto response =
              "HTTP/1.1 200 OK\r\nContent-Length: " +
              std::to_string(response_body.size()) +
              "\r\nConnection: close\r\n\r\n" +
              std::string(response_body);
          (void)write_all(ssl, response);
        }
      }
      SSL_shutdown(ssl);
      SSL_free(ssl);
      ::close(descriptor);
    }
  }

  SSL_CTX *context_{};
  int listener_{-1};
  std::uint16_t port_{};
  std::atomic<bool> stopping_{};
  std::atomic<unsigned> requests_{};
  std::atomic<bool> saw_default_{};
  std::atomic<bool> saw_xyz_{};
  std::thread worker_;
};

void test_gate_discovery_decimal_headers() {
  Certificate identity;
  TemporaryFile trust("mds-gate-trust");
  identity.write_trust_file(trust.path);
  assert(::setenv("SSL_CERT_FILE", trust.path.c_str(), 1) == 0);
  GateDiscoveryServer server(identity);
  TemporaryFile config("mds-gate-discovery");
  {
    std::ofstream output(config.path);
    assert(output);
    output
        << "shared_memory:\n"
        << "  prefix: /mds.gate.discovery.test\n"
        << "  backend: POSIX_SHM\n"
        << "  mode: OVERWRITE_OLDEST\n"
        << "  ring_bytes: 65536\n"
        << "  max_record_bytes: 4096\n"
        << "  max_readers: 2\n"
        << "  reader_lease_timeout_ns: 5000000000\n"
        << "  unlink_on_shutdown: true\n"
        << "  ring_layout: multiplex\n"
        << "  shard_count: 1\n"
        << "  multiplex_ring_bytes: 65536\n"
        << "capacity:\n"
        << "  max_instruments: 64\n"
        << "  max_rings: 16\n"
        << "  max_total_ring_bytes: 16777216\n"
        << "venues:\n"
        << "  - venue: gate\n"
        << "    product: PERPETUAL\n"
        << "    websocket_endpoint: wss://127.0.0.1/unused\n"
        << "    rest_endpoint: " << server.endpoint() << "\n"
        << "    protocol: JSON\n"
        << "subscriptions:\n"
        << "  - venue: gate\n"
        << "    product: PERPETUAL\n"
        << "    stream: ticker\n"
        << "    discovery:\n"
        << "      quote_assets: [USDT]\n"
        << "      minimum_turnover: 1\n";
  }
  mds::producer::ProducerRuntime runtime({
      .config_path = config.path,
      .discover_only = true,
  });
  const auto started = runtime.start();
  if (!started) {
    std::fprintf(stderr, "Gate discovery runtime failed: %.*s\n",
                 static_cast<int>(runtime.error().size()),
                 runtime.error().data());
  }
  assert(started);
  assert(!runtime.failed());
  assert(server.requests() == 2);
  assert(server.decimal_headers() == 2);
  assert(server.saw_contracts());
  assert(server.saw_tickers());
  runtime.stop();
  ::unsetenv("SSL_CERT_FILE");
}

void test_hyperliquid_xyz_discovery_is_best_effort() {
  Certificate identity;
  TemporaryFile trust("mds-hyperliquid-trust");
  identity.write_trust_file(trust.path);
  assert(::setenv("SSL_CERT_FILE", trust.path.c_str(), 1) == 0);
  HyperliquidDiscoveryServer server(identity);
  TemporaryFile config("mds-hyperliquid-discovery");
  {
    std::ofstream output(config.path);
    assert(output);
    output
        << "shared_memory:\n"
        << "  prefix: /mds.hyperliquid.discovery.test\n"
        << "  backend: POSIX_SHM\n"
        << "  mode: OVERWRITE_OLDEST\n"
        << "  ring_bytes: 65536\n"
        << "  max_record_bytes: 4096\n"
        << "  max_readers: 2\n"
        << "  reader_lease_timeout_ns: 5000000000\n"
        << "  unlink_on_shutdown: true\n"
        << "  ring_layout: multiplex\n"
        << "  shard_count: 1\n"
        << "  multiplex_ring_bytes: 65536\n"
        << "capacity:\n"
        << "  max_instruments: 64\n"
        << "  max_rings: 16\n"
        << "  max_total_ring_bytes: 16777216\n"
        << "venues:\n"
        << "  - venue: hyperliquid\n"
        << "    product: PERPETUAL\n"
        << "    websocket_endpoint: wss://127.0.0.1/unused\n"
        << "    rest_endpoint: " << server.endpoint() << "\n"
        << "    protocol: JSON\n"
        << "    max_symbols_per_connection: 64\n"
        << "    max_symbols_per_ws: 32\n"
        << "subscriptions:\n"
        << "  - venue: hyperliquid\n"
        << "    product: PERPETUAL\n"
        << "    stream: ticker\n"
        << "    discovery:\n"
        << "      quote_assets: [USDC]\n"
        << "      minimum_turnover: 1\n";
  }
  mds::producer::ProducerRuntime runtime({
      .config_path = config.path,
      .discover_only = true,
  });
  const auto started = runtime.start();
  if (!started) {
    std::fprintf(stderr,
                 "Hyperliquid discovery runtime failed: %.*s\n",
                 static_cast<int>(runtime.error().size()),
                 runtime.error().data());
  }
  assert(started);
  assert(!runtime.failed());
  assert(server.requests() == 2);
  assert(server.saw_default());
  assert(server.saw_xyz());
  runtime.stop();
  ::unsetenv("SSL_CERT_FILE");
}

int count_open_fds() {
  DIR *dir = ::opendir("/proc/self/fd");
  if (dir == nullptr) {
    return -1;
  }
  int count = 0;
  while (::readdir(dir) != nullptr) {
    ++count;
  }
  ::closedir(dir);
  return count;
}

int count_occurrences(const std::string &text, std::string_view needle) {
  int count = 0;
  for (std::size_t pos = 0; (pos = text.find(needle, pos)) != std::string::npos;
       pos += needle.size()) {
    ++count;
  }
  return count;
}

bool read_exact(SSL *ssl, std::span<std::byte> bytes) {
  std::size_t offset{};
  while (offset < bytes.size()) {
    const int received = SSL_read(ssl, bytes.data() + offset,
                                  static_cast<int>(bytes.size() - offset));
    if (received <= 0) {
      return false;
    }
    offset += static_cast<std::size_t>(received);
  }
  return true;
}

bool send_text(SSL *ssl, std::string_view payload) {
  std::string frame;
  frame.push_back(static_cast<char>(0x81));
  if (payload.size() < 126) {
    frame.push_back(static_cast<char>(payload.size()));
  } else {
    frame.push_back(static_cast<char>(126));
    frame.push_back(static_cast<char>((payload.size() >> 8U) & 0xffU));
    frame.push_back(static_cast<char>(payload.size() & 0xffU));
  }
  frame.append(payload);
  return write_all(ssl, frame);
}

class DiscoveryLoopback {
 public:
  explicit DiscoveryLoopback(const Certificate &identity) {
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
    assert(::listen(listener_, 128) == 0);
    socklen_t size = sizeof(address);
    assert(::getsockname(listener_, reinterpret_cast<sockaddr *>(&address),
                         &size) == 0);
    port_ = ntohs(address.sin_port);
    worker_ = std::thread([this] { accept_loop(); });
  }

  ~DiscoveryLoopback() {
    stopping_.store(true);
    ::shutdown(listener_, SHUT_RDWR);
    ::close(listener_);
    if (worker_.joinable()) {
      worker_.join();
    }
    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::seconds(2);
    while (inflight_.load() > 0 &&
           std::chrono::steady_clock::now() < deadline) {
      std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
    SSL_CTX_free(context_);
  }

  std::uint16_t port() const { return port_; }
  std::string https() const {
    return "https://127.0.0.1:" + std::to_string(port_);
  }
  std::string wss() const {
    return "wss://127.0.0.1:" + std::to_string(port_) + "/ws";
  }
  void set_include_eth(bool value) { include_eth_.store(value); }
  void fail_gate(bool value) { fail_gate_.store(value); }
  void stall_next_ws(int count) { stall_ws_.store(count); }

 private:
  std::string binance_spot() const {
    return include_eth_.load()
               ? R"({"symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","baseAssetPrecision":8,"quotePrecision":8,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000"}]},{"symbol":"ETHUSDT","status":"TRADING","baseAsset":"ETH","quoteAsset":"USDT","baseAssetPrecision":8,"quotePrecision":8,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000"}]}]})"
               : R"({"symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","baseAssetPrecision":8,"quotePrecision":8,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000"}]}]})";
  }

  std::string binance_perp() const {
    return include_eth_.load()
               ? R"({"timezone":"UTC","symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]},{"symbol":"ETHUSDT","status":"TRADING","baseAsset":"ETH","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})"
               : R"({"timezone":"UTC","symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
  }

  std::string ticker_24h() const {
    return include_eth_.load()
               ? R"([{"symbol":"BTCUSDT","quoteVolume":"1000"},{"symbol":"ETHUSDT","quoteVolume":"1000"}])"
               : R"([{"symbol":"BTCUSDT","quoteVolume":"1000"}])";
  }

  std::string gate_spot() const {
    return include_eth_.load()
               ? R"([{"id":"BTC_USDT","base":"BTC","quote":"USDT","precision":1,"amount_precision":3},{"id":"ETH_USDT","base":"ETH","quote":"USDT","precision":1,"amount_precision":3}])"
               : R"([{"id":"BTC_USDT","base":"BTC","quote":"USDT","precision":1,"amount_precision":3}])";
  }

  std::string gate_perp() const {
    return include_eth_.load()
               ? R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true},{"name":"ETH_USDT","order_price_round":"0.1","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true}])"
               : R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true}])";
  }

  std::string gate_spot_tickers() const {
    return include_eth_.load()
               ? R"([{"currency_pair":"BTC_USDT","quote_volume":"1000"},{"currency_pair":"ETH_USDT","quote_volume":"1000"}])"
               : R"([{"currency_pair":"BTC_USDT","quote_volume":"1000"}])";
  }

  std::string gate_perp_tickers() const {
    return include_eth_.load()
               ? R"([{"contract":"BTC_USDT","volume_24h_quote":"1000"},{"contract":"ETH_USDT","volume_24h_quote":"1000"}])"
               : R"([{"contract":"BTC_USDT","volume_24h_quote":"1000"}])";
  }

  void accept_loop() {
    while (!stopping_.load()) {
      const int fd = ::accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (fd < 0) {
        continue;
      }
      inflight_.fetch_add(1);
      std::thread([this, fd] {
        serve(fd);
        inflight_.fetch_sub(1);
      }).detach();
    }
  }

  void serve(int fd) {
    auto *ssl = SSL_new(context_);
    SSL_set_fd(ssl, fd);
    if (SSL_accept(ssl) != 1) {
      SSL_free(ssl);
      ::close(fd);
      return;
    }
    const auto headers = read_headers(ssl);
    if (headers.find("Upgrade: websocket") != std::string::npos ||
        headers.find("Upgrade: WebSocket") != std::string::npos) {
      serve_websocket(ssl, headers);
    } else {
      serve_http(ssl, headers);
    }
    SSL_set_quiet_shutdown(ssl, 1);
    SSL_shutdown(ssl);
    SSL_free(ssl);
    ::shutdown(fd, SHUT_RDWR);
    ::close(fd);
  }

  void serve_http(SSL *ssl, const std::string &request) {
    if (fail_gate_.load() && request.find("/api/v4/") != std::string::npos) {
      const std::string_view body{"fail"};
      const auto response =
          "HTTP/1.1 500 Internal Server Error\r\nContent-Length: " +
          std::to_string(body.size()) +
          "\r\nConnection: close\r\n\r\n" + std::string(body);
      (void)write_all(ssl, response);
      return;
    }
    std::string body;
    if (request.find("/api/v3/ticker/24hr") != std::string::npos ||
        request.find("/fapi/v1/ticker/24hr") != std::string::npos ||
        request.find("/fapi/v3/ticker/24hr") != std::string::npos) {
      body = ticker_24h();
    } else if (request.find("/fapi/") != std::string::npos) {
      body = binance_perp();
    } else if (request.find("/api/v3/exchangeInfo") != std::string::npos) {
      body = binance_spot();
    } else if (request.find("/api/v4/spot/tickers") != std::string::npos) {
      body = gate_spot_tickers();
    } else if (request.find("/api/v4/futures/usdt/tickers") !=
               std::string::npos) {
      body = gate_perp_tickers();
    } else if (request.find("/api/v4/spot/currency_pairs") !=
               std::string::npos) {
      body = gate_spot();
    } else if (request.find("/api/v4/futures/usdt/contracts") !=
               std::string::npos) {
      body = gate_perp();
    } else {
      body = "{}";
    }
    const auto response = "HTTP/1.1 200 OK\r\nContent-Length: " +
                          std::to_string(body.size()) +
                          "\r\nConnection: close\r\n\r\n" + body;
    (void)write_all(ssl, response);
  }

  void serve_websocket(SSL *ssl, const std::string &request) {
    const auto key_begin =
        request.find("Sec-WebSocket-Key: ") + std::strlen("Sec-WebSocket-Key: ");
    const auto key_end = request.find("\r\n", key_begin);
    if (key_begin < std::strlen("Sec-WebSocket-Key: ") ||
        key_end == std::string::npos) {
      return;
    }
    std::array<char, 29> accept{};
    if (!net::websocket_accept_value(
            std::string_view(request).substr(key_begin, key_end - key_begin),
            accept)) {
      return;
    }
    const auto response =
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
        "Connection: Upgrade\r\nSec-WebSocket-Accept: " +
        std::string(accept.data()) + "\r\n\r\n";
    if (!write_all(ssl, response)) {
      return;
    }

    while (!stopping_.load()) {
      std::array<std::byte, 2> header{};
      if (!read_exact(ssl, header)) {
        return;
      }
      const auto opcode = std::to_integer<unsigned>(header[0] & std::byte{0x0f});
      std::size_t size = std::to_integer<unsigned>(header[1] & std::byte{0x7f});
      if (size == 126) {
        std::array<std::byte, 2> extended{};
        if (!read_exact(ssl, extended)) {
          return;
        }
        size = (std::to_integer<unsigned>(extended[0]) << 8U) |
               std::to_integer<unsigned>(extended[1]);
      } else if (size == 127) {
        std::array<std::byte, 8> extended{};
        if (!read_exact(ssl, extended)) {
          return;
        }
        size = 0;
        for (auto byte : extended) {
          size = (size << 8U) | std::to_integer<unsigned>(byte);
        }
      }
      const bool masked =
          std::to_integer<unsigned>(header[1] & std::byte{0x80}) != 0;
      std::array<std::byte, 4> mask{};
      if (masked && !read_exact(ssl, mask)) {
        return;
      }
      std::vector<std::byte> payload(size);
      if (size != 0 && !read_exact(ssl, payload)) {
        return;
      }
      std::string text(size, '\0');
      for (std::size_t index = 0; index < size; ++index) {
        auto value = std::to_integer<unsigned>(payload[index]);
        if (masked) {
          value ^= std::to_integer<unsigned>(mask[index & 3U]);
        }
        text[index] = static_cast<char>(value);
      }
      if (opcode == 0x8) {
        return;
      }
      if (opcode == 0x9) {
        std::string pong;
        pong.push_back(static_cast<char>(0x8a));
        const auto pong_size = text.size() > 125 ? 125 : text.size();
        pong.push_back(static_cast<char>(pong_size));
        pong.append(text.data(), pong_size);
        if (!write_all(ssl, pong)) {
          return;
        }
        continue;
      }
      if (opcode != 0x1) {
        continue;
      }
      const int stall = stall_ws_.fetch_sub(1);
      if (stall > 0) {
        while (!stopping_.load()) {
          std::this_thread::sleep_for(std::chrono::milliseconds(20));
        }
        return;
      }
      if (text.find("\"event\":\"subscribe\"") != std::string::npos) {
        const char *ack =
            text.find("futures.book_ticker") != std::string::npos
                ? R"({"event":"subscribe","channel":"futures.book_ticker","result":{"status":"success"}})"
                : R"({"event":"subscribe","channel":"spot.book_ticker","result":{"status":"success"}})";
        if (!send_text(ssl, ack)) {
          return;
        }
      } else {
        if (!send_text(ssl, R"({"result":null,"id":1})")) {
          return;
        }
      }
    }
  }

  SSL_CTX *context_{};
  int listener_{-1};
  std::uint16_t port_{};
  std::atomic<bool> stopping_{};
  std::atomic<bool> include_eth_{};
  std::atomic<bool> fail_gate_{};
  std::atomic<int> stall_ws_{};
  std::atomic<int> inflight_{};
  std::thread worker_;
};

std::string discovery_yaml(const DiscoveryLoopback &server, std::string_view prefix,
                           bool six_connections, int recovery_ms) {
  std::ostringstream out;
  out << "shared_memory:\n"
      << "  prefix: " << prefix << "\n"
      << "  backend: POSIX_SHM\n"
      << "  mode: OVERWRITE_OLDEST\n"
      << "  ring_bytes: 65536\n"
      << "  max_record_bytes: 4096\n"
      << "  max_readers: 4\n"
      << "  reader_lease_timeout_ns: 5000000000\n"
      << "  unlink_on_shutdown: true\n"
      << "  ring_layout: multiplex\n"
      << "  shard_count: 1\n"
      << "  multiplex_ring_bytes: 65536\n"
      << "capacity:\n"
      << "  max_instruments: 64\n"
      << "  max_rings: 32\n"
      << "  max_total_ring_bytes: 16777216\n"
      << "venues:\n";
  auto add_venue = [&](std::string_view venue, std::string_view product) {
    out << "  - venue: " << venue << "\n"
        << "    product: " << product << "\n"
        << "    websocket_endpoint: " << server.wss() << "\n"
        << "    rest_endpoint: " << server.https() << "\n"
        << "    protocol: JSON\n"
        << "    recovery_deadline_ms: " << recovery_ms << "\n"
        << "    max_continuous_recovery_ms: 300000\n"
        << "    max_symbols_per_ws: 64\n";
  };
  add_venue("binance", "SPOT");
  if (six_connections) {
    add_venue("binance", "PERPETUAL");
    add_venue("aster", "SPOT");
    add_venue("aster", "PERPETUAL");
    add_venue("gate", "SPOT");
    add_venue("gate", "PERPETUAL");
  }
  out << "subscriptions:\n";
  auto add_sub = [&](std::string_view venue, std::string_view product) {
    out << "  - venue: " << venue << "\n"
        << "    product: " << product << "\n"
        << "    stream: ticker\n"
        << "    discovery:\n"
        << "      quote_assets: [USDT]\n"
        << "      minimum_turnover: 1\n"
        << "      max_symbols: 8\n";
  };
  add_sub("binance", "SPOT");
  if (six_connections) {
    add_sub("binance", "PERPETUAL");
    add_sub("aster", "SPOT");
    add_sub("aster", "PERPETUAL");
    add_sub("gate", "SPOT");
    add_sub("gate", "PERPETUAL");
  }
  return out.str();
}

bool pump_until(mds::producer::ProducerRuntime &runtime,
                const std::function<bool()> &ready, int timeout_ms) {
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
  while (std::chrono::steady_clock::now() < deadline) {
    (void)runtime.run_once(5);
    if (runtime.failed()) {
      return false;
    }
    if (ready()) {
      return true;
    }
  }
  return false;
}

void pump_for(mds::producer::ProducerRuntime &runtime, int timeout_ms) {
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
  while (std::chrono::steady_clock::now() < deadline) {
    (void)runtime.run_once(5);
  }
}

void require_pump(mds::producer::ProducerRuntime &runtime,
                  const std::ostringstream &logs,
                  const std::ostringstream &errors,
                  const std::function<bool()> &ready, int timeout_ms,
                  const char *what) {
  if (pump_until(runtime, ready, timeout_ms)) {
    return;
  }
  std::fprintf(stderr, "%s failed. runtime_error=%.*s\nlogs:\n%s\nerrors:\n%s\n",
               what, static_cast<int>(runtime.error().size()),
               runtime.error().data(), logs.str().c_str(),
               errors.str().c_str());
  assert(false);
}

void test_discovery_batch_lifecycle() {
  Certificate identity;
  TemporaryFile trust("mds-disc-mem-trust");
  identity.write_trust_file(trust.path);
  assert(::setenv("SSL_CERT_FILE", trust.path.c_str(), 1) == 0);
  DiscoveryLoopback server(identity);
  const auto prefix = "/mds.disc.mem." + std::to_string(::getpid());

  TemporaryFile one("mds-disc-one");
  {
    std::ofstream out(one.path);
    out << discovery_yaml(server, prefix + ".one", false, 5000);
  }
  std::ostringstream logs;
  std::ostringstream errors;
  mds::producer::ProducerRuntime runtime({
      .config_path = one.path,
      .output = &logs,
      .error_output = &errors,
  });
  auto started = runtime.start();
  if (!started) {
    std::fprintf(stderr, "discovery runtime start failed: %.*s\n",
                 static_cast<int>(runtime.error().size()),
                 runtime.error().data());
  }
  assert(started);
  pump_for(runtime, 1500);
  assert(!runtime.failed());
  const auto instruments_before = runtime.instrument_count_for_test();
  assert(instruments_before >= 1);

  runtime.request_discovery_now_for_test();
  require_pump(
      runtime, logs, errors,
      [&] {
        return logs.str().find("unchanged connection_id=0") !=
                   std::string::npos &&
               logs.str().find("memory cleanup") != std::string::npos;
      },
      8000, "unchanged discovery cleanup");
  assert(count_occurrences(logs.str(), "memory cleanup") == 1);
  assert(!runtime.discovery_batch_active_for_test());
  assert(runtime.instrument_count_for_test() == instruments_before);

  server.set_include_eth(true);
  runtime.request_discovery_now_for_test();
  require_pump(
      runtime, logs, errors,
      [&] {
        return logs.str().find("candidate committed connection_id=0") !=
                   std::string::npos &&
               count_occurrences(logs.str(), "memory cleanup") >= 2;
      },
      12000, "single connection replace commit");
  assert(runtime.instrument_count_for_test() > instruments_before);
  assert(runtime.ring_count_for_test() >= 1);
  runtime.stop();

  TemporaryFile six("mds-disc-six");
  {
    std::ofstream out(six.path);
    out << discovery_yaml(server, prefix + ".six", true, 5000);
  }
  server.set_include_eth(false);
  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  mds::producer::ProducerRuntime six_runtime({
      .config_path = six.path,
      .output = &logs,
      .error_output = &errors,
  });
  started = six_runtime.start();
  if (!started) {
    std::fprintf(stderr, "six-connection start failed: %.*s\n",
                 static_cast<int>(six_runtime.error().size()),
                 six_runtime.error().data());
  }
  assert(started);
  pump_for(six_runtime, 2500);
  assert(!six_runtime.failed());
  server.set_include_eth(true);
  six_runtime.request_discovery_now_for_test();
  require_pump(
      six_runtime, logs, errors,
      [&] {
        return count_occurrences(logs.str(),
                                 "candidate committed connection_id=") == 6 &&
               logs.str().find("memory cleanup") != std::string::npos;
      },
      20000, "six connection sequential replace");
  assert(count_occurrences(logs.str(), "awaiting Live") == 6);
  six_runtime.stop();

  TemporaryFile fail("mds-disc-fail");
  {
    std::ofstream out(fail.path);
    out << discovery_yaml(server, prefix + ".fail", true, 5000);
  }
  server.set_include_eth(false);
  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  mds::producer::ProducerRuntime fail_runtime({
      .config_path = fail.path,
      .output = &logs,
      .error_output = &errors,
  });
  started = fail_runtime.start();
  if (!started) {
    std::fprintf(stderr, "partial-fail start failed: %.*s\n",
                 static_cast<int>(fail_runtime.error().size()),
                 fail_runtime.error().data());
  }
  assert(started);
  pump_for(fail_runtime, 2500);
  assert(!fail_runtime.failed());
  server.fail_gate(true);
  fail_runtime.request_discovery_now_for_test();
  require_pump(
      fail_runtime, logs, errors,
      [&] {
        return errors.str().find("refresh failed") != std::string::npos &&
               logs.str().find("memory cleanup") != std::string::npos;
      },
      12000, "partial discovery failure cleanup");
  fail_runtime.stop();
  server.fail_gate(false);

  TemporaryFile rollback("mds-disc-rollback");
  {
    std::ofstream out(rollback.path);
    out << discovery_yaml(server, prefix + ".rb", false, 400);
  }
  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  mds::producer::ProducerRuntime rollback_runtime({
      .config_path = rollback.path,
      .output = &logs,
      .error_output = &errors,
  });
  started = rollback_runtime.start();
  assert(started);
  pump_for(rollback_runtime, 1500);
  assert(!rollback_runtime.failed());
  const auto rollback_instruments =
      rollback_runtime.instrument_count_for_test();
  const auto rollback_rings = rollback_runtime.ring_count_for_test();
  server.set_include_eth(true);
  server.stall_next_ws(1);
  rollback_runtime.request_discovery_now_for_test();
  require_pump(
      rollback_runtime, logs, errors,
      [&] {
        return errors.str().find("rollback restored") != std::string::npos &&
               logs.str().find("memory cleanup") != std::string::npos;
      },
      12000, "candidate rollback");
  assert(rollback_runtime.instrument_count_for_test() == rollback_instruments);
  assert(rollback_runtime.ring_count_for_test() == rollback_rings);
  rollback_runtime.stop();
  server.set_include_eth(false);
  server.stall_next_ws(0);

  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  mds::producer::ProducerRuntime latch_runtime({
      .config_path = one.path,
      .output = &logs,
      .error_output = &errors,
  });
  assert(latch_runtime.start());
  pump_for(latch_runtime, 1500);
  server.set_include_eth(true);
  server.stall_next_ws(8);
  latch_runtime.request_discovery_now_for_test();
  require_pump(
      latch_runtime, logs, errors,
      [&] {
        return logs.str().find("awaiting Live") != std::string::npos &&
               latch_runtime.discovery_batch_active_for_test();
      },
      8000, "latch waiting for live");
  const auto first_refresh =
      count_occurrences(logs.str(), "daily UTC 00:05 refresh started");
  latch_runtime.request_discovery_now_for_test();
  for (int i = 0; i < 20; ++i) {
    (void)latch_runtime.run_once(5);
  }
  assert(count_occurrences(logs.str(), "daily UTC 00:05 refresh started") ==
         first_refresh);
  latch_runtime.stop();
  assert(!latch_runtime.discovery_batch_active_for_test());
  server.stall_next_ws(0);
  server.set_include_eth(false);

  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  mds::producer::ProducerRuntime restart_runtime({
      .config_path = one.path,
      .output = &logs,
      .error_output = &errors,
  });
  assert(restart_runtime.start());
  pump_for(restart_runtime, 1500);
  server.set_include_eth(true);
  server.stall_next_ws(8);
  restart_runtime.request_discovery_now_for_test();
  require_pump(
      restart_runtime, logs, errors,
      [&] { return restart_runtime.discovery_batch_active_for_test(); },
      8000, "discovery active before stop");
  restart_runtime.stop();
  server.stall_next_ws(0);
  server.set_include_eth(false);
  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  assert(restart_runtime.start());
  assert(!restart_runtime.discovery_batch_active_for_test());
  pump_for(restart_runtime, 1500);
  restart_runtime.request_discovery_now_for_test();
  require_pump(
      restart_runtime, logs, errors,
      [&] {
        return logs.str().find("daily UTC 00:05 refresh started") !=
               std::string::npos;
      },
      8000, "discovery after stop/start");
  restart_runtime.stop();

  logs.str("");
  logs.clear();
  errors.str("");
  errors.clear();
  mds::producer::ProducerRuntime batches({
      .config_path = one.path,
      .output = &logs,
      .error_output = &errors,
  });
  assert(batches.start());
  pump_for(batches, 1500);
  const auto fd_before = count_open_fds();
  for (int round = 0; round < 100; ++round) {
    batches.request_discovery_now_for_test();
    const auto target = round + 1;
    require_pump(
        batches, logs, errors,
        [&] {
          return count_occurrences(logs.str(), "memory cleanup") >= target;
        },
        8000, "repeated batch cleanup");
  }
  const auto fd_after = count_open_fds();
  assert(count_occurrences(logs.str(), "memory cleanup") == 100);
  assert(fd_after >= 0 && fd_before >= 0);
  assert(fd_after <= fd_before + 8);
  batches.stop();
  ::unsetenv("SSL_CERT_FILE");
}

}  // namespace

int main() {
  test_next_daily_discovery_utc();
  std::ostringstream output;
  mds::producer::ProducerRuntime runtime({
      .config_path = MDS_PRODUCER_EXAMPLE_CONFIG,
      .validate_only = true,
      .output = &output,
  });
  const auto started = runtime.start();
  assert(started);
  assert(!runtime.failed());
  assert(runtime.error().empty());
  assert(!output.str().empty());
  assert(output.str().find("mds_producer build_id=") != std::string::npos);
  assert(output.str().find(" build_utc=") != std::string::npos);
  assert(output.str().find(" git_sha=") != std::string::npos);
  assert(output.str().find(" dirty=") != std::string::npos);
  assert(output.str().find("recovery_deadline_ms=30000") !=
         std::string::npos);
  assert(output.str().find("max_continuous_recovery_ms=300000") !=
         std::string::npos);
  assert(!runtime.resolved_segments().empty());
  assert(runtime.run_once(0) == 0);
  for (const auto &segment : runtime.resolved_segments()) {
    assert(!segment.name.empty());
    assert(segment.name.front() == '/');
    assert(segment.ring_bytes == 8U << 20U);
    assert(segment.max_record_bytes == 64U << 10U);
  }
  const auto duplicate_start = runtime.start();
  assert(!duplicate_start);
  assert(duplicate_start.error == mds::api::ErrorCode::AlreadyStarted);
  assert(!runtime.failed());
  runtime.stop();
  runtime.stop();
  assert(runtime.start());
  runtime.stop();

  std::ostringstream errors;
  mds::producer::ProducerRuntime invalid({
      .config_path = "/path/that/does/not/exist.yaml",
      .error_output = &errors,
  });
  const auto failed = invalid.start();
  assert(!failed);
  assert(failed.error == mds::api::ErrorCode::InvalidConfig);
  assert(invalid.failed());
  assert(!invalid.error().empty());
  assert(errors.str().find(invalid.error()) != std::string::npos);
  assert(invalid.resolved_segments().empty());

  mds::producer::ProducerRuntime not_started({
      .config_path = MDS_PRODUCER_EXAMPLE_CONFIG,
      .validate_only = true,
  });
  assert(not_started.run_once(0) == -1);
  assert(not_started.failed());
  assert(not_started.start());
  assert(!not_started.failed());
  not_started.stop();

  std::string create_error{"stale"};
  auto created = mds::producer::ProducerRuntime::create(
      MDS_PRODUCER_EXAMPLE_CONFIG, create_error);
  assert(created);
  assert(create_error.empty());
  created->stop();
  test_gate_discovery_decimal_headers();
  test_hyperliquid_xyz_discovery_is_best_effort();
  test_discovery_batch_lifecycle();
  return 0;
}
