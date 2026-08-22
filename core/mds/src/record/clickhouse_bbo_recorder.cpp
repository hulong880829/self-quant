#include "mds/record/clickhouse_bbo_recorder.h"

#include "mds/exchange/capabilities.h"
#include "net/http_client.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <deque>
#include <mutex>
#include <poll.h>
#include <sys/epoll.h>
#include <thread>
#include <unordered_map>

namespace mds::record {
namespace {

template <std::size_t Size>
std::string fixed_string(const std::array<char, Size> &value) {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.data(), static_cast<std::size_t>(end - value.begin())};
}

bool identifier(std::string_view value) {
  return !value.empty() &&
         std::all_of(value.begin(), value.end(), [](unsigned char c) {
           return std::isalnum(c) != 0 || c == '_';
         });
}

std::string url_encode(std::string_view value) {
  static constexpr char hex[] = "0123456789ABCDEF";
  std::string result;
  for (const char raw : value) {
    const auto c = static_cast<unsigned char>(raw);
    if (std::isalnum(c) != 0 || c == '-' || c == '_' || c == '.' ||
        c == '~') {
      result.push_back(static_cast<char>(c));
    } else {
      result.push_back('%');
      result.push_back(hex[c >> 4U]);
      result.push_back(hex[c & 15U]);
    }
  }
  return result;
}

template <typename T>
void append_pod(std::vector<std::byte> &out, T value) {
  const auto *bytes = reinterpret_cast<const std::byte *>(&value);
  out.insert(out.end(), bytes, bytes + sizeof(value));
}

void append_varuint(std::vector<std::byte> &out, std::size_t value) {
  while (value >= 0x80U) {
    out.push_back(static_cast<std::byte>((value & 0x7fU) | 0x80U));
    value >>= 7U;
  }
  out.push_back(static_cast<std::byte>(value));
}

void append_string(std::vector<std::byte> &out, std::string_view value) {
  append_varuint(out, value.size());
  const auto *bytes = reinterpret_cast<const std::byte *>(value.data());
  out.insert(out.end(), bytes, bytes + value.size());
}

}  // namespace

struct ClickHouseBboRecorder::Impl {
  struct Identity {
    std::string venue;
    std::string product;
    std::string symbol;
    std::uint8_t price_scale{};
    std::uint8_t quantity_scale{};
    std::uint64_t expiry_unix_ns{};
  };
  struct Latest {
    utils::md::wire::BboRecord record{};
    std::uint64_t receive_mono_ns{};
  };
  struct Pending {
    utils::md::wire::BboRecord record{};
    std::uint64_t receive_mono_ns{};
  };
  struct Row {
    std::uint64_t wall_ns{};
    Identity identity;
    std::int64_t bid_price{};
    std::int64_t bid_quantity{};
    std::int64_t ask_price{};
    std::int64_t ask_quantity{};
  };

  ClickHouseBboOptions options;
  std::unordered_map<utils::md::InstrumentId, Identity> catalog;
  std::unordered_map<utils::md::InstrumentId, Latest> latest;
  std::deque<Pending> unresolved;
  std::deque<Row> queue;
  mutable std::mutex mutex;
  std::condition_variable wake;
  std::thread writer;
  std::atomic<bool> stopping{};
  std::atomic<bool> started{};
  std::atomic<std::uint64_t> rows_enqueued{};
  std::atomic<std::uint64_t> rows_written{};
  std::atomic<std::uint64_t> queue_drops{};
  std::atomic<std::uint64_t> unresolved_drops{};
  std::atomic<std::uint64_t> http_failures{};
  std::atomic<std::uint64_t> rows_requeued{};
  std::atomic<std::uint64_t> shutdown_drops{};
  std::atomic<std::uint64_t> stale_skips{};
  mutable std::mutex error_mutex;
  std::string error_message;
  std::uint64_t last_sample_mono_ns{};

  bool selected(utils::md::Venue venue,
                utils::md::ProductType product) const noexcept {
    if (venue == utils::md::Venue::Polymarket ||
        product == utils::md::ProductType::BinaryOption) {
      return false;
    }
    return std::any_of(
        options.selectors.begin(), options.selectors.end(),
        [&](const ClickHouseSelector &selector) {
          return selector.venue == venue && selector.product == product;
        });
  }

  void set_error(std::string message) {
    std::lock_guard lock(error_mutex);
    error_message = std::move(message);
  }

