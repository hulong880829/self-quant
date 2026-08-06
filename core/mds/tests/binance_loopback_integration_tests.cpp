#include "mds/network/websocket_codec.h"
#include "mds/service/binance_session.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <arpa/inet.h>
#include <array>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdio>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <iostream>
#include <mutex>
#include <openssl/ssl.h>
#include <openssl/x509v3.h>
#include <poll.h>
#include <signal.h>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <utility>
#include <vector>

namespace {

using namespace std::chrono_literals;
using Profile = mds::exchange::binance::Profile;

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::uint64_t now_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::steady_clock::now().time_since_epoch().count());
}

std::string fixture(std::string_view name) {
  std::ifstream input(std::string(MDS_TEST_FIXTURE_DIR) + "/" +
                          std::string(name),
                      std::ios::binary);
  require(input.good(), "failed to open loopback fixture");
  return {std::istreambuf_iterator<char>(input),
          std::istreambuf_iterator<char>()};
}

struct Certificate {
  EVP_PKEY *key{};
  X509 *certificate{};

  Certificate() {
    EVP_PKEY_CTX *key_context = EVP_PKEY_CTX_new_id(EVP_PKEY_RSA, nullptr);
    require(key_context != nullptr, "failed to create key context");
    const bool key_ok =
        EVP_PKEY_keygen_init(key_context) == 1 &&
        EVP_PKEY_CTX_set_rsa_keygen_bits(key_context, 2048) == 1 &&
        EVP_PKEY_keygen(key_context, &key) == 1;
    EVP_PKEY_CTX_free(key_context);
    require(key_ok, "failed to generate loopback key");

    certificate = X509_new();
    require(certificate != nullptr, "failed to allocate certificate");
    require(X509_set_version(certificate, 2) == 1 &&
                ASN1_INTEGER_set(X509_get_serialNumber(certificate), 1) == 1 &&
                X509_gmtime_adj(X509_getm_notBefore(certificate), -60) !=
                    nullptr &&
                X509_gmtime_adj(X509_getm_notAfter(certificate), 3600) !=
                    nullptr &&
                X509_set_pubkey(certificate, key) == 1,
            "failed to initialize certificate");
    X509_NAME *name = X509_get_subject_name(certificate);
    constexpr unsigned char common_name[] = "127.0.0.1";
    require(X509_NAME_add_entry_by_txt(
                name, "CN", MBSTRING_ASC, common_name, -1, -1, 0) == 1 &&
                X509_set_issuer_name(certificate, name) == 1,
            "failed to set certificate name");
    X509V3_CTX extension_context{};
    X509V3_set_ctx(&extension_context, certificate, certificate, nullptr,
                   nullptr, 0);
    X509_EXTENSION *san = X509V3_EXT_conf_nid(
        nullptr, &extension_context, NID_subject_alt_name,
        const_cast<char *>("IP:127.0.0.1"));
    require(san != nullptr && X509_add_ext(certificate, san, -1) == 1,
            "failed to add certificate SAN");
    X509_EXTENSION_free(san);
    require(X509_sign(certificate, key, EVP_sha256()) > 0,
            "failed to sign certificate");
  }

  ~Certificate() {
    X509_free(certificate);
    EVP_PKEY_free(key);
  }
};

mds::network::SharedSslContext client_context(const Certificate &identity) {
  SSL_CTX *raw = SSL_CTX_new(TLS_client_method());
  require(raw != nullptr, "failed to create client TLS context");
  mds::network::SharedSslContext context(raw, mds::network::SslCtxDeleter{});
  require(SSL_CTX_set_min_proto_version(raw, TLS1_2_VERSION) == 1,
          "failed to set client TLS version");
  SSL_CTX_set_verify(raw, SSL_VERIFY_PEER, nullptr);
  require(X509_STORE_add_cert(SSL_CTX_get_cert_store(raw),
                              identity.certificate) == 1,
          "failed to trust loopback certificate");
  return context;
}

class LoopbackBinanceServer {
public:
  enum class Recovery { Disconnect, SequenceGap };

