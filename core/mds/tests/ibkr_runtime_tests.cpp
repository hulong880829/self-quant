#include "mds/ibkr/ibkr_client.h"
#include "mds/ibkr/ibkr_mailbox.h"
#include "mds/ibkr/ibkr_runtime.h"
#include "mds/ibkr/ibkr_wake.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/wire_codec.h"

#include <atomic>
#include <chrono>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <cstring>
#include <limits>
#include <mutex>
#include <optional>
#include <stdexcept>
#include <string>
#include <string_view>
#include <thread>
#include <unistd.h>
#include <unordered_map>
#include <utility>
#include <vector>

namespace {

void check(bool condition, std::string_view message) {
  if (!condition) {
    throw std::runtime_error(std::string(message));
  }
}

std::uint64_t now_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

template <typename Predicate>
bool wait_until(Predicate predicate, std::chrono::milliseconds budget) {
  const auto deadline = std::chrono::steady_clock::now() + budget;
  while (std::chrono::steady_clock::now() < deadline) {
    if (predicate()) {
      return true;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(5));
  }
  return predicate();
}

mds::ibkr::InstrumentSpec spec_for(std::string symbol, std::int64_t con_id) {
  mds::ibkr::InstrumentSpec spec;
  spec.canonical_symbol = std::move(symbol);
  spec.request_symbol = spec.canonical_symbol;
  spec.con_id = con_id;
  spec.primary_exchange = "NASDAQ";
  spec.price_scale = 4;
  spec.quantity_scale = 4;
  return spec;
}

mds::ibkr::RuntimeOptions options_for(std::string prefix) {
  mds::ibkr::RuntimeOptions options;
  options.port = 4002;
  options.client_id = 3;
  options.connect_timeout_ms = 2000;
  options.reconnect_initial_ms = 20;
  options.reconnect_max_ms = 40;
  options.heartbeat_interval_ms = 20;
  options.subscribe_pacing_per_second = 1000;
  options.shm_prefix = std::move(prefix);
  options.ring_bytes = 1U << 20U;
  options.max_record_bytes = 65536;
  options.max_readers = 4;
  options.unlink_on_shutdown = true;
  options.filter.max_subscriptions = 90;
  return options;
}

class FakeClient final : public mds::ibkr::IbkrClient {
 public:
  std::atomic<int> connects{0};
  std::atomic<int> market_data_requests{0};
  std::atomic<int> metric_requests{0};
  std::atomic<int> metric_cancels{0};
  std::atomic<int> active_metric_requests{0};
  std::atomic<bool> persistent_started_with_metric_probes{false};
  std::atomic<bool> fail_second_contract{false};
  std::atomic<bool> hold_contract_details{false};
  std::atomic<int> fail_market_data_remaining{0};
  std::atomic<int> error_on_persistent_market_data{0};
  std::atomic<bool> emit_average_volume{true};
  std::atomic<bool> emit_market_cap{true};
  std::atomic<bool> emit_reference_price{true};
  std::vector<std::optional<double>> average_volume_lots;
  std::vector<std::optional<double>> market_cap_millions;
  std::vector<std::optional<double>> reference_prices;
  std::vector<std::pair<mds::ibkr::ReferencePriceSource, double>>
      reference_price_events;
  std::string details_sec_type;
  std::int64_t assigned_con_id{0};
  double details_min_tick{0.01};
  mds::ibkr::IbkrContractRequest last_market_request{};

  void set_listener(mds::ibkr::IbkrClientListener *listener) noexcept override {
    listener_.store(listener, std::memory_order_release);
  }

  bool connect(const mds::ibkr::IbkrConnectParams &, std::string &) override {
    {
      std::lock_guard lock(mutex_);
      connected_ = true;
    }
    connects.fetch_add(1, std::memory_order_relaxed);
    if (auto *listener = listener_.load(std::memory_order_acquire)) {
      listener->on_connected();
    }
    return true;
  }

  void disconnect() noexcept override {
    bool notify = false;
    {
      std::lock_guard lock(mutex_);
      notify = connected_;
      connected_ = false;
      wake_ = true;
    }
    cv_.notify_all();
    if (notify) {
      if (auto *listener = listener_.load(std::memory_order_acquire)) {
        listener->on_disconnected();
      }
    }
  }

  void select_live_market_data() override {}

  bool request_contract_details(
      int req_id, const mds::ibkr::IbkrContractRequest &request,
      std::string &) override {
    bool fail = false;
    bool hold = false;
    {
      std::lock_guard lock(req_mu_);
      contract_req_ids.push_back(req_id);
      contract_requests.store(static_cast<int>(contract_req_ids.size()),
                              std::memory_order_release);
      held_requests.insert_or_assign(req_id, request);
      if (fail_second_contract.load(std::memory_order_acquire) &&
          contract_req_ids.size() == 2) {
        fail = true;
      } else if (hold_contract_details.load(std::memory_order_acquire)) {
        held_contract_req_ids.push_back(req_id);
        hold = true;
      }
    }
    auto *listener = listener_.load(std::memory_order_acquire);
    if (listener == nullptr) {
      return false;
    }
    if (fail) {
      listener->on_error(req_id, 200, "No security definition has been found");
      return true;
    }
    if (hold) {
      return true;
    }
    deliver_details(req_id, request);
    return true;
  }