  bool http_request(std::string_view query, std::span<const std::byte> body) {
    std::string tls_error;
    auto tls = net::make_client_ssl_context(tls_error);
    if (!tls) {
      set_error(tls_error);
      return false;
    }
    std::string target = "/?database=" + url_encode(options.database) +
                         "&user=" + url_encode(options.user) +
                         "&password=" + url_encode(options.password) +
                         "&query=" + url_encode(query);
    net::HttpClient client(std::move(tls), 16U << 10U, 1U << 20U,
                           std::max<std::size_t>(8U << 10U,
                                                 body.size() + 4096));
    const auto deadline = net::HttpClient::Clock::now() +
                          std::chrono::milliseconds(options.request_timeout_ms);
    if (!client.start_post(options.host, options.service, target,
                           "application/octet-stream", body, deadline)) {
      set_error(std::string(client.error_message()));
      return false;
    }
    while (client.state() != net::HttpClientState::Complete) {
      if (client.state() == net::HttpClientState::Failed ||
          client.state() == net::HttpClientState::TimedOut) {
        set_error(std::string(client.error_message()));
        return false;
      }
      pollfd descriptor{client.fd(), 0, 0};
      const auto wanted = client.wanted_events();
      if ((wanted & EPOLLIN) != 0) descriptor.events |= POLLIN;
      if ((wanted & EPOLLOUT) != 0) descriptor.events |= POLLOUT;
      const int ready = ::poll(&descriptor, 1, 50);
      std::uint32_t events{};
      if (ready > 0) {
        if ((descriptor.revents & POLLIN) != 0) events |= EPOLLIN;
        if ((descriptor.revents & POLLOUT) != 0) events |= EPOLLOUT;
        if ((descriptor.revents & (POLLERR | POLLHUP | POLLNVAL)) != 0)
          events |= EPOLLERR | EPOLLHUP;
      }
      (void)client.on_event(events);
      (void)client.check_timeout();
    }
    if (client.response().status_class() != net::HttpStatusClass::Success) {
      set_error("ClickHouse HTTP request returned status " +
                std::to_string(client.response().status_code()));
      return false;
    }
    return true;
  }

  bool request(std::string_view query, std::span<const std::byte> body) {
    if (!options.request) {
      return http_request(query, body);
    }
    std::string transport_error;
    if (options.request(query, body, transport_error)) {
      return true;
    }
    set_error(transport_error.empty() ? "ClickHouse transport failed"
                                      : std::move(transport_error));
    return false;
  }

  std::vector<std::byte> encode(std::span<const Row> rows) {
    std::vector<std::byte> body;
    body.reserve(rows.size() * 128);
    for (const auto &row : rows) {
      append_pod(body, static_cast<std::int64_t>(row.wall_ns));
      append_string(body, row.identity.venue);
      append_string(body, row.identity.product);
      append_string(body, row.identity.symbol);
      append_pod(body, row.bid_price);
      append_pod(body, row.bid_quantity);
      append_pod(body, row.ask_price);
      append_pod(body, row.ask_quantity);
      append_pod(body, row.identity.price_scale);
      append_pod(body, row.identity.quantity_scale);
    }
    return body;
  }