  LoopbackBinanceServer(Profile profile, Recovery recovery,
                        const Certificate &identity)
      : profile_(profile), recovery_(recovery),
        exchange_info_(fixture(profile == Profile::Spot
                                   ? "binance_spot_exchange_info.json"
                                   : "binance_usdm_exchange_info.json")) {
    context_ = SSL_CTX_new(TLS_server_method());
    require(context_ != nullptr &&
                SSL_CTX_set_min_proto_version(context_, TLS1_2_VERSION) == 1 &&
                SSL_CTX_use_certificate(context_, identity.certificate) == 1 &&
                SSL_CTX_use_PrivateKey(context_, identity.key) == 1 &&
                SSL_CTX_check_private_key(context_) == 1,
            "failed to configure server TLS context");

    listener_ = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    require(listener_ >= 0, "failed to create loopback listener");
    int reuse = 1;
    require(::setsockopt(listener_, SOL_SOCKET, SO_REUSEADDR, &reuse,
                         sizeof(reuse)) == 0,
            "failed to configure loopback listener");
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    address.sin_port = 0;
    require(::bind(listener_, reinterpret_cast<sockaddr *>(&address),
                   sizeof(address)) == 0 &&
                ::listen(listener_, 16) == 0,
            "failed to bind loopback listener");
    socklen_t length = sizeof(address);
    require(::getsockname(listener_, reinterpret_cast<sockaddr *>(&address),
                          &length) == 0,
            "failed to query loopback port");
    port_ = ntohs(address.sin_port);
    accept_thread_ = std::thread([this] { accept_loop(); });
  }

  ~LoopbackBinanceServer() {
    stopping_.store(true, std::memory_order_release);
    ::shutdown(listener_, SHUT_RDWR);
    ::close(listener_);
    if (accept_thread_.joinable()) {
      accept_thread_.join();
    }
    {
      std::lock_guard lock(socket_mutex_);
      for (const int fd : sockets_) {
        ::shutdown(fd, SHUT_RDWR);
      }
    }
    for (auto &worker : workers_) {
      if (worker.joinable()) {
        worker.join();
      }
    }
    SSL_CTX_free(context_);
  }

  LoopbackBinanceServer(const LoopbackBinanceServer &) = delete;
  LoopbackBinanceServer &operator=(const LoopbackBinanceServer &) = delete;

  [[nodiscard]] std::string endpoint(std::string_view scheme) const {
    return std::string(scheme) + "://127.0.0.1:" +
           std::to_string(port_);
  }

  [[nodiscard]] bool pong_verified() const noexcept {
    return pong_verified_.load(std::memory_order_acquire);
  }

  [[nodiscard]] bool healthy() const {
    std::lock_guard lock(error_mutex_);
    return error_.empty();
  }

  [[nodiscard]] std::string error() const {
    std::lock_guard lock(error_mutex_);
    return error_;
  }

private:
  void fail(std::string message) {
    std::lock_guard lock(error_mutex_);
    if (error_.empty()) {
      error_ = std::move(message);
    }
  }

  void accept_loop() {
    while (!stopping_.load(std::memory_order_acquire)) {
      pollfd descriptor{listener_, POLLIN, 0};
      const int ready = ::poll(&descriptor, 1, 50);
      if (ready <= 0) {
        continue;
      }
      const int fd = ::accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (fd < 0) {
        if (!stopping_.load(std::memory_order_acquire)) {
          fail("loopback accept failed");
        }
        continue;
      }
      {
        std::lock_guard lock(socket_mutex_);
        sockets_.push_back(fd);
      }
      workers_.emplace_back([this, fd] { serve(fd); });
    }
  }

  static bool ssl_write_all(SSL *ssl, std::span<const std::byte> bytes,
                            std::size_t fragment = 7) {
    std::size_t offset = 0;
    while (offset < bytes.size()) {
      const std::size_t amount =
          std::min(fragment, bytes.size() - offset);
      const int written =
          SSL_write(ssl, bytes.data() + offset, static_cast<int>(amount));
      if (written <= 0) {
        return false;
      }
      offset += static_cast<std::size_t>(written);
    }
    return true;
  }

  static bool ssl_write_all(SSL *ssl, std::string_view text,
                            std::size_t fragment = 7) {
    return ssl_write_all(
        ssl,
        {reinterpret_cast<const std::byte *>(text.data()), text.size()},
        fragment);
  }