  bool request_market_data(int ticker_id,
                           const mds::ibkr::IbkrContractRequest &request,
                           std::string_view generic_ticks,
                           std::string &) override {
    last_market_request = request;
    if (!generic_ticks.empty()) {
      metric_requests.fetch_add(1, std::memory_order_relaxed);
      active_metric_requests.fetch_add(1, std::memory_order_relaxed);
      if (auto *listener = listener_.load(std::memory_order_acquire)) {
        const auto metric_index =
            static_cast<std::size_t>(ticker_id - 1'000'000);
        if (generic_ticks.find("165") != std::string_view::npos &&
            emit_average_volume.load(std::memory_order_acquire)) {
          if (average_volume_lots.empty()) {
            listener->on_average_volume(ticker_id, "1000");
          } else if (metric_index < average_volume_lots.size() &&
                     average_volume_lots[metric_index].has_value()) {
            listener->on_average_volume(
                ticker_id,
                std::to_string(*average_volume_lots[metric_index]));
          }
        }
        if (generic_ticks.find("258") != std::string_view::npos &&
            emit_market_cap.load(std::memory_order_acquire)) {
          if (market_cap_millions.empty()) {
            listener->on_fundamental_ratios(
                ticker_id, "PEEXCLXOR=20;MKTCAP=500");
          } else if (metric_index < market_cap_millions.size()) {
            if (market_cap_millions[metric_index].has_value()) {
              const auto ratios =
                  "PEEXCLXOR=20;MKTCAP=" +
                  std::to_string(*market_cap_millions[metric_index]);
              listener->on_fundamental_ratios(ticker_id, ratios);
            } else {
              listener->on_fundamental_ratios(ticker_id, "PEEXCLXOR=20");
            }
          }
        }
        if (emit_reference_price.load(std::memory_order_acquire)) {
          if (!reference_price_events.empty()) {
            for (const auto &[source, price] : reference_price_events) {
              listener->on_reference_price(ticker_id, source, price);
            }
          } else if (reference_prices.empty()) {
            listener->on_reference_price(
                ticker_id, mds::ibkr::ReferencePriceSource::Close, 10.0);
          } else if (metric_index < reference_prices.size() &&
                     reference_prices[metric_index].has_value()) {
            listener->on_reference_price(
                ticker_id, mds::ibkr::ReferencePriceSource::Close,
                *reference_prices[metric_index]);
          }
        }
      }
      return true;
    }
    if (active_metric_requests.load(std::memory_order_acquire) != 0) {
      persistent_started_with_metric_probes.store(true,
                                                  std::memory_order_release);
    }
    if (fail_market_data_remaining.load(std::memory_order_acquire) > 0) {
      fail_market_data_remaining.fetch_sub(1, std::memory_order_acq_rel);
      return false;
    }
    const auto injected =
        error_on_persistent_market_data.exchange(0, std::memory_order_acq_rel);
    market_data_requests.fetch_add(1, std::memory_order_relaxed);
    if (injected != 0) {
      if (auto *listener = listener_.load(std::memory_order_acquire)) {
        listener->on_error(0, injected, "injected session error");
      }
    }
    return true;
  }

  void cancel_market_data(int ticker_id) noexcept override {
    if (ticker_id >= 1'000'000) {
      metric_cancels.fetch_add(1, std::memory_order_relaxed);
      active_metric_requests.fetch_sub(1, std::memory_order_relaxed);
    }
  }

  void pump(std::chrono::milliseconds timeout) override {
    std::unique_lock lock(mutex_);
    cv_.wait_for(lock, timeout, [&] { return wake_ || !connected_; });
    wake_ = false;
  }

  void emit_price(int ticker_id, bool bid, double price) {
    if (auto *listener = listener_.load(std::memory_order_acquire)) {
      listener->on_tick_price(ticker_id, bid, price, 11);
    }
  }

  void emit_reference(int ticker_id, mds::ibkr::ReferencePriceSource source,
                      double price) {
    if (auto *listener = listener_.load(std::memory_order_acquire)) {
      listener->on_reference_price(ticker_id, source, price);
    }
  }

  void emit_size(int ticker_id, bool bid, std::string_view size) {
    if (auto *listener = listener_.load(std::memory_order_acquire)) {
      listener->on_tick_size(ticker_id, bid, size, 11);
    }
  }

  void emit_live(int ticker_id) {
    if (auto *listener = listener_.load(std::memory_order_acquire)) {
      listener->on_market_data_type(ticker_id, mds::ibkr::MarketDataKind::Live);
    }
  }

  void emit_book(int ticker_id, double bid, double ask) {
    emit_live(ticker_id);
    emit_price(ticker_id, true, bid);
    emit_price(ticker_id, false, ask);
    emit_size(ticker_id, true, "100");
    emit_size(ticker_id, false, "80");
  }

  void emit_error(int id, int code, std::string_view message = "") {
    if (auto *listener = listener_.load(std::memory_order_acquire)) {
      listener->on_error(id, code, message);
    }
  }

  void emit_contract(int req_id, std::int64_t con_id) {
    mds::ibkr::IbkrContractRequest request;
    {
      std::lock_guard lock(req_mu_);
      const auto found = held_requests.find(req_id);
      if (found == held_requests.end()) {
        return;
      }
      request = found->second;
    }
    if (con_id > 0) {
      request.con_id = con_id;
    }
    deliver_details(req_id, request);
  }

  void release_held_contracts() {
    std::vector<std::pair<int, mds::ibkr::IbkrContractRequest>> pending;
    {
      std::lock_guard lock(req_mu_);
      hold_contract_details.store(false, std::memory_order_release);
      pending.reserve(held_contract_req_ids.size());
      for (const auto req_id : held_contract_req_ids) {
        pending.emplace_back(req_id, held_requests[req_id]);
      }
      held_contract_req_ids.clear();
    }
    for (const auto &[req_id, request] : pending) {
      deliver_details(req_id, request);
    }
  }

  std::vector<int> contract_ids() const {
    std::lock_guard lock(req_mu_);
    return contract_req_ids;
  }

 private:
  void deliver_details(int req_id,
                       const mds::ibkr::IbkrContractRequest &request) {
    auto *listener = listener_.load(std::memory_order_acquire);
    if (listener == nullptr) {
      return;
    }
    mds::ibkr::IbkrContractDetails details;
    details.con_id = request.con_id > 0 ? request.con_id : assigned_con_id;
    details.symbol = request.symbol;
    details.local_symbol = request.symbol;
    details.exchange = request.exchange;
    details.sec_type = details_sec_type.empty()
                           ? (request.sec_type.empty() ? "STK" : request.sec_type)
                           : details_sec_type;
    details.currency = request.currency.empty() ? "USD" : request.currency;
    details.primary_exchange = request.primary_exchange;
    details.min_tick = details_min_tick;
    listener->on_contract_details(req_id, details);
    listener->on_contract_details_end(req_id);
  }

