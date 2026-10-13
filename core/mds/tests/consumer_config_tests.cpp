#include "consumer_config.h"

#include <algorithm>
#include <cassert>
#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <unistd.h>

namespace {

std::filesystem::path write_temp(std::string_view content,
                                 std::string_view suffix) {
  const auto path = std::filesystem::temp_directory_path() /
                    ("mds-consumer-" + std::to_string(getpid()) + "-" +
                     std::string(suffix) + ".yaml");
  std::ofstream output(path);
  output << content;
  return path;
}

std::string config_with(std::string_view segments,
                        std::string_view gateway = {}) {
  return "segments:\n" + std::string(segments) +
         (gateway.empty() ? "" : "gateway:\n" + std::string(gateway));
}

constexpr std::string_view kBbo =
    "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n";
constexpr std::string_view kBook =
    "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggorderbook.2\n";

void verify(bool condition, std::string_view message) {
  if (!condition) {
    throw std::runtime_error(std::string(message));
  }
}

}  // namespace

int main() {
  const auto example =
      mds::consumer::load_config(MDS_CONSUMER_EXAMPLE_CONFIG, false, false);
  assert(example);
  assert(example.value.segments.size() == 2);
  assert(example.value.gateway.publish_interval_ms == 50);
  assert(example.value.gateway.depth == 50);
  assert(example.value.ingestion.stale_after_ms == 5000);
  assert(example.value.ingestion.hard_reset_after_ms == 10000);
  assert(example.value.ingestion.max_drain_records == 512);
  assert(example.value.clickhouse_aggregate.options.catalog_scope ==
         "prod-agg");
  assert(example.value.clickhouse_aggregate.options.sample_interval_ms ==
         200);
  assert(example.value.clickhouse_aggregate.options.depth == 50);
  assert(example.value.segments[0].selector.kind ==
         mds::consumer::SelectorKind::All);

  const auto multiplex_path = write_temp(
      "segments:\n"
      "  - multiplex:\n"
      "      prefix: /test\n"
      "      venue: binance\n"
      "      product: PERPETUAL\n"
      "      stream: orderbook\n"
      "      shard_count: 2\n"
      "    selector:\n"
      "      symbol: BTCUSDT\n",
      "multiplex");
  const auto multiplex =
      mds::consumer::load_config(multiplex_path.string(), false, false);
  assert(multiplex);
  assert(multiplex.value.segments.size() == 2);
  assert(multiplex.value.segments[0].name ==
         "/test.binance.perpetual.orderbook.shard0.2");
  assert(multiplex.value.segments[1].name ==
         "/test.binance.perpetual.orderbook.shard1.2");
  assert(multiplex.value.segments[0].selector.kind ==
         mds::consumer::SelectorKind::Symbol);

  utils::md::Instrument identity{};
  identity.venue = utils::md::Venue::Binance;
  identity.product_type = utils::md::ProductType::Perpetual;
  std::copy_n("BTCUSDT", 7, identity.canonical_symbol.begin());
  assert(multiplex.value.segments[0].selector.matches(identity));
  identity.canonical_symbol[0] = 'E';
  assert(!multiplex.value.segments[0].selector.matches(identity));

  const auto selectors_path = write_temp(
      "segments:\n"
      "  - name: /test.binance.perpetual.ticker.shard0.2\n"
      "    selector: {product: PERPETUAL}\n"
      "  - name: /test.okx.spot.ticker.shard0.2\n"
      "    selector: {venue: okx}\n"
      "  - name: /test.gate.spot.ticker.shard0.2\n"
      "    selector: {all: true}\n",
      "selectors");
  const auto selectors =
      mds::consumer::load_config(selectors_path.string(), false, false);
  assert(selectors && selectors.value.segments.size() == 3);
  assert(selectors.value.segments[0].selector.kind ==
         mds::consumer::SelectorKind::Product);
  assert(selectors.value.segments[1].selector.kind ==
         mds::consumer::SelectorKind::Venue);
  assert(selectors.value.segments[2].selector.kind ==
         mds::consumer::SelectorKind::All);

  const std::string auth = "  auth_token_env: TEST_GATEWAY_TOKEN\n";
  const auto bbo_only_path = write_temp(
      config_with(kBbo, auth + "  publish_interval_ms: 10\n"), "bbo-only");
  const auto bbo_only =
      mds::consumer::load_config(bbo_only_path.string(), true, false);
  assert(bbo_only);
  assert(bbo_only.value.segments.size() == 1);

  const auto book_only_path = write_temp(
      config_with(kBook, auth + "  publish_interval_ms: 200\n"), "book-only");
  assert(mds::consumer::load_config(book_only_path.string(), true, false));

  const auto too_fast = write_temp(
      config_with(kBbo, auth + "  publish_interval_ms: 9\n"), "too-fast");
  assert(!mds::consumer::load_config(too_fast.string(), true, false));

  const auto too_deep = write_temp(
      config_with(kBook, auth + "  depth: 51\n"), "too-deep");
  assert(!mds::consumer::load_config(too_deep.string(), true, false));

  const auto raw_segment = write_temp(
      config_with("  - /test.usdm.btcusdt.orderbook.1\n", auth), "raw");
  assert(!mds::consumer::load_config(raw_segment.string(), true, false));

  const auto multiple_symbols = write_temp(
      config_with(
          "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n"
          "  - /test.agg_perp_usdt_binance-okx.ethusdt.aggorderbook.2\n",
          auth),
      "multiple-symbols");
  assert(mds::consumer::load_config(multiple_symbols.string(), true, false));

  const auto duplicate = write_temp(
      config_with(
          "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n"
          "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n",
          auth),
      "duplicate");
  assert(!mds::consumer::load_config(duplicate.string(), true, false));

  const auto unknown = write_temp(
      config_with(kBbo, auth + "  unexpected: true\n"), "unknown");
  assert(!mds::consumer::load_config(unknown.string(), true, false));

  const auto ingestion = write_temp(
      "segments:\n"
      "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n"
      "ingestion:\n"
      "  stale_after_ms: 7000\n"
      "  hard_reset_after_ms: 12000\n"
      "  max_drain_records: 1024\n",
      "ingestion");
  const auto loaded_ingestion =
      mds::consumer::load_config(ingestion.string(), false, false);
  assert(loaded_ingestion);
  assert(loaded_ingestion.value.ingestion.stale_after_ms == 7000);
  assert(loaded_ingestion.value.ingestion.hard_reset_after_ms == 12000);
  assert(loaded_ingestion.value.ingestion.max_drain_records == 1024);

  const auto bad_ingestion_order = write_temp(
      "segments:\n"
      "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n"
      "ingestion: {stale_after_ms: 10000, hard_reset_after_ms: 10000}\n",
      "ingestion-order");
  assert(!mds::consumer::load_config(bad_ingestion_order.string(), false,
                                     false));

  const auto bad_ingestion_batch = write_temp(
      "segments:\n"
      "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n"
      "ingestion: {max_drain_records: 0}\n",
      "ingestion-batch");
  assert(!mds::consumer::load_config(bad_ingestion_batch.string(), false,
                                     false));

  const auto unknown_ingestion = write_temp(
      "segments:\n"
      "  - /test.agg_perp_usdt_binance-okx.btcusdt.aggbbo.2\n"
      "ingestion: {unexpected: true}\n",
      "ingestion-unknown");
  assert(!mds::consumer::load_config(unknown_ingestion.string(), false,
                                     false));

  const auto public_without_auth = write_temp(
      config_with(kBbo, "  listen_address: 0.0.0.0\n"), "public");
  assert(!mds::consumer::load_config(public_without_auth.string(), true,
                                     false));

  const auto loopback_without_auth = write_temp(
      config_with(kBbo, "  listen_address: 127.0.0.1\n"), "loopback");
  assert(!mds::consumer::load_config(loopback_without_auth.string(), true,
                                     false));

  const auto removed_tls = write_temp(
      config_with(kBbo,
                  auth + "  tls_certificate: /tmp/cert.pem\n"),
      "removed-tls");
  assert(!mds::consumer::load_config(removed_tls.string(), true, false));

  const auto leftover_recording = write_temp(
      config_with(kBbo) +
          "recording:\n"
          "  output_directory: /tmp\n",
      "leftover-recording");
  assert(!mds::consumer::load_config(leftover_recording.string(), false,
                                     false));

  const auto clickhouse = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  sample_interval_ms: 1000\n"
      "  stale_cutoff_ms: 2500\n"
      "  selectors:\n"
      "    - {venue: binance, product: perpetual, shard: 0}\n",
      "clickhouse");
  const auto loaded_clickhouse =
      mds::consumer::load_config(clickhouse.string(), false, true);
  assert(loaded_clickhouse);
  assert(loaded_clickhouse.value.clickhouse_bbo.segments.size() == 1);
  assert(loaded_clickhouse.value.clickhouse_bbo.segments[0] ==
         "/test.binance.perpetual.ticker.shard0.2");
  assert(loaded_clickhouse.value.clickhouse_bbo.options.sample_interval_ms ==
         1000);
  assert(loaded_clickhouse.value.clickhouse_bbo.options.stale_cutoff_ms ==
         2500);

  const auto aster_lighter = mds::consumer::load_config(
      MDS_ASTER_LIGHTER_CONSUMER_CONFIG, false, true);
  verify(static_cast<bool>(aster_lighter),
         "Aster/Lighter ClickHouse config must load");
  verify(aster_lighter.value.clickhouse_bbo.segments.size() == 8,
         "Aster/Lighter consumer must select eight perpetual ticker shards");
  for (const auto &segment :
       aster_lighter.value.clickhouse_bbo.segments) {
    verify(segment.starts_with("/selfquant.mds.bbo."),
           "new consumer must use the isolated BBO prefix");
    verify(segment.find(".perpetual.ticker.shard") != std::string::npos,
           "new consumer must select only perpetual ticker rings");
    verify(segment.find(".orderbook.") == std::string::npos &&
               segment.find("/selfquant.mds.book.") == std::string::npos,
           "ClickHouse consumer must never select order-book rings");
  }

  const auto clickhouse_legacy = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  selectors:\n"
      "    - {venue: binance, product: perpetual, shard: 0}\n",
      "clickhouse-legacy");
  const auto loaded_clickhouse_legacy =
      mds::consumer::load_config(clickhouse_legacy.string(), false, true);
  assert(loaded_clickhouse_legacy);
  assert(loaded_clickhouse_legacy.value.clickhouse_bbo.options
             .sample_interval_ms == 1000);
  assert(loaded_clickhouse_legacy.value.clickhouse_bbo.options
             .stale_cutoff_ms == 5000);

  const auto clickhouse_all = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  selectors:\n"
      "    - {venue: all, product: perpetual, shard: 0}\n",
      "clickhouse-all");
  assert(!mds::consumer::load_config(clickhouse_all.string(), false, true));

  const auto clickhouse_poly = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  selectors:\n"
      "    - {venue: polymarket, product: binary-option, shard: 0}\n",
      "clickhouse-poly");
  assert(!mds::consumer::load_config(clickhouse_poly.string(), false, true));

  const auto clickhouse_ibkr = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /selfquant.mds.ibkr\n"
      "  table: equity_bbo\n"
      "  selectors:\n"
      "    - {venue: ibkr, product: equity, shard: 0}\n",
      "clickhouse-ibkr");
  const auto loaded_ibkr =
      mds::consumer::load_config(clickhouse_ibkr.string(), false, true);
  assert(loaded_ibkr);
  assert(loaded_ibkr.value.clickhouse_bbo.segments.size() == 1);
  assert(loaded_ibkr.value.clickhouse_bbo.segments[0] ==
         "/selfquant.mds.ibkr.ibkr.equity.ticker.shard0.2");

  const auto clickhouse_ibkr_unified = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /selfquant.mds.bbo\n"
      "  table: crypto_bbo\n"
      "  selectors:\n"
      "    - {venue: ibkr, product: equity, shard: 0}\n",
      "clickhouse-ibkr-unified");
  const auto loaded_ibkr_unified =
      mds::consumer::load_config(clickhouse_ibkr_unified.string(), false, true);
  assert(loaded_ibkr_unified);
  assert(loaded_ibkr_unified.value.clickhouse_bbo.segments.size() == 1);
  assert(loaded_ibkr_unified.value.clickhouse_bbo.segments[0] ==
         "/selfquant.mds.bbo.ibkr.equity.ticker.shard0.2");
  assert(loaded_ibkr.value.clickhouse_bbo.options.selectors[0].venue ==
         utils::md::Venue::Ibkr);
  assert(loaded_ibkr.value.clickhouse_bbo.options.selectors[0].product ==
         utils::md::ProductType::Equity);

  const auto clickhouse_ibkr_spot = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  selectors:\n"
      "    - {venue: ibkr, product: spot, shard: 0}\n",
      "clickhouse-ibkr-spot");
  assert(!mds::consumer::load_config(clickhouse_ibkr_spot.string(), false,
                                     true));

  const auto clickhouse_binance_equity = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  selectors:\n"
      "    - {venue: binance, product: equity, shard: 0}\n",
      "clickhouse-binance-equity");
  assert(!mds::consumer::load_config(clickhouse_binance_equity.string(), false,
                                     true));

  const auto clickhouse_fast = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  sample_interval_ms: 199\n"
      "  selectors:\n"
      "    - {venue: binance, product: perpetual, shard: 0}\n",
      "clickhouse-fast");
  assert(!mds::consumer::load_config(clickhouse_fast.string(), false, true));

  const auto clickhouse_no_stale_cutoff = write_temp(
      "clickhouse_bbo:\n"
      "  host: clickhouse.test\n"
      "  shm_prefix: /test\n"
      "  stale_cutoff_ms: 0\n"
      "  selectors:\n"
      "    - {venue: binance, product: perpetual, shard: 0}\n",
      "clickhouse-no-stale-cutoff");
  assert(!mds::consumer::load_config(clickhouse_no_stale_cutoff.string(),
                                     false, true));

  const std::string aggregate_ok =
      "clickhouse_aggregate:\n"
      "  host: 127.0.0.1\n"
      "  catalog_scope: prod-agg\n"
      "  password_env: CLICKHOUSE_PASSWORD\n";
  const auto gateway_aggregate = write_temp(
      config_with(kBbo, auth) + aggregate_ok, "gw-agg");
  const auto loaded_gateway_aggregate = mds::consumer::load_config(
      gateway_aggregate.string(), true, false, true);
  assert(loaded_gateway_aggregate);
  assert(loaded_gateway_aggregate.value.clickhouse_aggregate.options
             .bbo_table == "aggregate_bbo_history");

  const auto bad_table = write_temp(
      config_with(kBbo) +
          "clickhouse_aggregate:\n"
          "  host: 127.0.0.1\n"
          "  catalog_scope: prod-agg\n"
          "  password_env: CLICKHOUSE_PASSWORD\n"
          "  bbo_table: agg-bbo\n",
      "agg-bad-table");
  assert(!mds::consumer::load_config(bad_table.string(), false, false, true));

  const auto bad_scope = write_temp(
      config_with(kBbo) +
          "clickhouse_aggregate:\n"
          "  host: 127.0.0.1\n"
          "  catalog_scope: \"\"\n"
          "  password_env: CLICKHOUSE_PASSWORD\n",
      "agg-bad-scope");
  assert(!mds::consumer::load_config(bad_scope.string(), false, false, true));

  const auto bad_queue = write_temp(
      config_with(kBbo) +
          "clickhouse_aggregate:\n"
          "  host: 127.0.0.1\n"
          "  catalog_scope: prod-agg\n"
          "  password_env: CLICKHOUSE_PASSWORD\n"
          "  orderbook_queue_rows: 20000\n",
      "agg-bad-queue");
  assert(!mds::consumer::load_config(bad_queue.string(), false, false, true));

  const auto missing_aggregate = write_temp(config_with(kBbo), "agg-missing");
  assert(!mds::consumer::load_config(missing_aggregate.string(), false, false,
                                     true));

  std::filesystem::remove(bbo_only_path);
  std::filesystem::remove(multiplex_path);
  std::filesystem::remove(selectors_path);
  std::filesystem::remove(book_only_path);
  std::filesystem::remove(too_fast);
  std::filesystem::remove(too_deep);
  std::filesystem::remove(raw_segment);
  std::filesystem::remove(multiple_symbols);
  std::filesystem::remove(duplicate);
  std::filesystem::remove(unknown);
  std::filesystem::remove(public_without_auth);
  std::filesystem::remove(loopback_without_auth);
  std::filesystem::remove(removed_tls);
  std::filesystem::remove(leftover_recording);
  std::filesystem::remove(clickhouse);
  std::filesystem::remove(clickhouse_legacy);
  std::filesystem::remove(clickhouse_all);
  std::filesystem::remove(clickhouse_poly);
  std::filesystem::remove(clickhouse_ibkr);
  std::filesystem::remove(clickhouse_ibkr_spot);
  std::filesystem::remove(clickhouse_binance_equity);
  std::filesystem::remove(clickhouse_fast);
  std::filesystem::remove(clickhouse_no_stale_cutoff);
  std::filesystem::remove(gateway_aggregate);
  std::filesystem::remove(bad_table);
  std::filesystem::remove(bad_scope);
  std::filesystem::remove(bad_queue);
  std::filesystem::remove(missing_aggregate);
  return 0;
}