  static std::string read_headers(SSL *ssl) {
    std::string request;
    char buffer[256];
    while (request.find("\r\n\r\n") == std::string::npos &&
           request.size() < 16U * 1024U) {
      const int received = SSL_read(ssl, buffer, sizeof(buffer));
      if (received <= 0) {
        break;
      }
      request.append(buffer, static_cast<std::size_t>(received));
    }
    return request;
  }

  static std::vector<std::byte> frame(std::uint8_t opcode,
                                      std::string_view payload) {
    std::vector<std::byte> output;
    output.push_back(static_cast<std::byte>(0x80U | opcode));
    if (payload.size() <= 125) {
      output.push_back(static_cast<std::byte>(payload.size()));
    } else {
      require(payload.size() <= 65535, "test WebSocket payload too large");
      output.push_back(std::byte{126});
      output.push_back(static_cast<std::byte>((payload.size() >> 8U) & 0xffU));
      output.push_back(static_cast<std::byte>(payload.size() & 0xffU));
    }
    const auto old_size = output.size();
    output.resize(old_size + payload.size());
    std::memcpy(output.data() + old_size, payload.data(), payload.size());
    return output;
  }

  static bool send_frame(SSL *ssl, std::uint8_t opcode,
                         std::string_view payload) {
    const auto bytes = frame(opcode, payload);
    return ssl_write_all(ssl, bytes, 5);
  }

  static bool read_masked_pong(SSL *ssl) {
    std::vector<std::byte> bytes;
    bytes.reserve(64);
    while (bytes.size() < 8) {
      std::byte buffer[32];
      const int received = SSL_read(ssl, buffer, sizeof(buffer));
      if (received <= 0) {
        return false;
      }
      bytes.insert(bytes.end(), buffer,
                   buffer + static_cast<std::size_t>(received));
      if (bytes.size() >= 2) {
        const std::size_t length =
            static_cast<std::size_t>(bytes[1] & std::byte{0x7f});
        if (bytes.size() >= 6 + length) {
          break;
        }
      }
    }
    if (bytes.size() < 8 ||
        (std::to_integer<unsigned>(bytes[0]) & 0x0fU) != 0x0aU ||
        (std::to_integer<unsigned>(bytes[1]) & 0x80U) == 0) {
      return false;
    }
    const std::size_t length =
        std::to_integer<unsigned>(bytes[1] & std::byte{0x7f});
    if (length != 2 || bytes.size() < 6 + length) {
      return false;
    }
    char payload[2]{};
    for (std::size_t index = 0; index < length; ++index) {
      payload[index] = static_cast<char>(
          std::to_integer<unsigned>(bytes[6 + index] ^
                                    bytes[2 + (index & 3U)]));
    }
    return std::string_view(payload, 2) == "pi";
  }

  void serve(int fd) {
    SSL *ssl = SSL_new(context_);
    if (ssl == nullptr) {
      fail("server SSL_new failed");
      close_socket(fd);
      return;
    }
    SSL_set_fd(ssl, fd);
    if (SSL_accept(ssl) != 1) {
      if (!stopping_.load(std::memory_order_acquire)) {
        fail("server TLS handshake failed");
      }
      SSL_free(ssl);
      close_socket(fd);
      return;
    }
    const std::string request = read_headers(ssl);
    if (request.find("Upgrade: websocket") != std::string::npos) {
      serve_websocket(ssl, request);
    } else {
      serve_http(ssl, request);
    }
    SSL_shutdown(ssl);
    SSL_free(ssl);
    close_socket(fd);
  }

  void close_socket(int fd) {
    {
      std::lock_guard lock(socket_mutex_);
      const auto found = std::find(sockets_.begin(), sockets_.end(), fd);
      if (found != sockets_.end()) {
        sockets_.erase(found);
      }
    }
    ::close(fd);
  }

