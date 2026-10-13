#pragma once

#include "fairprice/bootstrap.h"
#include "fairprice/catalog.h"
#include "fairprice/config.h"
#include "fairprice/history.h"
#include "fairprice/log_summary.h"
#include "fairprice/market_data_repository.h"
#include "fairprice/publisher.h"
#include "fairprice/recorder.h"
#include "fairprice/server.h"
#include "fairprice/store.h"
#include "mds/consume/segment_reader.h"

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>

namespace fairprice {

enum class ReaderFaultPolicy : std::uint8_t {
  Ignore = 0,
  ResyncInPlace = 1,
  DestroyAndBackoff = 2,
};

inline ReaderFaultPolicy reader_fault_policy(mds::api::ErrorCode code) noexcept {
  switch (code) {
    case mds::api::ErrorCode::RecordOverwritten:
    case mds::api::ErrorCode::SubscriptionRejected:
      return ReaderFaultPolicy::ResyncInPlace;
    case mds::api::ErrorCode::InvalidHandle:
      return ReaderFaultPolicy::DestroyAndBackoff;
    default:
      return ReaderFaultPolicy::Ignore;
  }
}

struct OwnedOrderBookFrame {
  bool present{};
  utils::md::wire::AggOrderBookRecord record{};
  std::uint64_t epoch{};
  std::uint64_t sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  int frames{};
};

inline void consider_latest_orderbook(
    OwnedOrderBookFrame &latest, const utils::md::wire::AggOrderBookRecord &decoded,
    std::uint64_t epoch, std::uint64_t sequence, std::uint64_t generation,
    std::uint64_t wall_ns) {
  ++latest.frames;
  if (!latest.present || sequence > latest.sequence) {
    latest.present = true;
    latest.record = decoded;
    latest.epoch = epoch;
    latest.sequence = sequence;
    latest.generation = generation;
    latest.wall_ns = wall_ns;
  }
}

class Runtime {
 public:
  struct StartupPublishIntent {
    std::string key;
    std::string profile;
    std::string symbol;
    FairPricePod pod{};
    std::uint64_t epoch{};
    std::uint64_t sequence{};
    std::uint64_t generation{};
  };

  explicit Runtime(Config config, MarketDataQuery query = {},
                   GatewayFrameSource gateway = {});
  ~Runtime();

  mds::api::Result<void> start();
  void stop();
  [[nodiscard]] Store &store() noexcept { return store_; }
  [[nodiscard]] Catalog &catalog() noexcept { return catalog_; }
  [[nodiscard]] Server *server() noexcept { return server_.get(); }
  void set_history_provider_for_test(HistoryProvider *provider) {
    if (server_) {
      server_->set_history_provider_for_test(provider);
    }
  }
  void enqueue_startup_publish(StartupPublishIntent intent);
  [[nodiscard]] MarketDataRepository *repository() noexcept {
    return repository_.get();
  }

 private:
  struct Input {
    std::string key;
    std::string profile;
    std::string symbol;
    std::string segment;
    Kind kind{Kind::Unknown};
    std::optional<mds::consume::SegmentReader> reader;
    std::uint64_t retry_after_mono_ns{};
    std::uint64_t backoff_ns{50'000'000};
    bool logged_open_failure{};
    bool has_received_record{};
    std::uint64_t last_progress_mono_ns{};
  };

  void loop();
  void refresh_inputs();
  bool open_reader(Input &input, std::uint64_t now_mono_ns);
  void schedule_reopen(Input &input, std::uint64_t now_mono_ns);
  void note_valid_record(Input &input);
  void maybe_reopen_silent_reader(Input &input, std::uint64_t now_mono_ns);
  void drain(Input &input);
  void apply_owned_orderbook(Input &input, OwnedOrderBookFrame &latest);
  void handle_reset(Input &input, std::string_view reason, std::uint64_t epoch,
                    std::uint64_t sequence, std::uint64_t generation);
  void maybe_publish(const std::string &key, std::string_view profile,
                     std::string_view symbol, const FairPricePod &pod);
  void poll_stale();
  void maybe_emit_orderbook_summaries(std::uint64_t now_mono_ns);
  void flush_orderbook_summaries();
  void bootstrap_snapshots();
  void flush_startup_publish();
  [[nodiscard]] bool publisher_owned() const noexcept {
    return thread_.joinable();
  }
  void wait_startup_publish_drained();
  static BookSnapshot book_from_record(
      const utils::md::wire::AggOrderBookRecord &record);

  Config config_;
  std::unique_ptr<MarketDataRepository> repository_;
  GatewayFrameSource gateway_source_;
  Catalog catalog_{};
  Store store_;
  History history_;
  FairPublisher publisher_;
  std::unique_ptr<Recorder> recorder_;
  std::unique_ptr<Server> server_;
  std::unordered_map<std::string, Input> inputs_;
  std::mutex startup_publish_mu_;
  std::condition_variable startup_publish_cv_;
  std::vector<StartupPublishIntent> startup_publish_;
  std::atomic<std::uint64_t> pending_startup_publish_{0};
  std::atomic<bool> bootstrap_complete_{false};
  std::atomic<bool> stop_{false};
  std::thread thread_{};
  OrderbookLogSummary orderbook_summary_{};
};

}  // namespace fairprice
