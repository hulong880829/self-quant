#include "mds/gateway/gateway.h"
#include "mds/record/clickhouse_aggregate_recorder.h"
#include "net/websocket_codec.h"
#include "utils/md/wire.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <cassert>
#include <cctype>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <mutex>
#include <netinet/in.h>
#include <span>
#include <sstream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <sys/socket.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace {

using namespace std::chrono_literals;
namespace record = mds::record;
namespace consume = mds::consume;
namespace wire = utils::md::wire;

void require(bool condition, std::string_view message = {}) {
  if (!condition) {
    throw std::runtime_error(message.empty()
                                 ? "clickhouse aggregate recorder test failed"
                                 : std::string(message));
  }
}

std::string normalize_sql(std::string text) {
  std::string out;
  bool space = false;
  for (const unsigned char c : text) {
    if (std::isspace(c) != 0) {
      space = true;
      continue;
    }
    if (space && !out.empty()) {
      out.push_back(' ');
    }
    space = false;
    out.push_back(static_cast<char>(c));
  }
  return out;
}

std::string read_file(const char *path) {
  std::ifstream input(path);
  require(static_cast<bool>(input), "migration SQL is missing");
  std::ostringstream stream;
  stream << input.rdbuf();
  return stream.str();
}

std::span<const std::byte> bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte *>(value.data()), value.size()};
}

int connect_client(std::uint16_t port) {
  const int fd = ::socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  require(fd >= 0);
  timeval timeout{.tv_sec = 1, .tv_usec = 0};
  require(::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout,
                       sizeof(timeout)) == 0);
  sockaddr_in address{};
  address.sin_family = AF_INET;
  address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  address.sin_port = htons(port);
  require(::connect(fd, reinterpret_cast<sockaddr *>(&address),
                    sizeof(address)) == 0);
  return fd;
}

void send_all(int fd, std::span<const std::byte> data) {
  while (!data.empty()) {
    const auto sent = ::send(fd, data.data(), data.size(), MSG_NOSIGNAL);
    require(sent > 0);
    data = data.subspan(static_cast<std::size_t>(sent));
  }
}

void upgrade(int fd, std::string_view token) {
  const std::string request =
      "GET /v1/market-data HTTP/1.1\r\n"
      "Host: localhost\r\n"
      "Upgrade: websocket\r\n"
      "Connection: Upgrade\r\n"
      "Sec-WebSocket-Version: 13\r\n"
      "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
      "Authorization: Bearer " +
      std::string(token) + "\r\n\r\n";
  send_all(fd, bytes(request));
  std::array<char, 1024> response{};
  std::string headers;
  while (headers.find("\r\n\r\n") == std::string::npos) {
    const auto count = ::recv(fd, response.data(), response.size(), 0);
    require(count > 0);
    headers.append(response.data(), static_cast<std::size_t>(count));
  }
  require(headers.starts_with("HTTP/1.1 101"));
}

void send_text(int fd, std::string_view text, std::uint32_t mask) {
  net::WebSocketFrameWriter writer(4096);
  std::string_view error;
  require(writer.prepare_with_mask(net::WsOpcode::Text, bytes(text), true,
                                   mask, error));
  send_all(fd, writer.pending());
}

std::size_t read_varuint(std::span<const std::byte> body, std::size_t &offset) {
  std::size_t size{};
  unsigned shift{};
  while (true) {
    require(offset < body.size(), "varuint truncated");
    const auto byte = std::to_integer<unsigned>(body[offset++]);
    size |= static_cast<std::size_t>(byte & 0x7fU) << shift;
    if ((byte & 0x80U) == 0) {
      return size;
    }
    shift += 7;
    require(shift < 64, "varuint overflow");
  }
}

template <typename T>
T read_pod(std::span<const std::byte> body, std::size_t &offset) {
  require(offset + sizeof(T) <= body.size(), "pod truncated");
  T value{};
  std::memcpy(&value, body.data() + offset, sizeof(T));
  offset += sizeof(T);
  return value;
}

std::string read_string(std::span<const std::byte> body, std::size_t &offset) {
  const auto size = read_varuint(body, offset);
  require(offset + size <= body.size(), "string truncated");
  const auto *data = reinterpret_cast<const char *>(body.data() + offset);
  offset += size;
  return {data, size};
}