  void run() {
    const auto qualified = options.database + "." + options.table;
    const auto ddl =
        "CREATE TABLE IF NOT EXISTS " + qualified +
        " (ts DateTime64(9, 'UTC'), venue LowCardinality(String), "
        "product LowCardinality(String), canonical_symbol String, "
        "bid_price Int64, bid_quantity Int64, ask_price Int64, "
        "ask_quantity Int64, price_scale UInt8, quantity_scale UInt8) "
        "ENGINE=MergeTree PARTITION BY toDate(ts) "
        "ORDER BY (venue, product, canonical_symbol, ts)";
    if (!request(ddl, {})) ++http_failures;
    const auto insert =
        "INSERT INTO " + qualified +
        " (ts, venue, product, canonical_symbol, bid_price, bid_quantity, "
        "ask_price, ask_quantity, price_scale, quantity_scale) "
        "FORMAT RowBinary";
    while (true) {
      std::vector<Row> batch;
      {
        std::unique_lock lock(mutex);
        wake.wait_for(lock,
                      std::chrono::milliseconds(options.flush_interval_ms),
                      [&] {
                        return stopping.load(std::memory_order_relaxed) ||
                               queue.size() >= options.batch_rows;
                      });
        if (stopping.load(std::memory_order_relaxed) && queue.empty()) {
          break;
        }
        const auto count = std::min(queue.size(), options.batch_rows);
        batch.reserve(count);
        for (std::size_t i = 0; i < count; ++i) {
          batch.push_back(std::move(queue.front()));
          queue.pop_front();
        }
      }
      if (batch.empty()) continue;
      const auto body = encode(batch);
      if (request(insert, body)) {
        rows_written.fetch_add(batch.size(), std::memory_order_relaxed);
      } else {
        http_failures.fetch_add(1, std::memory_order_relaxed);
        std::unique_lock lock(mutex);
        if (stopping.load(std::memory_order_relaxed)) {
          queue_drops.fetch_add(batch.size(), std::memory_order_relaxed);
          shutdown_drops.fetch_add(batch.size(), std::memory_order_relaxed);
          continue;
        }
        while (queue.size() + batch.size() > options.queue_rows) {
          queue.pop_back();
          queue_drops.fetch_add(1, std::memory_order_relaxed);
        }
        for (auto row = batch.rbegin(); row != batch.rend(); ++row) {
          queue.push_front(std::move(*row));
        }
        rows_requeued.fetch_add(batch.size(), std::memory_order_relaxed);
        wake.wait_for(lock,
                      std::chrono::milliseconds(options.flush_interval_ms),
                      [&] {
                        return stopping.load(std::memory_order_relaxed);
                      });
      }
    }
  }
};

ClickHouseBboRecorder::ClickHouseBboRecorder(ClickHouseBboOptions options)
    : impl_(std::make_unique<Impl>()) {
  impl_->options = std::move(options);
}

ClickHouseBboRecorder::~ClickHouseBboRecorder() { stop(); }

bool ClickHouseBboRecorder::start() {
  auto &o = impl_->options;
  if (impl_->started.exchange(true) || o.host.empty() ||
      !identifier(o.database) || !identifier(o.table) ||
      o.sample_interval_ms < 200 || o.flush_interval_ms == 0 ||
      o.stale_cutoff_ms == 0 || o.request_timeout_ms == 0 ||
      o.batch_rows == 0 || o.queue_rows == 0 || o.unresolved_rows == 0 ||
      o.selectors.empty() ||
      std::any_of(o.selectors.begin(), o.selectors.end(),
                  [](const ClickHouseSelector &selector) {
                    return selector.venue == utils::md::Venue::Unknown ||
                           selector.venue == utils::md::Venue::Polymarket ||
                           selector.product == utils::md::ProductType::Unknown ||
                           selector.product ==
                               utils::md::ProductType::BinaryOption;
                  })) {
    impl_->set_error("invalid crypto ClickHouse recorder options");
    return false;
  }
  impl_->writer = std::thread([this] { impl_->run(); });
  return true;
}

void ClickHouseBboRecorder::stop() noexcept {
  impl_->stopping.store(true, std::memory_order_release);
  impl_->wake.notify_all();
  if (impl_->writer.joinable()) impl_->writer.join();
}