  void serve_http(SSL *ssl, const std::string &request) {
    if (request.find("exchangeInfo") != std::string::npos) {
      const auto split = exchange_info_.size() / 2U;
      const std::string body =
          [&] {
            char size[32]{};
            const int count =
                std::snprintf(size, sizeof(size), "%zx", split);
            return std::string(size, static_cast<std::size_t>(count));
          }() +
          "\r\n" +
          exchange_info_.substr(0, split) + "\r\n" +
          [&] {
            char size[32]{};
            const int count = std::snprintf(
                size, sizeof(size), "%zx", exchange_info_.size() - split);
            return std::string(size, static_cast<std::size_t>(count));
          }() +
          "\r\n" + exchange_info_.substr(split) + "\r\n0\r\n\r\n";
      const std::string response =
          "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n"
          "Connection: close\r\n\r\n" +
          body;
      if (!ssl_write_all(ssl, response)) {
        fail("failed to write chunked exchangeInfo");
      }
      return;
    }
    if (request.find("/depth") == std::string::npos) {
      fail("unexpected HTTPS target");
      return;
    }
    const unsigned number =
        snapshot_count_.fetch_add(1, std::memory_order_acq_rel) + 1U;
    const std::uint64_t sequence =
        profile_ == Profile::Spot ? (number == 1 ? 100U : 200U)
                                  : (number == 1 ? 160U : 260U);
    const std::string body =
        std::string("{\"lastUpdateId\":") + std::to_string(sequence) +
        (profile_ == Profile::Spot
             ? R"(,"bids":[["4.00","431.00000"],["3.99","2.00000"]],"asks":[["4.01","12.00000"],["4.02","5.00000"]]})"
             : R"(,"E":1,"T":1,"bids":[["100.0","10.000"],["99.9","2.000"]],"asks":[["100.1","20.000"],["100.2","5.000"]]})");
    const std::string response =
        "HTTP/1.1 200 OK\r\nContent-Length: " +
        std::to_string(body.size()) + "\r\nConnection: close\r\n\r\n" + body;
    if (!ssl_write_all(ssl, response)) {
      fail("failed to write depth snapshot");
    }
    condition_.notify_all();
  }

  bool wait_for_snapshots(unsigned wanted) {
    std::unique_lock lock(condition_mutex_);
    return condition_.wait_for(lock, 2s, [this, wanted] {
      return stopping_.load(std::memory_order_acquire) ||
             snapshot_count_.load(std::memory_order_acquire) >= wanted;
    });
  }