  std::atomic<mds::ibkr::IbkrClientListener *> listener_{nullptr};
  std::mutex mutex_;
  mutable std::mutex req_mu_;
  std::condition_variable cv_;
  bool connected_{false};
  bool wake_{false};
  std::atomic<int> contract_requests{0};
  std::vector<int> contract_req_ids;
  std::vector<int> held_contract_req_ids;
  std::unordered_map<int, mds::ibkr::IbkrContractRequest> held_requests;
};

struct Seen {
  int catalogs{};
  int bbos{};
  utils::md::InstrumentId instrument_id{};
  std::int64_t bid_price{};
  std::int64_t ask_price{};
  std::int64_t bid_size{};
  std::int64_t ask_size{};
  std::uint32_t book_generation{};
  std::uint64_t exchange_ts_ns{1};
  std::uint64_t source_seq{};
  std::uint16_t flags{};
  std::uint8_t state{255};
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  std::string symbol;
  std::string venue_symbol;
  std::string base;
  std::string quote;
  std::uint8_t price_scale{};
};

class RingTail {
 public:
  explicit RingTail(std::string name) {
    mds::transport::RingOptions options;
    options.name = std::move(name);
    options.create = false;
    options.ring_bytes = 1U << 20U;
    options.max_record_bytes = 65536;
    options.max_readers = 4;
    auto opened = mds::transport::SharedRing::open(options);
    check(static_cast<bool>(opened), opened.message);
    ring_ = std::move(opened.value);
    auto registered = ring_.register_reader(
        mds::transport::process_start_marker(
            static_cast<std::uint32_t>(::getpid())),
        now_ns());
    check(static_cast<bool>(registered), registered.message);
    reader_ = registered.value;
  }

  void drain(Seen &seen) {
    for (;;) {
      auto read = ring_.read(reader_);
      if (!read) {
        return;
      }
      utils::md::wire::InstrumentCatalogRecord catalog{};
      if (utils::md::wire::DecodeInstrumentCatalog(read.value->payload,
                                                   catalog) ==
          utils::md::wire::CodecError::Ok) {
        ++seen.catalogs;
        seen.instrument_id = catalog.catalog.instrument_id;
        seen.venue = catalog.catalog.venue;
        seen.product = catalog.catalog.product_type;
        seen.symbol = catalog.catalog.canonical_symbol.data();
        seen.venue_symbol = catalog.catalog.venue_symbol.data();
        seen.base = catalog.catalog.base_asset.data();
        seen.quote = catalog.catalog.quote_asset.data();
        seen.price_scale = catalog.catalog.price_scale;
      }
      utils::md::wire::BboRecord bbo{};
      if (utils::md::wire::DecodeBbo(read.value->payload, bbo) ==
          utils::md::wire::CodecError::Ok) {
        ++seen.bbos;
        seen.instrument_id = bbo.header.instrument_id;
        seen.bid_price = bbo.bid_price;
        seen.ask_price = bbo.ask_price;
        seen.bid_size = bbo.bid_quantity;
        seen.ask_size = bbo.ask_quantity;
        seen.book_generation = bbo.header.book_generation;
        seen.exchange_ts_ns = bbo.header.exchange_ts_ns;
        seen.source_seq = bbo.header.source_seq;
        seen.flags = bbo.header.flags;
        seen.state = bbo.header.state;
      }
      check(static_cast<bool>(read.value.commit()), "failed to commit record");
    }
  }

 private:
  mds::transport::SharedRing ring_;
  mds::transport::ReaderHandle reader_{};
};

std::string prefix_for(std::string_view name) {
  return "/sq.ibkr." + std::to_string(::getpid()) + "." + std::string(name);
}

void test_wake_seqlock_and_errors() {
  mds::ibkr::WakeGate gate;
  for (int index = 0; index < 100; ++index) {
    (void)gate.arm();
  }
  check(gate.syscalls() == 1, "eventfd wake was not coalesced");
  gate.clear();
  check(gate.arm() && gate.syscalls() == 2, "clear did not re-arm the wake");

  std::atomic<bool> dirty{false};
  std::atomic<std::uint64_t> overwrites{0};
  mds::ibkr::mark_dirty(dirty, overwrites);
  mds::ibkr::mark_dirty(dirty, overwrites);
  check(dirty.load() && overwrites.load() == 1,
        "dirty notification did not count an overwrite");

  mds::ibkr::SeqlockMailbox mailbox;
  std::atomic<bool> run{true};
  std::atomic<bool> torn{false};
  std::thread writer([&] {
    for (std::int64_t value = 1; value <= 100000; ++value) {
      mds::ibkr::BboSnapshot snapshot;
      snapshot.bid_price = value;
      snapshot.ask_price = value;
      snapshot.bid_size = value;
      snapshot.ask_size = value;
      snapshot.publishable = true;
      mailbox.write(snapshot);
    }
    run.store(false, std::memory_order_release);
  });
  std::thread reader([&] {
    mds::ibkr::BboSnapshot snapshot;
    while (run.load(std::memory_order_acquire)) {
      if (!mailbox.read(snapshot)) {
        continue;
      }
      if (snapshot.bid_price != snapshot.ask_price ||
          snapshot.bid_size != snapshot.ask_size) {
        torn.store(true, std::memory_order_relaxed);
      }
    }
  });
  writer.join();
  reader.join();
  check(!torn.load(), "seqlock snapshot was torn");

  using mds::ibkr::IbkrErrorClass;
  check(mds::ibkr::classify_ibkr_error(1100) == IbkrErrorClass::ConnectionLost,
        "1100 was not connection lost");
  check(mds::ibkr::classify_ibkr_error(1101) ==
            IbkrErrorClass::ConnectionRestoredDataLost,
        "1101 was not data-lost restoration");
  check(mds::ibkr::classify_ibkr_error(1102) ==
            IbkrErrorClass::ConnectionRestoredDataMaintained,
        "1102 was not data-maintained restoration");
  check(mds::ibkr::classify_ibkr_error(200) == IbkrErrorClass::Contract,
        "200 was not a contract error");
  check(mds::ibkr::classify_ibkr_error(354) == IbkrErrorClass::MarketDataDenied,
        "354 was not market-data denied");
  check(mds::ibkr::classify_ibkr_error(2104) == IbkrErrorClass::Informational,
        "2104 was not informational");
  check(mds::ibkr::classify_ibkr_error(999) == IbkrErrorClass::Ignored,
        "unknown IB error was not ignored");
}

void test_catalog_bbo_and_coalesce() {
  FakeClient client;
  auto options = options_for(prefix_for("happy"));
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "runtime did not become live");

