#include "mds/service/venue_connection.h"
#include "net/tls_websocket.h"

#include <cassert>
#include <set>
#include <string>
#include <vector>

namespace {

mds::service::SymbolStreamOptions stream(std::string symbol,
                                         bool ticker = true,
                                         bool orderbook = false) {
  mds::service::SymbolStreamOptions result;
  result.symbol = std::move(symbol);
  result.ticker = ticker;
  result.orderbook = orderbook;
  return result;
}

}  // namespace

int main() {
  using mds::service::PartitionWebSocketSymbols;
  using utils::md::Venue;

  const std::vector<mds::service::SymbolStreamOptions> empty;
  assert(PartitionWebSocketSymbols(empty, 2, Venue::Binance).empty());

  std::vector<mds::service::SymbolStreamOptions> partition_streams = {
      stream("DDDUSDT"), stream("BBBUSDT"),
      stream("AAAUSDT"), stream("CCCUSDT"),
      stream("BBBUSDT", false, true)};
  const auto unlimited =
      PartitionWebSocketSymbols(partition_streams, 0, Venue::Binance);
  assert(unlimited.size() == 1 && unlimited.front().size() == 5);
  const auto one =
      PartitionWebSocketSymbols(partition_streams, 1, Venue::Binance);
  assert(one.size() == 4);
  const auto boundary =
      PartitionWebSocketSymbols(partition_streams, 2, Venue::Binance);
  assert(boundary.size() == 2);
  assert(boundary[0].size() == 3);
  assert(boundary[1].size() == 2);
  std::set<std::size_t> union_indices;
  for (const auto &shard : boundary) {
    union_indices.insert(shard.begin(), shard.end());
  }
  assert(union_indices.size() == partition_streams.size());
  const auto n_plus_one = PartitionWebSocketSymbols(
      std::span<const mds::service::SymbolStreamOptions>(
          partition_streams.data(), 4),
      3, Venue::Binance);
  assert(n_plus_one.size() == 2);
  const auto polymarket_partition =
      PartitionWebSocketSymbols(partition_streams, 1, Venue::Polymarket);
  assert(polymarket_partition.size() == 1);

  std::string error;
  auto tls = net::make_client_ssl_context(error);
  assert(tls);

  net::EpollLoop loop;
  mds::service::VenueConnectionOptions options;
  options.venue = utils::md::Venue::Binance;
  options.product = utils::md::ProductType::Spot;
  options.max_symbols_per_ws = 2;
  options.streams = partition_streams;
  mds::service::VenueConnection connection(loop, tls, std::move(options));

  assert(connection.websocket_shard_count() == 2);
  assert(connection.metrics().ws_shards == 2);
  assert(connection.websocket_shard("AAAUSDT") == 0);
  assert(connection.websocket_shard("BBBUSDT") == 0);
  assert(connection.websocket_shard("CCCUSDT") == 1);
  assert(connection.websocket_shard("DDDUSDT") == 1);
  assert(!connection.websocket_shard("UNKNOWN"));

  mds::service::VenueConnectionOptions polymarket;
  polymarket.venue = utils::md::Venue::Polymarket;
  polymarket.product = utils::md::ProductType::BinaryOption;
  polymarket.max_symbols_per_ws = 1;
  polymarket.streams = {
      stream("BTC5MUP"), stream("BTC5MDOWN"),
      stream("ETH5MUP"), stream("ETH5MDOWN")};
  mds::service::VenueConnection polymarket_connection(
      loop, tls, std::move(polymarket));
  assert(polymarket_connection.websocket_shard_count() == 1);
  assert(polymarket_connection.metrics().ws_shards == 1);
  return 0;
}
