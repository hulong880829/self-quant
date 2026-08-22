#include "aggregator_config.h"

#include <cassert>
#include <filesystem>
#include <fstream>
#include <string>
#include <unistd.h>

namespace {

std::filesystem::path write_temp(std::string_view content,
                                 std::string_view suffix) {
  const auto path = std::filesystem::temp_directory_path() /
                    ("mds-aggregator-" + std::to_string(getpid()) + "-" +
                     std::string(suffix) + ".yaml");
  std::ofstream output(path);
  output << content;
  return path;
}

}  // namespace

int main() {
  const auto loaded =
      mds::aggregator::load_config(MDS_AGGREGATOR_EXAMPLE_CONFIG);
  assert(loaded);
  assert(loaded.value.book_count == 1);
  const auto &book = loaded.value.books[0];
  assert(book.member_count == 6);
  assert(book.enable_bbo);
  assert(book.enable_orderbook);
  assert(book.fx_enabled);
  assert(book.members[5].source_quote_asset == "USDC");
  assert(book.members[0].bbo_ttl_us > 0);
  assert(mds::aggregator::output_segment_name(
             loaded.value, book, "aggbbo") ==
         "/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-"
         "hyperliquid-okx."
         "btcusdt.aggbbo.2");
  assert(mds::aggregator::fx_segment_name(loaded.value) ==
         "/selfquant.mds.spot.usdcusdt.ticker.2");
  assert(mds::aggregator::input_segment_name(
             loaded.value, book, book.members[5], "ticker") ==
         "/selfquant.mds.hyperliquid_perp.btcusdc.ticker.2");

  const auto unknown =
      write_temp("shared_memory:\n  unexpected: true\nbooks: []\n",
                 "unknown");
  assert(!mds::aggregator::load_config(unknown.string()));

  const auto bad_quote = write_temp(
      "shared_memory:\n"
      "  prefix: /test\n"
      "  output:\n"
      "    ring_bytes: 1048576\n"
      "    max_record_bytes: 32768\n"
      "books:\n"
      "  - symbol: BTCUSDT\n"
      "    product: PERPETUAL\n"
      "    base_asset: BTC\n"
      "    quote_asset: USDT\n"
      "    members:\n"
      "      - venue: hyperliquid\n"
      "        source_quote_asset: USDC\n",
      "quote");
  assert(!mds::aggregator::load_config(bad_quote.string()));

  const auto bbo_only = write_temp(
      "shared_memory:\n"
      "  prefix: /test\n"
      "  output:\n"
      "    ring_bytes: 1048576\n"
      "    max_record_bytes: 32768\n"
      "books:\n"
      "  - symbol: ETHUSDT\n"
      "    product: PERPETUAL\n"
      "    base_asset: ETH\n"
      "    quote_asset: USDT\n"
      "    outputs:\n"
      "      aggbbo: true\n"
      "      aggorderbook: false\n"
      "    members:\n"
      "      - venue: binance\n",
      "bbo-only");
  const auto loaded_bbo_only =
      mds::aggregator::load_config(bbo_only.string());
  assert(loaded_bbo_only);
  assert(loaded_bbo_only.value.books[0].enable_bbo);
  assert(!loaded_bbo_only.value.books[0].enable_orderbook);
  assert(loaded_bbo_only.value.books[0].members[0].bbo_ttl_us > 0);
  assert(loaded_bbo_only.value.books[0].members[0].orderbook_ttl_us == 0);

  const auto book_only = write_temp(
      "shared_memory:\n"
      "  prefix: /test\n"
      "  output:\n"
      "    ring_bytes: 1048576\n"
      "    max_record_bytes: 32768\n"
      "books:\n"
      "  - symbol: ETHUSDT\n"
      "    product: PERPETUAL\n"
      "    base_asset: ETH\n"
      "    quote_asset: USDT\n"
      "    outputs:\n"
      "      aggbbo: false\n"
      "      aggorderbook: true\n"
      "    members:\n"
      "      - venue: okx\n",
      "book-only");
  const auto loaded_book_only =
      mds::aggregator::load_config(book_only.string());
  assert(loaded_book_only);
  assert(!loaded_book_only.value.books[0].enable_bbo);
  assert(loaded_book_only.value.books[0].enable_orderbook);
  assert(loaded_book_only.value.books[0].members[0].bbo_ttl_us == 0);
  assert(loaded_book_only.value.books[0].members[0].orderbook_ttl_us > 0);

  const auto ttl_override = write_temp(
      "shared_memory:\n"
      "  prefix: /test\n"
      "  output:\n"
      "    ring_bytes: 1048576\n"
      "    max_record_bytes: 32768\n"
      "books:\n"
      "  - symbol: BTCUSDT\n"
      "    product: PERPETUAL\n"
      "    base_asset: BTC\n"
      "    quote_asset: USDT\n"
      "    members:\n"
      "      - venue: okx\n"
      "        bbo_ttl_ms: 300\n"
      "        orderbook_ttl_ms: 450\n",
      "ttl-override");
  const auto loaded_ttl =
      mds::aggregator::load_config(ttl_override.string());
  assert(loaded_ttl);
  assert(loaded_ttl.value.books[0].members[0].bbo_ttl_us == 300'000);
  assert(loaded_ttl.value.books[0].members[0].orderbook_ttl_us == 450'000);

  const auto invalid_ttl = [&](std::string_view key, std::string_view value,
                               std::string_view outputs,
                               std::string_view suffix) {
    return write_temp(
        "shared_memory:\n"
        "  prefix: /test\n"
        "  output:\n"
        "    ring_bytes: 1048576\n"
        "    max_record_bytes: 32768\n"
        "books:\n"
        "  - symbol: BTCUSDT\n"
        "    product: PERPETUAL\n"
        "    base_asset: BTC\n"
        "    quote_asset: USDT\n" +
            std::string(outputs) +
            "    members:\n"
            "      - venue: okx\n"
            "        " +
            std::string(key) + ": " + std::string(value) + "\n",
        suffix);
  };
  const auto zero_ttl = invalid_ttl("bbo_ttl_ms", "0", "", "ttl-zero");
  const auto overflow_ttl =
      invalid_ttl("bbo_ttl_ms", "18446744073709552", "", "ttl-overflow");
  const auto bad_type_ttl =
      invalid_ttl("bbo_ttl_ms", "invalid", "", "ttl-type");
  const auto disabled_ttl = invalid_ttl(
      "bbo_ttl_ms", "300",
      "    outputs:\n"
      "      aggbbo: false\n"
      "      aggorderbook: true\n",
      "ttl-disabled");
  assert(!mds::aggregator::load_config(zero_ttl.string()));
  assert(!mds::aggregator::load_config(overflow_ttl.string()));
  assert(!mds::aggregator::load_config(bad_type_ttl.string()));
  assert(!mds::aggregator::load_config(disabled_ttl.string()));

  const auto overlong_asset = write_temp(
      "shared_memory:\n"
      "  prefix: /test\n"
      "  output:\n"
      "    ring_bytes: 1048576\n"
      "    max_record_bytes: 32768\n"
      "books:\n"
      "  - symbol: ETHUSDT\n"
      "    product: PERPETUAL\n"
      "    base_asset: ASSETNAMEOVER15X\n"
      "    quote_asset: USDT\n"
      "    members:\n"
      "      - venue: binance\n",
      "asset-capacity");
  assert(!mds::aggregator::load_config(overlong_asset.string()));

  const auto duplicate_output = write_temp(
      "shared_memory:\n"
      "  prefix: /test\n"
      "  output:\n"
      "    ring_bytes: 1048576\n"
      "    max_record_bytes: 32768\n"
      "books:\n"
      "  - &book\n"
      "    symbol: ETHUSDT\n"
      "    product: PERPETUAL\n"
      "    base_asset: ETH\n"
      "    quote_asset: USDT\n"
      "    members:\n"
      "      - venue: binance\n"
      "  - *book\n",
      "duplicate-output");
  assert(!mds::aggregator::load_config(duplicate_output.string()));

  std::filesystem::remove(unknown);
  std::filesystem::remove(bad_quote);
  std::filesystem::remove(bbo_only);
  std::filesystem::remove(book_only);
  std::filesystem::remove(ttl_override);
  std::filesystem::remove(zero_ttl);
  std::filesystem::remove(overflow_ttl);
  std::filesystem::remove(bad_type_ttl);
  std::filesystem::remove(disabled_ttl);
  std::filesystem::remove(overlong_asset);
  std::filesystem::remove(duplicate_output);
  return 0;
}
