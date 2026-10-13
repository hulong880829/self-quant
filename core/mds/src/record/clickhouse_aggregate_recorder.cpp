#include "mds/record/clickhouse_aggregate_recorder.h"

#include "net/http_client.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <cctype>
#include <chrono>
#include <condition_variable>
#include <cstring>
#include <deque>
#include <mutex>
#include <poll.h>
#include <sstream>
#include <sys/epoll.h>
#include <thread>
#include <type_traits>
#include <unistd.h>
#include <unordered_set>
#include <vector>

namespace mds::record {

static_assert(kGap == (1U << 0U));
static_assert(kReset == (1U << 1U));
static_assert(kMaximumDepth == 50);
static_assert(utils::md::wire::kSchemaMajor == 2);
static_assert(utils::md::wire::kSchemaMinor == 0);

namespace {

constexpr std::size_t kCompactBookDepth = 50;
constexpr std::size_t kMaximumOrderBookQueueRows = 4096;

std::uint64_t wall_now_ns() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

std::uint64_t mono_now_ns() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

bool identifier(std::string_view value) {
  return !value.empty() &&
         std::all_of(value.begin(), value.end(), [](unsigned char c) {
           return std::isalnum(c) != 0 || c == '_';
         });
}

bool valid_catalog_scope(std::string_view value) {
  return !value.empty() &&
         std::all_of(value.begin(), value.end(), [](unsigned char c) {
           return std::isalnum(c) != 0 || c == '_' || c == '-';
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

template <std::size_t Size>
std::string_view fixed_view(const std::array<char, Size> &value) noexcept {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.data(), static_cast<std::size_t>(end - value.begin())};
}

struct Identity {
  std::string profile;
  std::string symbol;
  std::string kind;
};

Identity parse_identity(const std::string &segment) {
  Identity result;
  const auto schema = std::to_string(utils::md::wire::kSchemaMajor);
  const auto bbo_suffix_storage = ".aggbbo." + schema;
  const auto book_suffix_storage = ".aggorderbook." + schema;
  const std::string_view bbo_suffix = bbo_suffix_storage;
  const std::string_view book_suffix = book_suffix_storage;
  std::string_view suffix;
  if (segment.ends_with(bbo_suffix)) {
    suffix = bbo_suffix;
    result.kind = "aggbbo";
  } else if (segment.ends_with(book_suffix)) {
    suffix = book_suffix;
    result.kind = "aggorderbook";
  } else {
    return result;
  }
  const auto identity_size = segment.size() - suffix.size();
  const auto symbol_dot = segment.rfind('.', identity_size - 1);
  if (segment.empty() || segment.front() != '/' ||
      symbol_dot == std::string::npos || symbol_dot + 1 >= identity_size) {
    return {};
  }
  const auto profile_dot = segment.rfind('.', symbol_dot - 1);
  if (profile_dot == std::string::npos || profile_dot + 1 == symbol_dot) {
    return {};
  }
  result.profile =
      segment.substr(profile_dot + 1, symbol_dot - profile_dot - 1);
  result.symbol =
      segment.substr(symbol_dot + 1, identity_size - symbol_dot - 1);
  return result;
}

void compact_book(const utils::md::wire::AggOrderBookRecord &source,
                  std::size_t depth,
                  utils::md::wire::AggOrderBookRecord &target) {
  target = source;
  target.bids = {};
  target.asks = {};
  target.bid_count = 0;
  target.ask_count = 0;
  const auto bid_limit =
      std::min<std::size_t>({depth, source.bid_count, kMaximumDepth});
  const auto source_bid_count = std::min<std::size_t>(
      source.bid_count, utils::md::wire::kAggMaxLevelsPerSide);
  for (std::size_t index = 0;
       index < source_bid_count && target.bid_count < bid_limit; ++index) {
    if (source.bids[index].price > 0 && source.bids[index].quantity > 0) {
      target.bids[target.bid_count++] = source.bids[index];
    }
  }
  const auto ask_limit =
      std::min<std::size_t>({depth, source.ask_count, kMaximumDepth});
  const auto source_ask_count = std::min<std::size_t>(
      source.ask_count, utils::md::wire::kAggMaxLevelsPerSide);
  for (std::size_t index = 0;
       index < source_ask_count && target.ask_count < ask_limit; ++index) {
    if (source.asks[index].price > 0 && source.asks[index].quantity > 0) {
      target.asks[target.ask_count++] = source.asks[index];
    }
  }
}

struct AggregateBboSample {
  std::uint16_t topic_index{};
  std::uint64_t sampled_at_ns{};
  std::uint64_t source_wall_ns{};
  std::uint64_t mono_ns{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint16_t record_flags{};
  std::uint64_t reset_generation{};
  utils::md::wire::AggBboRecord record{};
  consume::CrossBpsWindow cross_window{};
};

struct AggregateOrderBookSample {
  std::uint16_t topic_index{};
  std::uint64_t sampled_at_ns{};
  std::uint64_t source_wall_ns{};
  std::uint64_t mono_ns{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint16_t record_flags{};
  std::uint64_t reset_generation{};
  std::uint16_t header_flags{};
  std::uint64_t exchange_ts_ns{};
  std::uint32_t book_generation{};
  std::array<char, 16> base_asset{};
  std::array<char, 16> quote_asset{};
  std::array<std::uint8_t, utils::md::wire::kAggVenueSlots> venue_slot_ids{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t member_count{};
  std::uint32_t member_mask{};
  std::uint32_t active_mask{};
  std::uint16_t bid_count{};
  std::uint16_t ask_count{};
  std::array<utils::md::wire::AggLevel, kCompactBookDepth> bids{};
  std::array<utils::md::wire::AggLevel, kCompactBookDepth> asks{};
};

struct CatalogSample {
  std::uint8_t active{};
  std::uint16_t topic_index{};
  std::uint16_t intern_index{};
  std::uint64_t updated_at_ns{};
  bool tombstone{};
};

static_assert(std::is_trivially_copyable_v<AggregateBboSample>);
static_assert(std::is_trivially_copyable_v<AggregateOrderBookSample>);
static_assert(std::is_trivially_copyable_v<CatalogSample>);

void append_uint8_array(
    std::vector<std::byte> &out,
    const std::array<std::uint8_t, utils::md::wire::kAggVenueSlots> &values) {
  append_varuint(out, values.size());
  for (const auto value : values) {
    append_pod(out, value);
  }
}

void append_int64_array(
    std::vector<std::byte> &out,
    const std::array<std::int64_t, utils::md::wire::kAggVenueSlots> &values) {
  append_varuint(out, values.size());
  for (const auto value : values) {
    append_pod(out, value);
  }
}

void append_gated_side(std::vector<std::byte> &out,
                       const utils::md::wire::AggBboSide &side) {
  append_pod(out, side.price);
  append_pod(out, side.quantity);
  append_int64_array(out, side.venue_quantity);
  append_pod(out, side.exchange_ts_ns);
  append_pod(out, side.venue_mask);
  append_pod(out, side.worst_ingress_age_us);
  append_pod(out, side.best_venue);
  append_pod(out, side.timestamp_venue);
  append_pod(out, side.contributor_count);
}

void append_raw_side(std::vector<std::byte> &out,
                     const utils::md::wire::AggBboRawSide &side) {
  append_pod(out, side.price);
  append_pod(out, side.quantity);
  append_pod(out, side.venue_mask);
  append_pod(out, side.best_venue);
}

void append_book_side(
    std::vector<std::byte> &out, std::uint16_t count,
    const std::array<utils::md::wire::AggLevel, kCompactBookDepth> &levels) {
  const auto used = std::min<std::size_t>(count, kCompactBookDepth);
  append_varuint(out, used);
  for (std::size_t index = 0; index < used; ++index) {
    append_pod(out, levels[index].price);
  }
  append_varuint(out, used);
  for (std::size_t index = 0; index < used; ++index) {
    append_pod(out, levels[index].quantity);
  }
  append_varuint(out, used);
  for (std::size_t index = 0; index < used; ++index) {
    append_pod(out, levels[index].venue_mask);
  }
  append_varuint(out, used);
  for (std::size_t index = 0; index < used; ++index) {
    append_pod(out, levels[index].contributor_count);
  }
  append_varuint(out, used);
  for (std::size_t index = 0; index < used; ++index) {
    append_int64_array(out, levels[index].venue_quantity);
  }
}

constexpr std::string_view kBboInsertColumns =
    "sampled_at, source_wall_ns, mono_ns, profile, symbol, segment, "
    "ring_epoch, ring_sequence, generation, record_flags, reset_generation, "
    "header_flags, header_exchange_ts_ns, base_asset, quote_asset, "
    "venue_slot_ids, price_scale, quantity_scale, member_count, member_mask, "
    "live_mask, gated_bid_price, gated_bid_quantity, gated_bid_venue_quantity, "
    "gated_bid_exchange_ts_ns, gated_bid_venue_mask, "
    "gated_bid_worst_ingress_age_us, gated_bid_best_venue, "
    "gated_bid_timestamp_venue, gated_bid_contributor_count, gated_ask_price, "
    "gated_ask_quantity, gated_ask_venue_quantity, gated_ask_exchange_ts_ns, "
    "gated_ask_venue_mask, gated_ask_worst_ingress_age_us, gated_ask_best_venue, "
    "gated_ask_timestamp_venue, gated_ask_contributor_count, raw_bid_price, "
    "raw_bid_quantity, raw_bid_venue_mask, raw_bid_best_venue, raw_ask_price, "
    "raw_ask_quantity, raw_ask_venue_mask, raw_ask_best_venue, raw_cross_bps, "
    "gated_cross_bps, skew_us, cross_skew_threshold_us, fx_age_us, "
    "cross_bid_venue, cross_ask_venue, fx_venue, cross_window_start_mono_ns, "
    "cross_window_end_mono_ns, cross_window_raw_min, cross_window_raw_max, "
    "cross_window_gated_min, cross_window_gated_max, cross_window_samples";

constexpr std::string_view kOrderBookInsertColumns =
    "sampled_at, source_wall_ns, mono_ns, profile, symbol, segment, "
    "ring_epoch, ring_sequence, generation, record_flags, reset_generation, "
    "header_flags, exchange_ts_ns, book_generation, base_asset, quote_asset, "
    "venue_slot_ids, price_scale, quantity_scale, member_count, member_mask, "
    "active_mask, bid_count, ask_count, bid_price, bid_quantity, "
    "bid_venue_mask, bid_contributor_count, bid_venue_quantity, ask_price, "
    "ask_quantity, ask_venue_mask, ask_contributor_count, ask_venue_quantity";

constexpr std::string_view kCatalogInsertColumns =
    "catalog_scope, service_instance, profile, symbol, kind, segment, active, "
    "schema_major, schema_minor, updated_at, updated_at_ns";

}  // namespace

struct ClickHouseAggregateRecorder::Impl {
  struct TopicRuntime {
    ClickHouseAggregateTopic topic;
    Identity identity;
    std::uint64_t last_reset_generation{};
    std::uint64_t last_snapshot_generation{};
    std::uint64_t last_sequence{};
    std::uint64_t last_epoch{};
    std::uint64_t last_completed_window_generation{};
    bool pending_reset{};
  };

  ClickHouseAggregateOptions options;
  std::vector<TopicRuntime> topics;
  std::vector<std::string> interned_segments;
  std::deque<AggregateBboSample> bbo_queue;
  std::deque<AggregateOrderBookSample> book_queue;
  std::deque<CatalogSample> catalog_queue;
  mutable std::mutex mutex;
  std::condition_variable wake;
  std::thread writer;
  std::thread sampler;
  std::atomic<bool> stopping{};
  std::atomic<bool> started{};
  std::atomic<std::uint64_t> samples_seen{};
  std::atomic<std::uint64_t> rows_enqueued{};
  std::atomic<std::uint64_t> rows_written{};
  std::atomic<std::uint64_t> queue_drops{};
  std::atomic<std::uint64_t> bbo_queue_drops{};
  std::atomic<std::uint64_t> book_queue_drops{};
  std::atomic<std::uint64_t> catalog_queue_drops{};
  std::atomic<std::uint64_t> queued_bytes{};
  std::atomic<std::uint64_t> retry_rows{};
  std::atomic<std::uint64_t> http_failures{};
  std::atomic<std::uint64_t> stale_skips{};
  std::atomic<std::uint64_t> encode_failures{};
  std::atomic<std::uint64_t> last_success_at{};
  std::atomic<std::uint64_t> write_lag_ms{};
  std::atomic<std::uint64_t> shutdown_drops{};
  std::atomic<std::uint64_t> catalog_reconcile_ok{};
  std::atomic<std::uint64_t> catalog_reconcile_failures{};
  mutable std::mutex error_mutex;
  std::string error_message;
  bool tables_ready{};
  bool configured_catalog_written{};
  bool catalog_reconciled{};

  void set_error(std::string message) {
    std::lock_guard lock(error_mutex);
    error_message = std::move(message);
  }

  template <typename Snapshot>
  std::uint16_t flags_for(TopicRuntime &topic, const Snapshot &snapshot) {
    std::uint16_t flags{};
    if (topic.pending_reset ||
        snapshot.status.reset_generation != topic.last_reset_generation) {
      flags |= kReset;
    }
    const auto &receive = snapshot.status.receive;
    if (topic.last_snapshot_generation != 0 &&
        snapshot.status.generation != topic.last_snapshot_generation &&
        (receive.ring_epoch != topic.last_epoch ||
         receive.ring_sequence != topic.last_sequence + 1)) {
      flags |= kGap;
    }
    topic.pending_reset = false;
    topic.last_reset_generation = snapshot.status.reset_generation;
    topic.last_snapshot_generation = snapshot.status.generation;
    topic.last_sequence = receive.ring_sequence;
    topic.last_epoch = receive.ring_epoch;
    return flags;
  }

  bool poll_http(net::HttpClient &client, std::string &error) {
    while (client.state() != net::HttpClientState::Complete) {
      if (client.state() == net::HttpClientState::Failed ||
          client.state() == net::HttpClientState::TimedOut) {
        error = std::string(client.error_message());
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
      error = "ClickHouse HTTP request returned status " +
              std::to_string(client.response().status_code());
      return false;
    }
    return true;
  }

  bool http_execute(std::string_view query, std::span<const std::byte> body,
                    std::string *response_body) {
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
    const std::size_t body_capacity =
        response_body == nullptr ? (1U << 20U) : (2U << 20U);
    net::HttpClient client(std::move(tls), 16U << 10U, body_capacity,
                           std::max<std::size_t>(8U << 10U, body.size() + 4096));
    const auto deadline = net::HttpClient::Clock::now() +
                          std::chrono::milliseconds(options.request_timeout_ms);
    if (!client.start_post(options.host, options.service, target,
                           "application/octet-stream", body, deadline)) {
      set_error(std::string(client.error_message()));
      return false;
    }
    std::string transport_error;
    if (!poll_http(client, transport_error)) {
      set_error(std::move(transport_error));
      return false;
    }
    if (response_body != nullptr) {
      const auto bytes = client.response().body();
      response_body->assign(reinterpret_cast<const char *>(bytes.data()),
                            bytes.size());
    }
    return true;
  }

  bool request(std::string_view query, std::span<const std::byte> body) {
    if (options.request) {
      std::string transport_error;
      if (options.request(query, body, transport_error)) {
        return true;
      }
      set_error(transport_error.empty() ? "ClickHouse transport failed"
                                        : std::move(transport_error));
      return false;
    }
    return http_execute(query, body, nullptr);
  }

  bool query(std::string_view sql, std::string &body) {
    if (options.query) {
      std::string transport_error;
      if (options.query(sql, body, transport_error)) {
        return true;
      }
      set_error(transport_error.empty() ? "ClickHouse query failed"
                                        : std::move(transport_error));
      return false;
    }
    if (options.request) {
      set_error("ClickHouse query is unavailable");
      return false;
    }
    return http_execute(sql, {}, &body);
  }

  template <typename T>
  void drop_oldest(std::deque<T> &queue, std::atomic<std::uint64_t> &drops) {
    if (queue.empty()) {
      return;
    }
    queue.pop_front();
    queued_bytes.fetch_sub(sizeof(T), std::memory_order_relaxed);
    drops.fetch_add(1, std::memory_order_relaxed);
    queue_drops.fetch_add(1, std::memory_order_relaxed);
  }

  void make_room(std::size_t extra_bytes, std::size_t extra_rows,
                 std::size_t max_rows, std::deque<AggregateBboSample> *bbo,
                 std::deque<AggregateOrderBookSample> *book,
                 std::deque<CatalogSample> *catalog) {
    auto over_bytes = [&] {
      return queued_bytes.load(std::memory_order_relaxed) + extra_bytes >
             options.max_queued_bytes;
    };
    auto over_rows = [&](std::size_t current) {
      return current + extra_rows > max_rows;
    };
    while ((bbo != nullptr && over_rows(bbo->size())) ||
           (book != nullptr && over_rows(book->size())) ||
           (catalog != nullptr && over_rows(catalog->size())) || over_bytes()) {
      if (bbo != nullptr && !bbo->empty() &&
          (over_rows(bbo->size()) || over_bytes())) {
        drop_oldest(*bbo, bbo_queue_drops);
        continue;
      }
      if (book != nullptr && !book->empty() &&
          (over_rows(book->size()) || over_bytes())) {
        drop_oldest(*book, book_queue_drops);
        continue;
      }
      if (catalog != nullptr && !catalog->empty() &&
          (over_rows(catalog->size()) || over_bytes())) {
        drop_oldest(*catalog, catalog_queue_drops);
        continue;
      }
      if (!bbo_queue.empty() && over_bytes()) {
        drop_oldest(bbo_queue, bbo_queue_drops);
        continue;
      }
      if (!book_queue.empty() && over_bytes()) {
        drop_oldest(book_queue, book_queue_drops);
        continue;
      }
      if (!catalog_queue.empty() && over_bytes()) {
        drop_oldest(catalog_queue, catalog_queue_drops);
        continue;
      }
      break;
    }
  }

  template <typename T>
  void enqueue(std::deque<T> &queue, T sample, std::size_t max_rows,
               std::atomic<std::uint64_t> &drops) {
    std::lock_guard lock(mutex);
    make_room(sizeof(T), 1, max_rows,
              std::is_same_v<T, AggregateBboSample> ? &bbo_queue : nullptr,
              std::is_same_v<T, AggregateOrderBookSample> ? &book_queue
                                                          : nullptr,
              std::is_same_v<T, CatalogSample> ? &catalog_queue : nullptr);
    if (queue.size() >= max_rows ||
        queued_bytes.load(std::memory_order_relaxed) + sizeof(T) >
            options.max_queued_bytes) {
      drops.fetch_add(1, std::memory_order_relaxed);
      queue_drops.fetch_add(1, std::memory_order_relaxed);
      return;
    }
    queue.push_back(std::move(sample));
    queued_bytes.fetch_add(sizeof(T), std::memory_order_relaxed);
    rows_enqueued.fetch_add(1, std::memory_order_relaxed);
    wake.notify_one();
  }

  template <typename T>
  void requeue(std::deque<T> &queue, std::vector<T> &batch, std::size_t max_rows,
               std::atomic<std::uint64_t> &drops) {
    if (batch.empty()) {
      return;
    }
    make_room(sizeof(T) * batch.size(), batch.size(), max_rows,
              std::is_same_v<T, AggregateBboSample> ? &bbo_queue : nullptr,
              std::is_same_v<T, AggregateOrderBookSample> ? &book_queue
                                                          : nullptr,
              std::is_same_v<T, CatalogSample> ? &catalog_queue : nullptr);
    while (!batch.empty() &&
           (queue.size() + batch.size() > max_rows ||
            queued_bytes.load(std::memory_order_relaxed) +
                    sizeof(T) * batch.size() >
                options.max_queued_bytes)) {
      batch.erase(batch.begin());
      drops.fetch_add(1, std::memory_order_relaxed);
      queue_drops.fetch_add(1, std::memory_order_relaxed);
    }
    for (auto row = batch.rbegin(); row != batch.rend(); ++row) {
      queue.push_front(std::move(*row));
      queued_bytes.fetch_add(sizeof(T), std::memory_order_relaxed);
    }
    retry_rows.fetch_add(batch.size(), std::memory_order_relaxed);
    batch.clear();
  }

  bool encode_bbo(const AggregateBboSample &sample,
                  std::vector<std::byte> &out) {
    if (sample.topic_index >= topics.size()) {
      encode_failures.fetch_add(1, std::memory_order_relaxed);
      return false;
    }
    const auto &identity = topics[sample.topic_index].identity;
    const auto &record = sample.record;
    append_pod(out, static_cast<std::int64_t>(sample.sampled_at_ns));
    append_pod(out, sample.source_wall_ns);
    append_pod(out, sample.mono_ns);
    append_string(out, identity.profile);
    append_string(out, identity.symbol);
    append_string(out, topics[sample.topic_index].topic.segment);
    append_pod(out, sample.ring_epoch);
    append_pod(out, sample.ring_sequence);
    append_pod(out, sample.generation);
    append_pod(out, sample.record_flags);
    append_pod(out, sample.reset_generation);
    append_pod(out, record.header.flags);
    append_pod(out, record.header.exchange_ts_ns);
    append_string(out, fixed_view(record.base_asset));
    append_string(out, fixed_view(record.quote_asset));
    append_uint8_array(out, record.venue_slot_ids);
    append_pod(out, record.price_scale);
    append_pod(out, record.quantity_scale);
    append_pod(out, record.member_count);
    append_pod(out, record.member_mask);
    append_pod(out, record.live_mask);
    append_gated_side(out, record.gated_bid);
    append_gated_side(out, record.gated_ask);
    append_raw_side(out, record.raw_bid);
    append_raw_side(out, record.raw_ask);
    append_pod(out, record.raw_cross_bps);
    append_pod(out, record.gated_cross_bps);
    append_pod(out, record.skew_us);
    append_pod(out, record.cross_skew_threshold_us);
    append_pod(out, record.fx_age_us);
    append_pod(out, record.cross_bid_venue);
    append_pod(out, record.cross_ask_venue);
    append_pod(out, record.fx_venue);
    append_pod(out, sample.cross_window.start_mono_ns);
    append_pod(out, sample.cross_window.end_mono_ns);
    append_pod(out, sample.cross_window.raw_min);
    append_pod(out, sample.cross_window.raw_max);
    append_pod(out, sample.cross_window.gated_min);
    append_pod(out, sample.cross_window.gated_max);
    append_pod(out, sample.cross_window.sample_count);
    return true;
  }

  bool encode_book(const AggregateOrderBookSample &sample,
                   std::vector<std::byte> &out) {
    if (sample.topic_index >= topics.size()) {
      encode_failures.fetch_add(1, std::memory_order_relaxed);
      return false;
    }
    const auto &identity = topics[sample.topic_index].identity;
    append_pod(out, static_cast<std::int64_t>(sample.sampled_at_ns));
    append_pod(out, sample.source_wall_ns);
    append_pod(out, sample.mono_ns);
    append_string(out, identity.profile);
    append_string(out, identity.symbol);
    append_string(out, topics[sample.topic_index].topic.segment);
    append_pod(out, sample.ring_epoch);
    append_pod(out, sample.ring_sequence);
    append_pod(out, sample.generation);
    append_pod(out, sample.record_flags);
    append_pod(out, sample.reset_generation);
    append_pod(out, sample.header_flags);
    append_pod(out, sample.exchange_ts_ns);
    append_pod(out, sample.book_generation);
    append_string(out, fixed_view(sample.base_asset));
    append_string(out, fixed_view(sample.quote_asset));
    append_uint8_array(out, sample.venue_slot_ids);
    append_pod(out, sample.price_scale);
    append_pod(out, sample.quantity_scale);
    append_pod(out, sample.member_count);
    append_pod(out, sample.member_mask);
    append_pod(out, sample.active_mask);
    append_pod(out, static_cast<std::uint32_t>(sample.bid_count));
    append_pod(out, static_cast<std::uint32_t>(sample.ask_count));
    append_book_side(out, sample.bid_count, sample.bids);
    append_book_side(out, sample.ask_count, sample.asks);
    return true;
  }

  bool encode_catalog(const CatalogSample &sample, std::vector<std::byte> &out) {
    Identity identity;
    std::string_view segment;
    if (sample.tombstone) {
      if (sample.intern_index >= interned_segments.size()) {
        encode_failures.fetch_add(1, std::memory_order_relaxed);
        return false;
      }
      segment = interned_segments[sample.intern_index];
      identity = parse_identity(interned_segments[sample.intern_index]);
    } else {
      if (sample.topic_index >= topics.size()) {
        encode_failures.fetch_add(1, std::memory_order_relaxed);
        return false;
      }
      identity = topics[sample.topic_index].identity;
      segment = topics[sample.topic_index].topic.segment;
    }
    append_string(out, options.catalog_scope);
    append_string(out, options.service_instance);
    append_string(out, identity.profile);
    append_string(out, identity.symbol);
    append_string(out, identity.kind);
    append_string(out, segment);
    append_pod(out, sample.active);
    append_pod(out, utils::md::wire::kSchemaMajor);
    append_pod(out, utils::md::wire::kSchemaMinor);
    append_pod(out, static_cast<std::int64_t>(sample.updated_at_ns));
    append_pod(out, sample.updated_at_ns);
    return true;
  }

  bool ensure_tables() {
    const auto bbo =
        clickhouse_aggregate_bbo_ddl(options.database, options.bbo_table);
    const auto book = clickhouse_aggregate_orderbook_ddl(
        options.database, options.orderbook_table);
    const auto catalog = clickhouse_aggregate_catalog_ddl(
        options.database, options.catalog_table);
    return request(bbo, {}) && request(book, {}) && request(catalog, {});
  }

  void enqueue_configured_catalog(bool active) {
    const auto now = wall_now_ns();
    for (std::uint16_t index = 0; index < topics.size(); ++index) {
      CatalogSample sample{};
      sample.active = active ? std::uint8_t{1} : std::uint8_t{0};
      sample.topic_index = index;
      sample.updated_at_ns = now;
      enqueue(catalog_queue, sample, options.catalog_queue_rows,
              catalog_queue_drops);
    }
  }

  bool reconcile_catalog() {
    std::string sql = "SELECT catalog_scope, segment, argMax(active, "
                      "tuple(updated_at_ns, service_instance)) AS active FROM ";
    sql.append(options.database);
    sql.push_back('.');
    sql.append(options.catalog_table);
    sql.append(" WHERE catalog_scope = '");
    sql.append(options.catalog_scope);
    sql.append("' GROUP BY catalog_scope, segment FORMAT TabSeparated");
    std::string body;
    if (!query(sql, body)) {
      catalog_reconcile_failures.fetch_add(1, std::memory_order_relaxed);
      return false;
    }
    std::unordered_set<std::string> configured;
    configured.reserve(topics.size());
    for (const auto &topic : topics) {
      configured.insert(topic.topic.segment);
    }
    std::istringstream stream(body);
    std::string line;
    const auto now = wall_now_ns();
    while (std::getline(stream, line)) {
      if (line.empty()) {
        continue;
      }
      const auto first = line.find('\t');
      const auto second =
          first == std::string::npos ? std::string::npos : line.find('\t', first + 1);
      if (first == std::string::npos || second == std::string::npos) {
        continue;
      }
      const auto scope = line.substr(0, first);
      const auto segment = line.substr(first + 1, second - first - 1);
      auto active_text = line.substr(second + 1);
      if (!active_text.empty() && active_text.back() == '\r') {
        active_text.pop_back();
      }
      if (scope != options.catalog_scope) {
        continue;
      }
      const bool active = active_text == "1" || active_text == "true";
      if (!active || configured.contains(segment)) {
        continue;
      }
      interned_segments.push_back(segment);
      CatalogSample sample{};
      sample.active = 0;
      sample.intern_index =
          static_cast<std::uint16_t>(interned_segments.size() - 1);
      sample.updated_at_ns = now;
      sample.tombstone = true;
      enqueue(catalog_queue, sample, options.catalog_queue_rows,
              catalog_queue_drops);
    }
    catalog_reconcile_ok.fetch_add(1, std::memory_order_relaxed);
    return true;
  }

  template <typename T, typename Encode>
  bool flush_queue(std::deque<T> &queue, std::size_t max_rows,
                   std::atomic<std::uint64_t> &drops, std::string_view insert,
                   Encode &&encode) {
    std::vector<T> batch;
    {
      std::unique_lock lock(mutex);
      const auto count = std::min(queue.size(), options.batch_rows);
      batch.reserve(count);
      for (std::size_t index = 0; index < count; ++index) {
        batch.push_back(std::move(queue.front()));
        queue.pop_front();
        queued_bytes.fetch_sub(sizeof(T), std::memory_order_relaxed);
      }
    }
    if (batch.empty()) {
      return true;
    }
    std::vector<std::byte> body;
    body.reserve(batch.size() * 256);
    std::vector<T> encoded;
    encoded.reserve(batch.size());
    for (auto &row : batch) {
      const auto before = body.size();
      if (encode(row, body)) {
        encoded.push_back(std::move(row));
      } else {
        body.resize(before);
      }
    }
    if (encoded.empty()) {
      return true;
    }
    const auto oldest = encoded.front().sampled_at_ns;
    if (request(insert, body)) {
      rows_written.fetch_add(encoded.size(), std::memory_order_relaxed);
      const auto now = wall_now_ns();
      last_success_at.store(now, std::memory_order_relaxed);
      if (oldest != 0 && now >= oldest) {
        write_lag_ms.store((now - oldest) / 1'000'000ULL,
                           std::memory_order_relaxed);
      }
      return true;
    }
    http_failures.fetch_add(1, std::memory_order_relaxed);
    std::lock_guard lock(mutex);
    if (stopping.load(std::memory_order_relaxed)) {
      shutdown_drops.fetch_add(encoded.size(), std::memory_order_relaxed);
      queue_drops.fetch_add(encoded.size(), std::memory_order_relaxed);
      return false;
    }
    requeue(queue, encoded, max_rows, drops);
    return false;
  }

  bool flush_catalog() {
    const auto insert = "INSERT INTO " + options.database + "." +
                        options.catalog_table + " (" +
                        std::string(kCatalogInsertColumns) +
                        ") FORMAT RowBinary";
    std::vector<CatalogSample> batch;
    {
      std::unique_lock lock(mutex);
      const auto count = std::min(catalog_queue.size(), options.batch_rows);
      batch.reserve(count);
      for (std::size_t index = 0; index < count; ++index) {
        batch.push_back(std::move(catalog_queue.front()));
        catalog_queue.pop_front();
        queued_bytes.fetch_sub(sizeof(CatalogSample),
                               std::memory_order_relaxed);
      }
    }
    if (batch.empty()) {
      return true;
    }
    std::vector<std::byte> body;
    std::vector<CatalogSample> encoded;
    for (auto &row : batch) {
      const auto before = body.size();
      if (encode_catalog(row, body)) {
        encoded.push_back(std::move(row));
      } else {
        body.resize(before);
      }
    }
    if (encoded.empty()) {
      return true;
    }
    if (request(insert, body)) {
      rows_written.fetch_add(encoded.size(), std::memory_order_relaxed);
      last_success_at.store(wall_now_ns(), std::memory_order_relaxed);
      return true;
    }
    http_failures.fetch_add(1, std::memory_order_relaxed);
    std::lock_guard lock(mutex);
    if (stopping.load(std::memory_order_relaxed)) {
      shutdown_drops.fetch_add(encoded.size(), std::memory_order_relaxed);
      queue_drops.fetch_add(encoded.size(), std::memory_order_relaxed);
      return false;
    }
    requeue(catalog_queue, encoded, options.catalog_queue_rows,
            catalog_queue_drops);
    return false;
  }

  void run_writer() {
    const auto bbo_insert = "INSERT INTO " + options.database + "." +
                            options.bbo_table + " (" +
                            std::string(kBboInsertColumns) +
                            ") FORMAT RowBinary";
    const auto book_insert = "INSERT INTO " + options.database + "." +
                             options.orderbook_table + " (" +
                             std::string(kOrderBookInsertColumns) +
                             ") FORMAT RowBinary";
    while (true) {
      if (!tables_ready) {
        if (ensure_tables()) {
          tables_ready = true;
        } else {
          http_failures.fetch_add(1, std::memory_order_relaxed);
        }
      }
      if (tables_ready && !configured_catalog_written) {
        enqueue_configured_catalog(true);
        configured_catalog_written = true;
      }
      if (tables_ready && !catalog_reconciled) {
        catalog_reconciled = reconcile_catalog();
      }
      bool wrote = true;
      if (tables_ready) {
        wrote = flush_queue(
                    bbo_queue, options.bbo_queue_rows, bbo_queue_drops,
                    bbo_insert,
                    [this](const AggregateBboSample &sample,
                           std::vector<std::byte> &out) {
                      return encode_bbo(sample, out);
                    }) &&
                flush_queue(
                    book_queue, options.orderbook_queue_rows, book_queue_drops,
                    book_insert,
                    [this](const AggregateOrderBookSample &sample,
                           std::vector<std::byte> &out) {
                      return encode_book(sample, out);
                    }) &&
                flush_catalog();
      }
      {
        std::unique_lock lock(mutex);
        if (stopping.load(std::memory_order_relaxed) && bbo_queue.empty() &&
            book_queue.empty() && catalog_queue.empty()) {
          break;
        }
        if (stopping.load(std::memory_order_relaxed)) {
          if (!tables_ready) {
            shutdown_drops.fetch_add(bbo_queue.size() + book_queue.size() +
                                         catalog_queue.size(),
                                     std::memory_order_relaxed);
            queue_drops.fetch_add(bbo_queue.size() + book_queue.size() +
                                      catalog_queue.size(),
                                  std::memory_order_relaxed);
            bbo_queue.clear();
            book_queue.clear();
            catalog_queue.clear();
            queued_bytes.store(0, std::memory_order_relaxed);
            break;
          }
          continue;
        }
        wake.wait_for(lock,
                      std::chrono::milliseconds(options.flush_interval_ms),
                      [&] {
                        return stopping.load(std::memory_order_relaxed) ||
                               bbo_queue.size() >= options.batch_rows ||
                               book_queue.size() >= options.batch_rows ||
                               catalog_queue.size() >= options.batch_rows;
                      });
      }
      (void)wrote;
    }
  }

  void sample_bbo(TopicRuntime &topic, std::uint16_t index,
                  std::uint64_t wall_ns, std::uint64_t mono_ns) {
    consume::AggBboSnapshot snapshot{};
    if (topic.topic.latest == nullptr ||
        !topic.topic.latest->snapshot(snapshot)) {
      return;
    }
    if (!snapshot.status.ready) {
      topic.pending_reset = true;
      topic.last_reset_generation = snapshot.status.reset_generation;
      topic.last_sequence = snapshot.status.receive.ring_sequence;
      topic.last_epoch = snapshot.status.receive.ring_epoch;
      stale_skips.fetch_add(1, std::memory_order_relaxed);
      return;
    }
    AggregateBboSample sample{};
    sample.topic_index = index;
    sample.sampled_at_ns = wall_ns;
    sample.source_wall_ns = snapshot.status.receive.receive_wall_ns;
    sample.mono_ns = mono_ns;
    sample.ring_epoch = snapshot.status.receive.ring_epoch;
    sample.ring_sequence = snapshot.status.receive.ring_sequence;
    sample.generation = snapshot.status.generation;
    sample.record_flags = flags_for(topic, snapshot);
    sample.reset_generation = snapshot.status.reset_generation;
    sample.record = snapshot.record;
    sample.cross_window = snapshot.cross_window;
    consume::AggBboWindowSnapshot completed{};
    if (topic.topic.latest->snapshot_bbo_window(0, completed) &&
        completed.status.generation > topic.last_completed_window_generation) {
      topic.last_completed_window_generation = completed.status.generation;
      sample.cross_window = completed.cross_window;
    }
    enqueue(bbo_queue, sample, options.bbo_queue_rows, bbo_queue_drops);
  }

  void sample_book(TopicRuntime &topic, std::uint16_t index,
                   std::uint64_t wall_ns, std::uint64_t mono_ns) {
    consume::AggOrderBookSnapshot snapshot{};
    if (topic.topic.latest == nullptr ||
        !topic.topic.latest->snapshot(snapshot)) {
      return;
    }
    if (!snapshot.status.ready) {
      topic.pending_reset = true;
      topic.last_reset_generation = snapshot.status.reset_generation;
      topic.last_sequence = snapshot.status.receive.ring_sequence;
      topic.last_epoch = snapshot.status.receive.ring_epoch;
      stale_skips.fetch_add(1, std::memory_order_relaxed);
      return;
    }
    utils::md::wire::AggOrderBookRecord compacted{};
    compact_book(snapshot.record, options.depth, compacted);
    AggregateOrderBookSample sample{};
    sample.topic_index = index;
    sample.sampled_at_ns = wall_ns;
    sample.source_wall_ns = snapshot.status.receive.receive_wall_ns;
    sample.mono_ns = mono_ns;
    sample.ring_epoch = snapshot.status.receive.ring_epoch;
    sample.ring_sequence = snapshot.status.receive.ring_sequence;
    sample.generation = snapshot.status.generation;
    sample.record_flags = flags_for(topic, snapshot);
    sample.reset_generation = snapshot.status.reset_generation;
    sample.header_flags = compacted.header.flags;
    sample.exchange_ts_ns = compacted.header.exchange_ts_ns;
    sample.book_generation = compacted.header.book_generation;
    sample.base_asset = compacted.base_asset;
    sample.quote_asset = compacted.quote_asset;
    sample.venue_slot_ids = compacted.venue_slot_ids;
    sample.price_scale = compacted.price_scale;
    sample.quantity_scale = compacted.quantity_scale;
    sample.member_count = compacted.member_count;
    sample.member_mask = compacted.member_mask;
    sample.active_mask = compacted.active_mask;
    sample.bid_count = compacted.bid_count;
    sample.ask_count = compacted.ask_count;
    const auto bid_copy =
        std::min<std::size_t>(compacted.bid_count, kCompactBookDepth);
    const auto ask_copy =
        std::min<std::size_t>(compacted.ask_count, kCompactBookDepth);
    for (std::size_t index = 0; index < bid_copy; ++index) {
      sample.bids[index] = compacted.bids[index];
    }
    for (std::size_t index = 0; index < ask_copy; ++index) {
      sample.asks[index] = compacted.asks[index];
    }
    enqueue(book_queue, sample, options.orderbook_queue_rows, book_queue_drops);
  }

  void run_sampler() {
    while (!stopping.load(std::memory_order_relaxed)) {
      sample(wall_now_ns(), mono_now_ns());
      std::unique_lock lock(mutex);
      wake.wait_for(lock,
                    std::chrono::milliseconds(options.sample_interval_ms),
                    [&] { return stopping.load(std::memory_order_relaxed); });
    }
  }

  void sample(std::uint64_t wall_ns, std::uint64_t mono_ns) {
    samples_seen.fetch_add(1, std::memory_order_relaxed);
    for (std::uint16_t index = 0; index < topics.size(); ++index) {
      auto &topic = topics[index];
      if (topic.topic.kind == consume::AggregateTopic::AggBbo) {
        sample_bbo(topic, index, wall_ns, mono_ns);
      } else if (topic.topic.kind == consume::AggregateTopic::AggOrderBook) {
        sample_book(topic, index, wall_ns, mono_ns);
      }
    }
  }
};

ClickHouseAggregateRecorder::ClickHouseAggregateRecorder(
    ClickHouseAggregateOptions options)
    : impl_(std::make_unique<Impl>()) {
  impl_->options = std::move(options);
  impl_->topics.reserve(impl_->options.topics.size());
  for (auto &topic : impl_->options.topics) {
    Impl::TopicRuntime runtime;
    runtime.identity = parse_identity(topic.segment);
    runtime.topic = std::move(topic);
    impl_->topics.push_back(std::move(runtime));
  }
}

ClickHouseAggregateRecorder::~ClickHouseAggregateRecorder() { stop(); }

bool ClickHouseAggregateRecorder::start() {
  auto &o = impl_->options;
  if (impl_->started.exchange(true) || o.host.empty() ||
      !identifier(o.database) || !identifier(o.bbo_table) ||
      !identifier(o.orderbook_table) || !identifier(o.catalog_table) ||
      !identifier(o.user) || !valid_catalog_scope(o.catalog_scope) ||
      o.sample_interval_ms < 200 || o.depth == 0 || o.depth > kMaximumDepth ||
      o.flush_interval_ms == 0 || o.request_timeout_ms == 0 ||
      o.batch_rows == 0 || o.bbo_queue_rows == 0 ||
      o.orderbook_queue_rows == 0 ||
      o.orderbook_queue_rows > kMaximumOrderBookQueueRows ||
      o.catalog_queue_rows == 0 || o.max_queued_bytes == 0 ||
      impl_->topics.empty()) {
    impl_->set_error("invalid ClickHouse aggregate recorder options");
    impl_->started.store(false, std::memory_order_relaxed);
    return false;
  }
  for (const auto &topic : impl_->topics) {
    if (topic.identity.profile.empty() || topic.identity.symbol.empty() ||
        topic.identity.kind.empty() || topic.topic.latest == nullptr) {
      impl_->set_error("invalid ClickHouse aggregate topic identity");
      impl_->started.store(false, std::memory_order_relaxed);
      return false;
    }
  }
  if (o.service_instance.empty()) {
    std::array<char, 256> host{};
    if (gethostname(host.data(), host.size()) != 0) {
      std::memcpy(host.data(), "unknown", 7);
    }
    host.back() = '\0';
    o.service_instance =
        std::string(host.data()) + ":" + std::to_string(::getpid());
  }
  impl_->writer = std::thread([this] { impl_->run_writer(); });
  if (o.enable_sampler) {
    impl_->sampler = std::thread([this] { impl_->run_sampler(); });
  }
  return true;
}

void ClickHouseAggregateRecorder::stop() noexcept {
  impl_->stopping.store(true, std::memory_order_release);
  impl_->wake.notify_all();
  if (impl_->sampler.joinable()) {
    impl_->sampler.join();
  }
  if (impl_->writer.joinable()) {
    impl_->writer.join();
  }
}

void ClickHouseAggregateRecorder::sample(std::uint64_t wall_ns,
                                         std::uint64_t mono_ns) noexcept {
  impl_->sample(wall_ns, mono_ns);
}

ClickHouseAggregateMetrics ClickHouseAggregateRecorder::metrics()
    const noexcept {
  ClickHouseAggregateMetrics result;
  result.samples_seen = impl_->samples_seen.load(std::memory_order_relaxed);
  result.rows_enqueued = impl_->rows_enqueued.load(std::memory_order_relaxed);
  result.rows_written = impl_->rows_written.load(std::memory_order_relaxed);
  {
    std::lock_guard lock(impl_->mutex);
    result.bbo_queue_depth = impl_->bbo_queue.size();
    result.orderbook_queue_depth = impl_->book_queue.size();
    result.catalog_queue_depth = impl_->catalog_queue.size();
  }
  result.queue_drops = impl_->queue_drops.load(std::memory_order_relaxed);
  result.bbo_queue_drops = impl_->bbo_queue_drops.load(std::memory_order_relaxed);
  result.orderbook_queue_drops =
      impl_->book_queue_drops.load(std::memory_order_relaxed);
  result.catalog_queue_drops =
      impl_->catalog_queue_drops.load(std::memory_order_relaxed);
  result.queued_bytes = impl_->queued_bytes.load(std::memory_order_relaxed);
  result.retry_rows = impl_->retry_rows.load(std::memory_order_relaxed);
  result.http_failures = impl_->http_failures.load(std::memory_order_relaxed);
  result.stale_skips = impl_->stale_skips.load(std::memory_order_relaxed);
  result.encode_failures =
      impl_->encode_failures.load(std::memory_order_relaxed);
  result.last_success_at =
      impl_->last_success_at.load(std::memory_order_relaxed);
  result.write_lag_ms = impl_->write_lag_ms.load(std::memory_order_relaxed);
  result.shutdown_drops = impl_->shutdown_drops.load(std::memory_order_relaxed);
  result.catalog_reconcile_ok =
      impl_->catalog_reconcile_ok.load(std::memory_order_relaxed);
  result.catalog_reconcile_failures =
      impl_->catalog_reconcile_failures.load(std::memory_order_relaxed);
  return result;
}

std::string ClickHouseAggregateRecorder::error() const {
  std::lock_guard lock(impl_->error_mutex);
  return impl_->error_message;
}

}  // namespace mds::record
