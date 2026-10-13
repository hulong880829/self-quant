#pragma once

#include "fairprice/config.h"
#include "fairprice/history.h"
#include "fairprice/recorder.h"
#include "fairprice/store.h"
#include "net/tcp_acceptor.h"

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace fairprice {

enum class HistoryJobKind : std::uint8_t {
  SpreadHistory = 0,
  FairPriceHistory = 1,
};

class Server {
 public:
  Server(Config config, Store *store, Catalog *catalog, History *history,
         Recorder *recorder);
  ~Server();
  bool start();
  void stop();
  void set_history_provider_for_test(HistoryProvider *provider);

 private:
  struct HistoryJob {
    int fd{-1};
    std::uint64_t client_id{};
    std::uint64_t request_id{};
    std::string origin;
    bool keep_alive{};
    bool close_after{};
    HistoryJobKind kind{HistoryJobKind::SpreadHistory};
    CatalogMarket market;
    std::uint64_t sample_interval_ms{};
    std::vector<LiveSample> live;
    std::string bps_type;
    std::uint64_t now_ns{};
    std::string start;
    std::string end;
    std::string resolution;
    std::string model_id;
  };
  struct HistoryResult {
    int fd{-1};
    std::uint64_t client_id{};
    std::uint64_t request_id{};
    std::string origin;
    bool keep_alive{};
    bool close_after{};
    int status{200};
    std::string body;
  };

  void loop();
  void history_worker();
  void join_history_workers();
  void drain_history_eventfd();
  HistoryProvider *history_provider() const;

  Config config_;
  Store *store_{};
  Catalog *catalog_{};
  History *history_{};
  Recorder *recorder_{};
  std::string browser_token_;
  std::atomic<bool> stop_{false};
  std::atomic<bool> stopping_{false};
  std::atomic<HistoryProvider *> history_override_{nullptr};
  std::thread thread_{};
  net::TcpAcceptor acceptor_{};
  int history_event_fd_{-1};
  std::size_t history_queue_capacity_{8};
  std::size_t history_outstanding_{};
  std::uint64_t next_client_id_{1};
  std::mutex history_mutex_{};
  std::condition_variable history_cv_{};
  std::deque<HistoryJob> history_jobs_{};
  std::deque<HistoryResult> history_results_{};
  std::vector<std::thread> history_workers_{};
};

}  // namespace fairprice
