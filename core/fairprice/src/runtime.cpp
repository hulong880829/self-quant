#include "fairprice/runtime.h"

#include "fairprice/bootstrap.h"
#include "fairprice/log.h"
#include "utils/md/wire_codec.h"
#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <memory>
#include <optional>
#include <unordered_map>

namespace fairprice {
namespace {

std::uint64_t now_wall_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

std::uint64_t now_mono_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

void emit_orderbook_summary_line(const OrderbookLogSummaryLine &line) {
  log_info("orderbook_summary profile=%s symbol=%s frames=%llu "
           "coalesced_drains=%llu coalesced_extra_frames=%llu "
           "target_depth_shortfall_count=%llu actual_bid_depth=%d "
           "actual_ask_depth=%d",
           line.profile.c_str(), line.symbol.c_str(),
           static_cast<unsigned long long>(line.counters.frames),
           static_cast<unsigned long long>(line.counters.coalesced_drains),
           static_cast<unsigned long long>(line.counters.coalesced_extra_frames),
           static_cast<unsigned long long>(
               line.counters.target_depth_shortfall_count),
           line.counters.actual_bid_depth, line.counters.actual_ask_depth);
}

}  // namespace

Runtime::Runtime(Config config, MarketDataQuery query, GatewayFrameSource gateway)
    : config_(std::move(config)),
      repository_(std::make_unique<MarketDataRepository>(
          config_.market_data.clickhouse, std::move(query))),
      gateway_source_(std::move(gateway)),
      store_([&] {
        std::optional<Engine> engine;
        if (config_.fair_price.enabled) {
          std::string error;
          engine = Engine::create(config_.fair_price, error);
        }
        return Store(std::move(engine),
                     config_.snapshot_stale_after_ms * 1'000'000ULL);
      }()),
      history_(repository_.get(), config_.history_resolution_ms * 1'000'000ULL),
      publisher_(config_.shm_prefix, config_.output_ring) {
  if (config_.persistence.enabled) {
    recorder_ = std::make_unique<Recorder>(
        &store_, config_.persistence,
        config_.persistence.database_url);
  }
  server_ = std::make_unique<Server>(config_, &store_, &catalog_, &history_,
                                     recorder_.get());
}

Runtime::~Runtime() { stop(); }

mds::api::Result<void> Runtime::start() {
  log_info("history sample interval=%llums",
           static_cast<unsigned long long>(
               config_.market_data.sample_interval_ms));
  log_info("orderbook depth=%u", config_.market_data.depth);
  log_info("catalog_scope=%s",
           config_.market_data.clickhouse.catalog_scope.c_str());
  auto opened = Catalog::open(
      repository_.get(),
      CatalogSettings{.sample_interval_ms = config_.market_data.sample_interval_ms,
                      .retention_hours = config_.market_data.retention_hours,
                      .depth = config_.market_data.depth});
  if (!opened) {
    return {.error = opened.error, .message = std::move(opened.message)};
  }
  catalog_ = std::move(opened.value);
  if (auto *snapshot = catalog_.snapshot()) {
    store_.reconcile(*snapshot);
  }
  refresh_inputs();
  stop_ = false;
  bootstrap_complete_ = false;
  thread_ = std::thread([this] { loop(); });
  bootstrap_snapshots();
  log_info("bootstrap snapshots done");
  std::fflush(stderr);
  wait_startup_publish_drained();
  bootstrap_complete_ = true;
  if (!server_->start()) {
    stop();
    return {.error = mds::api::ErrorCode::InvalidConfig,
            .message = "http listen failed"};
  }
  log_info("http listen %s:%u", config_.listen_address.c_str(),
           static_cast<unsigned>(config_.port));
  std::fflush(stderr);
  if (recorder_) {
    recorder_->start();
  }
  return {};
}

void Runtime::stop() {
  stop_ = true;
  if (thread_.joinable()) {
    thread_.join();
  }
  if (server_) {
    server_->stop();
  }
  if (recorder_) {
    recorder_->stop();
  }
}

void Runtime::loop() {
  auto last_refresh = now_mono_ns();
  auto last_stale = now_mono_ns();
  while (!stop_.load()) {
    const auto now = now_mono_ns();
    const bool bootstrapped = bootstrap_complete_.load();
    if (bootstrapped &&
        now - last_refresh >= config_.catalog_refresh_interval_ms * 1'000'000ULL) {
      last_refresh = now;
      auto refreshed = catalog_.refresh();
      if (refreshed && refreshed.value) {
        if (auto *snapshot = catalog_.snapshot()) {
          store_.reconcile(*snapshot);
        }
        refresh_inputs();
      }
    }
    flush_startup_publish();
    for (auto &[_, input] : inputs_) {
      if (!input.reader) {
        open_reader(input, now);
      }
      drain(input);
      maybe_reopen_silent_reader(input, now);
    }
    if (bootstrapped && now - last_stale >= 50'000'000ULL) {
      last_stale = now;
      poll_stale();
    }
    bool idle = true;
    for (const auto &[_, input] : inputs_) {
      if (input.reader) {
        idle = false;
        break;
      }
    }
    if (idle) {
      std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
    maybe_emit_orderbook_summaries(now_mono_ns());
  }
  flush_orderbook_summaries();
}

void Runtime::maybe_emit_orderbook_summaries(std::uint64_t now_mono_ns) {
  for (const auto &line : orderbook_summary_.poll(now_mono_ns)) {
    emit_orderbook_summary_line(line);
  }
}

void Runtime::flush_orderbook_summaries() {
  for (const auto &line : orderbook_summary_.flush_on_stop()) {
    emit_orderbook_summary_line(line);
  }
}

void Runtime::refresh_inputs() {
  auto *snapshot = catalog_.snapshot();
  std::unordered_map<std::string, bool> active_keys;
  std::unordered_map<std::string, Input> next;
  if (snapshot) {
    for (const auto &market : snapshot->markets) {
      const auto key = market_key(market.profile, market.symbol);
      active_keys[key] = true;
      auto attach = [&](const std::string &segment, Kind kind) {
        if (segment.empty()) {
          return;
        }
        Input input;
        if (auto it = inputs_.find(segment); it != inputs_.end()) {
          input = std::move(it->second);
        }
        input.key = key;
        input.profile = market.profile;
        input.symbol = market.symbol;
        input.segment = segment;
        input.kind = kind;
        if (!input.reader) {
          open_reader(input, now_mono_ns());
        }
        next.emplace(segment, std::move(input));
      };
      attach(market.bbo_segment, Kind::Bbo);
      attach(market.book_segment, Kind::Book);
    }
  }
  for (auto &[segment, input] : inputs_) {
    if (next.find(segment) != next.end()) {
      continue;
    }
    handle_reset(input, "segment_removed", 0, 0, 0);
  }
  inputs_ = std::move(next);
  publisher_.close_missing(active_keys);
}

bool Runtime::open_reader(Input &input, std::uint64_t now_mono_ns) {
  if (input.reader) {
    return true;
  }
  if (now_mono_ns < input.retry_after_mono_ns) {
    return false;
  }
  auto opened = mds::consume::SegmentReader::open(input.segment);
  if (!opened) {
    if (!input.logged_open_failure) {
      log_warn("attach failed segment=%s profile=%s symbol=%s error=%s",
               input.segment.c_str(), input.profile.c_str(),
               input.symbol.c_str(), opened.message.c_str());
      input.logged_open_failure = true;
    }
    schedule_reopen(input, now_mono_ns);
    return false;
  }
  input.reader = std::move(opened.value);
  input.backoff_ns = 50'000'000;
  input.retry_after_mono_ns = 0;
  if (input.has_received_record) {
    input.last_progress_mono_ns = now_mono_ns;
  }
  if (input.logged_open_failure) {
    log_info("reader recovered segment=%s profile=%s symbol=%s",
             input.segment.c_str(), input.profile.c_str(),
             input.symbol.c_str());
  }
  input.logged_open_failure = false;
  return true;
}

void Runtime::schedule_reopen(Input &input, std::uint64_t now_mono_ns) {
  input.retry_after_mono_ns = now_mono_ns + input.backoff_ns;
  input.backoff_ns = std::min<std::uint64_t>(input.backoff_ns * 2, 1'000'000'000ULL);
}

void Runtime::note_valid_record(Input &input) {
  input.has_received_record = true;
  input.last_progress_mono_ns = now_mono_ns();
}

void Runtime::maybe_reopen_silent_reader(Input &input, std::uint64_t now_mono_ns) {
  if (!input.reader || !input.has_received_record ||
      now_mono_ns < input.retry_after_mono_ns ||
      input.last_progress_mono_ns == 0 ||
      now_mono_ns < input.last_progress_mono_ns) {
    return;
  }
  const auto stale_ns = config_.snapshot_stale_after_ms * 1'000'000ULL;
  const auto silent_ns = now_mono_ns - input.last_progress_mono_ns;
  if (silent_ns < stale_ns) {
    return;
  }
  handle_reset(input, "silent_reader_reopen", 0, 0, 0);
  input.reader.reset();
  schedule_reopen(input, now_mono_ns);
  input.last_progress_mono_ns = now_mono_ns;
  log_warn("silent_reader_reopen segment=%s profile=%s symbol=%s silent_ms=%llu "
           "reason=silent_reader_reopen",
           input.segment.c_str(), input.profile.c_str(), input.symbol.c_str(),
           static_cast<unsigned long long>(silent_ns / 1'000'000ULL));
}

void Runtime::apply_owned_orderbook(Input &input, OwnedOrderBookFrame &latest) {
  if (!latest.present) {
    return;
  }
  FairPricePod published{};
  ComputeStats stats{};
  auto error = store_.apply_book_key(input.key, latest.epoch, latest.sequence,
                                     latest.generation, latest.wall_ns,
                                     book_from_record(latest.record),
                                     &published, &stats);
  orderbook_summary_.note_apply(input.key, input.profile, input.symbol,
                                latest.frames, error.empty() ? &stats : nullptr);
  if (!error.empty() && error != "duplicate") {
    const auto reason =
        error == "generation rollback" ? "generation_rollback" : "seq_gap";
    if (reason == std::string_view("generation_rollback")) {
      log_warn("generation_rollback_hard_reset segment=%s",
               input.segment.c_str());
    }
    handle_reset(input, reason, latest.epoch, latest.sequence, latest.generation);
    return;
  }
  if (error.empty()) {
    maybe_publish(input.key, input.profile, input.symbol, published);
  }
}

void Runtime::drain(Input &input) {
  if (!input.reader) {
    return;
  }
  OwnedOrderBookFrame latest{};
  auto handle_recoverable = [&](mds::api::ErrorCode code, std::string_view message) {
    if (input.kind == Kind::Book) {
      apply_owned_orderbook(input, latest);
      latest = {};
    }
    const auto now = now_mono_ns();
    const auto policy = reader_fault_policy(code);
    if (policy == ReaderFaultPolicy::ResyncInPlace) {
      log_warn("overrun_resync_keep_last_good segment=%s error=%s",
               input.segment.c_str(), std::string(message).c_str());
      auto resync = input.reader->resync_to_latest();
      if (!resync) {
        log_warn("resync failed segment=%s error=%s", input.segment.c_str(),
                 resync.message.c_str());
        input.reader.reset();
        schedule_reopen(input, now);
      }
    } else if (policy == ReaderFaultPolicy::DestroyAndBackoff) {
      log_warn("invalid_handle_reopen_keep_last_good segment=%s error=%s",
               input.segment.c_str(), std::string(message).c_str());
      input.reader.reset();
      schedule_reopen(input, now);
    }
  };
  for (int i = 0; i < 64; ++i) {
    auto viewed = input.reader->try_read_view();
    if (!viewed) {
      handle_recoverable(viewed.error, viewed.message);
      return;
    }
    if (!viewed.value) {
      if (input.kind == Kind::Book) {
        apply_owned_orderbook(input, latest);
      }
      return;
    }
    auto &record = *viewed.value;
    const auto type = static_cast<utils::md::MessageType>(record.type());
    const auto epoch = record.epoch();
    const auto sequence = record.sequence();
    const auto wall = now_wall_ns();
    if (input.kind == Kind::Bbo) {
      if (type != utils::md::MessageType::AggBbo) {
        (void)record.commit();
        handle_reset(input, "illegal_record", epoch, sequence, 0);
        continue;
      }
      utils::md::wire::AggBboRecord decoded{};
      if (utils::md::wire::DecodeAggBbo(record.payload(), decoded) !=
          utils::md::wire::CodecError::Ok) {
        (void)record.commit();
        log_warn("decode_error_hard_reset segment=%s kind=bbo",
                 input.segment.c_str());
        handle_reset(input, "illegal_record", epoch, sequence,
                     decoded.header.book_generation);
        continue;
      }
      auto error = store_.apply_bbo_key(input.key, epoch, sequence,
                                        decoded.header.book_generation, wall,
                                        decoded, nullptr);
      (void)record.commit();
      note_valid_record(input);
      if (!error.empty() && error != "duplicate") {
        const auto reason =
            error == "generation rollback" ? "generation_rollback" : "seq_gap";
        if (reason == std::string_view("generation_rollback")) {
          log_warn("generation_rollback_hard_reset segment=%s kind=bbo",
                   input.segment.c_str());
        }
        handle_reset(input, reason, epoch, sequence,
                     decoded.header.book_generation);
      }
      continue;
    }
    if (type != utils::md::MessageType::AggOrderBook) {
      (void)record.commit();
      log_warn("decode_error_hard_reset segment=%s kind=illegal_record",
               input.segment.c_str());
      handle_reset(input, "illegal_record", epoch, sequence, 0);
      return;
    }
    utils::md::wire::AggOrderBookRecord decoded{};
    if (utils::md::wire::DecodeAggOrderBook(record.payload(), decoded) !=
        utils::md::wire::CodecError::Ok) {
      (void)record.commit();
      log_warn("decode_error_hard_reset segment=%s kind=orderbook",
               input.segment.c_str());
      handle_reset(input, "illegal_record", epoch, sequence, 0);
      return;
    }
    consider_latest_orderbook(latest, decoded, epoch, sequence,
                              decoded.header.book_generation, wall);
    (void)record.commit();
    note_valid_record(input);
  }
  if (input.kind == Kind::Book) {
    apply_owned_orderbook(input, latest);
  }
}

void Runtime::handle_reset(Input &input, std::string_view reason,
                           std::uint64_t epoch, std::uint64_t sequence,
                           std::uint64_t generation) {
  FairPricePod published{};
  if (input.kind == Kind::Book) {
    if (store_.reset_book(input.key, reason, epoch, sequence, generation,
                          &published) &&
        publisher_owned()) {
      maybe_publish(input.key, input.profile, input.symbol, published);
    }
  } else {
    (void)store_.reset_bbo(input.key, reason, epoch, sequence, generation,
                           nullptr);
  }
}

void Runtime::maybe_publish(const std::string &key, std::string_view profile,
                            std::string_view symbol, const FairPricePod &pod) {
  if (!config_.fair_price.enabled) {
    return;
  }
  publisher_.publish(key, profile, symbol, pod);
}

void Runtime::poll_stale() {
  const auto now = now_wall_ns();
  std::unordered_map<std::string, Input *> by_key;
  for (auto &[_, input] : inputs_) {
    auto it = by_key.find(input.key);
    if (it == by_key.end() || input.kind == Kind::Book) {
      by_key[input.key] = &input;
    }
  }
  for (auto &[_, input] : by_key) {
    FairPricePod published{};
    if (store_.mark_stale(input->key, now, &published)) {
      log_info("snapshot stale key=%s", input->key.c_str());
      if (input->kind == Kind::Book) {
        maybe_publish(input->key, input->profile, input->symbol, published);
      }
    }
  }
}

void Runtime::bootstrap_snapshots() {
  auto *snapshot = catalog_.snapshot();
  if (!snapshot) {
    return;
  }
  const auto now = now_wall_ns();
  std::vector<std::string> segments;
  for (const auto &market : snapshot->markets) {
    if (!market.bbo_segment.empty()) {
      segments.push_back(market.bbo_segment);
    }
    if (!market.book_segment.empty()) {
      segments.push_back(market.book_segment);
    }
  }
  GatewayBootstrapClient gateway(config_.market_data.gateway_bootstrap,
                                 gateway_source_);
  const auto learned = gateway.run(segments);
  std::unordered_map<std::string, const GatewayLearnedSnapshot *> by_segment;
  for (const auto &item : learned) {
    by_segment[item.segment] = &item;
  }

  auto side_empty = [](const std::optional<MarketView> &view, Kind kind) {
    if (!view) {
      return true;
    }
    if (kind == Kind::Bbo) {
      return view->bbo.wall_ns == 0 && view->bbo.record.member_count == 0 &&
             view->bbo.record.base_asset[0] == '\0';
    }
    return view->book.wall_ns == 0 && view->book.member_count == 0 &&
           view->book.base_asset[0] == '\0';
  };

  for (const auto &market : snapshot->markets) {
    const auto key = market_key(market.profile, market.symbol);
    if (auto it = by_segment.find(market.bbo_segment);
        it != by_segment.end() && it->second->bbo) {
      const auto *frame = it->second;
      (void)store_.restore_bbo_if_empty(key, frame->ring_epoch,
                                        frame->ring_sequence, frame->generation,
                                        frame->wall_ns, *frame->bbo, now);
    }
    RestoreResult restored{};
    if (auto it = by_segment.find(market.book_segment);
        it != by_segment.end() && it->second->book) {
      const auto *frame = it->second;
      restored = store_.restore_book_if_empty(
          key, frame->ring_epoch, frame->ring_sequence, frame->generation,
          frame->wall_ns, book_from_record(*frame->book), now);
    }
    FairPricePod pod = restored.fair;
    (void)store_.mark_stale(key, now, &pod);
    if (restored.restored) {
      StartupPublishIntent intent;
      intent.key = key;
      intent.profile = market.profile;
      intent.symbol = market.symbol;
      intent.pod = restored.fresh ? restored.fair : pod;
      intent.epoch = restored.epoch;
      intent.sequence = restored.sequence;
      intent.generation = restored.generation;
      enqueue_startup_publish(std::move(intent));
    }
  }

  std::vector<std::string> missing_bbo;
  std::vector<std::string> missing_book;
  for (const auto &market : snapshot->markets) {
    const auto key = market_key(market.profile, market.symbol);
    auto view = store_.copy_view(key);
    if (side_empty(view, Kind::Bbo) && !market.bbo_segment.empty()) {
      missing_bbo.push_back(market.bbo_segment);
    }
    if (side_empty(view, Kind::Book) && !market.book_segment.empty()) {
      missing_book.push_back(market.book_segment);
    }
  }
  const auto bbo_rows = repository_->query_latest_bbo(missing_bbo);
  const auto book_rows = repository_->query_latest_book(missing_book);
  if (!bbo_rows) {
    log_warn("ClickHouse latest BBO fallback failed: %s",
             bbo_rows.message.c_str());
  }
  if (!book_rows) {
    log_warn("ClickHouse latest OrderBook fallback failed: %s",
             book_rows.message.c_str());
  }
  std::unordered_map<std::string, const LatestBboRow *> bbo_by_segment;
  if (bbo_rows) {
    for (const auto &row : bbo_rows.value) {
      bbo_by_segment[row.segment] = &row;
    }
  }
  std::unordered_map<std::string, const LatestBookRow *> book_by_segment;
  if (book_rows) {
    for (const auto &row : book_rows.value) {
      book_by_segment[row.segment] = &row;
    }
  }
  for (const auto &market : snapshot->markets) {
    const auto key = market_key(market.profile, market.symbol);
    if (auto it = bbo_by_segment.find(market.bbo_segment);
        it != bbo_by_segment.end()) {
      const auto *row = it->second;
      log_info("CH restore BBO segment=%s sampled_at_ns=%llu source_wall_ns=%llu",
               row->segment.c_str(),
               static_cast<unsigned long long>(row->sampled_at_ns),
               static_cast<unsigned long long>(row->source_wall_ns));
      (void)store_.restore_bbo_if_empty(key, row->ring_epoch, row->ring_sequence,
                                        row->generation, row->sampled_at_ns,
                                        row->record, now);
    }
    RestoreResult restored{};
    if (auto it = book_by_segment.find(market.book_segment);
        it != book_by_segment.end()) {
      const auto *row = it->second;
      log_info(
          "CH restore OrderBook segment=%s sampled_at_ns=%llu source_wall_ns=%llu",
          row->segment.c_str(),
          static_cast<unsigned long long>(row->sampled_at_ns),
          static_cast<unsigned long long>(row->source_wall_ns));
      restored = store_.restore_book_if_empty(
          key, row->ring_epoch, row->ring_sequence, row->generation,
          row->sampled_at_ns, book_from_record(row->record), now);
    }
    FairPricePod pod = restored.fair;
    (void)store_.mark_stale(key, now, &pod);
    if (!restored.restored) {
      continue;
    }
    StartupPublishIntent intent;
    intent.key = key;
    intent.profile = market.profile;
    intent.symbol = market.symbol;
    intent.pod = restored.fresh ? restored.fair : pod;
    intent.epoch = restored.epoch;
    intent.sequence = restored.sequence;
    intent.generation = restored.generation;
    enqueue_startup_publish(std::move(intent));
  }
}

void Runtime::enqueue_startup_publish(StartupPublishIntent intent) {
  {
    std::lock_guard<std::mutex> lock(startup_publish_mu_);
    startup_publish_.push_back(std::move(intent));
    pending_startup_publish_.fetch_add(1);
  }
  startup_publish_cv_.notify_all();
}

void Runtime::flush_startup_publish() {
  std::vector<StartupPublishIntent> batch;
  {
    std::lock_guard<std::mutex> lock(startup_publish_mu_);
    batch.swap(startup_publish_);
  }
  if (batch.empty()) {
    return;
  }
  for (auto &intent : batch) {
    if (store_.should_discard_startup_publish(intent.key, intent.epoch,
                                              intent.sequence,
                                              intent.generation)) {
      continue;
    }
    maybe_publish(intent.key, intent.profile, intent.symbol, intent.pod);
  }
  pending_startup_publish_.fetch_sub(batch.size());
  startup_publish_cv_.notify_all();
}

void Runtime::wait_startup_publish_drained() {
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(2);
  while (pending_startup_publish_.load() != 0 &&
         std::chrono::steady_clock::now() < deadline) {
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
}

BookSnapshot Runtime::book_from_record(
    const utils::md::wire::AggOrderBookRecord &record) {
  return book_snapshot_from_record(record);
}

}  // namespace fairprice