  RingTail tail(runtime.segment_name());
  Seen seen;
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.catalogs > 0;
            },
            std::chrono::seconds(2)),
        "late reader did not receive a republished catalog");
  check(seen.venue == utils::md::Venue::Ibkr &&
            seen.product == utils::md::ProductType::Equity &&
            seen.symbol == "AAPL" && seen.venue_symbol == "265598" &&
            seen.instrument_id == runtime.instrument_id(0),
        "catalog identity was not ibkr|equity|AAPL|265598");

  client.emit_book(1, 10.25, 10.5);
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.bbos > 0;
            },
            std::chrono::seconds(2)),
        "publishable BBO was not written");
  check(seen.bid_price == 102500 && seen.ask_price == 105000 &&
            seen.bid_size == 1000000 && seen.ask_size == 800000 &&
            seen.exchange_ts_ns == 0 &&
            (seen.flags & utils::md::wire::kBboOriginTickerStream) != 0 &&
            seen.state == static_cast<std::uint8_t>(utils::md::BookState::Live) &&
            seen.book_generation == runtime.generation() && seen.source_seq >= 1,
        "BBO header or fixed-point values were wrong");

  const auto published_before = runtime.metrics().publish_count;
  constexpr int kBurst = 20000;
  for (int index = 0; index < kBurst; ++index) {
    client.emit_price(1, true, 190.5 + static_cast<double>(index % 5) * 0.0001);
  }
  check(wait_until(
            [&] {
              return runtime.metrics().mailbox_overwrite_count > 0 &&
                     runtime.metrics().wake_syscall_count <
                         static_cast<std::uint64_t>(kBurst);
            },
            std::chrono::seconds(1)),
        "mailbox overwrite or wake coalescing was not observed");
  check(runtime.metrics().publish_count <
            published_before + static_cast<std::uint64_t>(kBurst),
        "every coalesced tick was published");
  runtime.stop();
}

void test_disconnect_and_reconnect() {
  FakeClient client;
  auto options = options_for(prefix_for("reconnect"));
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "runtime did not become live before disconnect");
  RingTail tail(runtime.segment_name());
  Seen seen;
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.catalogs > 0;
            },
            std::chrono::seconds(2)),
        "catalog missing before disconnect");
  client.emit_book(1, 10.25, 10.5);
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.bbos > 0 && seen.bid_price == 102500;
            },
            std::chrono::seconds(2)),
        "initial BBO missing");
  tail.drain(seen);
  const auto published = runtime.metrics().publish_count;
  const auto generation = runtime.generation();
  client.emit_error(0, 1100);
  check(wait_until([&] { return runtime.generation() == generation + 1; },
                   std::chrono::seconds(2)),
        "connection lost did not advance book generation once");
  const auto connects = client.connects.load();
  check(wait_until([&] { return runtime.live() && client.connects.load() > connects; },
                   std::chrono::seconds(2)) ||
            (runtime.live() && client.connects.load() >= 2),
        "runtime did not reconnect");
  std::this_thread::sleep_for(std::chrono::milliseconds(80));
  Seen after;
  tail.drain(after);
  check(after.bbos == 0 || after.bid_price != 102500 ||
            after.book_generation == runtime.generation(),
        "old BBO was published after disconnect");
  check(runtime.metrics().publish_count == published || after.bbos == 0 ||
            after.book_generation != generation,
        "stale generation was published after disconnect");
  client.emit_book(1, 191.25, 191.5);
  Seen fresh;
  check(wait_until(
            [&] {
              tail.drain(fresh);
              return fresh.bbos > 0;
            },
            std::chrono::seconds(2)),
        "post-reconnect BBO was not published");
  check(fresh.bid_price == 1912500 && fresh.ask_price == 1915000 &&
            fresh.book_generation == runtime.generation() &&
            fresh.book_generation != generation,
        "reconnected BBO did not use the new generation");
  runtime.stop();
}

void test_errors_do_not_reconnect_storm() {
  FakeClient client;
  client.fail_second_contract.store(true);
  auto options = options_for(prefix_for("errors"));
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  options.subscriptions.push_back(spec_for("MSFT", 272093));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "partial contract failure prevented the session from going live");
  check(client.market_data_requests.load() == 1,
        "failed contract was not isolated from subscription");
  std::this_thread::sleep_for(std::chrono::milliseconds(150));
  check(runtime.live() && runtime.generation() == 1 &&
            client.connects.load() == 1,
        "a single contract error caused a reconnect");

  RingTail tail(runtime.segment_name());
  Seen seen;
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.catalogs > 0 && seen.symbol == "AAPL";
            },
            std::chrono::seconds(2)),
        "healthy symbol catalog was not republished");
  client.emit_book(2, 400.0, 400.1);
  client.emit_book(1, 10.25, 10.5);
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.bbos > 0;
            },
            std::chrono::seconds(2)),
        "healthy symbol BBO was not published");
  check(seen.instrument_id == runtime.instrument_id(0) &&
            seen.bid_price == 102500,
        "isolated symbol leaked into the published BBO");

  const auto generation = runtime.generation();
  client.emit_error(-1, 2104);
  std::this_thread::sleep_for(std::chrono::milliseconds(100));
  check(runtime.generation() == generation && runtime.live() &&
            runtime.metrics().informational_errors >= 1 &&
            client.connects.load() == 1,
        "informational farm status caused a reconnect");

  const auto published = runtime.metrics().publish_count;
  client.emit_error(1, 354);
  client.emit_book(1, 200.0, 200.1);
  std::this_thread::sleep_for(std::chrono::milliseconds(150));
  check(runtime.live() && runtime.generation() == generation &&
            client.connects.load() == 1 &&
            runtime.metrics().publish_count == published,
        "market-data permission error caused a reconnect or a fake BBO");
  runtime.stop();
}

