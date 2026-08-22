#include "oms/exchange/live_transport.h"

#include <algorithm>
#include <arpa/inet.h>
#include <array>
#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <openssl/evp.h>
#include <openssl/sha.h>
#include <openssl/ssl.h>
#include <openssl/x509v3.h>
#include <poll.h>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace {

using namespace std::chrono_literals;
using oms::exchange::live::Descriptor;
using oms::exchange::live::Event;
using oms::exchange::live::EventKind;
using oms::exchange::live::Failure;
using oms::exchange::live::LiveTransport;
using oms::exchange::live::SubmitResult;

void Require(bool condition, const char* message) {
  if (!condition) throw std::runtime_error(message);
}

struct Certificate {
  EVP_PKEY* key{};
  X509* certificate{};

  Certificate() {
    EVP_PKEY_CTX* key_context =
        EVP_PKEY_CTX_new_id(EVP_PKEY_RSA, nullptr);
    Require(key_context != nullptr, "key context");
    const bool key_ok =
        EVP_PKEY_keygen_init(key_context) == 1 &&
        EVP_PKEY_CTX_set_rsa_keygen_bits(key_context, 2048) == 1 &&
        EVP_PKEY_keygen(key_context, &key) == 1;
    EVP_PKEY_CTX_free(key_context);
    Require(key_ok, "key generation");

    certificate = X509_new();
    Require(certificate != nullptr, "certificate allocation");
    Require(X509_set_version(certificate, 2) == 1 &&
                ASN1_INTEGER_set(X509_get_serialNumber(certificate), 1) == 1 &&
                X509_gmtime_adj(X509_getm_notBefore(certificate), -60) !=
                    nullptr &&
                X509_gmtime_adj(X509_getm_notAfter(certificate), 3600) !=
                    nullptr &&
                X509_set_pubkey(certificate, key) == 1,
            "certificate setup");
    X509_NAME* name = X509_get_subject_name(certificate);
    constexpr unsigned char common_name[] = "127.0.0.1";
    Require(X509_NAME_add_entry_by_txt(name, "CN", MBSTRING_ASC, common_name,
                                       -1, -1, 0) == 1 &&
                X509_set_issuer_name(certificate, name) == 1,
            "certificate subject");
    X509V3_CTX extension_context{};
    X509V3_set_ctx(&extension_context, certificate, certificate, nullptr,
                   nullptr, 0);
    X509_EXTENSION* san = X509V3_EXT_conf_nid(
        nullptr, &extension_context, NID_subject_alt_name,
        const_cast<char*>("IP:127.0.0.1"));
    Require(san != nullptr && X509_add_ext(certificate, san, -1) == 1,
            "certificate SAN");
    X509_EXTENSION_free(san);
    Require(X509_sign(certificate, key, EVP_sha256()) > 0,
            "certificate signature");
  }

  ~Certificate() {
    X509_free(certificate);
    EVP_PKEY_free(key);
  }
};

net::SharedSslContext ClientContext(const Certificate& identity) {
  SSL_CTX* raw = SSL_CTX_new(TLS_client_method());
  Require(raw != nullptr, "client context");
  net::SharedSslContext context(raw, net::SslCtxDeleter{});
  Require(SSL_CTX_set_min_proto_version(raw, TLS1_2_VERSION) == 1,
          "client TLS version");
  SSL_CTX_set_verify(raw, SSL_VERIFY_PEER, nullptr);
  Require(X509_STORE_add_cert(SSL_CTX_get_cert_store(raw),
                              identity.certificate) == 1,
          "client trust");
  return context;
}

std::size_t ContentLength(std::string_view request) {
  const std::string_view key = "Content-Length: ";
  const std::size_t begin = request.find(key);
  if (begin == std::string_view::npos) return 0;
  const std::size_t value_begin = begin + key.size();
  const std::size_t end = request.find("\r\n", value_begin);
  return static_cast<std::size_t>(
      std::stoul(std::string(request.substr(value_begin, end - value_begin))));
}

std::string Header(std::string_view request, std::string_view name) {
  const std::string key = std::string(name) + ": ";
  const std::size_t begin = request.find(key);
  if (begin == std::string_view::npos) return {};
  const std::size_t value_begin = begin + key.size();
  const std::size_t end = request.find("\r\n", value_begin);
  return std::string(request.substr(value_begin, end - value_begin));
}