template <typename T>
std::vector<T> read_array(std::span<const std::byte> body,
                          std::size_t &offset) {
  const auto size = read_varuint(body, offset);
  std::vector<T> values;
  values.reserve(size);
  for (std::size_t index = 0; index < size; ++index) {
    values.push_back(read_pod<T>(body, offset));
  }
  return values;
}

struct CapturedCall {
  std::string sql;
  std::vector<std::byte> body;
};

struct Captured {
  std::mutex mutex;
  std::vector<CapturedCall> calls;
};

bool is_insert(std::string_view sql, std::string_view table) {
  return sql.find("INSERT INTO") != std::string_view::npos &&
         sql.find(table) != std::string_view::npos;
}

void capture_call(Captured &captured, std::string_view sql,
                  std::span<const std::byte> body) {
  std::lock_guard lock(captured.mutex);
  captured.calls.push_back({std::string(sql), {body.begin(), body.end()}});
}

record::ClickHouseAggregateOptions base_options(Captured &captured) {
  record::ClickHouseAggregateOptions options;
  options.host = "unit-test";
  options.catalog_scope = "prod-agg";
  options.service_instance = "test-instance";
  options.sample_interval_ms = 200;
  options.depth = 50;
  options.flush_interval_ms = 5;
  options.request_timeout_ms = 50;
  options.batch_rows = 8;
  options.bbo_queue_rows = 32;
  options.orderbook_queue_rows = 8;
  options.catalog_queue_rows = 32;
  options.enable_sampler = false;
  options.request = [&](std::string_view sql, std::span<const std::byte> body,
                        std::string &) {
    capture_call(captured, sql, body);
    return true;
  };
  options.query = [&](std::string_view sql, std::string &body, std::string &) {
    capture_call(captured, sql, {});
    body.clear();
    return true;
  };
  return options;
}

const std::string kBboSegment =
    "/selfquant.mds.agg_perp_usdt_a-b.btcusdt.aggbbo.2";
const std::string kBookSegment =
    "/selfquant.mds.agg_perp_usdt_a-b.btcusdt.aggorderbook.2";

wire::AggBboRecord make_bbo(std::int32_t cross_bps, std::uint16_t flags = 0) {
  wire::AggBboRecord record{};
  record.header = wire::MakeHeader(utils::md::MessageType::AggBbo,
                                   static_cast<std::uint16_t>(sizeof(record)));
  record.header.flags = flags;
  record.header.exchange_ts_ns = 1'000;
  record.gated_bid.price = 100;
  record.gated_bid.quantity = 2;
  record.gated_bid.venue_quantity[0] = 2;
  record.gated_ask.price = 101;
  record.gated_ask.quantity = 3;
  record.gated_ask.venue_quantity[0] = 3;
  record.raw_bid.price = 99;
  record.raw_ask.price = 102;
  record.raw_cross_bps = cross_bps;
  record.gated_cross_bps = cross_bps;
  record.venue_slot_ids[0] = 7;
  record.venue_slot_ids[1] = 9;
  std::memcpy(record.base_asset.data(), "BTC", 3);
  std::memcpy(record.quote_asset.data(), "USDT", 4);
  record.price_scale = 1;
  record.quantity_scale = 2;
  return record;
}

wire::AggOrderBookRecord make_book() {
  wire::AggOrderBookRecord record{};
  record.header = wire::MakeHeader(
      utils::md::MessageType::AggOrderBook,
      static_cast<std::uint16_t>(sizeof(record)));
  record.header.state =
      static_cast<std::uint8_t>(utils::md::BookState::Live);
  record.active_mask = 1;
  record.bid_count = 3;
  record.ask_count = 3;
  record.venue_slot_ids[0] = 4;
  for (std::size_t index = 0; index < 3; ++index) {
    record.bids[index].price = 100 - static_cast<std::int64_t>(index);
    record.bids[index].quantity = 1;
    record.asks[index].price = 101 + static_cast<std::int64_t>(index);
    record.asks[index].quantity = 1;
  }
  std::memcpy(record.base_asset.data(), "BTC", 3);
  return record;
}

bool wait_written(const record::ClickHouseAggregateRecorder &recorder,
                  std::uint64_t rows) {
  for (int attempt = 0; attempt < 400; ++attempt) {
    if (recorder.metrics().rows_written >= rows) {
      return true;
    }
    std::this_thread::sleep_for(5ms);
  }
  return false;
}

