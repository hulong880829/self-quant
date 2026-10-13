#include "fairprice/bootstrap.h"
#include "fairprice/catalog.h"
#include "fairprice/log.h"
#include "fairprice/runtime.h"
#include "fairprice/sqab.h"
#include "mds/consume/segment_reader.h"
#include "mds/publish/wire_publisher.h"
#include "net/tls_websocket.h"
#include "net/websocket_codec.h"
#include "utils/md/wire_codec.h"

#include <array>
#include <arpa/inet.h>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <fcntl.h>
#include <filesystem>
#include <fstream>
#include <memory>
#include <mutex>
#include <netinet/in.h>
#include <poll.h>
#include <stdexcept>
#include <string>
#include <string_view>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

void unlink_posix_shm(const std::string &name) {
  if (name.empty()) {
    return;
  }
  const auto shm_name = name.front() == '/' ? name : "/" + name;
  (void)::shm_unlink(shm_name.c_str());
}

std::uint16_t unique_test_port() {
  static std::atomic<int> next{0};
  const int port = 20000 + static_cast<int>(
                               (::getpid() * 17ULL +
                                static_cast<unsigned>(next.fetch_add(1)) * 31ULL) %
                               40000ULL);
  require(port > 1024 && port < 65535, "test port overflow");
  return static_cast<std::uint16_t>(port);
}

mds::transport::SharedRing open_input_ring(const std::string &name) {
  unlink_posix_shm(name);
  mds::transport::RingOptions ring;
  ring.create = true;
  ring.unlink_on_close = true;
  ring.ring_bytes = 1U << 20U;
  ring.max_record_bytes = 65536;
  ring.max_readers = 8;
  ring.name = name;
  auto opened = mds::transport::SharedRing::open(ring);
  require(bool(opened), opened.message.c_str());
  return std::move(opened.value);
}