void test_cmdty_and_cash_keep_canonical_and_resolved_contract() {
  FakeClient cash;
  cash.assigned_con_id = 15016062;
  cash.details_min_tick = 0.00001;
  auto cash_options = options_for(prefix_for("usdjpy"));
  mds::ibkr::InstrumentSpec fx;
  fx.canonical_symbol = "USDJPY";
  fx.request_symbol = "USD";
  fx.sec_type = "CASH";
  fx.exchange = "IDEALPRO";
  fx.currency = "JPY";
  fx.price_scale = std::nullopt;
  fx.quantity_scale = 4;
  cash_options.subscriptions.push_back(fx);
  mds::ibkr::IbkrRuntime cash_runtime(cash_options, cash);
  std::string error;
  check(cash_runtime.start(error), error);
  check(wait_until([&] { return cash_runtime.live(); }, std::chrono::seconds(2)),
        "USDJPY session did not become live");
  RingTail cash_tail(cash_runtime.segment_name());
  Seen cash_seen;
  check(wait_until(
            [&] {
              cash_tail.drain(cash_seen);
              return cash_seen.symbol == "USDJPY";
            },
            std::chrono::seconds(2)),
        "USDJPY catalog was not published");
  check(cash_seen.base == "USD" && cash_seen.quote == "JPY" &&
            cash_seen.venue_symbol == "15016062" && cash_seen.price_scale == 5,
        "USDJPY catalog did not keep the pair and derived scale");
  check(cash.last_market_request.sec_type == "CASH" &&
            cash.last_market_request.symbol == "USD" &&
            cash.last_market_request.currency == "JPY" &&
            cash.last_market_request.con_id == 15016062,
        "USDJPY market data did not use the resolved cash contract");
  cash_runtime.stop();

  FakeClient metal;
  metal.assigned_con_id = 69067924;
  metal.details_sec_type = "CASH";
  metal.details_min_tick = 0.01;
  auto metal_options = options_for(prefix_for("xau"));
  mds::ibkr::InstrumentSpec cmdty;
  cmdty.canonical_symbol = "XAUUSD";
  cmdty.request_symbol = "XAUUSD";
  cmdty.sec_type = "CMDTY";
  cmdty.exchange = "SMART";
  cmdty.currency = "USD";
  cmdty.price_scale = std::nullopt;
  cmdty.quantity_scale = 4;
  metal_options.subscriptions.push_back(cmdty);
  mds::ibkr::IbkrRuntime metal_runtime(metal_options, metal);
  check(metal_runtime.start(error), error);
  check(wait_until([&] { return metal_runtime.live(); }, std::chrono::seconds(2)),
        "XAUUSD session did not become live");
  check(metal.last_market_request.sec_type == "CASH" &&
            metal.last_market_request.symbol == "XAUUSD" &&
            metal.last_market_request.con_id == 69067924,
        "CMDTY market data was not switched to the resolved CASH contract");
  metal_runtime.stop();
}

void test_metric_probe_filters_before_persistent_subscriptions() {
  FakeClient client;
  client.average_volume_lots = {10.0, 30.0, 20.0};
  auto options = options_for(prefix_for("selection"));
  options.filter.max_subscriptions = 2;
  options.filter.min_average_volume_90d = 1500.0;
  options.subscriptions.push_back(spec_for("AAPL", 1));
  options.subscriptions.push_back(spec_for("MSFT", 2));
  options.subscriptions.push_back(spec_for("NVDA", 3));

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "filtered runtime did not become live");
  check(client.metric_requests.load() == 3 &&
            client.metric_cancels.load() == 3 &&
            !client.persistent_started_with_metric_probes.load(),
        "metric probes were not all requested and cancelled");
  check(client.market_data_requests.load() == 2,
        "persistent subscriptions did not respect the filtered limit");
  const auto metrics = runtime.metrics();
  check(metrics.metric_success_average_volume == 3 &&
            metrics.filtered_below_threshold == 1 &&
            metrics.selected_subscriptions == 2,
        "runtime selection metrics were incorrect");

  const auto probe_count = client.metric_requests.load();
  client.emit_error(0, 1100);
  check(wait_until(
            [&] {
              return runtime.live() && client.connects.load() >= 2;
            },
            std::chrono::seconds(2)),
        "filtered runtime did not reconnect");
  check(client.metric_requests.load() == probe_count,
        "reconnect unexpectedly re-probed the selected universe");
  runtime.stop();
}

void test_turnover_persists_all_stocks_before_selection_with_price_priority() {
  FakeClient client;
  client.average_volume_lots = {10.0, 30.0, 20.0};
  client.reference_price_events = {
      {mds::ibkr::ReferencePriceSource::DelayedLast, 50.0},
      {mds::ibkr::ReferencePriceSource::Last, 40.0},
      {mds::ibkr::ReferencePriceSource::DelayedClose, 20.0},
      {mds::ibkr::ReferencePriceSource::Close, 10.0},
      {mds::ibkr::ReferencePriceSource::Last, 100.0},
  };
  auto options = options_for(prefix_for("turnover"));
  options.filter.max_subscriptions = 1;
  options.subscriptions.push_back(spec_for("AAPL", 1));
  options.subscriptions.push_back(spec_for("MSFT", 2));
  options.subscriptions.push_back(spec_for("NVDA", 3));
  std::vector<mds::ibkr::InstrumentTurnoverMetric> persisted;
  options.persist_turnover_metrics =
      [&](const std::vector<mds::ibkr::InstrumentTurnoverMetric> &metrics,
          std::size_t &updated, std::string &) {
        persisted = metrics;
        updated = metrics.size();
        return true;
      };

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "turnover runtime did not become live");
  check(persisted.size() == 3,
        "turnover persistence was incorrectly limited by selection");
  check(persisted[0].canonical_symbol == "AAPL" &&
            std::fabs(persisted[0].turnover_usd_90d - 10000.0) < 0.001 &&
            persisted[1].canonical_symbol == "MSFT" &&
            std::fabs(persisted[1].turnover_usd_90d - 30000.0) < 0.001 &&
            persisted[2].canonical_symbol == "NVDA" &&
            std::fabs(persisted[2].turnover_usd_90d - 20000.0) < 0.001,
        "turnover did not use the highest-priority close price");
  const auto metrics = runtime.metrics();
  check(metrics.metric_success_reference_price == 3 &&
            metrics.metric_missing_reference_price == 0 &&
            metrics.metric_persisted_turnover == 3 &&
            metrics.selected_subscriptions == 1 &&
            client.market_data_requests.load() == 1,
        "turnover metrics or pre-selection persistence order was incorrect");
  runtime.stop();
}

