#include "producer_config.h"

#include <array>
#include <cassert>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>
#include <unistd.h>

namespace {

std::string read_file(const std::filesystem::path &path) {
  std::ifstream input(path);
  std::ostringstream content;
  content << input.rdbuf();
  return content.str();
}

std::filesystem::path write_temp(std::string_view content,
                                 std::string_view suffix) {
  const auto path = std::filesystem::temp_directory_path() /
                    ("mds-producer-config-" + std::to_string(getpid()) + "-" +
                     std::string(suffix) + ".yaml");
  std::ofstream output(path);
  output << content;
  return path;
}

}  // namespace

int main() {
  const std::filesystem::path example(MDS_PRODUCER_EXAMPLE_CONFIG);
  const auto valid = mds::producer::load_config(example.string());
  assert(valid);
  assert(valid.value.connections.size() == 6);
  assert(valid.value.connections[0].streams.size() == 3);
  assert(valid.value.connections[0].endpoint.venue ==
         utils::md::Venue::Binance);
  assert(valid.value.connections[0].endpoint.product ==
         utils::md::ProductType::Perpetual);
  assert(valid.value.connections[0].endpoint.max_symbols_per_ws == 0);
  assert(valid.value.connections[0].endpoint.snapshot_failure_backoff_ms ==
         250);
  assert(valid.value.connections[0]
             .endpoint.snapshot_failure_backoff_max_ms == 30'000);
  assert(valid.value.connections[0]
             .endpoint.snapshot_max_consecutive_failures == 10);
  assert(valid.value.connections[0]
             .endpoint.snapshot_rate_limit_backoff_ms == 60'000);
  assert(valid.value.connections[0].endpoint.snapshot_ban_backoff_ms ==
         300'000);
  assert(valid.value.connections[0].endpoint.recovery_deadline_ms == 30'000);
  assert(valid.value.connections[0].endpoint.max_continuous_recovery_ms ==
         300'000);
  const auto &binance = valid.value.connections[0].streams;
  assert(binance[0].symbol == "BTCUSDT");
  assert(binance[0].subscribe_ticker);
  assert(binance[0].subscribe_orderbook);
  assert(binance[0].ladder_price_band_bps == 10);
  assert(binance[1].symbol == "ETHUSDT");
  assert(binance[1].subscribe_ticker);
  assert(!binance[1].subscribe_orderbook);
  assert(binance[2].symbol == "SOLUSDT");
  assert(!binance[2].subscribe_ticker);
  assert(binance[2].subscribe_orderbook);
  bool found_binance_perpetual{};
  for (const auto &connection : valid.value.connections) {
    if (connection.endpoint.venue != utils::md::Venue::Binance ||
        connection.endpoint.product !=
            utils::md::ProductType::Perpetual) {
      continue;
    }
    for (const auto &stream : connection.streams) {
      if (stream.subscribe_orderbook) {
        assert(stream.orderbook_depth == 1000);
        assert(stream.max_levels_per_message == 5000);
        found_binance_perpetual = true;
      }
    }
  }
  assert(found_binance_perpetual);

  const auto full_binance_path = example.parent_path() / "binance_spot.yaml";
  if (std::filesystem::exists(full_binance_path)) {
    const auto full_binance =
        mds::producer::load_config(full_binance_path.string());
    assert(full_binance);
    assert(full_binance.value.connections.size() == 2);
    assert(full_binance.value.ring_count == 8);
    assert(full_binance.value.max_rings == 8);
    assert(full_binance.value.max_total_ring_bytes == 134'217'728);
    for (const auto &connection : full_binance.value.connections) {
      assert(connection.endpoint.venue == utils::md::Venue::Binance);
      assert(connection.endpoint.max_symbols_per_ws == 200);
      assert(connection.streams.size() == 1);
      assert(connection.streams[0].discovery);
      assert(connection.streams[0].discovery->quote_assets ==
             std::vector<std::string>{"USDT"});
      assert(connection.streams[0].shard_count == 4);
      assert(connection.streams[0].multiplex_ring.ring_bytes ==
             16'777'216);
    }
  }

  auto text = read_file(example);
  auto configured_recovery = text;
  const auto perpetual =
      configured_recovery.find("    product: PERPETUAL");
  assert(perpetual != std::string::npos);
  const auto endpoint_end = configured_recovery.find(
      "\n\n", perpetual);
  assert(endpoint_end != std::string::npos);
  configured_recovery.insert(
      endpoint_end + 1,
      "    recovery_deadline_ms: 5000\n"
      "    max_continuous_recovery_ms: 12345\n");
  const auto configured_recovery_path =
      write_temp(configured_recovery, "recovery-duration");
  const auto configured =
      mds::producer::load_config(configured_recovery_path.string());
  assert(configured);
  assert(configured.value.connections[0]
             .endpoint.max_continuous_recovery_ms == 12'345);
  assert(configured.value.connections[0]
             .endpoint.recovery_deadline_ms == 5'000);

  auto invalid_recovery = configured_recovery;
  const auto recovery_duration =
      invalid_recovery.find("max_continuous_recovery_ms: 12345");
  assert(recovery_duration != std::string::npos);
  invalid_recovery.replace(
      recovery_duration,
      std::string("max_continuous_recovery_ms: 12345").size(),
      "max_continuous_recovery_ms: 0");
  const auto invalid_recovery_path =
      write_temp(invalid_recovery, "invalid-recovery-duration");
  assert(!mds::producer::load_config(invalid_recovery_path.string()));

  auto invalid_deadline = configured_recovery;
  const auto recovery_deadline =
      invalid_deadline.find("recovery_deadline_ms: 5000");
  assert(recovery_deadline != std::string::npos);
  invalid_deadline.replace(
      recovery_deadline,
      std::string("recovery_deadline_ms: 5000").size(),
      "recovery_deadline_ms: 20000");
  const auto invalid_deadline_path =
      write_temp(invalid_deadline, "invalid-recovery-deadline");
  assert(!mds::producer::load_config(invalid_deadline_path.string()));

  auto malformed = text;
  const auto ring = malformed.find("ring_bytes: 8388608");
  assert(ring != std::string::npos);
  malformed.replace(ring, std::string("ring_bytes: 8388608").size(),
                    "ring_bytes: 123");
  const auto bad_ring = write_temp(malformed, "ring");
  assert(!mds::producer::load_config(bad_ring.string()));

  malformed = text;
  const auto price_band =
      malformed.find("default_price_band_bps: 10");
  assert(price_band != std::string::npos);
  malformed.replace(
      price_band, std::string("default_price_band_bps: 10").size(),
      "default_price_band_bps: 0");
  const auto bad_price_band = write_temp(malformed, "price-band");
  assert(!mds::producer::load_config(bad_price_band.string()));

  malformed = text;
  malformed +=
      "\n  - stream: orderbook\n"
      "    symbol: BTCUSDT\n"
      "    product: PERPETUAL\n"
      "    depth: 1000\n"
      "    ladder_ticks_per_side: 8192\n"
      "    update_interval_ms: 1000\n";
  const auto conflict = write_temp(malformed, "conflict");
  assert(!mds::producer::load_config(conflict.string()));

  const auto unknown =
      write_temp("unknown_root_key: true\n", "unknown");
  assert(!mds::producer::load_config(unknown.string()));

  const std::string okx_books =
      "shared_memory:\n"
      "  prefix: /selfquant.test\n"
      "  ring_bytes: 1048576\n"
      "  max_record_bytes: 65536\n"
      "  max_readers: 4\n"
      "  reader_lease_timeout_ns: 1000000000\n"
      "order_book:\n"
      "  default_ladder_ticks_per_side: 8192\n"
      "  max_ladder_ticks_per_side: 16384\n"
      "venues:\n"
      "  - venue: okx\n"
      "    product: SPOT\n"
      "    websocket_endpoint: wss://ws.okx.com:8443/ws/v5/public\n"
      "    rest_endpoint: https://www.okx.com\n"
      "subscriptions:\n"
      "  - venue: okx\n"
      "    stream: orderbook\n"
      "    symbol: BTCUSDT\n"
      "    product: SPOT\n"
      "    orderbook_channel: books\n";
  const auto okx_path = write_temp(okx_books, "okx-books");
  const auto okx = mds::producer::load_config(okx_path.string());
  assert(okx);
  assert(okx.value.connections.size() == 1);
  assert(okx.value.connections[0].streams[0].orderbook_channel == "books");
  assert(okx.value.connections[0].streams[0].effective_interval_ms == 100);
  assert(okx.value.connections[0].streams[0].orderbook_depth == 400);
  assert(okx.value.connections[0].streams[0].max_levels_per_message == 1024);
  assert(okx.value.connections[0].streams[0].orderbook_channel_override);

  const auto okx_bad_interval = write_temp(
      okx_books + "    update_interval_ms: 100\n", "okx-interval");
  assert(!mds::producer::load_config(okx_bad_interval.string()));

  auto okx_fast = okx_books;
  const auto channel_line =
      okx_fast.find("    orderbook_channel: books\n");
  assert(channel_line != std::string::npos);
  okx_fast.erase(channel_line,
                 std::string("    orderbook_channel: books\n").size());
  const auto rest_line =
      okx_fast.find("    rest_endpoint: https://www.okx.com\n");
  assert(rest_line != std::string::npos);
  const auto auth_position =
      rest_line +
      std::string("    rest_endpoint: https://www.okx.com\n").size();
  okx_fast.insert(
      auth_position,
      "    auth:\n"
      "      api_key_env: MDS_TEST_OKX_KEY\n"
      "      secret_env: MDS_TEST_OKX_SECRET\n"
      "      passphrase_env: MDS_TEST_OKX_PASSPHRASE\n");
  const auto okx_fast_path = write_temp(okx_fast, "okx-fast");
  unsetenv("MDS_TEST_OKX_KEY");
  unsetenv("MDS_TEST_OKX_SECRET");
  unsetenv("MDS_TEST_OKX_PASSPHRASE");
  assert(!mds::producer::load_config(okx_fast_path.string()));
  setenv("MDS_TEST_OKX_KEY", "key", 1);
  setenv("MDS_TEST_OKX_SECRET", "secret", 1);
  setenv("MDS_TEST_OKX_PASSPHRASE", "passphrase", 1);
  const auto fast = mds::producer::load_config(okx_fast_path.string());
  assert(fast);
  assert(fast.value.connections[0].streams[0].orderbook_channel ==
         "books-l2-tbt");
  assert(fast.value.connections[0].streams[0].orderbook_depth == 400);
  assert(fast.value.connections[0].streams[0].max_levels_per_message == 1024);
  assert(fast.value.connections[0].streams[0]
             .requires_public_ws_login);
  unsetenv("MDS_TEST_OKX_KEY");
  unsetenv("MDS_TEST_OKX_SECRET");
  unsetenv("MDS_TEST_OKX_PASSPHRASE");

  const std::string gate_book =
      "shared_memory:\n"
      "  prefix: /selfquant.test\n"
      "  ring_bytes: 1048576\n"
      "  max_record_bytes: 65536\n"
      "  max_readers: 4\n"
      "  reader_lease_timeout_ns: 1000000000\n"
      "order_book:\n"
      "  default_ladder_ticks_per_side: 8192\n"
      "  max_ladder_ticks_per_side: 16384\n"
      "venues:\n"
      "  - venue: gate\n"
      "    product: PERPETUAL\n"
      "    websocket_endpoint: wss://fx-ws.gateio.ws/v4/ws/usdt\n"
      "    rest_endpoint: https://api.gateio.ws\n"
      "subscriptions:\n"
      "  - venue: gate\n"
      "    stream: orderbook\n"
      "    symbol: BEATUSDT\n"
      "    product: PERPETUAL\n"
      "    update_interval_ms: 20\n";
  const auto gate_path = write_temp(gate_book, "gate-book");
  const auto gate = mds::producer::load_config(gate_path.string());
  assert(gate);
  assert(gate.value.connections.size() == 1);
  assert(gate.value.connections[0].streams[0].orderbook_depth == 20);
  assert(gate.value.connections[0].streams[0].max_levels_per_message ==
         1024);

  const std::string polymarket_config =
      "shared_memory:\n"
      "  prefix: /selfquant.poly.test\n"
      "  ring_bytes: 1048576\n"
      "  max_record_bytes: 65536\n"
      "  max_readers: 4\n"
      "  reader_lease_timeout_ns: 1000000000\n"
      "order_book:\n"
      "  default_ladder_ticks_per_side: 1024\n"
      "  max_ladder_ticks_per_side: 4096\n"
      "venues:\n"
      "  - venue: polymarket\n"
      "    product: BINARY_OPTION\n"
      "    websocket_endpoint: wss://ws-subscriptions-clob.polymarket.com/ws/market\n"
      "    discovery_endpoint: https://gamma-api.polymarket.com\n"
      "subscriptions:\n"
      "  - venue: polymarket\n"
      "    stream: ticker\n"
      "    symbol: BTC5M\n"
      "    product: BINARY_OPTION\n"
      "    polymarket:\n"
      "      asset: BTC\n"
      "      period: 5m\n"
      "      outcomes: BOTH\n"
      "  - venue: polymarket\n"
      "    stream: orderbook\n"
      "    symbol: BTC5M\n"
      "    product: BINARY_OPTION\n"
      "    polymarket:\n"
      "      asset: BTC\n"
      "      period: 5m\n"
      "      outcomes: BOTH\n";
  const auto polymarket_path =
      write_temp(polymarket_config, "polymarket");
  const auto polymarket =
      mds::producer::load_config(polymarket_path.string());
  assert(polymarket);
  assert(polymarket.value.connections.size() == 1);
  assert(polymarket.value.connections[0].streams.size() == 2);
  assert(polymarket.value.connections[0].streams[0].symbol ==
         "BTC5MDOWN");
  assert(polymarket.value.connections[0].streams[1].symbol ==
         "BTC5MUP");
  for (const auto &stream :
       polymarket.value.connections[0].streams) {
    assert(stream.polymarket_rolling);
    assert(stream.polymarket_asset == "BTC");
    assert(stream.polymarket_period == "5m");
    assert(stream.subscribe_ticker);
    assert(stream.subscribe_orderbook);
  }

  const auto bad_polymarket = write_temp(
      polymarket_config +
          "      unknown_key: true\n",
      "polymarket-unknown");
  assert(!mds::producer::load_config(bad_polymarket.string()));

  const std::string discovery_config =
      "shared_memory:\n"
      "  prefix: /selfquant.discovery.test\n"
      "  ring_layout: multiplex\n"
      "  shard_count: 2\n"
      "  ring_bytes: 1048576\n"
      "  max_record_bytes: 65536\n"
      "  max_readers: 4\n"
      "venues:\n"
      "  - venue: binance\n"
      "    product: PERPETUAL\n"
      "    websocket_endpoint: wss://fstream.binance.com/ws\n"
      "    rest_endpoint: https://fapi.binance.com\n"
      "    max_symbols_per_connection: 100\n"
      "    max_symbols_per_ws: 25\n"
      "subscriptions:\n"
      "  - venue: binance\n"
      "    product: PERPETUAL\n"
      "    stream: ticker\n"
      "    discovery:\n"
      "      quote_assets: [USDT]\n"
      "      symbol_regex: '^[A-Z]+USDT$'\n"
      "      minimum_turnover: 1000\n";
  const auto discovery_path =
      write_temp(discovery_config, "discovery");
  const auto discovered =
      mds::producer::load_config(discovery_path.string());
  if (!discovered) {
    throw std::runtime_error("discovery config failed: " +
                             discovered.message);
  }
  assert(discovered.value.instrument_count == 100);
  assert(discovered.value.connections[0]
             .endpoint.max_symbols_per_connection == 100);
  assert(discovered.value.connections[0].endpoint.max_symbols_per_ws == 25);
  assert(discovered.value.ring_count == 2);
  assert(discovered.value.connections[0].streams[0].discovery);

  auto unsafe_discovery = discovery_config;
  const auto multiplex = unsafe_discovery.find("  ring_layout: multiplex\n");
  assert(multiplex != std::string::npos);
  unsafe_discovery.replace(
      multiplex, std::string("  ring_layout: multiplex\n").size(),
      "  ring_layout: per_symbol\n");
  const auto unsafe_path =
      write_temp(unsafe_discovery, "unsafe-discovery");
  assert(!mds::producer::load_config(unsafe_path.string()));

  mds::producer::ProductDiscovery filter;
  filter.quote_assets = {"USDT"};
  filter.symbol_regex = "^(BTC|ETH|SOL)USDT$";
  filter.minimum_turnover = 1000;
  filter.max_symbols = 3;
  std::vector<mds::exchange::InstrumentMetadata> metadata(4);
  metadata[0].canonical_symbol = "BTCUSDT";
  metadata[0].venue_symbol = "BTCUSDT";
  metadata[0].quote_asset = "USDT";
  metadata[0].tick_size = metadata[0].lot_size = 1;
  metadata[0].turnover_24h = 2000;
  metadata[1] = metadata[0];
  metadata[1].canonical_symbol = "ETHUSDT";
  metadata[1].venue_symbol = "ETHUSDT";
  metadata[1].turnover_24h = 999;
  metadata[2] = metadata[0];
  metadata[2].canonical_symbol = "SOLUSDT";
  metadata[2].venue_symbol = "SOLUSDT";
  metadata[2].turnover_24h = 0;  // unavailable: reject with threshold enabled
  metadata[3] = metadata[0];
  metadata[3].canonical_symbol = "BTCUSDC";
  metadata[3].venue_symbol = "BTCUSDC";
  metadata[3].quote_asset = "USDC";
  const auto universe =
      mds::producer::reconcile_universe(filter, metadata, {});
  assert(universe);
  assert(universe.value.added.size() == 1);
  auto unfiltered = filter;
  unfiltered.minimum_turnover = 0;
  const auto zero_threshold =
      mds::producer::reconcile_universe(unfiltered, metadata, {});
  assert(zero_threshold);
  assert(zero_threshold.value.added.size() == 3);
  auto capped = unfiltered;
  capped.max_symbols = 2;
  assert(!mds::producer::reconcile_universe(capped, metadata, {}));
  const std::array active{metadata[0], metadata[3]};
  const auto changed =
      mds::producer::reconcile_universe(filter, metadata, active);
  assert(changed);
  assert(changed.value.added.empty());
  assert(changed.value.retained.size() == 1);
  assert(changed.value.retired.size() == 1);

  std::string pagination_error;
  mds::producer::DiscoveryPaginationGuard pagination;
  assert(pagination.begin_page(pagination_error));
  assert(pagination.accept_page(true, "cursor-1", pagination_error));
  assert(!pagination.complete());
  assert(pagination.cursor() == "cursor-1");
  assert(pagination.begin_page(pagination_error));
  assert(pagination.accept_page(true, "", pagination_error));
  assert(pagination.complete());
  assert(!pagination.begin_page(pagination_error));
  assert(pagination_error.find("already complete") != std::string::npos);

  mds::producer::DiscoveryPaginationGuard repeated_cursor;
  assert(repeated_cursor.begin_page(pagination_error));
  assert(repeated_cursor.accept_page(
      true, "cursor-1", pagination_error));
  assert(repeated_cursor.begin_page(pagination_error));
  assert(!repeated_cursor.accept_page(
      true, "cursor-1", pagination_error));
  assert(pagination_error.find("cursor repeated") != std::string::npos);

  mds::producer::DiscoveryPaginationGuard empty_page;
  assert(empty_page.begin_page(pagination_error));
  assert(!empty_page.accept_page(
      false, "cursor-1", pagination_error));
  assert(pagination_error.find("empty page") != std::string::npos);

  mds::producer::DiscoveryPaginationGuard page_limit;
  for (std::size_t page = 0;
       page + 1 <
       mds::producer::DiscoveryPaginationGuard::maximum_pages;
       ++page) {
    assert(page_limit.begin_page(pagination_error));
    assert(page_limit.accept_page(
        true, "cursor-" + std::to_string(page + 1),
        pagination_error));
  }
  assert(page_limit.begin_page(pagination_error));
  assert(!page_limit.accept_page(
      true, "cursor-limit", pagination_error));
  assert(pagination_error.find("page limit") != std::string::npos);

  std::filesystem::remove(bad_ring);
  std::filesystem::remove(configured_recovery_path);
  std::filesystem::remove(invalid_recovery_path);
  std::filesystem::remove(invalid_deadline_path);
  std::filesystem::remove(bad_price_band);
  std::filesystem::remove(conflict);
  std::filesystem::remove(unknown);
  std::filesystem::remove(okx_path);
  std::filesystem::remove(okx_bad_interval);
  std::filesystem::remove(okx_fast_path);
  std::filesystem::remove(gate_path);
  std::filesystem::remove(polymarket_path);
  std::filesystem::remove(bad_polymarket);
  std::filesystem::remove(discovery_path);
  std::filesystem::remove(unsafe_path);
  return 0;
}