mds::consume::SegmentReader open_segment_reader(const std::string &name) {
  mds::api::Result<mds::consume::SegmentReader> opened;
  for (int attempt = 0; attempt < 100; ++attempt) {
    opened = mds::consume::SegmentReader::open(name);
    if (opened) {
      return std::move(opened.value);
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }
  require(false, opened.message.c_str());
  return std::move(opened.value);
}

std::string market_data_yaml() {
  return "market_data:\n"
         "  sample_interval_ms: 200\n"
         "  depth: 50\n"
         "  snapshot_stale_after_ms: 15000\n"
         "  catalog_refresh_interval_ms: 200\n"
         "  clickhouse:\n"
         "    host: 127.0.0.1\n"
         "    service: \"8443\"\n"
         "    database: market_data\n"
         "    user: bbo_writer\n"
         "    password: \"test-clickhouse-password\"\n"
         "  gateway_bootstrap:\n"
         "    url: \"ws://127.0.0.1:9443/v1/market-data\"\n"
         "    token: \"test-gateway-token\"\n"
         "    timeout_ms: 50\n";
}

fairprice::MarketDataQuery query_catalog_segments(
    const std::vector<std::string> &segments) {
  std::string body;
  for (const auto &seg : segments) {
    std::string profile;
    std::string symbol;
    std::string error;
    fairprice::Kind kind = fairprice::Kind::Unknown;
    require(fairprice::parse_segment(seg, profile, symbol, kind, error),
            error.c_str());
    const char *kind_text =
        kind == fairprice::Kind::Bbo ? "aggbbo" : "aggorderbook";
    body += "{\"profile\":\"" + profile + "\",\"symbol\":\"" + symbol +
            "\",\"kind\":\"" + kind_text + "\",\"segment\":\"" + seg +
            "\",\"active\":1,\"schema_major\":2,\"schema_minor\":0,"
            "\"updated_at_ns\":1}\n";
  }
  return [body](std::string_view sql) {
    fairprice::MarketDataQueryResult result;
    if (std::string(sql).find("aggregate_topic_catalog") != std::string::npos) {
      result.body = body;
    }
    return result;
  };
}

fairprice::GatewayFrameSource no_gateway() {
  return [](std::string_view, std::uint64_t) {
    return std::vector<fairprice::GatewayWireFrame>{};
  };
}

int connect_loopback(std::uint16_t port, int timeout_ms) {
  const auto fd = ::socket(AF_INET, SOCK_STREAM, 0);
  require(fd >= 0, "tcp socket failed");
  const int flags = ::fcntl(fd, F_GETFL, 0);
  require(flags >= 0 && ::fcntl(fd, F_SETFL, flags | O_NONBLOCK) == 0,
          "fcntl nonblock failed");
  timeval tv{};
  tv.tv_sec = 1;
  tv.tv_usec = 0;
  (void)::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  (void)::setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
  sockaddr_in address{};
  address.sin_family = AF_INET;
  address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  address.sin_port = htons(port);
  const int cr =
      ::connect(fd, reinterpret_cast<sockaddr *>(&address), sizeof(address));
  if (cr == 0) {
    return fd;
  }
  if (errno != EINPROGRESS) {
    const int err = errno;
    ::close(fd);
    throw std::runtime_error(std::string("tcp connect failed: ") +
                             std::strerror(err));
  }
  const auto deadline = std::chrono::steady_clock::now() +
                        std::chrono::milliseconds(timeout_ms);
  while (std::chrono::steady_clock::now() < deadline) {
    int so_error = 0;
    socklen_t len = sizeof(so_error);
    if (::getsockopt(fd, SOL_SOCKET, SO_ERROR, &so_error, &len) != 0) {
      const int err = errno;
      ::close(fd);
      throw std::runtime_error(std::string("tcp connect failed: ") +
                               std::strerror(err));
    }
    if (so_error == 0) {
      pollfd item{fd, POLLOUT, 0};
      if (::poll(&item, 1, 0) > 0 && (item.revents & POLLOUT) != 0) {
        return fd;
      }
    } else if (so_error != EINPROGRESS && so_error != EALREADY) {
      ::close(fd);
      throw std::runtime_error(std::string("tcp connect failed: ") +
                               std::strerror(so_error));
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(5));
  }
  ::close(fd);
  throw std::runtime_error("tcp connect timed out");
}

void set_blocking(int fd) {
  const int flags = ::fcntl(fd, F_GETFL, 0);
  require(flags >= 0, "fcntl getfl failed");
  require(::fcntl(fd, F_SETFL, flags & ~O_NONBLOCK) == 0, "fcntl setfl failed");
}

std::string http_get(std::uint16_t port, const std::string &path) {
  const auto fd = connect_loopback(port, 200);
  const auto request = "GET " + path +
                       " HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n";
  const auto sent =
      ::send(fd, request.data(), request.size(), MSG_NOSIGNAL | MSG_DONTWAIT);
  if (sent != static_cast<ssize_t>(request.size())) {
    ::close(fd);
    throw std::runtime_error("http send failed");
  }
  std::string response;
  std::array<char, 4096> buffer{};
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(500);
  while (std::chrono::steady_clock::now() < deadline) {
    const auto n =
        ::recv(fd, buffer.data(), buffer.size(), MSG_DONTWAIT);
    if (n > 0) {
      response.append(buffer.data(), static_cast<std::size_t>(n));
      const auto header_end = response.find("\r\n\r\n");
      if (header_end == std::string::npos) {
        continue;
      }
      const auto length_at = response.find("Content-Length:");
      if (length_at == std::string::npos || length_at > header_end) {
        break;
      }
      const auto body = response.substr(header_end + 4);
      const auto length = static_cast<std::size_t>(
          std::strtoul(response.c_str() + length_at + 15, nullptr, 10));
      if (body.size() >= length) {
        break;
      }
      continue;
    }
    if (n == 0) {
      break;
    }
    if (errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR) {
      break;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(5));
  }
  ::close(fd);
  return response;
}

utils::md::EventHeader event_header(std::uint64_t seq, std::uint32_t generation);

struct LogCapture {
  std::vector<std::string> lines;
  LogCapture() { fairprice::set_log_capture_for_test(&lines); }
  ~LogCapture() { fairprice::set_log_capture_for_test(nullptr); }
  [[nodiscard]] int count(std::string_view needle) const {
    int found = 0;
    for (const auto &line : lines) {
      if (line.find(needle) != std::string::npos) {
        ++found;
      }
    }
    return found;
  }
};

std::string market_data_yaml_stale(unsigned stale_ms) {
  auto yaml = market_data_yaml();
  const std::string from = "snapshot_stale_after_ms: 15000\n";
  const auto pos = yaml.find(from);
  require(pos != std::string::npos, "stale yaml marker missing");
  yaml.replace(pos, from.size(),
               "snapshot_stale_after_ms: " + std::to_string(stale_ms) + "\n");
  return yaml;
}

utils::md::wire::AggBboRecord sample_bbo() {
  utils::md::wire::AggBboRecord bbo{};
  bbo.member_count = 1;
  bbo.member_mask = 1;
  bbo.live_mask = 1;
  bbo.venue_slot_ids[0] = 1;
  bbo.gated_bid.price = 99;
  bbo.gated_bid.quantity = 10;
  bbo.gated_bid.venue_quantity[0] = 10;
  bbo.gated_bid.venue_mask = 1;
  bbo.gated_bid.contributor_count = 1;
  bbo.gated_ask.price = 101;
  bbo.gated_ask.quantity = 10;
  bbo.gated_ask.venue_quantity[0] = 10;
  bbo.gated_ask.venue_mask = 1;
  bbo.gated_ask.contributor_count = 1;
  bbo.raw_bid.price = 99;
  bbo.raw_bid.quantity = 10;
  bbo.raw_bid.venue_mask = 1;
  bbo.raw_ask.price = 101;
  bbo.raw_ask.quantity = 10;
  bbo.raw_ask.venue_mask = 1;
  std::memcpy(bbo.base_asset.data(), "BTC", 3);
  std::memcpy(bbo.quote_asset.data(), "USDT", 4);
  return bbo;
}

utils::md::wire::AggOrderBookRecord sample_book() {
  utils::md::wire::AggOrderBookRecord book{};
  book.member_count = 1;
  book.member_mask = 1;
  book.active_mask = 1;
  book.venue_slot_ids[0] = 1;
  book.bid_count = 2;
  book.ask_count = 2;
  book.bids[0].price = 99;
  book.bids[0].quantity = 10;
  book.bids[0].venue_quantity[0] = 10;
  book.bids[0].venue_mask = 1;
  book.bids[0].contributor_count = 1;
  book.bids[1].price = 98;
  book.bids[1].quantity = 10;
  book.bids[1].venue_quantity[0] = 10;
  book.bids[1].venue_mask = 1;
  book.bids[1].contributor_count = 1;
  book.asks[0].price = 101;
  book.asks[0].quantity = 10;
  book.asks[0].venue_quantity[0] = 10;
  book.asks[0].venue_mask = 1;
  book.asks[0].contributor_count = 1;
  book.asks[1].price = 102;
  book.asks[1].quantity = 10;
  book.asks[1].venue_quantity[0] = 10;
  book.asks[1].venue_mask = 1;
  book.asks[1].contributor_count = 1;
  std::memcpy(book.base_asset.data(), "BTC", 3);
  std::memcpy(book.quote_asset.data(), "USDT", 4);
  return book;
}

struct InputPublishers {
  std::unique_ptr<mds::publish::WirePublisher> bbo;
  std::unique_ptr<mds::publish::WirePublisher> book;
};

InputPublishers open_publishers(const std::string &bbo_name,
                                const std::string &book_name) {
  InputPublishers pubs;
  pubs.bbo = std::make_unique<mds::publish::WirePublisher>(open_input_ring(bbo_name));
  pubs.book =
      std::make_unique<mds::publish::WirePublisher>(open_input_ring(book_name));
  require(bool(pubs.book->prepare_aggregate_orderbook()),
          "prepare aggregate orderbook failed");
  return pubs;
}

void publish_pair(InputPublishers &pubs, std::uint64_t seq, std::uint32_t generation) {
  require(bool(pubs.bbo->publish_agg_bbo(event_header(seq, generation), sample_bbo())),
          "publish bbo failed");
  require(bool(pubs.book->publish_agg_orderbook(event_header(seq, generation), sample_book())),
          "publish book failed");
}

bool wait_readyz(std::uint16_t port, bool want_ready, int attempts) {
  for (int i = 0; i < attempts; ++i) {
    try {
      const auto body = http_get(port, "/readyz");
      const bool ready = body.find("HTTP/1.1 200") != std::string::npos &&
                         body.find("\"status\":\"ready\"") != std::string::npos;
      const bool not_ready = body.find("HTTP/1.1 503") != std::string::npos;
      if (want_ready ? ready : not_ready) {
        return true;
      }
    } catch (...) {
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  return false;
}

utils::md::EventHeader event_header(std::uint64_t seq, std::uint32_t generation) {
  utils::md::EventHeader header{};
  header.bus_seq = seq;
  header.source_seq = seq;
  header.book_generation = generation;
  header.state = utils::md::BookState::Live;
  return header;
}

}  // namespace

void test_history_isolation();
void test_bootstrap_stop_flow_and_first_frame();
void test_bootstrap_live_publish_discards_history();
void test_silent_reader_reopen();

void run_fairprice_service_tests() {
  const auto pid = std::to_string(::getpid());
  const auto dir = std::filesystem::temp_directory_path() / ("fairprice-svc-" + pid);
  std::filesystem::create_directories(dir);
  const auto input_prefix = "/selfquant.fp.in" + pid;
  const auto prefix = "/selfquant.fp.out" + pid;
  const auto bbo_name = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggbbo");
  const auto book_name = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggorderbook");
  const auto fair_name = mds::publish::make_publisher_segment_name(
      prefix, "agg", "BTCUSDT", "fairprice");
  require(!bbo_name.empty() && !book_name.empty() && !fair_name.empty(),
          "segment names failed");

  auto bbo_ring = open_input_ring(bbo_name);
  auto book_ring = open_input_ring(book_name);
  unlink_posix_shm(fair_name);
  mds::publish::WirePublisher bbo_publisher(std::move(bbo_ring));
  mds::publish::WirePublisher book_publisher(std::move(book_ring));
  require(bool(book_publisher.prepare_aggregate_orderbook()),
          "prepare aggregate orderbook failed");

  const auto port = unique_test_port();
  std::ofstream yaml(dir / "fairprice.yaml");
  yaml << "version: 2\n"
       << "runtime:\n  log_level: info\n  development: true\n"
       << market_data_yaml()
       << "shared_memory:\n  backend: posix_shm\n  output:\n    prefix: " << prefix
       << "\n    ring_bytes: 1048576\n    max_record_bytes: 65536\n    max_readers: 8\n"
       << "    reader_lease_timeout_ms: 5000\n    unlink_on_shutdown: true\n"
       << "server:\n  listen_address: 127.0.0.1\n  port: " << port << "\n"
       << "  allowed_origins:\n    - http://localhost:3000\n"
       << "  browser_token: \"\"\n  max_clients: 8\n"
       << "  max_subscriptions_per_client: 4\n  read_header_timeout_ms: 5000\n"
       << "  idle_timeout_ms: 60000\n  max_header_bytes: 16384\n"
       << "  websocket:\n    handshake_timeout_ms: 5000\n    read_limit_bytes: 4096\n"
       << "    write_buffer_bytes: 65536\n    pong_timeout_ms: 45000\n"
       << "stream:\n  interval_ms: 20\n"
       << "history:\n  workers: 1\n  resolution_ms: 60000\n"
       << "fair_price:\n  enabled: true\n  depth: 2\n  lambda_per_bp: 0.0\n"
       << "  imbalance_alpha: 1.0\n  ewma_tau_ms: 500\n  impact_notional: 0.0\n"
       << "  venue_dominance_ratio: 0.9\n  stream_interval_ms: 5\n  heartbeat_ms: 1000\n"
       << "  persistence:\n    enabled: false\n    database_url: \"\"\n"
       << "    sample_interval_ms: 1000\n    retention_hours: 24\n"
       << "    cleanup_interval_ms: 300000\n    delete_batch: 10000\n"
       << "    delete_max_batches: 20\n    operation_timeout_ms: 5000\n";
  yaml.close();

  auto loaded = fairprice::load_config((dir / "fairprice.yaml").string());
  require(bool(loaded), loaded.message.c_str());
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());
  fairprice::Runtime runtime(std::move(loaded.value),
                             query_catalog_segments({bbo_name, book_name}),
                             no_gateway());
  auto started = runtime.start();
  require(bool(started), started.message.c_str());

  std::string health;
  std::string readyz;
  for (int i = 0; i < 50; ++i) {
    try {
      health = http_get(port, "/healthz");
      readyz = http_get(port, "/readyz");
      if (health.find("\"status\":\"ok\"") != std::string::npos) {
        break;
      }
    } catch (...) {
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  require(health.find("\"status\":\"ok\"") != std::string::npos,
          "healthz did not return ok");
  require(readyz.find("HTTP/1.1 503") != std::string::npos &&
              readyz.find("not_ready") != std::string::npos,
          "readyz was ready before first frames");

  utils::md::wire::AggBboRecord bbo{};
  bbo.member_count = 1;
  bbo.member_mask = 1;
  bbo.live_mask = 1;
  bbo.venue_slot_ids[0] = 1;
  bbo.gated_bid.price = 99;
  bbo.gated_bid.quantity = 10;
  bbo.gated_bid.venue_quantity[0] = 10;
  bbo.gated_bid.venue_mask = 1;
  bbo.gated_bid.contributor_count = 1;
  bbo.gated_ask.price = 101;
  bbo.gated_ask.quantity = 10;
  bbo.gated_ask.venue_quantity[0] = 10;
  bbo.gated_ask.venue_mask = 1;
  bbo.gated_ask.contributor_count = 1;
  bbo.raw_bid.price = 99;
  bbo.raw_bid.quantity = 10;
  bbo.raw_bid.venue_mask = 1;
  bbo.raw_ask.price = 101;
  bbo.raw_ask.quantity = 10;
  bbo.raw_ask.venue_mask = 1;
  std::memcpy(bbo.base_asset.data(), "BTC", 3);
  std::memcpy(bbo.quote_asset.data(), "USDT", 4);
  require(bool(bbo_publisher.publish_agg_bbo(event_header(1, 1), bbo)),
          "publish bbo failed");

  utils::md::wire::AggOrderBookRecord book{};
  book.member_count = 1;
  book.member_mask = 1;
  book.active_mask = 1;
  book.venue_slot_ids[0] = 1;
  book.bid_count = 2;
  book.ask_count = 2;
  book.bids[0].price = 99;
  book.bids[0].quantity = 10;
  book.bids[0].venue_quantity[0] = 10;
  book.bids[0].venue_mask = 1;
  book.bids[0].contributor_count = 1;
  book.bids[1].price = 98;
  book.bids[1].quantity = 10;
  book.bids[1].venue_quantity[0] = 10;
  book.bids[1].venue_mask = 1;
  book.bids[1].contributor_count = 1;
  book.asks[0].price = 101;
  book.asks[0].quantity = 10;
  book.asks[0].venue_quantity[0] = 10;
  book.asks[0].venue_mask = 1;
  book.asks[0].contributor_count = 1;
  book.asks[1].price = 102;
  book.asks[1].quantity = 10;
  book.asks[1].venue_quantity[0] = 10;
  book.asks[1].venue_mask = 1;
  book.asks[1].contributor_count = 1;
  std::memcpy(book.base_asset.data(), "BTC", 3);
  std::memcpy(book.quote_asset.data(), "USDT", 4);
  require(bool(book_publisher.publish_agg_orderbook(event_header(1, 1), book)),
          "publish book failed");

  std::string snapshot;
  for (int i = 0; i < 100; ++i) {
    try {
      snapshot = http_get(port, "/v1/markets/BTCUSDT/snapshot?profile=agg&depth=2");
      readyz = http_get(port, "/readyz");
      if (snapshot.find("orderbook") != std::string::npos &&
          snapshot.find("raw_spread_bps") != std::string::npos &&
          readyz.find("\"status\":\"ready\"") != std::string::npos) {
        break;
      }
    } catch (...) {
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  require(readyz.find("HTTP/1.1 200") != std::string::npos &&
              readyz.find("\"status\":\"ready\"") != std::string::npos,
          "readyz did not become ready after first frames");
  const auto markets = http_get(port, "/v1/markets");
  require(markets.find("BTCUSDT") != std::string::npos &&
              markets.find("\"has_order_book\":true") != std::string::npos &&
              markets.find("\"has_bbo\":true") != std::string::npos &&
              markets.find("\"base\":\"BTC\"") != std::string::npos &&
              markets.find("\"quote\":\"USDT\"") != std::string::npos &&
              markets.find("\"live\":true") != std::string::npos,
          "markets response missing Go identity fields");
  require(snapshot.find("HTTP/1.1 200") != std::string::npos &&
              snapshot.find("orderbook") != std::string::npos &&
              snapshot.find("raw_spread_bps") != std::string::npos &&
              snapshot.find("gated_spread_bps") != std::string::npos &&
              snapshot.find("raw_cross_bps") != std::string::npos,
          "orderbook snapshot was not served from AggOrderBook SHM");
  const auto bad_depth =
      http_get(port, "/v1/markets/BTCUSDT/snapshot?profile=agg&depth=51");
  require(bad_depth.find("HTTP/1.1 400") != std::string::npos &&
              bad_depth.find("depth must be within 1..50") != std::string::npos,
          "invalid HTTP depth was not rejected");
  const auto depth20 =
      http_get(port, "/v1/markets/BTCUSDT/snapshot?profile=agg&depth=20");
  const auto depth50 =
      http_get(port, "/v1/markets/BTCUSDT/snapshot?profile=agg&depth=50");
  require(depth20.find("HTTP/1.1 200") != std::string::npos &&
              depth50.find("HTTP/1.1 200") != std::string::npos,
          "HTTP depth 20/50 snapshots failed");
  const auto history = http_get(
      port, "/v1/markets/BTCUSDT/spread-history?profile=agg&type=gated&range=24h");
  require(history.find("HTTP/1.1 200") != std::string::npos &&
              history.find("\"buckets\"") != std::string::npos &&
              history.find("\"distribution_bps\"") != std::string::npos,
          "spread history json missing Go fields");
  const auto fair_history = http_get(
      port,
      "/v1/markets/BTCUSDT/fair-price-history?profile=agg&start=2020-01-01T00:00:00Z&end=2020-01-01T01:00:00Z");
  require(fair_history.find("fair price history is unavailable") !=
              std::string::npos,
          "disabled fair-price-history error mismatch");
  const auto fair_history_400 = http_get(
      port, "/v1/markets/BTCUSDT/fair-price-history?profile=agg&start=nope&end=2020-01-01T01:00:00Z");
  require(fair_history_400.find("HTTP/1.1 400") != std::string::npos,
          "invalid fair-price-history must stay synchronous 400");

  auto reader = open_segment_reader(fair_name);
  book.bids[0].quantity = 11;
  book.bids[0].venue_quantity[0] = 11;
  require(bool(book_publisher.publish_agg_orderbook(event_header(2, 2), book)),
          "publish follow-up book failed");

  bool saw_ready = false;
  for (int i = 0; i < 50; ++i) {
    auto viewed = reader.try_read_view();
    if (viewed && viewed.value) {
      utils::md::wire::FairPriceRecordV2 decoded{};
      if (utils::md::wire::DecodeFairPriceV2(viewed.value->payload(), decoded) ==
              utils::md::wire::CodecError::Ok &&
          decoded.ready) {
        saw_ready = true;
        (void)viewed.value->commit();
        break;
      }
      (void)viewed.value->commit();
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  runtime.stop();
  require(saw_ready, "fairprice shm did not publish a ready snapshot");
  test_silent_reader_reopen();
  test_history_isolation();
  test_bootstrap_stop_flow_and_first_frame();
  test_bootstrap_live_publish_discards_history();
}

std::unique_ptr<fairprice::Runtime> start_silent_runtime(
    const std::filesystem::path &dir, std::uint16_t port, const std::string &prefix,
    const std::string &bbo_name, const std::string &book_name) {
  std::ofstream yaml(dir / "fairprice.yaml");
  yaml << "version: 2\n"
       << "runtime:\n  log_level: info\n  development: true\n"
       << market_data_yaml_stale(200)
       << "shared_memory:\n  backend: posix_shm\n  output:\n    prefix: " << prefix
       << "\n    ring_bytes: 1048576\n    max_record_bytes: 65536\n    max_readers: 8\n"
       << "    reader_lease_timeout_ms: 5000\n    unlink_on_shutdown: true\n"
       << "server:\n  listen_address: 127.0.0.1\n  port: " << port << "\n"
       << "  allowed_origins:\n    - http://localhost:3000\n"
       << "  browser_token: \"\"\n  max_clients: 8\n"
       << "  max_subscriptions_per_client: 4\n  read_header_timeout_ms: 5000\n"
       << "  idle_timeout_ms: 60000\n  max_header_bytes: 16384\n"
       << "  websocket:\n    handshake_timeout_ms: 5000\n    read_limit_bytes: 4096\n"
       << "    write_buffer_bytes: 65536\n    pong_timeout_ms: 45000\n"
       << "stream:\n  interval_ms: 20\n"
       << "history:\n  workers: 1\n  resolution_ms: 60000\n"
       << "fair_price:\n  enabled: true\n  depth: 2\n  lambda_per_bp: 0.0\n"
       << "  imbalance_alpha: 1.0\n  ewma_tau_ms: 500\n  impact_notional: 0.0\n"
       << "  venue_dominance_ratio: 0.9\n  stream_interval_ms: 5\n  heartbeat_ms: 1000\n"
       << "  persistence:\n    enabled: false\n    database_url: \"\"\n"
       << "    sample_interval_ms: 1000\n    retention_hours: 24\n"
       << "    cleanup_interval_ms: 300000\n    delete_batch: 10000\n"
       << "    delete_max_batches: 20\n    operation_timeout_ms: 5000\n";
  yaml.close();
  auto loaded = fairprice::load_config((dir / "fairprice.yaml").string());
  require(bool(loaded), loaded.message.c_str());
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());
  auto runtime = std::make_unique<fairprice::Runtime>(
      std::move(loaded.value), query_catalog_segments({bbo_name, book_name}),
      no_gateway());
  auto started = runtime->start();
  require(bool(started), started.message.c_str());
  return runtime;
}

void test_silent_reader_reopen() {
  LogCapture logs;
  const auto pid = std::to_string(::getpid());
  const auto dir =
      std::filesystem::temp_directory_path() / ("fairprice-silent-" + pid);
  std::filesystem::create_directories(dir);
  const unsigned stale_ms = 200;

  {
    const auto input_prefix = "/selfquant.fp.live" + pid;
    const auto prefix = "/selfquant.fp.liveout" + pid;
    const auto bbo_name = mds::publish::make_publisher_segment_name(
        input_prefix, "agg", "BTCUSDT", "aggbbo");
    const auto book_name = mds::publish::make_publisher_segment_name(
        input_prefix, "agg", "BTCUSDT", "aggorderbook");
    auto pubs = open_publishers(bbo_name, book_name);
    const auto port = unique_test_port();
    auto runtime = start_silent_runtime(dir, port, prefix, bbo_name, book_name);
    require(wait_readyz(port, false, 50), "continuous case was ready before frames");
    const auto started = std::chrono::steady_clock::now();
    std::uint64_t seq = 1;
    while (std::chrono::steady_clock::now() - started < std::chrono::milliseconds(700)) {
      publish_pair(pubs, seq, 3);
      ++seq;
      std::this_thread::sleep_for(std::chrono::milliseconds(40));
    }
    require(wait_readyz(port, true, 50), "continuous writes did not stay ready");
    runtime->stop();
    require(logs.count("silent_reader_reopen") == 0,
            "continuous writes triggered silent reopen");
  }

  logs.lines.clear();
  {
    const auto input_prefix = "/selfquant.fp.cold" + pid;
    const auto prefix = "/selfquant.fp.coldout" + pid;
    const auto bbo_name = mds::publish::make_publisher_segment_name(
        input_prefix, "agg", "BTCUSDT", "aggbbo");
    const auto book_name = mds::publish::make_publisher_segment_name(
        input_prefix, "agg", "BTCUSDT", "aggorderbook");
    auto pubs = open_publishers(bbo_name, book_name);
    const auto port = unique_test_port();
    auto runtime = start_silent_runtime(dir, port, prefix, bbo_name, book_name);
    std::string health;
    for (int i = 0; i < 50; ++i) {
      try {
        health = http_get(port, "/healthz");
        if (health.find("\"status\":\"ok\"") != std::string::npos) {
          break;
        }
      } catch (...) {
      }
      std::this_thread::sleep_for(std::chrono::milliseconds(20));
    }
    require(health.find("\"status\":\"ok\"") != std::string::npos,
            "cold start healthz failed");
    pubs.bbo.reset();
    pubs.book.reset();
    pubs = open_publishers(bbo_name, book_name);
    publish_pair(pubs, 1, 1);
    std::this_thread::sleep_for(std::chrono::milliseconds(stale_ms * 4));
    require(wait_readyz(port, false, 10),
            "startup with no prior record recovered after segment rebuild");
    runtime->stop();
    require(logs.count("silent_reader_reopen") == 0,
            "startup empty ring triggered silent reopen");
  }

  logs.lines.clear();
  {
    const auto input_prefix = "/selfquant.fp.reopen" + pid;
    const auto prefix = "/selfquant.fp.reopenout" + pid;
    const auto bbo_name = mds::publish::make_publisher_segment_name(
        input_prefix, "agg", "BTCUSDT", "aggbbo");
    const auto book_name = mds::publish::make_publisher_segment_name(
        input_prefix, "agg", "BTCUSDT", "aggorderbook");
    auto pubs = open_publishers(bbo_name, book_name);
    const auto port = unique_test_port();
    auto runtime = start_silent_runtime(dir, port, prefix, bbo_name, book_name);
    publish_pair(pubs, 20, 8);
    require(wait_readyz(port, true, 100), "initial frames did not become ready");
    pubs.bbo.reset();
    pubs.book.reset();
    std::this_thread::sleep_for(std::chrono::milliseconds(stale_ms * 2));
    require(wait_readyz(port, false, 20), "readyz stayed ready during silent reopen");
    pubs = open_publishers(bbo_name, book_name);
    bool restored = false;
    for (int i = 0; i < 100; ++i) {
      publish_pair(pubs, static_cast<std::uint64_t>(i + 1), 1);
      try {
        const auto body = http_get(port, "/readyz");
        if (body.find("HTTP/1.1 200") != std::string::npos &&
            body.find("\"status\":\"ready\"") != std::string::npos) {
          restored = true;
          break;
        }
      } catch (...) {
      }
      std::this_thread::sleep_for(std::chrono::milliseconds(20));
    }
    require(restored, "rebuilt ring did not restore readyz");
    const auto markets = http_get(port, "/v1/markets");
    require(markets.find("\"live\":true") != std::string::npos,
            "rebuilt ring did not restore live");
    runtime->stop();
    require(logs.count("reason=silent_reader_reopen") >= 2,
            "silent reopen did not run for bbo and orderbook");
    require(logs.count(bbo_name) > 0 && logs.count(book_name) > 0,
            "silent reopen log missing a segment");
  }
}

class BlockingHistory final : public fairprice::HistoryProvider {
 public:
  fairprice::HistoryResponse canned;
  std::atomic<int> queries{0};

  fairprice::HistoryResponse query(const fairprice::CatalogMarket &market,
                                   std::uint64_t,
                                   const std::vector<fairprice::LiveSample> &,
                                   std::string_view bps_type,
                                   std::uint64_t) override {
    in_query_.store(true, std::memory_order_release);
    struct Guard {
      std::atomic<bool> *flag;
      ~Guard() { flag->store(false, std::memory_order_release); }
    } guard{&in_query_};
    queries.fetch_add(1);
    {
      std::lock_guard lock(mu_);
      started_ = true;
    }
    started_cv_.notify_all();
    std::unique_lock lock(mu_);
    release_cv_.wait(lock, [&] { return released_; });
    auto out = canned;
    out.symbol = market.symbol;
    out.type = std::string(bps_type);
    return out;
  }

  bool started_flag() {
    std::lock_guard lock(mu_);
    return started_;
  }

  void wait_started() {
    while (!started_flag()) {
      std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
  }

  bool wait_started_for(std::chrono::milliseconds timeout) {
    const auto deadline = std::chrono::steady_clock::now() + timeout;
    while (std::chrono::steady_clock::now() < deadline) {
      if (started_flag()) {
        return true;
      }
      std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
    return started_flag();
  }

  void release() {
    {
      std::lock_guard lock(mu_);
      released_ = true;
    }
    release_cv_.notify_all();
  }

  void reset() {
    release();
    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::milliseconds(1000);
    while (in_query_.load(std::memory_order_acquire) &&
           std::chrono::steady_clock::now() < deadline) {
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    std::lock_guard lock(mu_);
    started_ = false;
    released_ = false;
    queries.store(0);
  }

 private:
  std::mutex mu_;
  std::condition_variable started_cv_;
  std::condition_variable release_cv_;
  std::atomic<bool> in_query_{false};
  bool started_{};
  bool released_{};
};

int tcp_connect(std::uint16_t port) {
  const auto fd = connect_loopback(port, 500);
  set_blocking(fd);
  return fd;
}

void send_all(int fd, std::string_view payload) {
  require(::send(fd, payload.data(), payload.size(), MSG_NOSIGNAL) ==
              static_cast<ssize_t>(payload.size()),
          "tcp send failed");
}

bool poll_readable(int fd, int timeout_ms) {
  pollfd item{};
  item.fd = fd;
  item.events = POLLIN;
  const auto ready = ::poll(&item, 1, timeout_ms);
  return ready > 0 && (item.revents & POLLIN) != 0;
}

std::string recv_available(int fd) {
  std::string response;
  std::array<char, 4096> buffer{};
  for (;;) {
    const auto n = ::recv(fd, buffer.data(), buffer.size(), 0);
    if (n > 0) {
      response.append(buffer.data(), static_cast<std::size_t>(n));
      if (!poll_readable(fd, 0)) {
        break;
      }
      continue;
    }
    break;
  }
  return response;
}

std::string http_request(std::uint16_t port, const std::string &request,
                         int timeout_ms = 1000) {
  const auto fd = tcp_connect(port);
  send_all(fd, request);
  std::string response;
  std::array<char, 4096> buffer{};
  const auto deadline = std::chrono::steady_clock::now() +
                        std::chrono::milliseconds(timeout_ms);
  while (std::chrono::steady_clock::now() < deadline) {
    if (!poll_readable(fd, 20)) {
      continue;
    }
    const auto n = ::recv(fd, buffer.data(), buffer.size(), 0);
    if (n <= 0) {
      break;
    }
    response.append(buffer.data(), static_cast<std::size_t>(n));
  }
  ::close(fd);
  return response;
}

struct IsolationHarness {
  std::filesystem::path dir;
  std::uint16_t port{};
  BlockingHistory *blocking_history{};
  std::unique_ptr<mds::publish::WirePublisher> bbo;
  std::unique_ptr<mds::publish::WirePublisher> book;
  std::unique_ptr<fairprice::Runtime> runtime;
  utils::md::wire::AggBboRecord bbo_record{};
  utils::md::wire::AggOrderBookRecord book_record{};

  IsolationHarness() = default;
  IsolationHarness(const IsolationHarness &) = delete;
  IsolationHarness &operator=(const IsolationHarness &) = delete;
  IsolationHarness(IsolationHarness &&other) noexcept { move_from(other); }
  IsolationHarness &operator=(IsolationHarness &&other) noexcept {
    if (this != &other) {
      shutdown();
      move_from(other);
    }
    return *this;
  }
  ~IsolationHarness() { shutdown(); }

 private:
  void move_from(IsolationHarness &other) noexcept {
    dir = std::move(other.dir);
    port = other.port;
    blocking_history = other.blocking_history;
    bbo = std::move(other.bbo);
    book = std::move(other.book);
    runtime = std::move(other.runtime);
    bbo_record = other.bbo_record;
    book_record = other.book_record;
    other.blocking_history = nullptr;
    other.port = 0;
  }

  void shutdown() {
    if (blocking_history != nullptr) {
      blocking_history->release();
      blocking_history = nullptr;
    }
    if (runtime) {
      runtime->stop();
      runtime.reset();
    }
  }
};

IsolationHarness start_isolation(std::uint16_t port, int idle_ms, int max_clients,
                                 BlockingHistory *history) {
  IsolationHarness harness;
  harness.port = port;
  harness.dir = std::filesystem::temp_directory_path() /
                ("fairprice-hist-" + std::to_string(::getpid()) + "-" +
                 std::to_string(port));
  std::filesystem::create_directories(harness.dir);
  const auto token = std::to_string(::getpid()) + "p" + std::to_string(port);
  const auto input_prefix = "/selfquant.fp.h" + token;
  const auto prefix = "/selfquant.fp.o" + token;
  const auto bbo_name = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggbbo");
  const auto book_name = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggorderbook");
  unlink_posix_shm(mds::publish::make_publisher_segment_name(
      prefix, "agg", "BTCUSDT", "fairprice"));

  auto bbo_ring = open_input_ring(bbo_name);
  auto book_ring = open_input_ring(book_name);
  harness.bbo =
      std::make_unique<mds::publish::WirePublisher>(std::move(bbo_ring));
  harness.book =
      std::make_unique<mds::publish::WirePublisher>(std::move(book_ring));
  require(bool(harness.book->prepare_aggregate_orderbook()),
          "prepare aggregate orderbook failed");

  std::ofstream yaml(harness.dir / "fairprice.yaml");
  yaml << "version: 2\nruntime:\n  log_level: info\n  development: true\n"
       << market_data_yaml()
       << "shared_memory:\n  backend: posix_shm\n  output:\n    prefix: " << prefix
       << "\n    ring_bytes: 1048576\n    max_record_bytes: 65536\n    max_readers: 8\n"
       << "    reader_lease_timeout_ms: 5000\n    unlink_on_shutdown: true\n"
       << "server:\n  listen_address: 127.0.0.1\n  port: " << port << "\n"
       << "  allowed_origins:\n    - http://localhost:3000\n"
       << "  browser_token: \"\"\n  max_clients: "
       << max_clients << "\n  max_subscriptions_per_client: 4\n"
       << "  read_header_timeout_ms: 5000\n  idle_timeout_ms: " << idle_ms
       << "\n  max_header_bytes: 16384\n"
       << "  websocket:\n    handshake_timeout_ms: 5000\n    read_limit_bytes: 4096\n"
       << "    write_buffer_bytes: 65536\n    pong_timeout_ms: 45000\n"
       << "stream:\n  interval_ms: 20\n"
       << "history:\n  workers: 1\n  resolution_ms: 60000\n"
       << "fair_price:\n  enabled: true\n  depth: 2\n  lambda_per_bp: 0.0\n"
       << "  imbalance_alpha: 1.0\n  ewma_tau_ms: 500\n  impact_notional: 0.0\n"
       << "  venue_dominance_ratio: 0.9\n  stream_interval_ms: 5\n  heartbeat_ms: 1000\n"
       << "  persistence:\n    enabled: false\n    database_url: \"\"\n"
       << "    sample_interval_ms: 1000\n    retention_hours: 24\n"
       << "    cleanup_interval_ms: 300000\n    delete_batch: 10000\n"
       << "    delete_max_batches: 20\n    operation_timeout_ms: 5000\n";
  yaml.close();

  auto loaded = fairprice::load_config((harness.dir / "fairprice.yaml").string());
  require(bool(loaded), loaded.message.c_str());
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());
  harness.blocking_history = history;
  harness.runtime = std::make_unique<fairprice::Runtime>(
      std::move(loaded.value), query_catalog_segments({bbo_name, book_name}),
      no_gateway());
  harness.runtime->set_history_provider_for_test(history);
  auto started = harness.runtime->start();
  require(bool(started), started.message.c_str());

  harness.bbo_record.member_count = 1;
  harness.bbo_record.member_mask = 1;
  harness.bbo_record.live_mask = 1;
  harness.bbo_record.venue_slot_ids[0] = 1;
  harness.bbo_record.gated_bid.price = 99;
  harness.bbo_record.gated_bid.quantity = 10;
  harness.bbo_record.gated_bid.venue_quantity[0] = 10;
  harness.bbo_record.gated_bid.venue_mask = 1;
  harness.bbo_record.gated_bid.contributor_count = 1;
  harness.bbo_record.gated_ask.price = 101;
  harness.bbo_record.gated_ask.quantity = 10;
  harness.bbo_record.gated_ask.venue_quantity[0] = 10;
  harness.bbo_record.gated_ask.venue_mask = 1;
  harness.bbo_record.gated_ask.contributor_count = 1;
  harness.bbo_record.raw_bid.price = 99;
  harness.bbo_record.raw_bid.quantity = 10;
  harness.bbo_record.raw_bid.venue_mask = 1;
  harness.bbo_record.raw_ask.price = 101;
  harness.bbo_record.raw_ask.quantity = 10;
  harness.bbo_record.raw_ask.venue_mask = 1;
  std::memcpy(harness.bbo_record.base_asset.data(), "BTC", 3);
  std::memcpy(harness.bbo_record.quote_asset.data(), "USDT", 4);
  require(bool(harness.bbo->publish_agg_bbo(event_header(1, 1), harness.bbo_record)),
          "isolation bbo publish failed");

  harness.book_record.member_count = 1;
  harness.book_record.member_mask = 1;
  harness.book_record.active_mask = 1;
  harness.book_record.venue_slot_ids[0] = 1;
  harness.book_record.bid_count = 2;
  harness.book_record.ask_count = 2;
  harness.book_record.bids[0].price = 99;
  harness.book_record.bids[0].quantity = 10;
  harness.book_record.bids[0].venue_quantity[0] = 10;
  harness.book_record.bids[0].venue_mask = 1;
  harness.book_record.bids[0].contributor_count = 1;
  harness.book_record.bids[1].price = 98;
  harness.book_record.bids[1].quantity = 10;
  harness.book_record.bids[1].venue_quantity[0] = 10;
  harness.book_record.bids[1].venue_mask = 1;
  harness.book_record.bids[1].contributor_count = 1;
  harness.book_record.asks[0].price = 101;
  harness.book_record.asks[0].quantity = 10;
  harness.book_record.asks[0].venue_quantity[0] = 10;
  harness.book_record.asks[0].venue_mask = 1;
  harness.book_record.asks[0].contributor_count = 1;
  harness.book_record.asks[1].price = 102;
  harness.book_record.asks[1].quantity = 10;
  harness.book_record.asks[1].venue_quantity[0] = 10;
  harness.book_record.asks[1].venue_mask = 1;
  harness.book_record.asks[1].contributor_count = 1;
  std::memcpy(harness.book_record.base_asset.data(), "BTC", 3);
  std::memcpy(harness.book_record.quote_asset.data(), "USDT", 4);
  require(bool(harness.book->publish_agg_orderbook(event_header(1, 1),
                                                   harness.book_record)),
          "isolation book publish failed");
  std::string readyz;
  for (int attempt = 0; attempt < 50; ++attempt) {
    try {
      const auto health = http_get(port, "/healthz");
      readyz = http_get(port, "/readyz");
      if (health.find("\"status\":\"ok\"") != std::string::npos &&
          readyz.find("\"status\":\"ready\"") != std::string::npos) {
        return harness;
      }
    } catch (...) {
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  require(readyz.find("\"status\":\"ready\"") != std::string::npos,
          "isolation store never became ready");
  return harness;
}

const char *kHistoryPath =
    "/v1/markets/BTCUSDT/spread-history?profile=agg&type=gated&range=24h";

std::string history_get_request(std::string_view extra_headers = {}) {
  std::string request = "GET ";
  request += kHistoryPath;
  request += " HTTP/1.1\r\nHost: 127.0.0.1\r\n";
  request += extra_headers;
  if (request.find("Connection:") == std::string::npos) {
    request += "Connection: close\r\n";
  }
  request += "\r\n";
  return request;
}

int open_ws(std::uint16_t port) {
  const auto fd = tcp_connect(port);
  send_all(fd,
           "GET /v1/stream HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\n"
           "Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
           "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
           "Origin: http://localhost:3000\r\n\r\n");
  std::string response;
  std::array<char, 1024> buffer{};
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(1000);
  while (response.find("\r\n\r\n") == std::string::npos) {
    require(std::chrono::steady_clock::now() < deadline,
            "websocket handshake timed out");
    if (!poll_readable(fd, 50)) {
      continue;
    }
    const auto n = ::recv(fd, buffer.data(), buffer.size(), 0);
    require(n > 0, "websocket handshake recv failed");
    response.append(buffer.data(), static_cast<std::size_t>(n));
  }
  require(response.find("101") != std::string::npos, "websocket upgrade failed");
  net::WebSocketFrameWriter writer;
  std::string_view error;
  const auto payload =
      std::string("{\"op\":\"subscribe\",\"profile\":\"agg\",\"symbol\":"
                  "\"BTCUSDT\",\"channel\":\"bbo\"}");
  require(writer.prepare_with_mask(
              net::WsOpcode::Text,
              {reinterpret_cast<const std::byte *>(payload.data()), payload.size()},
              true, 0x12345678U, error),
          "subscribe frame failed");
  const auto pending = writer.pending();
  send_all(fd, {reinterpret_cast<const char *>(pending.data()), pending.size()});
  return fd;
}

bool recv_sqab(int fd, int timeout_ms) {
  net::WebSocketParser parser;
  const auto deadline = std::chrono::steady_clock::now() +
                        std::chrono::milliseconds(timeout_ms);
  std::array<char, 4096> buffer{};
  bool saw = false;
  while (std::chrono::steady_clock::now() < deadline && !saw) {
    if (!poll_readable(fd, 20)) {
      continue;
    }
    const auto n = ::recv(fd, buffer.data(), buffer.size(), 0);
    if (n <= 0) {
      break;
    }
    std::string error;
    parser.feed(
        {reinterpret_cast<const std::byte *>(buffer.data()),
         static_cast<std::size_t>(n)},
        [&](const net::WsFrameView &frame) {
          if (frame.opcode == net::WsOpcode::Binary && frame.payload.size() >= 4) {
            std::uint32_t magic{};
            std::memcpy(&magic, frame.payload.data(), sizeof(magic));
            if (magic == fairprice::kSqabMagic) {
              saw = true;
            }
          }
          return true;
        },
        error);
  }
  return saw;
}

namespace {

std::string write_bootstrap_yaml(const std::filesystem::path &dir,
                                 const std::string &prefix,
                                 std::uint16_t port) {
  std::ofstream yaml(dir / "fairprice.yaml");
  yaml << "version: 2\n"
       << "runtime:\n  log_level: info\n  development: true\n"
       << market_data_yaml()
       << "shared_memory:\n  backend: posix_shm\n  output:\n    prefix: " << prefix
       << "\n    ring_bytes: 1048576\n    max_record_bytes: 65536\n    max_readers: 8\n"
       << "    reader_lease_timeout_ms: 5000\n    unlink_on_shutdown: true\n"
       << "server:\n  listen_address: 127.0.0.1\n  port: " << port << "\n"
       << "  allowed_origins:\n    - http://localhost:3000\n"
       << "  browser_token: \"\"\n  max_clients: 8\n"
       << "  max_subscriptions_per_client: 4\n  read_header_timeout_ms: 5000\n"
       << "  idle_timeout_ms: 60000\n  max_header_bytes: 16384\n"
       << "  websocket:\n    handshake_timeout_ms: 5000\n    read_limit_bytes: 4096\n"
       << "    write_buffer_bytes: 65536\n    pong_timeout_ms: 45000\n"
       << "stream:\n  interval_ms: 20\n"
       << "history:\n  workers: 1\n  resolution_ms: 60000\n"
       << "fair_price:\n  enabled: true\n  depth: 2\n  lambda_per_bp: 0.0\n"
       << "  imbalance_alpha: 1.0\n  ewma_tau_ms: 500\n  impact_notional: 0.0\n"
       << "  venue_dominance_ratio: 0.9\n  stream_interval_ms: 5\n  heartbeat_ms: 1000\n"
       << "  persistence:\n    enabled: false\n    database_url: \"\"\n"
       << "    sample_interval_ms: 1000\n    retention_hours: 24\n"
       << "    cleanup_interval_ms: 300000\n    delete_batch: 10000\n"
       << "    delete_max_batches: 20\n    operation_timeout_ms: 5000\n";
  yaml.close();
  return (dir / "fairprice.yaml").string();
}

utils::md::wire::AggOrderBookRecord live_book_record(std::int64_t bid,
                                                     std::int64_t ask) {
  utils::md::wire::AggOrderBookRecord book{};
  book.member_count = 1;
  book.member_mask = 1;
  book.active_mask = 1;
  book.venue_slot_ids[0] = 1;
  book.price_scale = 2;
  book.quantity_scale = 3;
  book.bid_count = 1;
  book.ask_count = 1;
  book.bids[0].price = bid;
  book.bids[0].quantity = 10;
  book.bids[0].venue_quantity[0] = 10;
  book.bids[0].venue_mask = 1;
  book.bids[0].contributor_count = 1;
  book.asks[0].price = ask;
  book.asks[0].quantity = 10;
  book.asks[0].venue_quantity[0] = 10;
  book.asks[0].venue_mask = 1;
  book.asks[0].contributor_count = 1;
  std::memcpy(book.base_asset.data(), "BTC", 3);
  std::memcpy(book.quote_asset.data(), "USDT", 4);
  return book;
}

bool wait_fair_frame(mds::consume::SegmentReader &reader, bool want_ready,
                     std::uint64_t generation, std::int64_t forbidden_price,
                     int attempts) {
  for (int i = 0; i < attempts; ++i) {
    auto viewed = reader.try_read_view();
    if (viewed && viewed.value) {
      utils::md::wire::FairPriceRecordV2 decoded{};
      const auto ok = utils::md::wire::DecodeFairPriceV2(
                          viewed.value->payload(), decoded) ==
                      utils::md::wire::CodecError::Ok;
      (void)viewed.value->commit();
      if (ok && decoded.ready == static_cast<std::uint8_t>(want_ready) &&
          (generation == 0 || decoded.generation == generation) &&
          decoded.price != forbidden_price) {
        return true;
      }
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  return false;
}

}  // namespace

void test_bootstrap_stop_flow_and_first_frame() {
  const auto pid = std::to_string(::getpid());
  const auto dir = std::filesystem::temp_directory_path() /
                   ("fairprice-boot-" + pid);
  std::filesystem::remove_all(dir);
  std::filesystem::create_directories(dir);
  const auto input_prefix = "/selfquant.fp.bin" + pid;
  const auto prefix = "/selfquant.fp.bout" + pid;
  const auto btc_bbo = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggbbo");
  const auto btc_book = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggorderbook");
  const auto sol_bbo = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "SOLUSDT", "aggbbo");
  const auto sol_book = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "SOLUSDT", "aggorderbook");
  const auto fair_name = mds::publish::make_publisher_segment_name(
      prefix, "agg", "BTCUSDT", "fairprice");
  auto btc_bbo_ring = open_input_ring(btc_bbo);
  auto btc_book_ring = open_input_ring(btc_book);
  auto sol_bbo_ring = open_input_ring(sol_bbo);
  auto sol_book_ring = open_input_ring(sol_book);
  unlink_posix_shm(fair_name);

  const auto now = static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
  fairprice::GatewayFrameSource gateway =
      [now](std::string_view segment, std::uint64_t) {
        fairprice::GatewayWireFrame frame;
        frame.ring_epoch = 1;
        frame.ring_sequence = 11;
        frame.generation = 4;
        const bool btc = segment.find("btcusdt") != std::string_view::npos;
        frame.wall_ns = btc ? now - 1'000'000ULL : 1'000ULL;
        if (segment.find("aggbbo") != std::string_view::npos) {
          frame.kind = fairprice::Kind::Bbo;
          auto record = utils::md::wire::AggBboRecord{};
          record.member_count = 1;
          record.member_mask = 1;
          record.live_mask = 1;
          record.venue_slot_ids[0] = 1;
          record.gated_bid.price = btc ? 99 : 9;
          record.gated_bid.quantity = 10;
          record.gated_bid.venue_quantity[0] = 10;
          record.gated_bid.venue_mask = 1;
          record.gated_bid.contributor_count = 1;
          record.gated_ask.price = btc ? 101 : 11;
          record.gated_ask.quantity = 10;
          record.gated_ask.venue_quantity[0] = 10;
          record.gated_ask.venue_mask = 1;
          record.gated_ask.contributor_count = 1;
          record.raw_bid.price = record.gated_bid.price;
          record.raw_bid.quantity = record.gated_bid.quantity;
          record.raw_bid.venue_mask = 1;
          record.raw_ask.price = record.gated_ask.price;
          record.raw_ask.quantity = record.gated_ask.quantity;
          record.raw_ask.venue_mask = 1;
          if (btc) {
            std::memcpy(record.base_asset.data(), "BTC", 3);
          } else {
            std::memcpy(record.base_asset.data(), "SOL", 3);
          }
          std::memcpy(record.quote_asset.data(), "USDT", 4);
          frame.bbo = record;
        } else {
          frame.kind = fairprice::Kind::Book;
          frame.book = live_book_record(btc ? 99 : 9, btc ? 101 : 11);
          if (!btc) {
            std::memcpy(frame.book->base_asset.data(), "SOL", 3);
          }
        }
        static std::uint16_t next_id = 1;
        frame.topic_id = next_id++;
        return std::vector<fairprice::GatewayWireFrame>{frame};
      };

  const auto port = unique_test_port();
  auto loaded = fairprice::load_config(write_bootstrap_yaml(dir, prefix, port));
  require(bool(loaded), loaded.message.c_str());
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());
  fairprice::Runtime runtime(
      std::move(loaded.value),
      query_catalog_segments({btc_bbo, btc_book, sol_bbo, sol_book}), gateway);
  auto started = runtime.start();
  require(bool(started), started.message.c_str());

  std::string markets;
  std::string readyz;
  std::string btc_snapshot;
  std::string sol_snapshot;
  for (int i = 0; i < 50; ++i) {
    try {
      markets = http_get(port, "/v1/markets");
      readyz = http_get(port, "/readyz");
      btc_snapshot =
          http_get(port, "/v1/markets/BTCUSDT/snapshot?profile=agg&depth=2");
      sol_snapshot =
          http_get(port, "/v1/markets/SOLUSDT/snapshot?profile=agg&depth=2");
      if (markets.find("\"base\":\"BTC\"") != std::string::npos &&
          markets.find("\"base\":\"SOL\"") != std::string::npos) {
        break;
      }
    } catch (...) {
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  require(markets.find("\"symbol\":\"BTCUSDT\"") != std::string::npos &&
              markets.find("\"base\":\"BTC\"") != std::string::npos &&
              markets.find("\"quote\":\"USDT\"") != std::string::npos &&
              markets.find("\"symbol\":\"SOLUSDT\"") != std::string::npos &&
              markets.find("\"base\":\"SOL\"") != std::string::npos &&
              markets.find("\"live\":false") != std::string::npos,
          "stop-flow bootstrap must restore BTC+SOL identity");
  require(readyz.find("HTTP/1.1 503") != std::string::npos,
          "readyz must follow freshness when a restored market is stale");
  require(sol_snapshot.find("HTTP/1.1 503") != std::string::npos &&
              sol_snapshot.find("snapshot is not ready") != std::string::npos,
          "expired restored snapshot must be 503");
  require(btc_snapshot.find("HTTP/1.1 200") != std::string::npos &&
              btc_snapshot.find("orderbook") != std::string::npos,
          "fresh restored book must serve snapshot");

  const auto btc_key = fairprice::market_key("agg", "BTCUSDT");
  auto view = runtime.store().copy_view(btc_key);
  require(view && view->fair.ready && view->book.base_asset[0] == 'B',
          "fresh bootstrap must leave a ready fair pod in the store");
  auto reader = open_segment_reader(fair_name);
  fairprice::Runtime::StartupPublishIntent intent;
  intent.key = btc_key;
  intent.profile = "agg";
  intent.symbol = "BTCUSDT";
  intent.pod = view->fair;
  intent.epoch = view->book.ring_epoch;
  intent.sequence = view->book.ring_sequence;
  intent.generation = view->book.generation;
  runtime.enqueue_startup_publish(intent);
  require(wait_fair_frame(reader, true, 0, 0, 50),
          "runtime must publish the bootstrap fairprice first frame");

  runtime.stop();
  std::filesystem::remove_all(dir);
  (void)btc_bbo_ring;
  (void)btc_book_ring;
  (void)sol_bbo_ring;
  (void)sol_book_ring;
}

void test_bootstrap_live_publish_discards_history() {
  const auto pid = std::to_string(::getpid());
  const auto dir = std::filesystem::temp_directory_path() /
                   ("fairprice-livehist-" + pid);
  std::filesystem::remove_all(dir);
  std::filesystem::create_directories(dir);
  const auto input_prefix = "/selfquant.fp.lin" + pid;
  const auto prefix = "/selfquant.fp.lout" + pid;
  const auto bbo_name = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggbbo");
  const auto book_name = mds::publish::make_publisher_segment_name(
      input_prefix, "agg", "BTCUSDT", "aggorderbook");
  const auto fair_name = mds::publish::make_publisher_segment_name(
      prefix, "agg", "BTCUSDT", "fairprice");

  auto bbo_ring = open_input_ring(bbo_name);
  auto book_ring = open_input_ring(book_name);
  unlink_posix_shm(fair_name);
  mds::publish::WirePublisher bbo_publisher(std::move(bbo_ring));
  mds::publish::WirePublisher book_publisher(std::move(book_ring));
  require(bool(book_publisher.prepare_aggregate_orderbook()),
          "prepare aggregate orderbook failed");

  const auto port = unique_test_port();
  auto loaded = fairprice::load_config(write_bootstrap_yaml(dir, prefix, port));
  require(bool(loaded), loaded.message.c_str());
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());
  fairprice::Runtime runtime(std::move(loaded.value),
                             query_catalog_segments({bbo_name, book_name}),
                             no_gateway());
  auto started = runtime.start();
  require(bool(started), started.message.c_str());

  utils::md::wire::AggBboRecord bbo{};
  bbo.member_count = 1;
  bbo.member_mask = 1;
  bbo.live_mask = 1;
  bbo.venue_slot_ids[0] = 1;
  bbo.price_scale = 2;
  bbo.quantity_scale = 3;
  bbo.gated_bid.price = 199;
  bbo.gated_bid.quantity = 10;
  bbo.gated_bid.venue_quantity[0] = 10;
  bbo.gated_bid.venue_mask = 1;
  bbo.gated_bid.contributor_count = 1;
  bbo.gated_ask.price = 201;
  bbo.gated_ask.quantity = 10;
  bbo.gated_ask.venue_quantity[0] = 10;
  bbo.gated_ask.venue_mask = 1;
  bbo.gated_ask.contributor_count = 1;
  bbo.raw_bid.price = 199;
  bbo.raw_bid.quantity = 10;
  bbo.raw_bid.venue_mask = 1;
  bbo.raw_ask.price = 201;
  bbo.raw_ask.quantity = 10;
  bbo.raw_ask.venue_mask = 1;
  std::memcpy(bbo.base_asset.data(), "BTC", 3);
  std::memcpy(bbo.quote_asset.data(), "USDT", 4);
  require(bool(bbo_publisher.publish_agg_bbo(event_header(1, 8), bbo)),
          "live bbo publish failed");
  auto book = live_book_record(199, 201);
  require(bool(book_publisher.publish_agg_orderbook(event_header(1, 8), book)),
          "live book publish failed");

  std::string markets;
  for (int i = 0; i < 100; ++i) {
    try {
      markets = http_get(port, "/v1/markets");
      if (markets.find("\"live\":true") != std::string::npos) {
        break;
      }
    } catch (...) {
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  require(markets.find("\"live\":true") != std::string::npos,
          "live SHM must mark the market live before the stale enqueue");

  auto reader = open_segment_reader(fair_name);
  const auto key = fairprice::market_key("agg", "BTCUSDT");
  require(runtime.store().book_has_live_sequence(key),
          "live apply must set the sequence tracker");

  fairprice::FairPricePod stale{};
  stale.ready = 1;
  stale.generation = 1;
  stale.ring_epoch = 1;
  stale.ring_sequence = 1;
  stale.price = 42;
  stale.best_bid = 40;
  stale.best_ask = 44;
  fairprice::Runtime::StartupPublishIntent intent;
  intent.key = key;
  intent.profile = "agg";
  intent.symbol = "BTCUSDT";
  intent.pod = stale;
  intent.epoch = 1;
  intent.sequence = 1;
  intent.generation = 1;
  runtime.enqueue_startup_publish(intent);
  std::this_thread::sleep_for(std::chrono::milliseconds(80));

  book.bids[0].price = 198;
  require(bool(book_publisher.publish_agg_orderbook(event_header(2, 9), book)),
          "follow-up live book failed");
  bool saw_stale = false;
  bool saw_live = false;
  for (int i = 0; i < 50; ++i) {
    auto viewed = reader.try_read_view();
    if (viewed && viewed.value) {
      utils::md::wire::FairPriceRecordV2 decoded{};
      if (utils::md::wire::DecodeFairPriceV2(viewed.value->payload(),
                                            decoded) ==
          utils::md::wire::CodecError::Ok) {
        if (decoded.price == 42 || decoded.generation == 1) {
          saw_stale = true;
        }
        if (decoded.ready && decoded.generation == 9) {
          saw_live = true;
        }
      }
      (void)viewed.value->commit();
      if (saw_live) {
        break;
      }
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
  }
  runtime.stop();
  std::filesystem::remove_all(dir);
  require(!saw_stale, "queued historical bootstrap must not overwrite live SHM");
  require(saw_live, "live follow-up frame must still publish");
}

void test_history_isolation() {
  BlockingHistory fake;
  fake.canned.distribution = {1.0, 2.0, 3.0};
  fake.canned.type = "gated";
  const auto port = unique_test_port();
  auto harness = start_isolation(port, 80, 16, &fake);

  const auto history_fd = tcp_connect(port);
  send_all(history_fd, history_get_request(
                           "Origin: http://localhost:3000\r\nConnection: close\r\n"));
  require(fake.wait_started_for(std::chrono::milliseconds(1000)),
          "history worker did not start");

  const auto t0 = std::chrono::steady_clock::now();
  const auto readyz = http_get(port, "/readyz");
  const auto readyz_ms = std::chrono::duration_cast<std::chrono::milliseconds>(
                             std::chrono::steady_clock::now() - t0)
                             .count();
  require(readyz.find("\"status\":\"ready\"") != std::string::npos,
          "readyz blocked behind history");
  require(readyz_ms < 500, "readyz was too slow during history");

  const auto ws = open_ws(port);
  require(recv_sqab(ws, 500), "websocket missing initial SQAB");
  harness.bbo_record.gated_bid.quantity = 11;
  harness.bbo_record.gated_bid.venue_quantity[0] = 11;
  require(bool(harness.bbo->publish_agg_bbo(event_header(2, 2), harness.bbo_record)),
          "follow-up bbo publish failed");
  require(recv_sqab(ws, 500), "websocket stalled during history");
  ::close(ws);

  require(!poll_readable(history_fd, 50),
          "history response arrived before worker release");
  fake.release();
  std::string history_body;
  for (int i = 0; i < 50 && history_body.find("distribution_bps") == std::string::npos;
       ++i) {
    if (poll_readable(history_fd, 50)) {
      history_body += recv_available(history_fd);
    }
  }
  ::close(history_fd);
  require(history_body.find("HTTP/1.1 200") != std::string::npos &&
              history_body.find("distribution_bps") != std::string::npos &&
              history_body.find("Access-Control-Allow-Origin: http://localhost:3000") !=
                  std::string::npos &&
              history_body.find("Cache-Control: no-store") != std::string::npos,
          "history json contract/CORS mismatch");

  fake.reset();
  const auto dropped = tcp_connect(port);
  send_all(dropped, history_get_request());
  require(fake.wait_started_for(std::chrono::milliseconds(1000)),
          "disconnect history did not start");
  ::close(dropped);
  const auto reused = tcp_connect(port);
  send_all(reused, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
  fake.release();
  std::string reused_body;
  for (int i = 0; i < 50 && reused_body.find("ok") == std::string::npos; ++i) {
    if (poll_readable(reused, 50)) {
      reused_body += recv_available(reused);
    }
  }
  ::close(reused);
  require(reused_body.find("\"status\":\"ok\"") != std::string::npos,
          "fd reuse delivered the wrong response");
  require(reused_body.find("distribution_bps") == std::string::npos,
          "stale history was delivered to a reused fd");

  fake.reset();
  const auto idle_fd = tcp_connect(port);
  send_all(idle_fd, history_get_request("Connection: keep-alive\r\n"));
  require(fake.wait_started_for(std::chrono::milliseconds(1000)),
          "idle history did not start");
  send_all(idle_fd, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
  std::this_thread::sleep_for(std::chrono::milliseconds(200));
  require(!poll_readable(idle_fd, 20) || recv_available(idle_fd).empty(),
          "pipelined HTTP or idle-close ran during history");
  fake.release();
  std::string idle_body;
  for (int i = 0; i < 50 && idle_body.find("ok") == std::string::npos; ++i) {
    if (poll_readable(idle_fd, 50)) {
      idle_body += recv_available(idle_fd);
    }
  }
  ::close(idle_fd);
  require(idle_body.find("distribution_bps") != std::string::npos,
          "keep-alive history response missing");
  require(idle_body.find("\"status\":\"ok\"") != std::string::npos,
          "pipelined healthz was not served after history");

  fake.reset();
  std::vector<int> queued;
  queued.push_back(tcp_connect(port));
  send_all(queued.back(), history_get_request());
  require(fake.wait_started_for(std::chrono::milliseconds(1000)),
          "queue-full first job did not start");
  for (int i = 0; i < 7; ++i) {
    queued.push_back(tcp_connect(port));
    send_all(queued.back(), history_get_request());
  }
  std::string overflow;
  for (int i = 0; i < 40; ++i) {
    overflow = http_request(
        port,
        history_get_request("Origin: http://localhost:3000\r\nConnection: close\r\n"),
        50);
    if (overflow.find("HTTP/1.1 503") != std::string::npos) {
      break;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }
  require(overflow.find("HTTP/1.1 503") != std::string::npos &&
              overflow.find("history is unavailable") != std::string::npos,
          "queue-full must return 503");
  fake.release();
  for (int fd : queued) {
    (void)recv_available(fd);
    ::close(fd);
  }

  fake.reset();
  const auto stop_fd = tcp_connect(port);
  send_all(stop_fd, history_get_request());
  require(fake.wait_started_for(std::chrono::milliseconds(1000)),
          "stop history did not start");
  std::vector<int> extra;
  for (int i = 0; i < 3; ++i) {
    extra.push_back(tcp_connect(port));
    send_all(extra.back(), history_get_request());
  }
  std::thread releaser([&] {
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
    fake.release();
  });
  harness.runtime->stop();
  releaser.join();
  ::close(stop_fd);
  for (int fd : extra) {
    ::close(fd);
  }
  std::filesystem::remove_all(harness.dir);
}