std::string WebSocketAccept(std::string_view key) {
  const std::string material =
      std::string(key) + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
  std::array<unsigned char, SHA_DIGEST_LENGTH> digest{};
  SHA1(reinterpret_cast<const unsigned char*>(material.data()), material.size(),
       digest.data());
  std::array<unsigned char, 64> encoded{};
  const int size =
      EVP_EncodeBlock(encoded.data(), digest.data(), digest.size());
  Require(size > 0, "websocket accept encoding");
  return {reinterpret_cast<const char*>(encoded.data()),
          static_cast<std::size_t>(size)};
}

class LoopbackServer {
 public:
  explicit LoopbackServer(const Certificate& identity) {
    context_ = SSL_CTX_new(TLS_server_method());
    Require(context_ != nullptr &&
                SSL_CTX_set_min_proto_version(context_, TLS1_2_VERSION) == 1 &&
                SSL_CTX_use_certificate(context_, identity.certificate) == 1 &&
                SSL_CTX_use_PrivateKey(context_, identity.key) == 1,
            "server context");
    listener_ = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    Require(listener_ >= 0, "server socket");
    int reuse = 1;
    Require(::setsockopt(listener_, SOL_SOCKET, SO_REUSEADDR, &reuse,
                         sizeof(reuse)) == 0,
            "server reuse");
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    address.sin_port = 0;
    Require(::bind(listener_, reinterpret_cast<sockaddr*>(&address),
                   sizeof(address)) == 0 &&
                ::listen(listener_, 8) == 0,
            "server bind");
    socklen_t length = sizeof(address);
    Require(::getsockname(listener_, reinterpret_cast<sockaddr*>(&address),
                          &length) == 0,
            "server port");
    port_ = ntohs(address.sin_port);
    thread_ = std::thread([this] { Run(); });
  }

  ~LoopbackServer() {
    stopping_.store(true, std::memory_order_release);
    ::shutdown(listener_, SHUT_RDWR);
    ::close(listener_);
    if (thread_.joinable()) thread_.join();
    SSL_CTX_free(context_);
  }

  [[nodiscard]] std::string service() const {
    return std::to_string(port_);
  }
  [[nodiscard]] bool saw_put() const noexcept {
    return saw_put_.load(std::memory_order_acquire);
  }
  [[nodiscard]] bool saw_delete() const noexcept {
    return saw_delete_.load(std::memory_order_acquire);
  }

 private:
  static bool WriteFragmented(SSL* ssl, std::string_view value,
                              std::size_t fragment) {
    std::size_t offset = 0;
    while (offset < value.size()) {
      const std::size_t amount = std::min(fragment, value.size() - offset);
      const int written =
          SSL_write(ssl, value.data() + offset, static_cast<int>(amount));
      if (written <= 0) return false;
      offset += static_cast<std::size_t>(written);
      std::this_thread::sleep_for(1ms);
    }
    return true;
  }