  std::string depth_message(std::uint64_t first, std::uint64_t final,
                            std::uint64_t previous, bool mutate) const {
    const std::string route =
        R"({"stream":"btcusdt@depth@100ms","data":{"U":)" +
        std::to_string(first) + R"(,"u":)" + std::to_string(final) +
        (profile_ == Profile::UsdM
             ? R"(,"pu":)" + std::to_string(previous)
             : std::string{}) +
        R"(,"s":"BTCUSDT","b":[[")" +
        (profile_ == Profile::Spot ? "4.00" : "100.0") + R"(",")" +
        (mutate ? (profile_ == Profile::Spot ? "432.00000" : "11.000")
                : (profile_ == Profile::Spot ? "431.00000" : "10.000")) +
        R"("]],"a":[[")" +
        (profile_ == Profile::Spot ? "4.01" : "100.1") + R"(",")" +
        (mutate ? (profile_ == Profile::Spot ? "13.00000" : "21.000")
                : (profile_ == Profile::Spot ? "12.00000" : "20.000")) +
        R"("]],"E":2,"T":2}})";
    return route;
  }

  std::string ticker_message(std::uint64_t sequence, bool mutate) const {
    return R"({"stream":"btcusdt@bookTicker","data":{"u":)" +
           std::to_string(sequence) +
           R"(,"s":"BTCUSDT","b":")" +
           (profile_ == Profile::Spot ? "4.00" : "100.0") + R"(","B":")" +
           (mutate ? (profile_ == Profile::Spot ? "432.00000" : "11.000")
                   : (profile_ == Profile::Spot ? "431.00000" : "10.000")) +
           R"(","a":")" +
           (profile_ == Profile::Spot ? "4.01" : "100.1") + R"(","A":")" +
           (mutate ? (profile_ == Profile::Spot ? "13.00000" : "21.000")
                   : (profile_ == Profile::Spot ? "12.00000" : "20.000")) +
           R"(","E":2,"T":2}})";
  }

  void send_bridge_and_live(SSL *ssl, std::uint64_t snapshot) {
    const std::uint64_t bridge =
        profile_ == Profile::Spot ? snapshot + 1U : snapshot;
    const std::uint64_t previous =
        profile_ == Profile::Spot ? 0U : snapshot - 1U;
    std::this_thread::sleep_for(50ms);
    if (!send_frame(ssl, 1, ticker_message(bridge, false)) ||
        !send_frame(ssl, 1,
                    depth_message(bridge, bridge, previous, false)) ||
        !send_frame(ssl, 1, ticker_message(bridge + 1U, true)) ||
        !send_frame(ssl, 1,
                    depth_message(bridge + 1U, bridge + 1U, bridge, true))) {
      fail("failed to send Binance WebSocket data");
    }
  }

  void serve_websocket(SSL *ssl, const std::string &request) {
    const auto key_start = request.find("Sec-WebSocket-Key: ");
    if (key_start == std::string::npos) {
      fail("upgrade request omitted WebSocket key");
      return;
    }
    const auto value_start = key_start + std::strlen("Sec-WebSocket-Key: ");
    const auto value_end = request.find("\r\n", value_start);
    const std::string_view key(request.data() + value_start,
                               value_end - value_start);
    std::array<char, 29> accept{};
    if (!mds::network::websocket_accept_value(key, accept)) {
      fail("failed to compute WebSocket accept");
      return;
    }
    const std::string response =
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
        "Connection: Upgrade\r\nSec-WebSocket-Accept: " +
        std::string(accept.data()) + "\r\n\r\n";
    if (!ssl_write_all(ssl, response, 3)) {
      fail("WebSocket upgrade response write failed");
      return;
    }
    if (!send_frame(ssl, 9, "pi")) {
      fail("WebSocket ping write failed");
      return;
    }
    if (!read_masked_pong(ssl)) {
      fail("WebSocket client pong was absent or invalid");
      return;
    }
    pong_verified_.store(true, std::memory_order_release);

    const unsigned connection =
        websocket_count_.fetch_add(1, std::memory_order_acq_rel) + 1U;
    const unsigned wanted_snapshot =
        profile_ == Profile::Spot && connection > 1 ? 2U : 1U;
    if (!wait_for_snapshots(wanted_snapshot)) {
      fail("timed out waiting for REST snapshot");
      return;
    }
    const std::uint64_t snapshot =
        profile_ == Profile::Spot && connection > 1
            ? 200U
            : (profile_ == Profile::Spot ? 100U : 160U);
    send_bridge_and_live(ssl, snapshot);

    if (recovery_ == Recovery::Disconnect && connection == 1) {
      std::this_thread::sleep_for(250ms);
      return;
    }
    if (recovery_ == Recovery::SequenceGap && connection == 1) {
      std::this_thread::sleep_for(250ms);
      if (!send_frame(ssl, 1, depth_message(163, 163, 162, true)) ||
          !wait_for_snapshots(2)) {
        fail("failed to drive sequence-gap recovery");
        return;
      }
      send_bridge_and_live(ssl, 260);
    }
    while (!stopping_.load(std::memory_order_acquire)) {
      std::this_thread::sleep_for(10ms);
    }
  }

  Profile profile_;
  Recovery recovery_;
  std::string exchange_info_;
  SSL_CTX *context_{};
  int listener_{-1};
  std::uint16_t port_{};
  std::atomic<bool> stopping_{};
  std::atomic<unsigned> snapshot_count_{};
  std::atomic<unsigned> websocket_count_{};
  std::atomic<bool> pong_verified_{};
  std::thread accept_thread_;
  std::vector<std::thread> workers_;
  mutable std::mutex error_mutex_;
  std::string error_;
  std::mutex socket_mutex_;
  std::vector<int> sockets_;
  std::mutex condition_mutex_;
  std::condition_variable condition_;
};

struct WireAudit final : utils::md::wire::RecordVisitor {
  explicit WireAudit(utils::md::ProductType wanted) : wanted_product(wanted) {}

  bool OnInstrument(
      const utils::md::wire::InstrumentUpdateRecord &record) noexcept override {
    instrument = record.instrument.venue == utils::md::Venue::Binance &&
                 record.instrument.product_type == wanted_product &&
                 std::string_view(record.instrument.canonical_symbol.data()) ==
                     "BTCUSDT";
    return instrument;
  }

  bool OnBbo(const utils::md::wire::BboRecord &record) noexcept override {
    if (record.header.state ==
            static_cast<std::uint8_t>(utils::md::BookState::Live) &&
        record.bid_price < record.ask_price && record.bid_quantity > 0 &&
        record.ask_quantity > 0) {
      live_bbo = true;
    }
    return true;
  }

  bool OnTicker(const utils::md::wire::TickerRecord &) noexcept override {
    return true;
  }

