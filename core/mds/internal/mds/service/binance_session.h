#pragma once

#include "mds/book/bbo_overlay.h"
#include "mds/book/book_bridge.h"
#include "mds/exchange/binance/binance_rest.h"
#include "mds/exchange/binance/binance_streams.h"
#include "mds/instrument/instrument_manager.h"
#include "net/epoll_loop.h"
#include "net/http_client.h"
#include "net/websocket_client.h"
#include "mds/publish/wire_publisher.h"

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace mds::service {

enum class BinanceSessionState : std::uint8_t {
  Resolving,
  Tcp,
  Tls,
  Upgrade,
  Buffering,
  Metadata,
  Snapshot,
  Bridging,
  Live,
  Resync,
  ReconnectWait,
  Stopped,
  Failed,
};

std::string_view to_string(BinanceSessionState state) noexcept;

struct BinanceSessionMetrics {
  std::uint64_t reconnects{};
  std::uint64_t resyncs{};
  std::uint64_t rotations{};
  std::uint64_t parse_errors{};
  std::uint64_t publish_errors{};
  std::uint64_t depth_updates{};
  std::uint64_t ticker_updates{};
  std::uint64_t outside_depth_levels_ignored{};
  bool rotation_is_seamless{};
};

struct BinanceSessionOptions {
  exchange::binance::Profile profile{exchange::binance::Profile::Spot};
  std::string symbol{"BTCUSDT"};
  std::string websocket_endpoint{};
  std::string rest_endpoint{};
  std::string shm_prefix{"/selfquant.mds"};
  std::size_t ladder_ticks_per_side{8192};
  std::size_t max_ladder_ticks_per_side{16384};
  std::size_t max_buffered_updates{4096};
  std::size_t max_depth_levels_per_side{
      exchange::binance::kDefaultMaxDepthLevelsPerSide};
  std::size_t snapshot_depth{};
  std::uint32_t depth_update_interval_ms{100};
  std::chrono::milliseconds connect_timeout{10'000};
  std::chrono::milliseconds request_timeout{10'000};
  std::chrono::milliseconds idle_timeout{60'000};
  std::chrono::milliseconds reconnect_base{250};
  std::chrono::milliseconds reconnect_max{30'000};
  std::chrono::nanoseconds reader_lease_timeout{5'000'000'000};
  transport::RingOptions ring{};
  bool publish{true};
  bool subscribe_ticker{true};
  bool subscribe_orderbook{true};
};

class BinanceSession {
public:
  using Clock = std::chrono::steady_clock;
  struct Endpoint {
    std::string host;
    std::string service{"443"};
  };

  BinanceSession(net::EpollLoop &loop, net::SharedSslContext tls,
                 BinanceSessionOptions options);
  ~BinanceSession();
  BinanceSession(const BinanceSession &) = delete;
  BinanceSession &operator=(const BinanceSession &) = delete;

  api::Result<void> start(Clock::time_point now = Clock::now());
  void tick(Clock::time_point now = Clock::now()) noexcept;
  void stop() noexcept;

  // Offline entry points used by deterministic tests and mock transports.
  bool ingest_exchange_info(std::string_view json, std::string &error);
  bool ingest_depth_snapshot(std::string_view json, std::string &error);
  bool ingest_websocket_message(std::string_view json, std::string &error);
  bool poll_reader_changes() noexcept;

  [[nodiscard]] BinanceSessionState state() const noexcept {
    return state_.load(std::memory_order_acquire);
  }
  [[nodiscard]] std::uint32_t generation() const noexcept {
    return generation_.load(std::memory_order_acquire);
  }
  [[nodiscard]] const BinanceSessionMetrics &metrics() const noexcept {
    return metrics_;
  }
  [[nodiscard]] std::string_view error_message() const noexcept { return error_; }
  [[nodiscard]] std::string_view ticker_segment() const noexcept;
  [[nodiscard]] std::string_view orderbook_segment() const noexcept;
  [[nodiscard]] exchange::binance::Profile profile() const noexcept {
    return options_.profile;
  }
  [[nodiscard]] std::string_view symbol() const noexcept {
    return options_.symbol;
  }
  [[nodiscard]] bool snapshot_deferred() const noexcept {
    return snapshot_deferred_;
  }
  [[nodiscard]] static bool snapshot_allowed(
      net::WebSocketClientState state) noexcept {
    return state == net::WebSocketClientState::Open;
  }

private:
  enum class ClientKind : std::uint8_t { WebSocket, Metadata, Snapshot };

  bool open_publishers(std::string &error);
  bool begin_connection(Clock::time_point now, std::string &error);
  bool begin_metadata(Clock::time_point now, std::string &error);
  bool begin_snapshot(Clock::time_point now, std::string &error);
  bool request_snapshot(Clock::time_point now, std::string &error);
  void on_client_event(ClientKind kind, std::uint32_t events) noexcept;
  void sync_registration(ClientKind kind) noexcept;
  void remove_registration(ClientKind kind) noexcept;
  void handle_ws_state(Clock::time_point now) noexcept;
  void handle_http_state(ClientKind kind, Clock::time_point now) noexcept;
  bool handle_ticker(std::string_view data, std::string &error);
  bool handle_depth(std::string_view data, std::string &error);
  bool apply_depth(const exchange::binance::DepthUpdate &update,
                   bool publish) noexcept;
  static bool apply_buffered(void *context,
                             const exchange::binance::DepthUpdate &update) noexcept;
  bool publish_canonical(std::uint64_t sequence,
                         std::uint64_t exchange_time_ms) noexcept;
  bool publish_live_image(std::uint64_t sequence,
                          std::uint64_t exchange_time_ms) noexcept;
  bool republish_ticker() noexcept;
  bool republish_order_book() noexcept;
  void handle_publish_failure(std::string_view reason,
                              Clock::time_point now) noexcept;
  void schedule_reconnect(std::string_view reason, Clock::time_point now,
                          bool advance_generation = true) noexcept;
  void request_resync(std::string_view reason, Clock::time_point now) noexcept;
  void set_state(BinanceSessionState state) noexcept {
    state_.store(state, std::memory_order_release);
  }
  [[nodiscard]] Endpoint websocket_endpoint() const;
  [[nodiscard]] Endpoint rest_endpoint() const;
  [[nodiscard]] int client_fd(ClientKind kind) const noexcept;
  [[nodiscard]] std::uint64_t
  client_socket_generation(ClientKind kind) const noexcept;
  [[nodiscard]] std::uint32_t client_events(ClientKind kind) const noexcept;

  net::EpollLoop &loop_;
  net::SharedSslContext tls_;
  BinanceSessionOptions options_;
  net::WebSocketClient websocket_;
  net::HttpClient metadata_http_;
  net::HttpClient snapshot_http_;
  exchange::binance::CombinedStreamParser combined_parser_;
  exchange::binance::RestParser rest_parser_;
  exchange::binance::DepthSynchronizer synchronizer_;
  std::unique_ptr<exchange::binance::JsonParser> json_parser_;
  exchange::binance::DepthUpdate depth_scratch_;
  exchange::binance::InstrumentMetadata metadata_;
  utils::md::Instrument instrument_{};
  utils::md::InstrumentCatalog catalog_{};
  instrument::InstrumentManager instrument_manager_;
  std::unique_ptr<utils::md::OrderBook> order_book_;
  std::unique_ptr<book::BookBridge> bridge_;
  book::BboOverlay overlay_;
  std::optional<utils::md::BboEvent> latest_ticker_;
  std::unique_ptr<publish::WirePublisher> ticker_publisher_;
  std::unique_ptr<publish::WirePublisher> orderbook_publisher_;
  std::deque<std::string> raw_depth_buffer_;
  std::atomic<BinanceSessionState> state_{BinanceSessionState::Stopped};
  BinanceSessionMetrics metrics_{};
  std::string websocket_target_;
  std::string error_;
  Clock::time_point last_message_{};
  Clock::time_point opened_at_{};
  Clock::time_point reconnect_at_{};
  Clock::time_point next_snapshot_attempt_{};
  Clock::time_point last_reader_reclaim_{};
  std::atomic<std::uint32_t> generation_{};
  std::uint32_t reconnect_attempt_{};
  std::uint64_t bus_sequence_{1};
  int websocket_fd_{-1};
  int metadata_fd_{-1};
  int snapshot_fd_{-1};
  std::uint64_t websocket_socket_generation_{};
  std::uint64_t metadata_socket_generation_{};
  std::uint64_t snapshot_socket_generation_{};
  bool metadata_ready_{};
  bool instrument_ready_{};
  bool snapshot_deferred_{};
  bool snapshot_request_active_{};
  bool started_{};
  bool handling_publish_failure_{};
};

class SessionManager {
public:
  SessionManager();
  explicit SessionManager(net::SharedSslContext tls);

  api::Result<BinanceSession *>
  create(BinanceSessionOptions options, bool start_immediately = true);
  BinanceSession *find(exchange::binance::Profile profile,
                       std::string_view symbol) noexcept;
  int run_once(int timeout_ms);
  void stop() noexcept;

private:
  BinanceSession *find_unlocked(exchange::binance::Profile profile,
                                std::string_view symbol) noexcept;
  net::EpollLoop loop_;
  net::SharedSslContext tls_;
  mutable std::mutex mutex_;
  std::vector<std::unique_ptr<BinanceSession>> sessions_;
};

} // namespace mds::service
