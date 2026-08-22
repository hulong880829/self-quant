#include "mds/record/clickhouse_bbo_recorder.h"
#include "utils/md/wire_codec.h"

#include <array>
#include <atomic>
#include <cassert>
#include <chrono>
#include <cstring>
#include <mutex>
#include <string>
#include <string_view>
#include <thread>

namespace {

using namespace std::chrono_literals;
namespace md = utils::md;
namespace wire = utils::md::wire;

template <typename Encode>
std::vector<std::byte> encode(Encode &&call) {
  std::array<std::byte, sizeof(wire::InstrumentCatalogRecord)> storage{};
  const auto result = call(storage);
  assert(result);
  return {storage.begin(), storage.begin() +
                              static_cast<std::ptrdiff_t>(result.size)};
}

wire::HeaderFields header(md::InstrumentId id, std::uint64_t sequence) {
  return {.instrument_id = id,
          .bus_seq = sequence,
          .source_seq = sequence,
          .exchange_ts_ns = sequence,
          .book_generation = 1,
          .state = md::BookState::Live};
}

std::vector<std::byte> catalog(md::InstrumentId id,
                               md::ProductType product =
                                   md::ProductType::Perpetual,
                               std::string_view symbol = "BTCUSDT") {
  md::InstrumentCatalog value{};
  value.instrument_id = id;
  value.venue = md::Venue::Binance;
  value.product_type = product;
  value.price_scale = 2;
  value.quantity_scale = 3;
  assert(symbol.size() < value.canonical_symbol.size());
  std::memcpy(value.canonical_symbol.data(), symbol.data(), symbol.size());
  return encode([&](std::span<std::byte> destination) {
    return wire::EncodeInstrumentCatalog(destination, header(id, 1), value);
  });
}

std::vector<std::byte> bbo(md::InstrumentId id, std::uint64_t sequence,
                           std::int64_t bid_price = 10'000) {
  return encode([&](std::span<std::byte> destination) {
    return wire::EncodeBbo(destination, header(id, sequence),
                           {.price = bid_price, .quantity = 200},
                           {.price = bid_price + 1, .quantity = 300});
  });
}

mds::record::ClickHouseBboOptions options() {
  mds::record::ClickHouseBboOptions result;
  result.host = "unit-test";
  result.selectors = {
      {md::Venue::Binance, md::ProductType::Perpetual}};
  result.sample_interval_ms = 1000;
  result.stale_cutoff_ms = 5000;
  result.flush_interval_ms = 10;
  result.request_timeout_ms = 10;
  result.batch_rows = 1;
  result.queue_rows = 4;
  result.unresolved_rows = 1;
  return result;
}

template <typename T>
T read_pod(std::span<const std::byte> body, std::size_t &offset) {
  assert(offset + sizeof(T) <= body.size());
  T value{};
  std::memcpy(&value, body.data() + offset, sizeof(T));
  offset += sizeof(T);
  return value;
}

std::string read_string(std::span<const std::byte> body,
                        std::size_t &offset) {
  std::size_t size{};
  unsigned shift{};
  while (true) {
    assert(offset < body.size());
    const auto byte = std::to_integer<unsigned>(body[offset++]);
    size |= static_cast<std::size_t>(byte & 0x7fU) << shift;
    if ((byte & 0x80U) == 0) break;
    shift += 7;
  }
  assert(offset + size <= body.size());
  const auto *data = reinterpret_cast<const char *>(body.data() + offset);
  offset += size;
  return {data, size};
}

struct CapturedRow {
  std::string product;
  std::string symbol;
  std::int64_t bid_price{};
};

CapturedRow decode_row(std::span<const std::byte> body) {
  std::size_t offset{};
  (void)read_pod<std::int64_t>(body, offset);
  assert(read_string(body, offset) == "binance");
  CapturedRow row;
  row.product = read_string(body, offset);
  row.symbol = read_string(body, offset);
  row.bid_price = read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::int64_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  (void)read_pod<std::uint8_t>(body, offset);
  assert(offset == body.size());
  return row;
}

bool wait_for(const mds::record::ClickHouseBboRecorder &recorder,
              std::uint64_t rows) {
  for (int attempt = 0; attempt < 200; ++attempt) {
    if (recorder.metrics().rows_written >= rows) return true;
    std::this_thread::sleep_for(5ms);
  }
  return false;
}

void test_retry() {
  std::atomic<unsigned> insert_attempts{};
  auto configured = options();
  configured.request =
      [&](std::string_view query, std::span<const std::byte> body,
          std::string &error) {
        if (!query.starts_with("INSERT")) return true;
        assert(!body.empty());
        if (insert_attempts.fetch_add(1) == 0) {
          error = "injected HTTP failure";
          return false;
        }
        return true;
      };

  mds::record::ClickHouseBboRecorder recorder(std::move(configured));
  assert(recorder.start());
  const auto first_bbo = bbo(42, 2);
  const auto second_bbo = bbo(43, 3);
  recorder.consume(first_bbo, 1'000'000'000ULL);
  recorder.consume(second_bbo, 1'000'000'001ULL);
  assert(recorder.metrics().unresolved_instrument_drops == 1);
  const auto instrument = catalog(42);
  recorder.consume(instrument, 1'000'000'002ULL);

  recorder.sample(2'000'000'000ULL, 2'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 1);
  recorder.sample(2'999'000'000ULL, 2'999'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 1);
  assert(wait_for(recorder, 1));
  auto metrics = recorder.metrics();
  assert(metrics.http_failures == 1);
  assert(metrics.rows_requeued == 1);
  assert(metrics.queue_drops == 0);

  recorder.sample(3'000'000'000ULL, 3'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 2);
  recorder.stop();
  metrics = recorder.metrics();
  assert(metrics.rows_written == 2);
  assert(metrics.shutdown_drops == 0);
}

void test_stale_cutoff_and_product_isolation() {
  std::mutex rows_mutex;
  std::vector<CapturedRow> rows;
  auto configured = options();
  configured.selectors = {
      {md::Venue::Binance, md::ProductType::Spot},
      {md::Venue::Binance, md::ProductType::Perpetual}};
  configured.stale_cutoff_ms = 2500;
  configured.queue_rows = 16;
  configured.request =
      [&](std::string_view query, std::span<const std::byte> body,
          std::string &) {
        if (!query.starts_with("INSERT")) return true;
        std::lock_guard lock(rows_mutex);
        rows.push_back(decode_row(body));
        return true;
      };

  mds::record::ClickHouseBboRecorder recorder(std::move(configured));
  assert(recorder.start());
  const auto spot_catalog =
      catalog(101, md::ProductType::Spot, "BTCUSDT-SPOT");
  const auto perpetual_catalog =
      catalog(202, md::ProductType::Perpetual, "BTCUSDT-PERP");
  recorder.consume(spot_catalog, 1'000'000'000ULL);
  recorder.consume(perpetual_catalog, 1'000'000'000ULL);
  const auto spot_bbo = bbo(101, 2, 20'000);
  const auto perpetual_bbo = bbo(202, 2, 30'000);
  recorder.consume(spot_bbo, 1'000'000'000ULL);
  recorder.consume(perpetual_bbo, 1'000'000'000ULL);

  recorder.sample(1'000'000'000ULL, 1'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 2);
  recorder.sample(1'999'000'000ULL, 1'999'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 2);
  recorder.sample(2'000'000'000ULL, 2'000'000'000ULL);
  recorder.sample(3'000'000'000ULL, 3'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 6);
  assert(recorder.metrics().stale_skips == 0);

  recorder.sample(4'000'000'000ULL, 4'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 6);
  assert(recorder.metrics().stale_skips == 2);

  const auto recovered_perpetual = bbo(202, 3, 30'100);
  recorder.consume(recovered_perpetual, 4'000'000'000ULL);
  recorder.sample(5'000'000'000ULL, 5'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 7);
  assert(recorder.metrics().stale_skips == 3);

  const auto recovered_spot = bbo(101, 3, 20'100);
  recorder.consume(recovered_spot, 5'000'000'000ULL);
  recorder.sample(6'000'000'000ULL, 6'000'000'000ULL);
  assert(recorder.metrics().rows_enqueued == 9);
  assert(recorder.metrics().stale_skips == 3);
  assert(wait_for(recorder, 9));
  recorder.stop();

  std::lock_guard lock(rows_mutex);
  assert(rows.size() == 9);
  std::size_t spot_rows{};
  std::size_t perpetual_rows{};
  for (const auto &row : rows) {
    if (row.product == "spot") {
      ++spot_rows;
      assert(row.symbol == "BTCUSDT-SPOT");
      assert(row.bid_price == 20'000 || row.bid_price == 20'100);
    } else {
      assert(row.product == "perpetual");
      ++perpetual_rows;
      assert(row.symbol == "BTCUSDT-PERP");
      assert(row.bid_price == 30'000 || row.bid_price == 30'100);
    }
  }
  assert(spot_rows == 4);
  assert(perpetual_rows == 5);
}

}  // namespace

int main() {
  test_retry();
  test_stale_cutoff_and_product_isolation();
  return 0;
}
