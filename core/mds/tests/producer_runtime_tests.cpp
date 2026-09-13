#include "mds/producer/producer_runtime.h"

#include <arpa/inet.h>
#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <openssl/evp.h>
#include <openssl/pem.h>
#include <openssl/ssl.h>
#include <openssl/x509v3.h>
#include <sstream>
#include <string>
#include <string_view>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>

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
  return 0;
}
