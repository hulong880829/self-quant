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
#include <cstring>
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
  Bitget,
  GateDirtyQuantity
};

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
  unsigned gate_snapshot_requests() const {
    return gate_snapshot_requests_.load();
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
        websocket(ssl, request);
      } else {
        metadata(ssl, request);
      }
    }
    SSL_shutdown(ssl);
    SSL_free(ssl);
    ::close(fd);
  }

  void metadata(SSL *ssl, const std::string &request) {
    static constexpr std::string_view binance_body =
        R"({"timezone":"UTC","symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]},{"symbol":"ETHUSDT","status":"TRADING","baseAsset":"ETH","quoteAsset":"USDT","marginAsset":"USDT","contractType":"PERPETUAL","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01","minPrice":"0.01","maxPrice":"1000000.00"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"100000.000"}]}]})";
    static constexpr std::string_view bitget_body =
        R"({"code":"00000","data":[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","pricePlace":"1","volumePlace":"3","priceEndStep":"1","sizeMultiplier":"0.001"},{"symbol":"ETHUSDT","baseCoin":"ETH","quoteCoin":"USDT","pricePlace":"1","volumePlace":"3","priceEndStep":"1","sizeMultiplier":"0.001"}]})";
    static constexpr std::string_view gate_metadata =
        R"([{"id":"BTC_USDT","base":"BTC","quote":"USDT","precision":2,"amount_precision":3},{"id":"ETH_USDT","base":"ETH","quote":"USDT","precision":2,"amount_precision":3},{"id":"SOL_USDT","base":"SOL","quote":"USDT","precision":3,"amount_precision":8}])";
    static constexpr std::string_view gate_snapshot =
        R"({"id":10,"current":10,"bids":[["150.000","1.00000000"]],"asks":[["150.001","2.00000000"]]})";
    std::string_view body = binance_body;
    if (protocol_ == Protocol::Bitget) {
      body = bitget_body;
    } else if (protocol_ == Protocol::GateDirtyQuantity) {
      if (request.find("/api/v4/spot/order_book?") != std::string::npos) {
        ++gate_snapshot_requests_;
        body = gate_snapshot;
      } else {
        body = gate_metadata;
      }
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
    const bool btc = subscription.find("btcusdt") != std::string::npos ||
                     subscription.find("BTCUSDT") != std::string::npos;
    const auto number = btc ? ++btc_connections_ : ++eth_connections_;
    const std::string symbol = btc ? "BTCUSDT" : "ETHUSDT";
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
    if (protocol_ == Protocol::BinanceClose && btc && number == 1) {
      (void)send_close(ssl, 1001, "rotate");
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
  std::atomic<unsigned> gate_snapshot_requests_{};
  Protocol protocol_{};
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
    observe(record.header.bus_seq);
    return true;
  }
  bool OnTicker(const utils::md::wire::TickerRecord &) noexcept override {
    return true;
  }
  bool OnDelta(const utils::md::wire::DeltaRecord &) noexcept override {
    return true;
  }
  bool OnSnapshotBegin(
      const utils::md::wire::SnapshotBeginRecord &) noexcept override {
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

void test_continuous_recovery_timeout_fails_connection() {
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
  options.max_continuous_recovery_duration = 250ms;
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
  auto *connection = created.value;
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  while (std::chrono::steady_clock::now() < deadline &&
         connection->state() != mds::service::MarketDataState::Failed) {
    (void)manager.run_once(5);
  }
  assert(connection->state() == mds::service::MarketDataState::Failed);
  if (connection->error_message().find(
          "continuous recovery timeout symbol=BTCUSDT") ==
      std::string_view::npos) {
    std::cerr << "unexpected recovery timeout error: "
              << connection->error_message() << '\n';
  }
  assert(connection->error_message().find(
             "continuous recovery timeout symbol=BTCUSDT") !=
         std::string_view::npos);
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
  assert(connection->metrics().ws_shards_live == 1);
  assert(std::string_view(connection->metrics().last_resync_symbol.data()) ==
         "SOLUSDT");
  assert(connection->metrics().last_resync_reason ==
         mds::service::ResyncReason::DirtyData);
  assert(server.gate_connections() == 1);
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

int main() {
  std::signal(SIGPIPE, SIG_IGN);
  test_binance_shard_reconnect();
  test_binance_ticker_only_ack_is_live();
  test_continuous_recovery_timeout_fails_connection();
  test_binance_malformed_json_reconnect();
  test_binance_close_detail_reconnect();
  test_bitget_malformed_json_reconnect();
  test_gate_dirty_quantity_resyncs_only_sol();
  return 0;
}