void test_metric_probe_waits_for_reference_price() {
  FakeClient client;
  client.emit_reference_price.store(false);
  auto options = options_for(prefix_for("turnover-wait"));
  options.subscriptions.push_back(spec_for("AAPL", 1));
  std::vector<mds::ibkr::InstrumentTurnoverMetric> persisted;
  options.persist_turnover_metrics =
      [&](const std::vector<mds::ibkr::InstrumentTurnoverMetric> &metrics,
          std::size_t &updated, std::string &) {
        persisted = metrics;
        updated = metrics.size();
        return true;
      };

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return client.metric_requests.load() == 1; },
                   std::chrono::seconds(1)),
        "metric probe did not start");
  std::this_thread::sleep_for(std::chrono::milliseconds(100));
  check(runtime.state() == mds::ibkr::SessionState::ProbingMetrics &&
            !runtime.live(),
        "average volume completed the probe before a reference price arrived");
  client.emit_reference(1'000'000, mds::ibkr::ReferencePriceSource::Close,
                        25.0);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "reference price did not release the metric probe");
  check(persisted.size() == 1 &&
            std::fabs(persisted[0].turnover_usd_90d - 2500000.0) < 0.001,
        "delayed reference price was not persisted");
  runtime.stop();
}

void test_missing_price_and_persist_failure_do_not_block_live() {
  {
    FakeClient client;
    client.emit_reference_price.store(false);
    auto options = options_for(prefix_for("turnover-no-price"));
    options.subscriptions.push_back(spec_for("AAPL", 1));
    std::atomic<int> persist_calls{0};
    options.persist_turnover_metrics =
        [&](const std::vector<mds::ibkr::InstrumentTurnoverMetric> &,
            std::size_t &, std::string &) {
          persist_calls.fetch_add(1);
          return true;
        };
    mds::ibkr::IbkrRuntime runtime(options, client);
    std::string error;
    check(runtime.start(error), error);
    check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(7)),
          "missing reference price blocked the runtime");
    const auto metrics = runtime.metrics();
    check(persist_calls.load() == 0 &&
              metrics.metric_missing_reference_price == 1 &&
              metrics.metric_persisted_turnover == 0,
          "missing reference price was persisted or not counted");
    runtime.stop();
  }
  {
    FakeClient client;
    auto options = options_for(prefix_for("turnover-write-failure"));
    options.subscriptions.push_back(spec_for("AAPL", 1));
    std::atomic<int> persist_calls{0};
    options.persist_turnover_metrics =
        [&](const std::vector<mds::ibkr::InstrumentTurnoverMetric> &,
            std::size_t &updated, std::string &error) {
          persist_calls.fetch_add(1);
          updated = 0;
          error = "injected write failure";
          return false;
        };
    mds::ibkr::IbkrRuntime runtime(options, client);
    std::string error;
    check(runtime.start(error), error);
    check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
          "turnover persistence failure blocked the runtime");
    check(persist_calls.load() == 1 &&
              runtime.metrics().metric_persisted_turnover == 0,
          "turnover persistence failure was not reported correctly");
    runtime.stop();
  }
}

void test_market_cap_probe_converts_millions_and_rejects_missing() {
  FakeClient client;
  client.market_cap_millions = {1000.0, std::nullopt, 3000.0};
  auto options = options_for(prefix_for("market-cap"));
  options.filter.min_market_cap_usd = 2.0e9;
  options.filter.max_subscriptions = 3;
  options.subscriptions.push_back(spec_for("AAPL", 1));
  options.subscriptions.push_back(spec_for("MSFT", 2));
  options.subscriptions.push_back(spec_for("NVDA", 3));

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "market-cap runtime did not become live");
  const auto metrics = runtime.metrics();
  check(metrics.metric_success_market_cap == 2 &&
            metrics.filtered_below_threshold == 1 &&
            metrics.missing_metric_rejected == 1 &&
            metrics.selected_subscriptions == 1 &&
            !metrics.market_cap_filter_degraded &&
            client.market_data_requests.load() == 1,
        "MKTCAP conversion or missing-value policy was incorrect");
  runtime.stop();
}

void test_market_cap_canaries_degrade_when_source_is_unavailable() {
  FakeClient client;
  client.emit_market_cap.store(false);
  auto options = options_for(prefix_for("market-cap-missing"));
  options.filter.min_market_cap_usd = 2.0e9;
  options.filter.max_subscriptions = 2;
  options.subscriptions.push_back(spec_for("AAPL", 1));
  options.subscriptions.push_back(spec_for("MSFT", 2));
  options.subscriptions.push_back(spec_for("NVDA", 3));

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(8)),
        "missing-market-cap runtime did not become live");
  const auto metrics = runtime.metrics();
  check(client.metric_requests.load() == 3 &&
            client.metric_cancels.load() == 3 &&
            metrics.metric_success_market_cap == 0 &&
            metrics.market_cap_filter_degraded &&
            metrics.selected_subscriptions == 2 &&
            client.market_data_requests.load() == 2,
        "market-cap canaries did not degrade when tick 258 was unavailable");
  runtime.stop();
}

void test_partial_market_cap_canary_keeps_filter() {
  FakeClient client;
  client.market_cap_millions = {std::nullopt, std::nullopt, 3000.0,
                                std::nullopt, std::nullopt, std::nullopt};
  auto options = options_for(prefix_for("market-cap-partial"));
  options.filter.min_market_cap_usd = 2.0e9;
  options.filter.max_subscriptions = 6;
  for (int index = 1; index <= 6; ++index) {
    options.subscriptions.push_back(
        spec_for("S" + std::to_string(index), index));
  }

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "partial market-cap runtime did not become live");
  const auto metrics = runtime.metrics();
  check(client.metric_requests.load() == 6 &&
            metrics.metric_success_market_cap == 1 &&
            metrics.missing_metric_rejected == 5 &&
            metrics.selected_subscriptions == 1 &&
            !metrics.market_cap_filter_degraded,
        "one available market-cap canary did not keep the market-cap filter");
  runtime.stop();
}