  bool OnDelta(const utils::md::wire::DeltaRecord &record) noexcept override {
    if (record.header.state ==
        static_cast<std::uint8_t>(utils::md::BookState::Live)) {
      bid_delta |= record.side ==
                   static_cast<std::uint8_t>(utils::md::Side::Bid);
      ask_delta |= record.side ==
                   static_cast<std::uint8_t>(utils::md::Side::Ask);
    }
    return true;
  }

  bool OnSnapshotBegin(
      const utils::md::wire::SnapshotBeginRecord &record) noexcept override {
    snapshot_generation = record.header.book_generation;
    snapshot_begin = record.item_count != 0 && record.chunk_count_or_checksum != 0;
    return true;
  }

  bool OnSnapshotChunk(
      const utils::md::wire::SnapshotChunkRecord &record) noexcept override {
    snapshot_chunk |= record.level_count != 0 &&
                      record.header.book_generation == snapshot_generation;
    bid_chunk |= record.side ==
                 static_cast<std::uint8_t>(utils::md::Side::Bid);
    ask_chunk |= record.side ==
                 static_cast<std::uint8_t>(utils::md::Side::Ask);
    return true;
  }

  bool OnSnapshotEnd(
      const utils::md::wire::SnapshotEndRecord &record) noexcept override {
    snapshot_end |= record.item_count != 0 &&
                    record.header.book_generation == snapshot_generation;
    return true;
  }

  utils::md::ProductType wanted_product;
  std::uint32_t snapshot_generation{};
  bool instrument{};
  bool live_bbo{};
  bool bid_delta{};
  bool ask_delta{};
  bool snapshot_begin{};
  bool snapshot_chunk{};
  bool snapshot_end{};
  bool bid_chunk{};
  bool ask_chunk{};
};

void drain(mds::transport::SharedRing &ring,
           mds::transport::ReaderHandle &reader, WireAudit &audit) {
  for (;;) {
    auto record = ring.read(reader);
    if (!record) {
      require(record.error == mds::api::ErrorCode::QuotaExceeded,
              "SharedRing consumer read failed");
      return;
    }
    utils::md::wire::RecordHeader header{};
    require(utils::md::wire::ValidateHeader(record.value->payload, &header) ==
                    utils::md::wire::CodecError::Ok &&
                record.value->type == header.message_type &&
                utils::md::wire::Decode(record.value->payload, audit) ==
                    utils::md::wire::CodecError::Ok,
            "strict wire decode failed");
    require(bool(record.value.commit()), "SharedRing commit failed");
  }
}