  void Serve(int fd) {
    SSL* ssl = SSL_new(context_);
    if (ssl == nullptr) return;
    SSL_set_fd(ssl, fd);
    if (SSL_accept(ssl) != 1) {
      SSL_free(ssl);
      return;
    }
    std::string request;
    std::array<char, 256> buffer{};
    for (;;) {
      const int read = SSL_read(ssl, buffer.data(), buffer.size());
      if (read <= 0) break;
      request.append(buffer.data(), static_cast<std::size_t>(read));
      const std::size_t headers_end = request.find("\r\n\r\n");
      if (headers_end != std::string::npos &&
          request.size() >= headers_end + 4 + ContentLength(request)) {
        break;
      }
    }

    if (request.starts_with("PUT /put ")) {
      saw_put_.store(request.ends_with("payload") &&
                         Header(request, "X-Custom") == "opaque-value",
                     std::memory_order_release);
      (void)WriteFragmented(
          ssl,
          "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
          "3\r\nabc\r\n4\r\ndefg\r\n0\r\n\r\n",
          2);
      (void)SSL_shutdown(ssl);
    } else if (request.starts_with("DELETE /delete ")) {
      saw_delete_.store(Header(request, "X-Delete") == "present",
                        std::memory_order_release);
      (void)WriteFragmented(
          ssl,
          "HTTP/1.1 429 Too Many Requests\r\nContent-Length: 4\r\n"
          "Retry-After: 2\r\nX-MBX-USED-WEIGHT-1M: 9\r\n\r\nslow",
          3);
      (void)SSL_shutdown(ssl);
    } else if (request.starts_with("GET /server ")) {
      (void)WriteFragmented(
          ssl,
          "HTTP/1.1 503 Unavailable\r\nContent-Length: 3\r\n\r\nbad", 1);
      (void)SSL_shutdown(ssl);
    } else if (request.starts_with("GET /oversize ")) {
      (void)WriteFragmented(
          ssl, "HTTP/1.1 200 OK\r\nContent-Length: 64\r\n\r\n" +
                   std::string(64, 'x'),
          5);
      (void)SSL_shutdown(ssl);
    } else if (request.starts_with("GET /partial ")) {
      (void)WriteFragmented(
          ssl, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\npartial", 1);
      (void)SSL_shutdown(ssl);
    } else if (request.starts_with("GET /disconnect ")) {
      (void)SSL_shutdown(ssl);
    } else if (request.starts_with("GET /ws ")) {
      const std::string accept =
          WebSocketAccept(Header(request, "Sec-WebSocket-Key"));
      (void)WriteFragmented(
          ssl,
          "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
          "Connection: Upgrade\r\nSec-WebSocket-Accept: " +
              accept + "\r\n\r\n",
          2);
      std::this_thread::sleep_for(10ms);
      // Abrupt TLS/socket disconnect exercises session-loss reporting.
    }
    SSL_free(ssl);
  }

  void Run() {
    while (!stopping_.load(std::memory_order_acquire)) {
      const int fd = ::accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (fd < 0) break;
      Serve(fd);
      ::shutdown(fd, SHUT_RDWR);
      ::close(fd);
    }
  }

  SSL_CTX* context_{};
  int listener_{-1};
  std::uint16_t port_{};
  std::atomic<bool> stopping_{false};
  std::atomic<bool> saw_put_{false};
  std::atomic<bool> saw_delete_{false};
  std::thread thread_;
};

std::uint32_t EpollEvents(short events) {
  std::uint32_t result = 0;
  if ((events & POLLIN) != 0) result |= EPOLLIN;
  if ((events & POLLOUT) != 0) result |= EPOLLOUT;
  if ((events & POLLERR) != 0) result |= EPOLLERR;
  if ((events & POLLHUP) != 0) result |= EPOLLHUP;
  return result;
}

Event Await(LiveTransport& transport) {
  const auto deadline = LiveTransport::Clock::now() + 5s;
  for (;;) {
    Event event{};
    if (transport.poll(event)) return event;
    Require(LiveTransport::Clock::now() < deadline, "transport event timeout");
    std::array<Descriptor, 8> descriptors{};
    const std::size_t count = transport.descriptors(descriptors);
    std::array<pollfd, 8> polled{};
    for (std::size_t index = 0; index < count; ++index) {
      polled[index].fd = descriptors[index].fd;
      if ((descriptors[index].events & EPOLLIN) != 0)
        polled[index].events |= POLLIN;
      if ((descriptors[index].events & EPOLLOUT) != 0)
        polled[index].events |= POLLOUT;
    }
    if (count != 0) {
      const int ready = ::poll(polled.data(), count, 20);
      Require(ready >= 0, "poll failed");
      for (std::size_t index = 0; index < count; ++index) {
        if (polled[index].revents != 0) {
          transport.service(descriptors[index].fd,
                            descriptors[index].generation,
                            EpollEvents(polled[index].revents));
        }
      }
    } else {
      std::this_thread::sleep_for(1ms);
    }
    transport.check_timeouts();
  }
}

Event RequestAndAwait(LiveTransport& transport, std::uint64_t id,
                      net::HttpMethod method, std::string_view target,
                      std::string_view content_type = {},
                      std::span<const std::byte> body = {},
                      std::span<const net::HttpHeader> headers = {}) {
  Require(transport.submit({id, method, target, content_type, body, headers,
                            LiveTransport::Clock::now() + 2s}) ==
              SubmitResult::Accepted,
          "request submission");
  return Await(transport);
}

}  // namespace