void test_non_stock_bypasses_threshold_and_uses_cap() {
  FakeClient client;
  client.average_volume_lots = {std::nullopt, 10.0, 30.0};
  auto options = options_for(prefix_for("non-stock"));
  options.filter.min_average_volume_90d = 1500.0;
  options.filter.max_subscriptions = 2;
  mds::ibkr::InstrumentSpec fx;
  fx.canonical_symbol = "USDJPY";
  fx.request_symbol = "USD";
  fx.con_id = 50;
  fx.sec_type = "CASH";
  fx.exchange = "IDEALPRO";
  fx.currency = "JPY";
  fx.price_scale = std::nullopt;
  options.subscriptions.push_back(fx);
  options.subscriptions.push_back(spec_for("AAPL", 1));
  options.subscriptions.push_back(spec_for("MSFT", 2));

  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "non-stock runtime did not become live");
  const auto metrics = runtime.metrics();
  check(metrics.non_stock_exempt == 1 && metrics.selected_non_stock == 1 &&
            metrics.selected_stock == 1 &&
            metrics.filtered_below_threshold == 1 &&
            metrics.selected_subscriptions == 2 &&
            client.market_data_requests.load() == 2,
        "non-stock contracts were not reserved ahead of filtered stocks");
  runtime.stop();
}

class StateWatch {
 public:
  explicit StateWatch(mds::ibkr::IbkrRuntime &runtime)
      : thread_([&] {
          while (!stop_.load(std::memory_order_acquire)) {
            if (runtime.generation() == 1) {
              const auto state = runtime.state();
              if (state == mds::ibkr::SessionState::ProbingMetrics) {
                saw_probe_at_generation_one.store(true);
              }
              if (state == mds::ibkr::SessionState::Subscribing) {
                saw_subscribe_at_generation_one.store(true);
              }
              if (runtime.live()) {
                saw_live_at_generation_one.store(true);
              }
            }
            std::this_thread::sleep_for(std::chrono::milliseconds(1));
          }
        }) {}

  ~StateWatch() {
    stop_.store(true, std::memory_order_release);
    if (thread_.joinable()) {
      thread_.join();
    }
  }

  std::atomic<bool> saw_probe_at_generation_one{false};
  std::atomic<bool> saw_subscribe_at_generation_one{false};
  std::atomic<bool> saw_live_at_generation_one{false};

 private:
  std::atomic<bool> stop_{false};
  std::thread thread_;
};

void test_data_maintained_does_not_rebuild_session() {
  FakeClient client;
  auto options = options_for(prefix_for("1102"));
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "runtime did not become live before 1102");
  const auto connects = client.connects.load();
  const auto requests = client.market_data_requests.load();
  const auto generation = runtime.generation();
  client.emit_error(0, 1102, "data maintained");
  std::this_thread::sleep_for(std::chrono::milliseconds(120));
  check(runtime.live() && client.connects.load() == connects &&
            client.market_data_requests.load() == requests &&
            runtime.generation() == generation,
        "1102 rebuilt the session or advanced the book generation");
  RingTail tail(runtime.segment_name());
  Seen seen;
  client.emit_book(1, 10.25, 10.5);
  check(wait_until(
            [&] {
              tail.drain(seen);
              return seen.bbos > 0 && seen.book_generation == generation;
            },
            std::chrono::seconds(2)),
        "BBO was not published after 1102");
  runtime.stop();
}

void test_data_lost_rebuilds_once() {
  FakeClient client;
  auto options = options_for(prefix_for("1101"));
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "runtime did not become live before 1101");
  const auto connects = client.connects.load();
  const auto requests = client.market_data_requests.load();
  const auto generation = runtime.generation();
  const auto ids_before = client.contract_ids();
  client.emit_error(0, 1101, "data lost");
  check(wait_until(
            [&] {
              return runtime.live() && client.connects.load() == connects + 1 &&
                     runtime.generation() == generation + 1;
            },
            std::chrono::seconds(2)),
        "1101 did not rebuild the session exactly once");
  check(runtime.generation() == generation + 1 &&
            client.market_data_requests.load() == requests + 1,
        "1101 advanced generation more than once or skipped resubscribe");
  const auto ids = client.contract_ids();
  check(ids_before.size() == 1 && ids.size() == 2 &&
            ids[0] >= 2'000'000 && ids[1] == ids[0] + 1,
        "contract reqIds were not monotonic across the 1101 retry");
  runtime.stop();
}

void test_contract_timeout_retries_without_isolating() {
  FakeClient client;
  client.hold_contract_details.store(true);
  auto options = options_for(prefix_for("timeout"));
  options.connect_timeout_ms = 40;
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  StateWatch watch(runtime);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.generation() >= 2; },
                   std::chrono::seconds(2)),
        "contract-details timeout did not retry the session");
  check(!watch.saw_probe_at_generation_one.load() &&
            !watch.saw_subscribe_at_generation_one.load() &&
            !watch.saw_live_at_generation_one.load(),
        "contract-details timeout entered a later session phase");
  client.release_held_contracts();
  check(wait_until(
            [&] {
              return runtime.live() &&
                     runtime.metrics().active_subscriptions == 1 &&
                     client.market_data_requests.load() >= 1;
            },
            std::chrono::seconds(2)),
        "AAPL was not resubscribed after the contract-details timeout");
  runtime.stop();
}

void test_session_error_during_resolving_or_subscribing_does_not_reach_live() {
  FakeClient resolving_client;
  resolving_client.hold_contract_details.store(true);
  auto resolving_options = options_for(prefix_for("resolve-1100"));
  resolving_options.connect_timeout_ms = 2000;
  resolving_options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime resolving(resolving_options, resolving_client);
  StateWatch resolving_watch(resolving);
  std::string error;
  check(resolving.start(error), error);
  check(wait_until(
            [&] {
              return resolving.state() ==
                     mds::ibkr::SessionState::ResolvingContracts;
            },
            std::chrono::seconds(2)),
        "runtime did not reach contract resolving");
  resolving_client.emit_error(-1, 1100, "connectivity lost");
  check(wait_until([&] { return resolving.generation() == 2 && !resolving.live(); },
                   std::chrono::seconds(2)),
        "1100 during resolving did not end the phase with one generation");
  check(!resolving_watch.saw_live_at_generation_one.load() &&
            resolving.generation() == 2,
        "1100 during resolving entered Live or advanced generation twice");
  resolving_client.release_held_contracts();
  check(wait_until([&] { return resolving.live() && resolving.generation() == 2; },
                   std::chrono::seconds(2)),
        "runtime did not recover at the post-1100 generation");
  resolving.stop();

  FakeClient subscribing_client;
  subscribing_client.error_on_persistent_market_data.store(1101);
  auto subscribing_options = options_for(prefix_for("subscribe-1101"));
  subscribing_options.subscriptions.push_back(spec_for("AAPL", 265598));
  subscribing_options.subscriptions.push_back(spec_for("MSFT", 272093));
  mds::ibkr::IbkrRuntime subscribing(subscribing_options, subscribing_client);
  StateWatch subscribing_watch(subscribing);
  check(subscribing.start(error), error);
  check(wait_until(
            [&] {
              return subscribing.live() && subscribing.generation() == 2 &&
                     subscribing_client.market_data_requests.load() >= 2;
            },
            std::chrono::seconds(2)),
        "1101 during subscribing did not retry once and then go live");
  check(!subscribing_watch.saw_live_at_generation_one.load() &&
            subscribing.generation() == 2,
        "1101 during subscribing entered Live before one generation advance");
  subscribing.stop();
}