void run_profile(Profile profile, LoopbackBinanceServer::Recovery recovery) {
  Certificate identity;
  auto tls = client_context(identity);
  LoopbackBinanceServer server(profile, recovery, identity);
  mds::service::SessionManager manager(std::move(tls));
  mds::service::BinanceSessionOptions options;
  options.profile = profile;
  options.symbol = "BTCUSDT";
  options.websocket_endpoint = server.endpoint("wss");
  options.rest_endpoint = server.endpoint("https");
  options.shm_prefix =
      "/mds.loopback." + std::to_string(::getpid()) +
      (profile == Profile::Spot ? ".spot" : ".usdm");
  options.ladder_levels_per_side = 32;
  options.max_ladder_levels_per_side = 32;
  options.connect_timeout = 1s;
  options.request_timeout = 1s;
  options.idle_timeout = 2s;
  options.reconnect_base = 10ms;
  options.reconnect_max = 10ms;
  options.ring.ring_bytes = 64U << 10U;
  options.ring.max_record_bytes = 1024;
  options.ring.max_readers = 4;
  options.ring.unlink_on_close = true;
  auto created = manager.create(std::move(options));
  require(bool(created), created.message.c_str());
  auto *session = created.value;

  mds::transport::RingOptions attach;
  attach.create = false;
  attach.name = std::string(session->ticker_segment());
  auto ticker_opened = mds::transport::SharedRing::open(attach);
  require(bool(ticker_opened), ticker_opened.message.c_str());
  auto ticker = std::move(ticker_opened.value);
  auto ticker_registered = ticker.register_reader(
      mds::transport::process_start_marker(::getpid()), now_ns());
  require(bool(ticker_registered), ticker_registered.message.c_str());
  auto ticker_reader = ticker_registered.value;

  attach.name = std::string(session->orderbook_segment());
  auto book_opened = mds::transport::SharedRing::open(attach);
  require(bool(book_opened), book_opened.message.c_str());
  auto book = std::move(book_opened.value);
  auto book_registered = book.register_reader(
      mds::transport::process_start_marker(::getpid()), now_ns());
  require(bool(book_registered), book_registered.message.c_str());
  auto book_reader = book_registered.value;

  const auto product = profile == Profile::Spot
                           ? utils::md::ProductType::Spot
                           : utils::md::ProductType::Perpetual;
  WireAudit ticker_audit(product);
  WireAudit book_audit(product);
  bool saw_connecting = false;
  bool saw_bridging = false;
  std::uint32_t first_live_generation = 0;
  const auto deadline = std::chrono::steady_clock::now() + 4s;
  while (std::chrono::steady_clock::now() < deadline) {
    (void)manager.run_once(5);
    drain(ticker, ticker_reader, ticker_audit);
    drain(book, book_reader, book_audit);
    const auto state = session->state();
    saw_connecting |=
        state == mds::service::BinanceSessionState::Tcp ||
        state == mds::service::BinanceSessionState::Tls ||
        state == mds::service::BinanceSessionState::Upgrade ||
        state == mds::service::BinanceSessionState::Buffering ||
        state == mds::service::BinanceSessionState::Metadata ||
        state == mds::service::BinanceSessionState::Snapshot;
    saw_bridging |= state == mds::service::BinanceSessionState::Bridging;
    if (state == mds::service::BinanceSessionState::Live &&
        first_live_generation == 0) {
      first_live_generation = session->generation();
    }
    const bool recovered =
        first_live_generation != 0 &&
        session->generation() > first_live_generation &&
        state == mds::service::BinanceSessionState::Live;
    if (recovered && ticker_audit.instrument && ticker_audit.live_bbo &&
        book_audit.instrument && book_audit.live_bbo &&
        book_audit.bid_delta && book_audit.ask_delta &&
        book_audit.snapshot_begin && book_audit.snapshot_chunk &&
        book_audit.snapshot_end && book_audit.bid_chunk &&
        book_audit.ask_chunk &&
        book_audit.snapshot_generation == session->generation() &&
        server.pong_verified()) {
      break;
    }
  }

  if (!server.healthy()) {
    const std::string detail =
        server.error() + "; session=" +
        std::string(mds::service::to_string(session->state())) +
        "; client-error=" + std::string(session->error_message());
    throw std::runtime_error(detail);
  }
  require(saw_connecting && saw_bridging && first_live_generation != 0,
          "session did not traverse connecting, Bridging, and Live");
  require(session->state() == mds::service::BinanceSessionState::Live &&
              session->generation() > first_live_generation,
          "session did not recover Live with a newer generation");
  require(ticker_audit.instrument && ticker_audit.live_bbo,
          "ticker consumer missed Instrument or Live BBO");
  require(book_audit.instrument && book_audit.live_bbo &&
              book_audit.bid_delta && book_audit.ask_delta,
          "order-book consumer missed Instrument, BBO, or sided Delta");
  require(book_audit.snapshot_begin && book_audit.snapshot_chunk &&
              book_audit.snapshot_end && book_audit.bid_chunk &&
              book_audit.ask_chunk &&
              book_audit.snapshot_generation == session->generation(),
          "consumer missed side-aware Snapshot Begin/Chunk/End");
  require(server.pong_verified(), "client pong was absent or unmasked");
  if (profile == Profile::UsdM) {
    require(session->metrics().resyncs >= 1,
            "USD-M pu sequence gap did not trigger resync");
  } else {
    require(session->metrics().reconnects >= 1,
            "Spot disconnect did not trigger reconnect");
  }
  manager.stop();
}

} // namespace

int main() {
  ::signal(SIGPIPE, SIG_IGN);
  try {
    run_profile(Profile::Spot,
                LoopbackBinanceServer::Recovery::Disconnect);
    run_profile(Profile::UsdM,
                LoopbackBinanceServer::Recovery::SequenceGap);
    std::cout << "all Binance loopback integration tests passed\n";
    return 0;
  } catch (const std::exception &exception) {
    std::cerr << "Binance loopback integration test failure: "
              << exception.what() << '\n';
    return 1;
  }
}