struct DecodedBbo {
  std::string profile;
  std::string symbol;
  std::string segment;
  std::uint16_t record_flags{};
  std::uint16_t header_flags{};
  std::vector<std::uint8_t> venue_slot_ids;
  consume::CrossBpsWindow window{};
};

DecodedBbo decode_bbo_row(std::span<const std::byte> body,
                          std::size_t &offset) {
  DecodedBbo row;
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  row.profile = read_string(body, offset);
  row.symbol = read_string(body, offset);
  row.segment = read_string(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  row.record_flags = read_pod<std::uint16_t>(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  row.header_flags = read_pod<std::uint16_t>(body, offset);
  (void)read_pod<std::uint64_t>(body, offset);
  (void)read_string(body, offset);
  (void)read_string(body, offset);
  row.venue_slot_ids = read_array<std::uint8_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  auto skip_gated = [&] {
    (void)read_pod<std::int64_t>(body, offset);
    (void)read_pod<std::int64_t>(body, offset);
    (void)read_array<std::int64_t>(body, offset);
    (void)read_pod<std::uint64_t>(body, offset);
    (void)read_pod<std::uint32_t>(body, offset);
    (void)read_pod<std::uint32_t>(body, offset);
    (void)read_pod<std::uint8_t>(body, offset);
    (void)read_pod<std::uint8_t>(body, offset);
    (void)read_pod<std::uint8_t>(body, offset);
  };
  skip_gated();
  skip_gated();
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::int32_t>(body, offset);
  (void)read_pod<std::int32_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  (void)read_pod<std::uint32_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  row.window.start_mono_ns = read_pod<std::uint64_t>(body, offset);
  row.window.end_mono_ns = read_pod<std::uint64_t>(body, offset);
  row.window.raw_min = read_pod<std::int32_t>(body, offset);
  row.window.raw_max = read_pod<std::int32_t>(body, offset);
  row.window.gated_min = read_pod<std::int32_t>(body, offset);
  row.window.gated_max = read_pod<std::int32_t>(body, offset);
  row.window.sample_count = read_pod<std::uint64_t>(body, offset);
  return row;
}

std::vector<DecodedBbo> decode_all_bbo(std::span<const std::byte> body) {
  std::vector<DecodedBbo> rows;
  std::size_t offset{};
  while (offset < body.size()) {
    rows.push_back(decode_bbo_row(body, offset));
  }
  return rows;
}

struct DecodedCatalog {
  std::string scope;
  std::string segment;
  std::uint8_t active{};
  std::uint16_t schema_major{};
  std::uint16_t schema_minor{};
};

std::vector<DecodedCatalog> decode_catalog(std::span<const std::byte> body) {
  std::vector<DecodedCatalog> rows;
  std::size_t offset{};
  while (offset < body.size()) {
    DecodedCatalog row;
    row.scope = read_string(body, offset);
    (void)read_string(body, offset);
    (void)read_string(body, offset);
    (void)read_string(body, offset);
    (void)read_string(body, offset);
    row.segment = read_string(body, offset);
    row.active = read_pod<std::uint8_t>(body, offset);
    row.schema_major = read_pod<std::uint16_t>(body, offset);
    row.schema_minor = read_pod<std::uint16_t>(body, offset);
    (void)read_pod<std::int64_t>(body, offset);
    (void)read_pod<std::uint64_t>(body, offset);
    rows.push_back(std::move(row));
  }
  return rows;
}

std::vector<std::byte> first_insert(Captured &captured, std::string_view table) {
  std::lock_guard lock(captured.mutex);
  for (const auto &call : captured.calls) {
    if (is_insert(call.sql, table) && !call.body.empty()) {
      return call.body;
    }
  }
  return {};
}

void test_schema() {
  const auto file = normalize_sql(read_file(MDS_AGGREGATE_HISTORY_SQL));
  const auto expected = normalize_sql(
      record::clickhouse_aggregate_bbo_ddl("market_data",
                                           "aggregate_bbo_history") +
      ";\n\n" +
      record::clickhouse_aggregate_orderbook_ddl(
          "market_data", "aggregate_orderbook_history") +
      ";\n\n" +
      record::clickhouse_aggregate_catalog_ddl(
          "market_data", "aggregate_topic_catalog") +
      ";\n");
  require(file == expected, "migration SQL must match runtime DDL");
  constexpr std::string_view kNewTtl =
      "TTL toDateTime(sampled_at) + INTERVAL 24 HOUR";
  std::size_t ttl_count = 0;
  for (std::size_t pos = 0;; ++pos) {
    pos = file.find(kNewTtl, pos);
    if (pos == std::string::npos) {
      break;
    }
    ++ttl_count;
  }
  require(ttl_count == 2, "BBO and OrderBook TTL must each use toDateTime");
  require(file.find("TTL sampled_at + INTERVAL 24 HOUR") == std::string::npos,
          "old DateTime64 TTL expression must not remain");
  const auto catalog_pos = file.find("aggregate_topic_catalog");
  require(catalog_pos != std::string::npos, "catalog table missing");
  require(file.find("TTL", catalog_pos) == std::string::npos,
          "catalog must not use TTL");
  require(file.find("Array(UInt8)") != std::string::npos, "UInt8 array missing");
  require(file.find("catalog_scope") != std::string::npos, "scope missing");
  require(file.find("ReplacingMergeTree(updated_at_ns)") != std::string::npos,
          "replacing engine missing");
}

void test_windows_and_flags() {
  Captured captured;
  consume::AggregateLatestState latest(200'000'000ULL);
  auto options = base_options(captured);
  options.topics.push_back({.segment = kBboSegment,
                            .kind = consume::AggregateTopic::AggBbo,
                            .latest = &latest});
  record::ClickHouseAggregateRecorder recorder(std::move(options));
  require(recorder.start(), recorder.error());
  auto bbo = make_bbo(11, 0x20);
  latest.publish(bbo, {.ring_epoch = 1,
                       .ring_sequence = 10,
                       .receive_mono_ns = 1'000,
                       .receive_wall_ns = 2'000});
  consume::AggBboSnapshot live{};
  require(latest.snapshot(live), "live snapshot");
  recorder.sample(3'000, 4'000);
  const auto first_request = latest.request_bbo_window_rollover();
  require(first_request == 1, "CH sampler must not request rollover");

  latest.publish(make_bbo(15), {.ring_epoch = 1,
                                .ring_sequence = 11,
                                .receive_mono_ns = 1'000 + 200'000'000ULL,
                                .receive_wall_ns = 2'100});
  consume::AggBboWindowSnapshot completed{};
  require(latest.snapshot_bbo_window(0, completed), "completed window missing");
  consume::AggBboSnapshot after{};
  require(latest.snapshot(after), "after snapshot");
  recorder.sample(5'000, 6'000);
  recorder.sample(7'000, 8'000);
  require(latest.request_bbo_window_rollover() == 2, "extra rollover request");

  latest.reset(consume::AggregateTopic::AggBbo,
               {.ring_epoch = 1, .ring_sequence = 11});
  recorder.sample(9'000, 10'000);
  require(recorder.metrics().stale_skips >= 1, "stale skip");
  latest.publish(make_bbo(20), {.ring_epoch = 1,
                                .ring_sequence = 12,
                                .receive_mono_ns = 3'000,
                                .receive_wall_ns = 4'000});
  recorder.sample(11'000, 12'000);
  require(wait_written(recorder, 1), "rows not written");
  recorder.stop();

  const auto body = first_insert(captured, "aggregate_bbo_history");
  require(!body.empty(), "expected BBO insert");
  const auto decoded = decode_all_bbo(body);
  require(!decoded.empty());
  const auto &first = decoded.front();
  require(first.profile == "agg_perp_usdt_a-b", "profile");
  require(first.symbol == "btcusdt", "symbol");
  require(first.segment == kBboSegment, "segment");
  require(first.header_flags == 0x20, "header flags");
  require(first.venue_slot_ids.size() == 8, "slot count");
  require(first.venue_slot_ids[0] == 7, "slot 0");
  require(first.venue_slot_ids[1] == 9, "slot 1");
  require(first.window.sample_count == live.cross_window.sample_count,
          "first window samples");
  require(first.window.raw_min == live.cross_window.raw_min, "first window min");

  std::vector<DecodedBbo> rows;
  {
    std::lock_guard lock(captured.mutex);
    for (const auto &call : captured.calls) {
      if (!is_insert(call.sql, "aggregate_bbo_history") || call.body.empty()) {
        continue;
      }
      const auto decoded = decode_all_bbo(call.body);
      rows.insert(rows.end(), decoded.begin(), decoded.end());
    }
  }
  require(!rows.empty());
  bool saw_completed = false;
  bool saw_in_progress = false;
  bool saw_reset = false;
  for (const auto &row : rows) {
    if (row.window.raw_min == completed.cross_window.raw_min &&
        row.window.raw_max == completed.cross_window.raw_max &&
        row.window.sample_count == completed.cross_window.sample_count) {
      saw_completed = true;
    }
    if (row.window.raw_min == after.cross_window.raw_min &&
        row.window.sample_count == after.cross_window.sample_count &&
        row.window.sample_count == 1) {
      saw_in_progress = true;
    }
    if ((row.record_flags & record::kReset) != 0) {
      saw_reset = true;
    }
  }
  require(saw_completed, "completed window must be used once");
  require(saw_in_progress, "later sample must keep in-progress window");
  require(saw_reset, "ready sample after reset must set kReset");
}

void test_clickhouse_only_window_auto_complete() {
  require(record::kGap == (1U << 0U), "gap flag must stay bit0");
  require(record::kReset == (1U << 1U), "reset flag must stay bit1");
  require(record::kMaximumDepth == 50, "depth cap must stay 50");
  require(wire::kSchemaMajor == 2 && wire::kSchemaMinor == 0,
          "wire schema must stay 2/0");

  Captured captured;
  consume::AggregateLatestState latest(200'000'000ULL);
  auto options = base_options(captured);
  options.depth = 50;
  options.topics.push_back({.segment = kBboSegment,
                            .kind = consume::AggregateTopic::AggBbo,
                            .latest = &latest});
  record::ClickHouseAggregateRecorder recorder(std::move(options));
  require(recorder.start(), recorder.error());
  latest.publish(make_bbo(11), {.ring_epoch = 1,
                                .ring_sequence = 10,
                                .receive_mono_ns = 1'000,
                                .receive_wall_ns = 2'000});
  recorder.sample(3'000, 4'000);
  latest.publish(make_bbo(15), {.ring_epoch = 1,
                                .ring_sequence = 11,
                                .receive_mono_ns = 1'000 + 200'000'000ULL,
                                .receive_wall_ns = 2'100});
  consume::AggBboWindowSnapshot completed{};
  require(latest.snapshot_bbo_window(0, completed),
          "CH-only window must auto-complete without --record");
  recorder.sample(5'000, 6'000);
  require(latest.request_bbo_window_rollover() == 1,
          "CH sampler must not request rollover");
  require(wait_written(recorder, 1), "rows not written");
  recorder.stop();

  bool saw_completed = false;
  {
    std::lock_guard lock(captured.mutex);
    for (const auto &call : captured.calls) {
      if (!is_insert(call.sql, "aggregate_bbo_history") || call.body.empty()) {
        continue;
      }
      for (const auto &row : decode_all_bbo(call.body)) {
        if (row.window.raw_min == completed.cross_window.raw_min &&
            row.window.raw_max == completed.cross_window.raw_max &&
            row.window.sample_count == completed.cross_window.sample_count) {
          saw_completed = true;
        }
      }
    }
  }
  require(saw_completed, "auto-completed window must be written");
}

void test_catalog_scope_tombstone() {
  Captured captured;
  consume::AggregateLatestState latest;
  auto options = base_options(captured);
  options.topics.push_back({.segment = kBboSegment,
                            .kind = consume::AggregateTopic::AggBbo,
                            .latest = &latest});
  std::atomic<int> queries{};
  options.query = [&](std::string_view sql, std::string &body, std::string &) {
    require(sql.find("catalog_scope = 'prod-agg'") != std::string_view::npos,
          "query must filter catalog_scope");
    require(sql.find("GROUP BY catalog_scope, segment") !=
            std::string_view::npos,
            "query must group by scope and segment");
    const auto attempt = queries.fetch_add(1);
    if (attempt == 0) {
      return false;
    }
    body = "prod-agg\t/old.agg_perp_usdt_a-b.ethusdt.aggbbo.2\t1\n"
           "shadow-agg\t/shadow.agg_perp_usdt_a-b.ethusdt.aggbbo.2\t1\n";
    return true;
  };
  record::ClickHouseAggregateRecorder recorder(std::move(options));
  require(recorder.start());
  latest.publish(make_bbo(1), {.ring_epoch = 1,
                               .ring_sequence = 1,
                               .receive_mono_ns = 1,
                               .receive_wall_ns = 1});
  recorder.sample(2, 2);
  for (int attempt = 0; attempt < 400 &&
                         recorder.metrics().catalog_reconcile_ok == 0;
       ++attempt) {
    std::this_thread::sleep_for(5ms);
  }
  require(recorder.metrics().catalog_reconcile_failures >= 1,
          "query failure must be counted");
  require(recorder.metrics().catalog_reconcile_ok >= 1,
          "query must succeed after retry");
  require(wait_written(recorder, 2), "catalog rows not written");
  recorder.stop();

  bool tombstoned_old = false;
  bool tombstoned_shadow = false;
  bool activated_self = false;
  {
    std::lock_guard lock(captured.mutex);
    for (const auto &call : captured.calls) {
      if (!is_insert(call.sql, "aggregate_topic_catalog") ||
          call.body.empty()) {
        continue;
      }
      for (const auto &row : decode_catalog(call.body)) {
        require(row.scope == "prod-agg", "catalog insert escaped scope");
        require(row.schema_major == utils::md::wire::kSchemaMajor,
                "catalog schema_major must be wire 2");
        require(row.schema_minor == utils::md::wire::kSchemaMinor,
                "catalog schema_minor must be wire 0");
        if (row.segment == kBboSegment && row.active == 1) {
          activated_self = true;
        }
        if (row.segment == "/old.agg_perp_usdt_a-b.ethusdt.aggbbo.2" &&
            row.active == 0) {
          tombstoned_old = true;
        }
        if (row.segment.find("shadow") != std::string::npos &&
            row.active == 0) {
          tombstoned_shadow = true;
        }
      }
    }
  }
  require(activated_self, "configured segment must be active");
  require(tombstoned_old, "same-scope removed segment must be inactive");
  require(!tombstoned_shadow, "other-scope segment must not be tombstoned");
}

void test_queue_retry_and_book() {
  Captured captured;
  consume::AggregateLatestState bbo_state;
  consume::AggregateLatestState book_state;
  auto options = base_options(captured);
  options.bbo_queue_rows = 2;
  options.orderbook_queue_rows = 2;
  options.batch_rows = 1;
  options.max_queued_bytes = 1U << 20U;
  std::atomic<int> inserts{};
  options.request = [&](std::string_view sql, std::span<const std::byte> body,
                        std::string &) {
    capture_call(captured, sql, body);
    if (sql.find("INSERT INTO") != std::string_view::npos &&
        sql.find("aggregate_bbo_history") != std::string_view::npos) {
      const auto attempt = inserts.fetch_add(1);
      return attempt != 0;
    }
    return true;
  };
  options.topics.push_back({.segment = kBboSegment,
                            .kind = consume::AggregateTopic::AggBbo,
                            .latest = &bbo_state});
  options.topics.push_back({.segment = kBookSegment,
                            .kind = consume::AggregateTopic::AggOrderBook,
                            .latest = &book_state});
  record::ClickHouseAggregateRecorder recorder(std::move(options));
  require(recorder.start());
  bbo_state.publish(make_bbo(3), {.ring_epoch = 1,
                                  .ring_sequence = 1,
                                  .receive_mono_ns = 1,
                                  .receive_wall_ns = 1});
  book_state.publish(make_book(), {.ring_epoch = 2,
                                   .ring_sequence = 2,
                                   .receive_mono_ns = 2,
                                   .receive_wall_ns = 2});
  for (int sample = 0; sample < 6; ++sample) {
    recorder.sample(10 + static_cast<std::uint64_t>(sample),
                    20 + static_cast<std::uint64_t>(sample));
  }
  require(wait_written(recorder, 1));
  recorder.stop();
  require(recorder.metrics().retry_rows >= 1 ||
          recorder.metrics().bbo_queue_drops >= 1);
  require(recorder.metrics().queue_drops >= 1 ||
          recorder.metrics().retry_rows >= 1);
  bool saw_book = false;
  bool saw_nested = false;
  {
    std::lock_guard lock(captured.mutex);
    for (const auto &call : captured.calls) {
      if (is_insert(call.sql, "aggregate_orderbook_history") &&
          !call.body.empty()) {
        saw_book = true;
      }
      if (call.sql.find("CREATE TABLE") != std::string::npos &&
          call.sql.find("Array(Array(Int64))") != std::string::npos) {
        saw_nested = true;
      }
    }
  }
  require(saw_book);
  require(saw_nested);
}

void test_gateway_isolation() {
  consume::AggregateLatestState latest;
  Captured captured;
  auto options = base_options(captured);
  options.request = [&](std::string_view sql, std::span<const std::byte> body,
                        std::string &error) {
    if (sql.find("INSERT INTO") != std::string_view::npos) {
      std::this_thread::sleep_for(150ms);
      error = "refused";
      return false;
    }
    capture_call(captured, sql, body);
    return true;
  };
  options.query = [&](std::string_view, std::string &body, std::string &) {
    body.clear();
    return true;
  };
  options.topics.push_back({.segment = kBboSegment,
                            .kind = consume::AggregateTopic::AggBbo,
                            .latest = &latest});
  record::ClickHouseAggregateRecorder recorder(std::move(options));
  require(recorder.start());

  mds::gateway::Gateway gateway(
      {.listen_address = "127.0.0.1",
       .port = 0,
       .bearer_token = "secret",
       .max_clients = 2,
       .max_subscriptions_per_client = 2,
       .publish_interval_ms = 10,
       .ping_interval_ms = 5'000,
       .pong_timeout_ms = 1'000,
       .slow_client_timeout_ms = 1'000,
       .depth = 2},
      {{.segment = kBboSegment,
        .kind = consume::AggregateTopic::AggBbo,
        .latest = &latest}});
  require(gateway.start());
  latest.publish(make_bbo(1), {.ring_epoch = 1,
                               .ring_sequence = 1,
                               .receive_mono_ns = 1,
                               .receive_wall_ns = 1});
  recorder.sample(1, 1);
  const int client = connect_client(gateway.port());
  upgrade(client, "secret");
  send_text(client, "{\"op\":\"subscribe\",\"topic\":\"" + kBboSegment + "\"}",
            0x04030201U);
  net::WebSocketParser parser(64U << 10U);
  std::uint64_t first_sequence = 0;
  std::uint64_t last_sequence = 0;
  std::size_t binary_frames = 0;
  const auto callback = [&](const net::WsFrameView &frame) {
    if (frame.opcode == net::WsOpcode::Binary && frame.payload.size() >= 36) {
      const auto *data =
          reinterpret_cast<const std::uint8_t *>(frame.payload.data());
      require(data[0] == 'S' && data[1] == 'Q' && data[2] == 'G' &&
              data[3] == 'W');
      std::uint64_t sequence{};
      std::memcpy(&sequence, data + 28, sizeof(sequence));
      if (first_sequence == 0) {
        first_sequence = sequence;
      }
      last_sequence = sequence;
      ++binary_frames;
    }
    return true;
  };
  std::array<std::byte, 64U << 10U> receive{};
  const auto deadline = std::chrono::steady_clock::now() + 3s;
  std::uint64_t published = 1;
  while (std::chrono::steady_clock::now() < deadline &&
         (binary_frames < 3 || last_sequence <= first_sequence)) {
    ++published;
    latest.publish(make_bbo(static_cast<std::int32_t>(published)),
                   {.ring_epoch = 1,
                    .ring_sequence = published,
                    .receive_mono_ns = published,
                    .receive_wall_ns = published});
    recorder.sample(published, published);
    const auto count = ::recv(client, receive.data(), receive.size(), 0);
    if (count > 0) {
      std::string error;
      require(parser.feed(std::span<const std::byte>(
                              receive.data(), static_cast<std::size_t>(count)),
                          callback, error));
    }
  }
  ::close(client);
  gateway.stop();
  recorder.stop();
  require(!gateway.failed());
  require(binary_frames >= 1, "gateway must still publish first frame");
  require(last_sequence > first_sequence,
          "gateway sequence must advance during ClickHouse failures");
}

void test_no_sqrec_header() {
  for (const char *path : {MDS_CLICKHOUSE_AGGREGATE_RECORDER_H,
                           MDS_CLICKHOUSE_AGGREGATE_RECORDER_CPP}) {
    std::ifstream input(path);
    require(static_cast<bool>(input), "recorder source must be readable");
    std::ostringstream body;
    body << input.rdbuf();
    require(body.str().find("mds/record/record.h") == std::string::npos,
            "ClickHouseAggregateRecorder must not include record.h");
  }
}

}  // namespace

int main() {
  try {
    test_schema();
    test_windows_and_flags();
    test_clickhouse_only_window_auto_complete();
    test_catalog_scope_tombstone();
    test_queue_retry_and_book();
    test_gateway_isolation();
    test_no_sqrec_header();
  } catch (const std::exception &error) {
    std::fprintf(stderr, "test failed: %s\n", error.what());
    return 1;
  }
  return 0;
}