int main() {
  Certificate identity;
  LoopbackServer server(identity);
  oms::exchange::live::Config config{};
  config.rest = {"127.0.0.1", server.service()};
  config.websocket = config.rest;
  config.request_slots = 1;
  config.event_slots = 4;
  config.websocket_outbound_slots = 1;
  config.response_capacity = 16;
  config.websocket_message_capacity = 128;
  config.request_capacity = 1024;
  config.tls_receive_capacity = 1024;
  LiveTransport transport(ClientContext(identity), config);

  const std::string body_text = "payload";
  const auto body = std::span<const std::byte>(
      reinterpret_cast<const std::byte*>(body_text.data()), body_text.size());
  const std::array<net::HttpHeader, 1> put_headers{{
      {"X-Custom", "opaque-value"},
  }};
  Event event = RequestAndAwait(transport, 1, net::HttpMethod::Put, "/put",
                                "application/json", body, put_headers);
  Require(event.kind == EventKind::HttpResponse && event.status_code == 200 &&
              event.payload == "abcdefg" && server.saw_put(),
          "PUT/chunked/custom-header response");

  const std::array<net::HttpHeader, 1> delete_headers{{
      {"X-Delete", "present"},
  }};
  event = RequestAndAwait(transport, 2, net::HttpMethod::Delete, "/delete", {},
                          {}, delete_headers);
  Require(event.kind == EventKind::HttpResponse && event.status_code == 429 &&
              event.status_class == net::HttpStatusClass::RateLimited &&
              event.has_retry_after && event.retry_after_ms == 2000 &&
              event.has_used_weight && event.used_weight_1m == 9 &&
              server.saw_delete(),
          "DELETE/429 response");

  event =
      RequestAndAwait(transport, 3, net::HttpMethod::Get, "/server");
  Require(event.kind == EventKind::HttpResponse && event.status_code == 503 &&
              event.status_class == net::HttpStatusClass::ServerError,
          "5xx response");

  event =
      RequestAndAwait(transport, 4, net::HttpMethod::Get, "/oversize");
  Require(event.kind == EventKind::HttpFailure &&
              event.failure == Failure::ResponseTooLarge,
          "oversize response");

  Require(transport.submit({5, net::HttpMethod::Get, "/partial", {}, {}, {},
                            LiveTransport::Clock::now() + 2s}) ==
              SubmitResult::Accepted,
          "partial request");
  Require(transport.submit({6, net::HttpMethod::Get, "/server", {}, {}, {},
                            LiveTransport::Clock::now() + 2s}) ==
              SubmitResult::WouldBlock,
          "fixed request capacity");
  event = Await(transport);
  Require(event.kind == EventKind::HttpResponse &&
              event.payload == "partial",
          "partial reads");

  event =
      RequestAndAwait(transport, 7, net::HttpMethod::Get, "/disconnect");
  Require(event.kind == EventKind::HttpFailure &&
              (event.failure == Failure::InvalidResponse ||
               event.failure == Failure::Transport),
          "REST disconnect");

  Require(transport.start_websocket("/ws",
                                    LiveTransport::Clock::now() + 2s),
          "websocket start");
  std::array<Descriptor, 8> first_descriptors{};
  const std::size_t first_count = transport.descriptors(first_descriptors);
  Require(first_count != 0, "first websocket descriptor");
  const Descriptor first_websocket = first_descriptors[first_count - 1];
  event = Await(transport);
  Require(event.kind == EventKind::WebSocketOpen, "websocket open");
  event = Await(transport);
  Require(event.kind == EventKind::WebSocketClosed,
          "websocket disconnect");

  Require(transport.start_websocket("/ws",
                                    LiveTransport::Clock::now() + 2s),
          "websocket reconnect");
  std::array<Descriptor, 8> second_descriptors{};
  const std::size_t second_count = transport.descriptors(second_descriptors);
  Require(second_count != 0, "second websocket descriptor");
  const Descriptor second_websocket = second_descriptors[second_count - 1];
  Require(second_websocket.generation != first_websocket.generation,
          "websocket generation advances");
  // A readiness event retained by epoll from the prior socket must not touch
  // the replacement connection, even when the kernel reuses its fd.
  transport.service(first_websocket.fd, first_websocket.generation, EPOLLIN);
  event = Await(transport);
  Require(event.kind == EventKind::WebSocketOpen,
          "stale generation ignored during reconnect");
  event = Await(transport);
  Require(event.kind == EventKind::WebSocketClosed,
          "reconnected websocket disconnect");
}