void test_market_data_send_failure_retries_without_isolate() {
  FakeClient client;
  client.fail_market_data_remaining.store(1);
  auto options = options_for(prefix_for("md-send"));
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  StateWatch watch(runtime);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until(
            [&] {
              return runtime.live() && runtime.generation() == 2 &&
                     runtime.metrics().active_subscriptions == 1;
            },
            std::chrono::seconds(2)),
        "market-data send failure did not retry the verified contract");
  check(!watch.saw_live_at_generation_one.load(),
        "market-data send failure entered Live before session retry");
  runtime.stop();
}

void test_stale_contract_callbacks_and_permission_scope() {
  FakeClient client;
  client.hold_contract_details.store(true);
  auto options = options_for(prefix_for("stale"));
  options.connect_timeout_ms = 40;
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return client.contract_ids().size() >= 2; },
                   std::chrono::seconds(2)),
        "retry did not allocate a second contract reqId");
  const auto ids = client.contract_ids();
  client.emit_contract(ids.front(), 1);
  client.emit_error(ids.front(), 322, "stale contract error");
  const auto active_before = runtime.metrics().active_subscriptions;
  check(wait_until([&] { return runtime.metrics().stale_contract_callbacks > 0; },
                   std::chrono::seconds(1)),
        "stale contract callbacks were not counted");
  client.release_held_contracts();
  check(wait_until(
            [&] {
              return runtime.live() &&
                     runtime.metrics().active_subscriptions == 1;
            },
            std::chrono::seconds(2)),
        "stale contract callbacks prevented the new request from completing");
  check(runtime.metrics().active_subscriptions == active_before ||
            runtime.metrics().active_subscriptions == 1,
        "a stale 322 changed the persistent market-data active count");
  client.emit_error(2'000'000, 322, "unmapped contract code");
  check(runtime.metrics().active_subscriptions == 1,
        "an unmapped 322 cleared the persistent subscription");
  client.emit_error(1, 354, "market data not subscribed");
  check(wait_until([&] { return runtime.metrics().active_subscriptions == 0; },
                   std::chrono::seconds(1)),
        "market-data permission error did not clear the ticker active state");
  check(runtime.live(), "market-data permission error restarted the session");
  runtime.stop();
}

void test_req_id_exhaustion_is_process_fatal() {
  FakeClient client;
  auto options = options_for(prefix_for("reqid"));
  options.contract_req_id_origin = std::numeric_limits<int>::max();
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.failed(); }, std::chrono::seconds(2)),
        "reqId exhaustion did not fail the runtime");
  check(runtime.exit_code() == 1 && !runtime.live(),
        "reqId exhaustion did not request a non-zero exit");
  runtime.stop();
}

void test_health_requires_selected_active_and_catalog() {
  FakeClient client;
  client.average_volume_lots = {1.0};
  auto options = options_for(prefix_for("health"));
  options.filter.min_average_volume_90d = 1.0e12;
  options.subscriptions.push_back(spec_for("AAPL", 265598));
  mds::ibkr::IbkrRuntime runtime(options, client);
  std::string error;
  check(runtime.start(error), error);
  check(wait_until([&] { return runtime.live(); }, std::chrono::seconds(2)),
        "empty selection did not reach a stable session");
  const auto metrics = runtime.metrics();
  check(metrics.selected_subscriptions == 0 &&
            metrics.active_subscriptions == 0 &&
            metrics.catalog_publish_count == 0 &&
            metrics.bbo_publish_count == 0,
        "an empty selection reported publishable health");
  runtime.stop();
}

void test_second_writer_is_rejected_by_shared_memory() {
  FakeClient first_client;
  FakeClient second_client;
  auto first_options = options_for(prefix_for("exclusive"));
  first_options.subscriptions.push_back(spec_for("AAPL", 265598));
  auto second_options = first_options;
  mds::ibkr::IbkrRuntime first(first_options, first_client);
  std::string error;
  check(first.start(error), error);
  mds::ibkr::IbkrRuntime second(second_options, second_client);
  std::string second_error;
  check(!second.start(second_error), "second runtime created the same ring");
  check(second_error.find("File exists") != std::string::npos, second_error);
  first.stop();
}

}  // namespace

int main() {
  test_wake_seqlock_and_errors();
  test_catalog_bbo_and_coalesce();
  test_disconnect_and_reconnect();
  test_errors_do_not_reconnect_storm();
  test_cmdty_and_cash_keep_canonical_and_resolved_contract();
  test_metric_probe_filters_before_persistent_subscriptions();
  test_turnover_persists_all_stocks_before_selection_with_price_priority();
  test_metric_probe_waits_for_reference_price();
  test_missing_price_and_persist_failure_do_not_block_live();
  test_market_cap_probe_converts_millions_and_rejects_missing();
  test_market_cap_canaries_degrade_when_source_is_unavailable();
  test_partial_market_cap_canary_keeps_filter();
  test_non_stock_bypasses_threshold_and_uses_cap();
  test_data_maintained_does_not_rebuild_session();
  test_data_lost_rebuilds_once();
  test_contract_timeout_retries_without_isolating();
  test_session_error_during_resolving_or_subscribing_does_not_reach_live();
  test_market_data_send_failure_retries_without_isolate();
  test_stale_contract_callbacks_and_permission_scope();
  test_req_id_exhaustion_is_process_fatal();
  test_health_requires_selected_active_and_catalog();
  test_second_writer_is_rejected_by_shared_memory();
  return 0;
}