void ClickHouseBboRecorder::consume(std::span<const std::byte> payload,
                                    std::uint64_t receive_mono_ns) noexcept {
  utils::md::wire::RecordHeader header{};
  if (utils::md::wire::ValidateHeader(payload, &header) !=
      utils::md::wire::CodecError::Ok) {
    return;
  }
  const auto type = static_cast<utils::md::MessageType>(header.message_type);
  std::lock_guard lock(impl_->mutex);
  if (type == utils::md::MessageType::InstrumentCatalog) {
    utils::md::wire::InstrumentCatalogRecord record{};
    if (utils::md::wire::DecodeInstrumentCatalog(payload, record) !=
            utils::md::wire::CodecError::Ok ||
        !impl_->selected(record.catalog.venue,
                         record.catalog.product_type)) {
      return;
    }
    const std::string venue(exchange::venue_name(record.catalog.venue));
    const std::string product(
        exchange::product_name(record.catalog.product_type));
    const std::string symbol =
        fixed_string(record.catalog.canonical_symbol);
    const auto now = static_cast<std::uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            std::chrono::system_clock::now().time_since_epoch())
            .count());
    for (auto current = impl_->catalog.begin();
         current != impl_->catalog.end();) {
      const auto& identity = current->second;
      if (current->first != header.instrument_id &&
          identity.venue == venue && identity.product == product &&
          identity.symbol == symbol && identity.expiry_unix_ns != 0 &&
          identity.expiry_unix_ns <= now) {
        impl_->latest.erase(current->first);
        current = impl_->catalog.erase(current);
      } else {
        ++current;
      }
    }
    impl_->catalog[header.instrument_id] = {
        venue, product, symbol, record.catalog.price_scale,
        record.catalog.quantity_scale, record.catalog.expiry_unix_ns};
    for (auto pending = impl_->unresolved.begin();
         pending != impl_->unresolved.end();) {
      if (pending->record.header.instrument_id == header.instrument_id) {
        impl_->latest[header.instrument_id] =
            {pending->record, pending->receive_mono_ns};
        pending = impl_->unresolved.erase(pending);
      } else {
        ++pending;
      }
    }
    return;
  }
  if (type != utils::md::MessageType::Bbo &&
      type != utils::md::MessageType::Ticker) {
    return;
  }
  utils::md::wire::BboRecord bbo{};
  if (type == utils::md::MessageType::Bbo) {
    if (utils::md::wire::DecodeBbo(payload, bbo) !=
        utils::md::wire::CodecError::Ok)
      return;
  } else {
    utils::md::wire::TickerRecord ticker{};
    if (utils::md::wire::DecodeTicker(payload, ticker) !=
        utils::md::wire::CodecError::Ok)
      return;
    bbo.header = ticker.header;
    bbo.bid_price = ticker.bid_price;
    bbo.bid_quantity = ticker.bid_quantity;
    bbo.ask_price = ticker.ask_price;
    bbo.ask_quantity = ticker.ask_quantity;
  }
  if (impl_->catalog.contains(header.instrument_id)) {
    impl_->latest[header.instrument_id] = {bbo, receive_mono_ns};
  } else if (impl_->unresolved.size() < impl_->options.unresolved_rows) {
    impl_->unresolved.push_back({bbo, receive_mono_ns});
  } else {
    impl_->unresolved_drops.fetch_add(1, std::memory_order_relaxed);
  }
}

void ClickHouseBboRecorder::sample(std::uint64_t wall_ns,
                                   std::uint64_t mono_ns) noexcept {
  std::lock_guard lock(impl_->mutex);
  const auto interval = impl_->options.sample_interval_ms * 1'000'000ULL;
  if (impl_->last_sample_mono_ns != 0 &&
      mono_ns - impl_->last_sample_mono_ns < interval) {
    return;
  }
  impl_->last_sample_mono_ns = mono_ns;
  const auto stale_cutoff =
      impl_->options.stale_cutoff_ms * 1'000'000ULL;
  while (!impl_->unresolved.empty() &&
         mono_ns - impl_->unresolved.front().receive_mono_ns > interval) {
    impl_->unresolved.pop_front();
    impl_->unresolved_drops.fetch_add(1, std::memory_order_relaxed);
  }
  for (const auto &[id, latest] : impl_->latest) {
    const auto identity = impl_->catalog.find(id);
    if (identity == impl_->catalog.end()) continue;
    if (mono_ns >= latest.receive_mono_ns &&
        mono_ns - latest.receive_mono_ns > stale_cutoff) {
      impl_->stale_skips.fetch_add(1, std::memory_order_relaxed);
      continue;
    }
    if (impl_->queue.size() >= impl_->options.queue_rows) {
      impl_->queue.pop_front();
      impl_->queue_drops.fetch_add(1, std::memory_order_relaxed);
    }
    impl_->queue.push_back(
        {wall_ns, identity->second, latest.record.bid_price,
         latest.record.bid_quantity, latest.record.ask_price,
         latest.record.ask_quantity});
    impl_->rows_enqueued.fetch_add(1, std::memory_order_relaxed);
  }
  impl_->wake.notify_one();
}

ClickHouseBboMetrics ClickHouseBboRecorder::metrics() const noexcept {
  return {impl_->rows_enqueued.load(std::memory_order_relaxed),
          impl_->rows_written.load(std::memory_order_relaxed),
          impl_->queue_drops.load(std::memory_order_relaxed),
          impl_->unresolved_drops.load(std::memory_order_relaxed),
          impl_->http_failures.load(std::memory_order_relaxed),
          impl_->rows_requeued.load(std::memory_order_relaxed),
          impl_->shutdown_drops.load(std::memory_order_relaxed),
          impl_->stale_skips.load(std::memory_order_relaxed)};
}

std::string ClickHouseBboRecorder::error() const {
  std::lock_guard lock(impl_->error_mutex);
  return impl_->error_message;
}

}  // namespace mds::record
