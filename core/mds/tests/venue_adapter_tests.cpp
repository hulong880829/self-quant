#include "mds/exchange/venue_adapter.h"
#include "net/http_client.h"

#include <array>
#include <atomic>
#include <cassert>
#include <cstdio>
#include <cstdlib>
#include <memory>
#include <new>
#include <string>
#include <vector>

namespace {
std::atomic_bool track_allocations{};
std::atomic<std::size_t> tracked_allocations{};
}

void *operator new(std::size_t size) {
  if (track_allocations.load(std::memory_order_relaxed)) {
    tracked_allocations.fetch_add(1, std::memory_order_relaxed);
  }
  if (void *memory = std::malloc(size)) {
    return memory;
  }
  throw std::bad_alloc();
}
void *operator new[](std::size_t size) { return ::operator new(size); }
void *operator new(std::size_t size, const std::nothrow_t &) noexcept {
  try {
    return ::operator new(size);
  } catch (...) {
    return nullptr;
  }
}
void *operator new[](std::size_t size,
                     const std::nothrow_t &) noexcept {
  return ::operator new(size, std::nothrow);
}
void operator delete(void *memory) noexcept { std::free(memory); }
void operator delete[](void *memory) noexcept { std::free(memory); }
void operator delete(void *memory, std::size_t) noexcept {
  std::free(memory);
}
void operator delete[](void *memory, std::size_t) noexcept {
  std::free(memory);
}
void operator delete(void *memory,
                     const std::nothrow_t &) noexcept {
  std::free(memory);
}
void operator delete[](void *memory,
                       const std::nothrow_t &) noexcept {
  std::free(memory);
}

namespace {

using mds::exchange::AdapterEventType;
using mds::exchange::InstrumentMetadata;
using mds::exchange::MetadataRequestBatch;
using mds::exchange::NormalizedEvent;
using mds::exchange::ParseFailure;
using mds::exchange::ParseFailureCategory;
using mds::exchange::ParseFailureCode;
using mds::exchange::ParseFailureScope;
using mds::exchange::StreamRequest;
using mds::exchange::VenueAdapter;
using utils::md::ProductType;
using utils::md::Venue;

void expect_no_allocation(VenueAdapter &adapter, std::string_view json,
                          NormalizedEvent &event, std::string &error) {
  tracked_allocations.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_release);
  const bool parsed = adapter.parse_ws(json, event, error);
  track_allocations.store(false, std::memory_order_release);
  assert(parsed);
  if (tracked_allocations.load(std::memory_order_relaxed) != 0) {
    std::fprintf(stderr, "unexpected parse allocation json_bytes=%zu count=%zu\n",
                 json.size(),
                 tracked_allocations.load(std::memory_order_relaxed));
  }
  assert(tracked_allocations.load(std::memory_order_relaxed) == 0);
}

void expect_no_allocation(VenueAdapter &adapter, std::string_view json,
                          NormalizedEvent &event, ParseFailure &failure) {
  tracked_allocations.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_release);
  const bool parsed = adapter.parse_ws(json, event, failure);
  track_allocations.store(false, std::memory_order_release);
  assert(parsed);
  assert(failure.category == ParseFailureCategory::None);
  assert(failure.diagnostic_view().empty());
  if (tracked_allocations.load(std::memory_order_relaxed) != 0) {
    std::fprintf(stderr,
                 "unexpected structured parse allocation json_bytes=%zu "
                 "count=%zu\n",
                 json.size(),
                 tracked_allocations.load(std::memory_order_relaxed));
  }
  assert(tracked_allocations.load(std::memory_order_relaxed) == 0);
}

std::string okx_book_message(std::size_t ask_count) {
  std::string json =
      R"({"arg":{"channel":"books","instId":"BTC-USDT"},"action":"snapshot","data":[{"asks":[)";
  json.reserve(json.size() + ask_count * 28);
  for (std::size_t index = 0; index < ask_count; ++index) {
    if (index != 0) {
      json += ',';
    }
    json += R"(["100.1","1.0","0","1"])";
  }
  json +=
      R"(],"bids":[["100.0","1.0","0","1"]],"ts":"12","seqId":9,"prevSeqId":-1}]})";
  return json;
}

std::string gate_book_message(std::size_t bid_count) {
  std::string json =
      R"({"channel":"futures.order_book_update","event":"update","time_ms":17,"result":{"s":"BEAT_USDT","U":20,"u":21,"b":[)";
  json.reserve(json.size() + bid_count * 23);
  for (std::size_t index = 0; index < bid_count; ++index) {
    if (index != 0) {
      json += ',';
    }
    json += R"({"p":"0.1000","s":1})";
  }
  json += R"(],"a":[{"p":"0.1001","s":1}]}})";
  return json;
}

std::string bitget_book_message(std::size_t bid_count,
                                std::size_t ask_count,
                                std::string_view action = "snapshot",
                                std::uint64_t sequence = 1,
                                std::uint64_t previous = 0) {
  std::string json =
      R"({"arg":{"channel":"books","instId":"BTCUSDT"},"action":")";
  json += action;
  json += R"(","data":[{"bids":[)";
  json.reserve(json.size() + (bid_count + ask_count) * 20);
  for (std::size_t index = 0; index < bid_count; ++index) {
    if (index != 0) {
      json += ',';
    }
    json += R"(["100.0","1.000"])";
  }
  json += R"(],"asks":[)";
  for (std::size_t index = 0; index < ask_count; ++index) {
    if (index != 0) {
      json += ',';
    }
    json += R"(["100.1","2.000"])";
  }
  json += R"(],"ts":"10","seq":)";
  json += std::to_string(sequence);
  if (action == "update") {
    json += R"(,"pseq":)";
    json += std::to_string(previous);
  }
  json += "}]}";
  return json;
}

std::string binance_depth_message(std::size_t bid_count,
                                  std::uint64_t first = 157,
                                  std::uint64_t final = 160,
                                  std::uint64_t previous = 156) {
  std::string json =
      R"({"e":"depthUpdate","E":11,"T":11,"s":"BTCUSDT","U":)";
  json += std::to_string(first);
  json += R"(,"u":)";
  json += std::to_string(final);
  json += R"(,"pu":)";
  json += std::to_string(previous);
  json += R"(,"b":[)";
  json.reserve(json.size() + bid_count * 22);
  for (std::size_t index = 0; index < bid_count; ++index) {
    if (index != 0) {
      json += ',';
    }
    json += R"(["100.0","1.000"])";
  }
  json += R"(],"a":[["100.1","2.000"]]})";
  return json;
}

void test_binance() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Perpetual, 5000);
  assert(adapter);
  const StreamRequest request{"BTCUSDT", "BTCUSDT", "bookTicker",
                              "depth", true, true, 100};
  std::vector<std::string> batches;
  std::string error;
  assert(adapter->build_subscription_batches({&request, 1}, batches, error));
  assert(batches.size() == 1);
  assert(batches[0].find("btcusdt@bookTicker") != std::string::npos);
  assert(batches[0].find("btcusdt@depth@100ms") != std::string::npos);

  constexpr std::string_view metadata_json =
      R"({"serverTime":1,"symbols":[{"symbol":"BTCUSDT","pair":"BTCUSDT","contractType":"PERPETUAL","deliveryDate":4133404800000,"onboardDate":1569398400000,"status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","pricePrecision":1,"quantityPrecision":3,"underlyingType":"COIN","filters":[{"filterType":"PRICE_FILTER","maxPrice":"1000000.0","minPrice":"0.1","tickSize":"0.1"},{"filterType":"LOT_SIZE","maxQty":"1000.000","minQty":"0.001","stepSize":"0.001"}]}]})";
  std::vector<InstrumentMetadata> metadata;
  assert(adapter->parse_metadata(metadata_json, {&request, 1}, metadata,
                                 error));
  assert(metadata.size() == 1);

  NormalizedEvent event(5000);
  assert(adapter->parse_ws(
      R"({"result":null,"id":1})", event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  constexpr std::string_view bbo =
      R"({"u":10,"s":"BTCUSDT","b":"100.0","B":"1.000","a":"100.1","A":"2.000","T":10,"E":10})";
  assert(adapter->parse_ws(bbo, event, error));
  assert(event.type == AdapterEventType::Bbo);
  expect_no_allocation(*adapter, bbo, event, error);
  constexpr std::string_view depth =
      R"({"e":"depthUpdate","E":11,"T":11,"s":"BTCUSDT","U":157,"u":160,"pu":156,"b":[["100.0","1.000"]],"a":[["100.1","2.000"]]})";
  assert(adapter->parse_ws(depth, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.first_sequence == 157);
  assert(event.final_sequence == 160);
  assert(event.previous_sequence == 156);
  expect_no_allocation(*adapter, depth, event, error);
  const auto burst = binance_depth_message(1001);
  expect_no_allocation(*adapter, burst, event, error);
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids.size() == 1001);
  const auto capacity_edge = binance_depth_message(5000);
  expect_no_allocation(*adapter, capacity_edge, event, error);
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids.size() == 5000);
  const auto overflow = binance_depth_message(5001, 161, 165, 160);
  assert(adapter->parse_ws(overflow, event, error));
  assert(event.type == AdapterEventType::BookGap);
  assert(event.input_side == mds::exchange::InputSide::Bid);
  assert(event.input_capacity == 5000);
  assert(event.first_sequence == 161);
  assert(event.final_sequence == 165);
  assert(event.previous_sequence == 160);
  assert(event.bids.empty());
  assert(event.asks.empty());
  assert(error.find("symbol=BTCUSDT") != std::string::npos);
  assert(error.find("capacity=5000") != std::string::npos);
  assert(adapter->parse_ws(depth, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids.size() == 1);
  assert(event.asks.size() == 1);
  assert(!adapter->parse_ws("{", event, error));

  auto beat_adapter = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Perpetual, 1000);
  assert(beat_adapter);
  const StreamRequest beat_request{
      "BEATUSDT", "BEATUSDT", "bookTicker", "depth", true, true, 100};
  constexpr std::string_view beat_metadata =
      R"({"serverTime":1,"symbols":[{"symbol":"BEATUSDT","pair":"BEATUSDT","contractType":"PERPETUAL","deliveryDate":4133404800000,"onboardDate":1760000000000,"status":"TRADING","baseAsset":"BEAT","quoteAsset":"USDT","marginAsset":"USDT","pricePrecision":4,"quantityPrecision":2,"underlyingType":"COIN","filters":[{"filterType":"PRICE_FILTER","maxPrice":"1000","minPrice":"0.001","tickSize":"0.001"},{"filterType":"LOT_SIZE","maxQty":"100000","minQty":"1","stepSize":"1"}]}]})";
  metadata.clear();
  assert(beat_adapter->parse_metadata(
      beat_metadata, {&beat_request, 1}, metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].price_scale == 4);
  assert(metadata[0].quantity_scale == 2);
  assert(metadata[0].tick_size == 10);
  assert(metadata[0].lot_size == 100);
  assert(metadata[0].refine_book_tick);

  assert(beat_adapter->parse_ws(
      R"({"u":20,"s":"BEATUSDT","b":"2.6413","B":"10.25","a":"2.6420","A":"11.50","T":20,"E":20})",
      event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.bid.price == 26'413);
  assert(event.bid.quantity == 1'025);
  assert(beat_adapter->parse_ws(
      R"({"e":"depthUpdate","E":21,"T":21,"s":"BEATUSDT","U":200,"u":201,"pu":199,"b":[["2.6413","10.25"]],"a":[["2.6420","11.50"]]})",
      event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids[0].price == 26'413);
  assert(event.bids[0].quantity == 1'025);
  assert(beat_adapter->parse_snapshot(
      R"({"lastUpdateId":201,"bids":[["2.6413","10.25"]],"asks":[["2.6420","11.50"]]})",
      "BEATUSDT", event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.bids[0].price == 26'413);
  assert(!beat_adapter->parse_snapshot(
      R"({"lastUpdateId":202,"bids":[["2.64131","10.25"]],"asks":[]})",
      "BEATUSDT", event, error));
  assert(error.find("BEATUSDT") != std::string::npos);
  assert(error.find("bid") != std::string::npos);
  assert(error.find("precision") != std::string::npos);
}

void test_binance_shared_parser_scales() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Perpetual, 8);
  assert(adapter);
  const std::array<StreamRequest, 2> requests{{
      {"BTCUSDT", "BTCUSDT", "bookTicker", "depth", true, true, 100},
      {"BEATUSDT", "BEATUSDT", "bookTicker", "depth", true, true, 100},
  }};
  constexpr std::string_view metadata_json =
      R"({"serverTime":1,"symbols":[{"symbol":"BTCUSDT","pair":"BTCUSDT","contractType":"PERPETUAL","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","pricePrecision":1,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","maxPrice":"1000000.0","minPrice":"0.1","tickSize":"0.1"},{"filterType":"LOT_SIZE","maxQty":"1000.000","minQty":"0.001","stepSize":"0.001"}]},{"symbol":"BEATUSDT","pair":"BEATUSDT","contractType":"PERPETUAL","status":"TRADING","baseAsset":"BEAT","quoteAsset":"USDT","marginAsset":"USDT","pricePrecision":4,"quantityPrecision":2,"filters":[{"filterType":"PRICE_FILTER","maxPrice":"1000","minPrice":"0.0001","tickSize":"0.0001"},{"filterType":"LOT_SIZE","maxQty":"100000","minQty":"0.01","stepSize":"0.01"}]}]})";
  std::vector<InstrumentMetadata> metadata;
  std::string error;
  assert(adapter->parse_metadata(metadata_json, requests, metadata, error));
  assert(metadata.size() == 2);

  NormalizedEvent event(8);
  assert(adapter->parse_ws(
      R"({"u":1,"s":"BTCUSDT","b":"100.1","B":"1.001","a":"100.2","A":"2.002","E":1})",
      event, error));
  assert(event.bid.price == 1'001 && event.bid.quantity == 1'001);
  assert(adapter->parse_ws(
      R"({"u":2,"s":"BEATUSDT","b":"2.6413","B":"10.25","a":"2.6420","A":"11.50","E":2})",
      event, error));
  assert(event.bid.price == 26'413 && event.bid.quantity == 1'025);
  assert(adapter->parse_ws(
      R"({"e":"depthUpdate","E":3,"s":"BTCUSDT","U":3,"u":3,"pu":2,"b":[["100.1","1.001"]],"a":[["100.2","2.002"]]})",
      event, error));
  assert(event.bids[0].price == 1'001 &&
         event.bids[0].quantity == 1'001);
  assert(adapter->parse_ws(
      R"({"e":"depthUpdate","E":4,"s":"BEATUSDT","U":4,"u":4,"pu":3,"b":[["2.6413","10.25"]],"a":[["2.6420","11.50"]]})",
      event, error));
  assert(event.bids[0].price == 26'413 &&
         event.bids[0].quantity == 1'025);
}

void test_binance_metadata_request_capacity() {
  auto spot = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Spot, 1);
  assert(spot);
  const std::array<StreamRequest, 2> small{{
      {"BTCUSDT", "BTCUSDT", "bookTicker", "", true, false, 0},
      {"ETHUSDT", "ETHUSDT", "bookTicker", "", true, false, 0},
  }};
  const auto small_request = spot->metadata_request(small);
  assert(small_request.target.starts_with(
      "/api/v3/exchangeInfo?symbols=%5B"));
  assert(small_request.target.find("BTCUSDT") != std::string::npos);
  assert(small_request.target.find("ETHUSDT") != std::string::npos);

  std::vector<std::string> symbols;
  symbols.reserve(600);
  for (std::size_t index = 0; index < 600; ++index) {
    symbols.push_back("ASSET" + std::to_string(index) + "USDT");
  }
  std::vector<StreamRequest> requests;
  requests.reserve(symbols.size());
  for (const auto &symbol : symbols) {
    requests.push_back(
        {symbol, symbol, "bookTicker", "", true, false, 0});
  }
  const auto large_request = spot->metadata_request(requests);
  assert(large_request.target == "/api/v3/exchangeInfo");
  std::array<std::byte, 8U << 10U> encoded{};
  std::size_t encoded_size{};
  assert(net::encode_http_request(
      "api.binance.com",
      {net::HttpMethod::Get, large_request.target, {}, {}, {}}, encoded,
      encoded_size));
  assert(encoded_size <= encoded.size());

  auto perpetual = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Perpetual, 1);
  assert(perpetual);
  assert(perpetual->metadata_request(requests).target ==
         "/fapi/v1/exchangeInfo");

  const auto usdm_entry = [](std::string_view symbol,
                             std::string_view contract_type) {
    return std::string(R"({"symbol":")") + std::string(symbol) +
           R"(","pair":"BTCUSDT","contractType":")" +
           std::string(contract_type) +
           R"(","deliveryDate":4133404800000,"onboardDate":1569398400000,"status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","pricePrecision":2,"quantityPrecision":3,"filters":[{"filterType":"PRICE_FILTER","maxPrice":"1000000.00","minPrice":"0.01","tickSize":"0.01"},{"filterType":"LOT_SIZE","maxQty":"1000.000","minQty":"0.001","stepSize":"0.001"}]})";
  };
  const auto perpetual_json =
      std::string(R"({"symbols":[)") +
      usdm_entry("BTCUSDT", "PERPETUAL") + "," +
      usdm_entry("BTCUSDT_260925", "CURRENT_QUARTER") + "]}";
  const std::array<StreamRequest, 2> perpetual_requests{{
      {"BTCUSDT", "BTCUSDT", "bookTicker", "", true, false, 0},
      {"BTCUSDT260925", "BTCUSDT_260925", "bookTicker", "", true, false, 0},
  }};
  std::vector<InstrumentMetadata> metadata;
  std::string error;
  assert(perpetual->parse_discovery_metadata(
      perpetual_json, perpetual_requests, metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].canonical_symbol == "BTCUSDT");
}

void test_binance_large_metadata() {
  const auto spot_entry = [](std::string_view symbol,
                             std::string_view base,
                             std::string_view status = "TRADING") {
    return std::string(R"({"symbol":")") + std::string(symbol) +
           R"(","status":")" + std::string(status) +
           R"(","baseAsset":")" + std::string(base) +
           R"(","quoteAsset":"USDT","filters":[{"filterType":"PRICE_FILTER","minPrice":"0.01","maxPrice":"1000000.00","tickSize":"0.01"},{"filterType":"LOT_SIZE","minQty":"0.00001","maxQty":"9000.00000","stepSize":"0.00001"}]})";
  };
  std::string large_json = R"({"symbols":[)";
  large_json.append((8U << 20U) + 1024U, ' ');
  large_json += spot_entry("BTCUSDT", "BTC");
  large_json += ',';
  large_json += spot_entry("ETHUSDT", "ETH");
  large_json += ',';
  large_json += spot_entry("OLDUSDT", "OLD", "BREAK");
  large_json += "]}";

  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Spot, 5000);
  assert(adapter);
  const std::array<StreamRequest, 3> requests{{
      {"BTCUSDT", "BTCUSDT", "bookTicker", "depth", true, true, 100},
      {"ETHUSDT", "ETHUSDT", "bookTicker", "depth", true, true, 100},
      {"OLDUSDT", "OLDUSDT", "bookTicker", "depth", true, true, 100},
  }};
  std::vector<InstrumentMetadata> metadata;
  std::string error;
  assert(adapter->parse_metadata(large_json, requests, metadata, error));
  assert(metadata.size() == 3);
  assert(metadata[0].canonical_symbol == "BTCUSDT");
  assert(metadata[1].canonical_symbol == "ETHUSDT");

  NormalizedEvent event(5000);
  assert(adapter->parse_snapshot(
      R"({"lastUpdateId":42,"bids":[["100.00","1.00000"]],"asks":[["100.01","2.00000"]]})",
      "ETHUSDT", event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.final_sequence == 42);
  assert(adapter->parse_ws(
      R"({"u":43,"s":"BTCUSDT","b":"100.00","B":"1.00000","a":"100.01","A":"2.00000","T":43,"E":43})",
      event, error));
  assert(event.type == AdapterEventType::Bbo);

  auto discovery_adapter = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Spot, 1);
  assert(discovery_adapter);
  metadata.clear();
  assert(discovery_adapter->parse_discovery_metadata(
      large_json, requests, metadata, error));
  assert(metadata.size() == 2);
  assert(metadata[0].canonical_symbol == "BTCUSDT");
  assert(metadata[1].canonical_symbol == "ETHUSDT");
  assert(!discovery_adapter->parse_snapshot(
      R"({"lastUpdateId":44,"bids":[],"asks":[]})", "BTCUSDT", event,
      error));
  assert(error == "Binance snapshot references an unknown symbol");
}

void test_okx() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Okx, ProductType::Spot, 1024);
  assert(adapter);
  const StreamRequest request{"BTCUSDT", "BTC-USDT", "bbo-tbt",
                              "books-l2-tbt", true, true};
  const StreamRequest eth{"ETHUSDT", "ETH-USDT", "bbo-tbt",
                          "books-l2-tbt", true, true};
  const StreamRequest requests[]{request, eth};
  std::vector<std::string> batches;
  std::string error;
  assert(adapter->build_subscription_batches(requests, batches, error));
  assert(batches.size() == 1);
  assert(batches[0].find("bbo-tbt") != std::string::npos);
  assert(batches[0].find("books-l2-tbt") != std::string::npos);
  assert(adapter->expected_subscription_acks(batches[0]) == 4);

  constexpr std::string_view metadata_json =
      R"({"code":"0","data":[{"instId":"BTC-USDT","baseCcy":"BTC","quoteCcy":"USDT","settleCcy":"","tickSz":"0.1","lotSz":"0.00000001"},{"instId":"ETH-USDT","baseCcy":"ETH","quoteCcy":"USDT","settleCcy":"","tickSz":"0.01","lotSz":"0.000001"}]})";
  std::vector<InstrumentMetadata> metadata;
  assert(adapter->parse_metadata(metadata_json, requests, metadata,
                                 error));
  assert(metadata.size() == 2);
  assert(metadata[0].tick_size == 1);
  assert(metadata[0].price_scale == 1);
  assert(metadata[0].quantity_scale == 8);
  assert(metadata[0].lot_size == 1);

  NormalizedEvent event(1024);
  assert(adapter->parse_ws(
      R"({"event":"login","code":"0","msg":"","connId":"x"})", event,
      error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(adapter->parse_ws(
      R"({"event":"login","code":"60009","msg":"Login failed"})", event,
      error));
  assert(event.type == AdapterEventType::SubscribeError);
  assert(error.find("60009") != std::string::npos);
  assert(adapter->parse_ws(
      R"({"event":"subscribe","arg":{"channel":"books-l2-tbt","instId":"BTC-USDT"},"connId":"x"})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(event.symbol_view() == "BTC-USDT");
  assert(adapter->parse_ws(
      R"({"event":"unsubscribe","arg":{"channel":"books-l2-tbt","instId":"BTC-USDT"},"connId":"x"})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(event.symbol_view() == "BTC-USDT");
  assert(error.empty());
  assert(adapter->parse_ws(
      R"({"event":"error","code":"60012","msg":"Invalid request","connId":"x"})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeError);
  assert(error.find("60012") != std::string::npos);
  assert(adapter->parse_ws(
      R"({"arg":{"channel":"bbo-tbt","instId":"BTC-USDT"},"data":[{"asks":[["100.1","2.0","0","1"]],"bids":[["100.0","1.0","0","1"]],"ts":"10","seqId":7}]})",
      event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.bid.price == 1000);
  assert(event.ask.price == 1001);
  assert(event.final_sequence == 7);
  expect_no_allocation(
      *adapter,
      R"({"arg":{"channel":"bbo-tbt","instId":"BTC-USDT"},"data":[{"asks":[["100.2","2.0","0","1"]],"bids":[["100.1","1.0","0","1"]],"ts":"11","seqId":8}]})",
      event, error);

  const auto book_401 = okx_book_message(401);
  assert(adapter->parse_ws(book_401, event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.asks.size() == 401);

  const auto book_1024 = okx_book_message(1024);
  assert(adapter->parse_ws(book_1024, event, error));
  assert(event.asks.size() == 1024);
  expect_no_allocation(*adapter, book_1024, event, error);

  const auto book_1025 = okx_book_message(1025);
  assert(!adapter->parse_ws(book_1025, event, error));
  assert(error.find("OKX ask levels exceed event capacity") !=
         std::string::npos);
  assert(error.find("configured=1024") != std::string::npos);
  assert(error.find("observed_at_least=1025") != std::string::npos);
  assert(error.find("symbol=BTC-USDT") != std::string::npos);
  assert(error.find("channel=books") != std::string::npos);
  assert(error.find("action=snapshot") != std::string::npos);

  auto perpetual = mds::exchange::make_venue_adapter(
      Venue::Okx, ProductType::Perpetual, 400);
  const StreamRequest swap{"BTCUSDT", "BTC-USDT-SWAP", "bbo-tbt",
                           "books-l2-tbt", true, true};
  constexpr std::string_view swap_metadata =
      R"({"code":"0","data":[{"instId":"BTC-USDT-SWAP","baseCcy":"BTC","quoteCcy":"USDT","settleCcy":"USDT","tickSz":"0.1","lotSz":"0.1","ctVal":"0.01"}]})";
  metadata.clear();
  assert(perpetual->parse_metadata(swap_metadata, {&swap, 1}, metadata,
                                   error));
  assert(metadata.size() == 1);
  assert(metadata[0].quantity_scale == 3);
  assert(metadata[0].lot_size == 1);
  assert(metadata[0].contract_multiplier == 1);
  assert(metadata[0].contract_multiplier_scale == 2);

  const StreamRequest preopen_swap{"JP225USDT", "JP225-USDT-SWAP",
                                   "bbo-tbt", "books-l2-tbt", true, true};
  const StreamRequest discovery_requests[]{swap, preopen_swap};
  constexpr std::string_view discovery_metadata =
      R"({"code":"0","data":[{"instId":"BTC-USDT-SWAP","state":"live","baseCcy":"BTC","quoteCcy":"USDT","settleCcy":"USDT","tickSz":"0.1","lotSz":"0.1","ctVal":"0.01"},{"instId":"JP225-USDT-SWAP","state":"preopen","baseCcy":"","quoteCcy":"","settleCcy":"","tickSz":"","lotSz":"","ctVal":""}]})";
  metadata.clear();
  assert(perpetual->parse_discovery_metadata(
      discovery_metadata, discovery_requests, metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].venue_symbol == "BTC-USDT-SWAP");

  metadata.clear();
  assert(!perpetual->parse_metadata(discovery_metadata, {&preopen_swap, 1},
                                    metadata, error));
  assert(error == "invalid OKX instrument metadata");

  assert(perpetual->parse_ws(
      R"({"arg":{"channel":"bbo-tbt","instId":"BTC-USDT-SWAP"},"data":[{"asks":[["100.1","3.0","0","1"]],"bids":[["100.0","2.0","0","1"]],"ts":"10","seqId":7}]})",
      event, error));
  assert(event.bid.quantity == 20);
  assert(event.ask.quantity == 30);
}

void test_bybit() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Bybit, ProductType::Perpetual, 50);
  assert(adapter);
  const StreamRequest request{"BTCUSDT", "BTCUSDT", "orderbook.1",
                              "orderbook.50", true, true};
  std::vector<std::string> batches;
  std::string error;
  assert(adapter->build_subscription_batches({&request, 1}, batches, error));
  assert(batches.size() == 1);
  assert(adapter->expected_subscription_acks(batches[0]) == 1);

  NormalizedEvent event(50);
  assert(adapter->parse_ws(
      R"({"success":true,"ret_msg":"subscribe","op":"subscribe","conn_id":"x"})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(error.empty());
  assert(adapter->parse_ws(
      R"({"success":true,"ret_msg":"unsubscribe","op":"unsubscribe","conn_id":"x"})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(error.empty());
  assert(adapter->parse_ws(
      R"({"success":false,"ret_msg":"rejected","op":"unsubscribe","conn_id":"x"})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeError);

  const std::array metadata_requests{
      request,
      StreamRequest{"ETHUSDT", "ETHUSDT", "orderbook.1",
                    "orderbook.50", true, true},
      StreamRequest{"SOLUSDT", "SOLUSDT", "orderbook.1",
                    "orderbook.50", true, true},
      StreamRequest{"BICOUSDT", "BICOUSDT", "orderbook.1",
                    "orderbook.50", true, true},
      StreamRequest{"ZECUSDT", "ZECUSDT", "orderbook.1",
                    "orderbook.50", true, true},
      StreamRequest{"BEATUSDT", "BEATUSDT", "orderbook.1",
                    "orderbook.50", true, true}};
  std::vector<MetadataRequestBatch> metadata_batches;
  const bool built_metadata_batches =
      adapter->build_metadata_request_batches(
          metadata_requests, metadata_batches, error);
  assert(built_metadata_batches);
  if (!built_metadata_batches) {
    std::abort();
  }
  assert(metadata_batches.size() == metadata_requests.size());
  for (std::size_t index = 0; index < metadata_requests.size(); ++index) {
    assert(metadata_batches[index].request_offset == index);
    assert(metadata_batches[index].request_count == 1);
    assert(metadata_batches[index].http.target ==
           "/v5/market/instruments-info?category=linear&symbol=" +
               std::string(metadata_requests[index].venue_symbol));
  }
  const StreamRequest encoded_request{
      "TESTUSDT", "TEST/USDT", "orderbook.1",
      "orderbook.50", true, true};
  metadata_batches.clear();
  assert(adapter->build_metadata_request_batches(
      {&encoded_request, 1}, metadata_batches, error));
  assert(metadata_batches.size() == 1);
  assert(metadata_batches[0].http.target ==
         "/v5/market/instruments-info?category=linear&symbol=TEST%2FUSDT");

  constexpr std::string_view metadata_json =
      R"({"retCode":0,"result":{"category":"linear","list":[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","settleCoin":"USDT","priceFilter":{"tickSize":"0.1"},"lotSizeFilter":{"qtyStep":"0.001"}}]}})";
  std::vector<InstrumentMetadata> metadata;
  assert(adapter->parse_metadata(metadata_json, {&request, 1}, metadata,
                                 error));
  assert(metadata.size() == 1);
  assert(metadata[0].quantity_scale == 3);
  assert(metadata[0].lot_size == 1);
  assert(metadata[0].contract_multiplier == 1);
  assert(metadata[0].contract_multiplier_scale == 0);

  std::vector<InstrumentMetadata> combined_metadata;
  for (const auto &metadata_request : metadata_requests) {
    const auto symbol = std::string(metadata_request.venue_symbol);
    const auto base = symbol.substr(0, symbol.size() - 4);
    const std::string response =
        "{\"retCode\":0,\"result\":{\"category\":\"linear\",\"list\":[{"
        "\"symbol\":\"" +
        symbol + "\",\"baseCoin\":\"" + base +
        "\",\"quoteCoin\":\"USDT\",\"settleCoin\":\"USDT\","
        "\"priceFilter\":{\"tickSize\":\"0.01\"},"
        "\"lotSizeFilter\":{\"qtyStep\":\"0.001\"}}]}}";
    metadata.clear();
    const bool parsed = adapter->parse_metadata(
        response, {&metadata_request, 1}, metadata, error);
    assert(parsed);
    if (!parsed || metadata.empty()) {
      std::abort();
    }
    assert(metadata.size() == 1);
    combined_metadata.push_back(std::move(metadata.front()));
  }
  assert(combined_metadata.size() == metadata_requests.size());

  metadata.clear();
  assert(!adapter->parse_metadata(
      R"({"retCode":0,"result":{"category":"linear","list":[]}})",
      {&metadata_requests[5], 1}, metadata, error));
  assert(error.find("BEATUSDT") != std::string::npos);
  metadata.clear();
  assert(!adapter->parse_metadata(
      R"({"retCode":0,"result":{"category":"linear","list":[{"symbol":"OTHERUSDT","baseCoin":"OTHER","quoteCoin":"USDT","settleCoin":"USDT","priceFilter":{"tickSize":"0.01"},"lotSizeFilter":{"qtyStep":"0.001"}}]}})",
      {&metadata_requests[5], 1}, metadata, error));
  assert(error.find("BEATUSDT") != std::string::npos);
  metadata.clear();
  assert(!adapter->parse_metadata(
      R"({"retCode":10001,"retMsg":"invalid symbol","result":{}})",
      {&metadata_requests[5], 1}, metadata, error));
  assert(error.find("BEATUSDT") != std::string::npos);

  constexpr std::string_view bbo =
      R"({"topic":"orderbook.1.BTCUSDT","type":"snapshot","ts":10,"data":{"s":"BTCUSDT","b":[["100.0","1.000"]],"a":[["100.1","2.000"]],"u":5,"seq":9}})";
  assert(adapter->parse_ws(bbo, event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.final_sequence == 5);
  assert(adapter->parse_ws(bbo, event, error));
  assert(event.type == AdapterEventType::Ignored);
  adapter->reset_connection_state();
  assert(adapter->parse_ws(bbo, event, error));
  assert(event.type == AdapterEventType::Bbo);
  expect_no_allocation(*adapter, bbo, event, error);

  constexpr std::string_view depth_snapshot =
      R"({"topic":"orderbook.50.BTCUSDT","type":"snapshot","ts":20,"data":{"s":"BTCUSDT","b":[["100.0","1.000"]],"a":[["100.1","2.000"]],"u":200,"seq":300}})";
  assert(adapter->parse_ws(depth_snapshot, event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.final_sequence == 200);
  assert(event.sequence_reset);

  constexpr std::string_view depth_delta =
      R"({"topic":"orderbook.50.BTCUSDT","type":"delta","ts":21,"data":{"s":"BTCUSDT","b":[["100.0","1.500"]],"a":[],"u":201,"seq":301}})";
  assert(adapter->parse_ws(depth_delta, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.final_sequence == 201);
  assert(!event.sequence_reset);

  constexpr std::string_view regressed_depth_snapshot =
      R"({"topic":"orderbook.50.BTCUSDT","type":"snapshot","ts":22,"data":{"s":"BTCUSDT","b":[["99.9","1.000"]],"a":[["100.2","2.000"]],"u":150,"seq":250}})";
  assert(adapter->parse_ws(regressed_depth_snapshot, event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.final_sequence == 150);
  assert(event.sequence_reset);
  assert(error.empty());

  constexpr std::string_view reset_depth_delta =
      R"({"topic":"orderbook.50.BTCUSDT","type":"delta","ts":23,"data":{"s":"BTCUSDT","b":[["99.9","1.250"]],"a":[],"u":1,"seq":1}})";
  assert(adapter->parse_ws(reset_depth_delta, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.final_sequence == 1);
  assert(event.sequence_reset);

  constexpr std::string_view ordinary_depth_delta =
      R"({"topic":"orderbook.50.BTCUSDT","type":"delta","ts":24,"data":{"s":"BTCUSDT","b":[["99.9","1.500"]],"a":[],"u":10,"seq":10}})";
  assert(adapter->parse_ws(ordinary_depth_delta, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(!event.sequence_reset);
  assert(adapter->parse_ws(ordinary_depth_delta, event, error));
  assert(event.type == AdapterEventType::Ignored);

  constexpr std::string_view regressed_depth_delta =
      R"({"topic":"orderbook.50.BTCUSDT","type":"delta","ts":25,"data":{"s":"BTCUSDT","b":[["99.9","1.750"]],"a":[],"u":9,"seq":11}})";
  assert(adapter->parse_ws(regressed_depth_delta, event, error));
  assert(event.type == AdapterEventType::Ignored);
  assert(error.empty());

  constexpr std::string_view regressed_seq_delta =
      R"({"topic":"orderbook.50.BTCUSDT","type":"delta","ts":26,"data":{"s":"BTCUSDT","b":[["99.9","1.800"]],"a":[],"u":11,"seq":9}})";
  assert(adapter->parse_ws(regressed_seq_delta, event, error));
  assert(event.type == AdapterEventType::Ignored);
  assert(error.empty());

  constexpr std::string_view next_depth_delta =
      R"({"topic":"orderbook.50.BTCUSDT","type":"delta","ts":27,"data":{"s":"BTCUSDT","b":[["99.9","2.000"]],"a":[],"u":12,"seq":12}})";
  assert(adapter->parse_ws(next_depth_delta, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.final_sequence == 12);
  assert(!event.sequence_reset);
  assert(error.empty());

  adapter->reset_connection_state();
  expect_no_allocation(*adapter, depth_snapshot, event, error);

  auto spot = mds::exchange::make_venue_adapter(
      Venue::Bybit, ProductType::Spot, 50);
  assert(spot);
  const StreamRequest spot_request{
      "BICOUSDT", "BICOUSDT", "orderbook.1",
      "orderbook.50", true, true};
  metadata.clear();
  assert(!spot->parse_metadata(
      R"({"retCode":0,"result":{"category":"spot","list":[{"symbol":"BICOUSDT","baseCoin":"BICO","quoteCoin":"USDT","priceFilter":{"tickSize":"0.0001"},"lotSizeFilter":{"qtyStep":"0.01"}}]}})",
      {&spot_request, 1}, metadata, error));
  assert(error.find("spot") != std::string::npos);
  assert(error.find("basePrecision") != std::string::npos);
  assert(error.find("BICOUSDT") != std::string::npos);

  metadata.clear();
  assert(spot->parse_metadata(
      R"({"retCode":0,"result":{"category":"spot","list":[{"symbol":"BICOUSDT","baseCoin":"BICO","quoteCoin":"USDT","priceFilter":{"tickSize":"0.0001"},"lotSizeFilter":{"basePrecision":"0.01"}}]}})",
      {&spot_request, 1}, metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].price_scale == 4);
  assert(metadata[0].quantity_scale == 2);
  assert(metadata[0].lot_size == 1);
  NormalizedEvent spot_event(50);
  constexpr std::string_view spot_bbo =
      R"({"topic":"orderbook.1.BICOUSDT","type":"snapshot","ts":10,"data":{"s":"BICOUSDT","b":[["0.1500","10.00"]],"a":[["0.1501","20.00"]],"u":5,"seq":9}})";
  assert(spot->parse_ws(spot_bbo, spot_event, error));
  assert(spot_event.type == AdapterEventType::Bbo);
  assert(spot_event.bid.quantity == 1000);
  expect_no_allocation(*spot, spot_bbo, spot_event, error);
}

void test_bitget() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Bitget, ProductType::Perpetual, 1000);
  assert(adapter);
  const StreamRequest request{"BTCUSDT", "BTCUSDT", "books1", "books",
                              true, true};
  std::vector<std::string> batches;
  std::string error;
  assert(adapter->build_subscription_batches({&request, 1}, batches, error));
  assert(!batches.empty());
  assert(batches[0].size() <= 4096);
  assert(adapter->expected_subscription_acks(batches[0]) == 2);
  NormalizedEvent event(1000);
  assert(adapter->parse_ws(
      R"({"event":"subscribe","arg":{"instType":"USDT-FUTURES","channel":"books","instId":"BTCUSDT"}})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(error.empty());
  assert(adapter->parse_ws(
      R"({"event":"unsubscribe","arg":{"instType":"USDT-FUTURES","channel":"books","instId":"BTCUSDT"}})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(error.empty());
  std::vector<MetadataRequestBatch> metadata_batches;
  assert(adapter->build_metadata_request_batches(
      {&request, 1}, metadata_batches, error));
  assert(metadata_batches.size() == 1);
  assert(metadata_batches[0].request_offset == 0);
  assert(metadata_batches[0].request_count == 1);

  constexpr std::string_view metadata_json =
      R"({"code":"00000","data":[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","pricePlace":"1","volumePlace":"3","priceEndStep":"1","sizeMultiplier":"0.001"}]})";
  std::vector<InstrumentMetadata> metadata;
  assert(adapter->parse_metadata(metadata_json, {&request, 1}, metadata,
                                 error));
  assert(metadata.size() == 1);
  assert(metadata[0].price_scale == 10);
  assert(metadata[0].tick_size == 1'000'000'000);
  assert(metadata[0].refine_book_tick);

  assert(!adapter->parse_ws(R"({"arg":)", event, error));
  assert(!error.empty());
  assert(!adapter->parse_ws(
      R"({"event":"subscribe"}{"event":"subscribe"})", event, error));
  assert(!error.empty());
  assert(!adapter->parse_ws("upstream unavailable", event, error));
  assert(!error.empty());
  assert(!adapter->parse_ws(
      R"({"arg":{"channel":"books1","instId":"BTCUSDT"},"action":"snapshot","data":[]})",
      event, error));
  assert(error == "Bitget market-data payload is empty");

  assert(adapter->parse_ws(
      R"({"arg":{"channel":"books1","instId":"BTCUSDT"},"action":"snapshot","data":[{"bids":[["100.0","1.000"]],"asks":[["100.1","2.000"]],"ts":"10","seq":1}]})",
      event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.bid.price == 1'000'000'000'000);
  assert(event.ask.price == 1'001'000'000'000);
  assert(event.bid.quantity == 1000);
  assert(event.ask.quantity == 2000);
  expect_no_allocation(
      *adapter,
      R"({"arg":{"channel":"books1","instId":"BTCUSDT"},"action":"snapshot","data":[{"bids":[["100.1","1.000"]],"asks":[["100.2","2.000"]],"ts":"11","seq":2}]})",
      event, error);

  auto limited = mds::exchange::make_venue_adapter(
      Venue::Bitget, ProductType::Perpetual, 2);
  assert(limited);
  metadata.clear();
  assert(limited->parse_metadata(metadata_json, {&request, 1}, metadata,
                                 error));
  NormalizedEvent limited_event(2);

  const auto exact = bitget_book_message(2, 2);
  assert(limited->parse_ws(exact, limited_event, error));
  assert(limited_event.type == AdapterEventType::BookSnapshot);
  assert(limited_event.bids.size() == 2);
  assert(limited_event.asks.size() == 2);
  expect_no_allocation(*limited, exact, limited_event, error);

  const auto bid_overflow = bitget_book_message(3, 1);
  assert(limited->parse_ws(bid_overflow, limited_event, error));
  assert(limited_event.type == AdapterEventType::BookGap);
  assert(limited_event.input_side == mds::exchange::InputSide::Bid);
  assert(limited_event.input_capacity == 2);
  assert(limited_event.bids.empty());
  assert(limited_event.asks.empty());
  assert(error.empty());
  expect_no_allocation(*limited, bid_overflow, limited_event, error);

  const auto ask_overflow =
      bitget_book_message(1, 3, "update", 3, 2);
  assert(limited->parse_ws(ask_overflow, limited_event, error));
  assert(limited_event.type == AdapterEventType::BookGap);
  assert(limited_event.input_side == mds::exchange::InputSide::Ask);
  assert(limited_event.input_capacity == 2);
  assert(limited_event.first_sequence == 3);
  assert(limited_event.final_sequence == 3);
  assert(limited_event.strict_previous_sequence);
  assert(error.empty());

  assert(limited->parse_ws(exact, limited_event, error));
  assert(limited_event.type == AdapterEventType::BookSnapshot);
  assert(limited_event.bids.size() == 2);
  assert(limited_event.asks.size() == 2);

  assert(!limited->parse_ws(
      R"({"arg":{"channel":"books","instId":"BTCUSDT"},"action":"snapshot","data":[{"bids":[["bad","1.000"]],"asks":[["100.1","2.000"]],"ts":"10","seq":4}]})",
      limited_event, error));
  assert(error.find("invalid Bitget order book price value=bad") !=
         std::string::npos);
  assert(error.find("symbol=BTCUSDT side=bid") != std::string::npos);

  auto beat = mds::exchange::make_venue_adapter(
      Venue::Bitget, ProductType::Perpetual, 1000);
  assert(beat);
  const StreamRequest beat_request{"BEATUSDT", "BEATUSDT", "books1",
                                   "books", true, true};
  constexpr std::string_view beat_metadata_json =
      R"({"code":"00000","data":[{"symbol":"BEATUSDT","baseCoin":"BEAT","quoteCoin":"USDT","pricePlace":"4","volumePlace":"2","priceEndStep":"1","sizeMultiplier":"0.01"}]})";
  metadata.clear();
  assert(beat->parse_metadata(beat_metadata_json, {&beat_request, 1},
                              metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].price_scale == 10);
  assert(metadata[0].tick_size == 1'000'000);
  assert(metadata[0].refine_book_tick);

  NormalizedEvent beat_event(1000);
  constexpr std::string_view beat_bbo =
      R"({"arg":{"channel":"books1","instId":"BEATUSDT"},"action":"snapshot","data":[{"bids":[["0.57383","1.00"]],"asks":[["0.57384","2.00"]],"ts":"20","seq":10}]})";
  assert(beat->parse_ws(beat_bbo, beat_event, error));
  assert(beat_event.type == AdapterEventType::Bbo);
  assert(beat_event.bid.price == 5'738'300'000);
  assert(beat_event.ask.price == 5'738'400'000);
  expect_no_allocation(*beat, beat_bbo, beat_event, error);

  constexpr std::string_view beat_snapshot =
      R"({"arg":{"channel":"books","instId":"BEATUSDT"},"action":"snapshot","data":[{"bids":[["0.57383","1.00"]],"asks":[["0.57384","2.00"]],"ts":"21","seq":11}]})";
  assert(beat->parse_ws(beat_snapshot, beat_event, error));
  assert(beat_event.type == AdapterEventType::BookSnapshot);
  assert(beat_event.bids[0].price == 5'738'300'000);
  expect_no_allocation(*beat, beat_snapshot, beat_event, error);

  constexpr std::string_view beat_delta =
      R"({"arg":{"channel":"books","instId":"BEATUSDT"},"action":"update","data":[{"bids":[["0.57385","3.00"]],"asks":[],"ts":"22","seq":12,"pseq":11}]})";
  assert(beat->parse_ws(beat_delta, beat_event, error));
  assert(beat_event.type == AdapterEventType::BookDelta);
  assert(beat_event.bids[0].price == 5'738'500'000);
  expect_no_allocation(*beat, beat_delta, beat_event, error);

  assert(!beat->parse_ws(
      R"({"arg":{"channel":"books","instId":"BEATUSDT"},"action":"snapshot","data":[{"bids":[["0.57383000001","1.00"]],"asks":[["0.57384","2.00"]],"ts":"23","seq":13}]})",
      beat_event, error));
  assert(error.find("price value=0.57383000001 scale=10") !=
         std::string::npos);

  auto invalid_precision = mds::exchange::make_venue_adapter(
      Venue::Bitget, ProductType::Perpetual, 1000);
  assert(invalid_precision);
  metadata.clear();
  assert(!invalid_precision->parse_metadata(
      R"({"code":"00000","data":[{"symbol":"BEATUSDT","baseCoin":"BEAT","quoteCoin":"USDT","pricePlace":"11","volumePlace":"2","priceEndStep":"1","sizeMultiplier":"0.01"}]})",
      {&beat_request, 1}, metadata, error));
  assert(error == "invalid Bitget contract price/quantity precision");
  assert(!invalid_precision->parse_metadata(
      R"({"code":"00000","data":[{"symbol":"BEATUSDT","baseCoin":"BEAT","quoteCoin":"USDT","pricePlace":"0","volumePlace":"2","priceEndStep":"9223372036854775807","sizeMultiplier":"0.01"}]})",
      {&beat_request, 1}, metadata, error));
  assert(error == "invalid Bitget contract price/quantity precision");

  assert(!limited->parse_ws(
      R"({"arg":{"channel":"books","instId":"BTCUSDT"},"action":"update","data":[{"bids":[["100.0","1.000"]],"asks":[["100.1","2.000"]],"ts":"10","seq":4,"pseq":4}]})",
      limited_event, error));
  assert(error == "Bitget update has invalid seq/pseq continuity");
}

void test_gate() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Spot, 100);
  assert(adapter);
  StreamRequest request{"BTCUSDT", "BTC_USDT", "spot.book_ticker",
                        "spot.order_book_update", true, true};
  request.update_interval_ms = 100;
  std::vector<std::string> batches;
  std::string error;
  assert(adapter->build_subscription_batches({&request, 1}, batches, error));
  assert(batches.size() == 2);
  assert(batches[1].find("100ms") != std::string::npos);

  constexpr std::string_view metadata_json =
      R"([{"id":"BTC_USDT","base":"BTC","quote":"USDT","precision":1,"amount_precision":3}])";
  std::vector<InstrumentMetadata> metadata;
  assert(adapter->parse_metadata(metadata_json, {&request, 1}, metadata,
                                 error));
  assert(metadata[0].quantity_scale == 6);
  assert(metadata[0].lot_size == 1000);

  NormalizedEvent event(100);
  assert(adapter->parse_ws(
      R"({"channel":"spot.book_ticker","event":"update","time_ms":10,"result":{"s":"BTC_USDT","b":"100.0","B":"1.000","a":"100.1","A":"2.000","u":7,"t":10}})",
      event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(adapter->parse_ws(
      R"({"channel":"spot.order_book_update","event":"update","time_ms":10,"result":{"s":"BTC_USDT","U":7,"u":8,"full":true,"b":[["100.0","1.000"]],"a":[["100.1","2.000"]]}})",
      event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.sequence_reset);
  assert(adapter->needs_rest_snapshot());
  assert(adapter->snapshot_request("BTC_USDT", 100)
             .target.find("with_id=true") != std::string::npos);
  expect_no_allocation(
      *adapter,
      R"({"channel":"spot.book_ticker","event":"update","time_ms":11,"result":{"s":"BTC_USDT","b":"100.1","B":"1.000","a":"100.2","A":"2.000","u":8,"t":11}})",
      event, error);

  auto sol_spot = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Spot, 100);
  assert(sol_spot);
  const StreamRequest sol_spot_request{
      "SOLUSDT", "SOL_USDT", "spot.book_ticker",
      "spot.order_book_update", true, true};
  metadata.clear();
  assert(sol_spot->parse_metadata(
      R"([{"id":"SOL_USDT","base":"SOL","quote":"USDT","precision":3,"amount_precision":8}])",
      {&sol_spot_request, 1}, metadata, error));
  constexpr std::string_view valid_sol_spot =
      R"({"channel":"spot.book_ticker","event":"update","time_ms":12,"result":{"s":"SOL_USDT","b":"150.000","B":"0.12345678","a":"150.001","A":"2.00000000","u":9,"t":12}})";
  assert(sol_spot->parse_ws(valid_sol_spot, event, error));
  assert(event.bid.quantity == 12'345'678);
  expect_no_allocation(*sol_spot, valid_sol_spot, event, error);

  ParseFailure failure;
  expect_no_allocation(*sol_spot, valid_sol_spot, event, failure);
  assert(!sol_spot->parse_ws(
      R"({"channel":"spot.book_ticker","event":"update","time_ms":13,"result":{"s":"SOL_USDT","b":"150.000","B":"0.123456789","a":"150.001","A":"2.00000000","u":10,"t":13}})",
      event, failure));
  assert(failure.category == ParseFailureCategory::DirtyData);
  assert(failure.scope == ParseFailureScope::Symbol);
  assert(failure.code == ParseFailureCode::InvalidQuantity);
  assert(failure.symbol_view() == "SOL_USDT");
  assert(failure.diagnostic_view().find("reason=quantity") !=
         std::string_view::npos);
  assert(!sol_spot->parse_ws(
      R"({"channel":"spot.book_ticker","event":"update","time_ms":14,"result":{"s":"SOL_USDT","b":"bad","B":"1.00000000","a":"150.001","A":"2.00000000","u":11,"t":14}})",
      event, failure));
  assert(failure.category == ParseFailureCategory::DirtyData);
  assert(failure.scope == ParseFailureScope::Symbol);
  assert(failure.code == ParseFailureCode::InvalidPrice);
  assert(failure.symbol_view() == "SOL_USDT");

  auto legacy_spot = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Spot, 100);
  assert(legacy_spot);
  const StreamRequest bico_request{
      "BICOUSDT", "BICO_USDT", "spot.book_ticker",
      "spot.order_book_update", true, true};
  metadata.clear();
  assert(legacy_spot->parse_metadata(
      R"([{"id":"BICO_USDT","base":"BICO","quote":"USDT","precision":6,"amount_precision":0}])",
      {&bico_request, 1}, metadata, error));
  assert(metadata[0].quantity_scale == 6);
  assert(metadata[0].lot_size == 1'000'000);
  assert(legacy_spot->parse_snapshot(
      R"({"id":8,"current":10,"bids":[["0.070000","100"]],"asks":[["0.080000","25.5"]]})",
      "BICO_USDT", event, error));
  assert(event.bids[0].quantity == 100'000'000);
  assert(event.asks[0].quantity == 25'500'000);

  auto perpetual = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Perpetual, 100);
  assert(perpetual);
  StreamRequest perpetual_request{
      "BTCUSDT", "BTC_USDT", "futures.book_ticker",
      "futures.order_book_update", true, true};
  perpetual_request.update_interval_ms = 20;
  constexpr std::string_view perpetual_metadata =
      R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.0001","order_size_min":1}])";
  metadata.clear();
  assert(perpetual->parse_metadata(
      perpetual_metadata, {&perpetual_request, 1}, metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].price_scale == 1);
  assert(metadata[0].quantity_scale == 4);
  assert(metadata[0].tick_size == 1);
  assert(metadata[0].lot_size == 1);
  assert(metadata[0].contract_multiplier == 1);
  assert(metadata[0].contract_multiplier_scale == 4);

  assert(perpetual->parse_ws(
      R"({"channel":"futures.book_ticker","event":"update","time_ms":10,"result":{"s":"BTC_USDT","b":"100.0","B":2,"a":"100.1","A":3,"u":7,"t":10}})",
      event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.bid.quantity == 2);
  assert(event.ask.quantity == 3);
  assert(perpetual->parse_ws(
      R"({"channel":"futures.order_book_update","event":"update","time_ms":10,"result":{"s":"BTC_USDT","U":7,"u":8,"b":[{"p":"100.0","s":2}],"a":[{"p":"100.1","s":3}]}})",
      event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids[0].quantity == 2);
  assert(event.asks[0].quantity == 3);
  expect_no_allocation(
      *perpetual,
      R"({"channel":"futures.book_ticker","event":"update","time_ms":11,"result":{"s":"BTC_USDT","b":"100.1","B":4,"a":"100.2","A":5,"u":8,"t":11}})",
      event, error);

  auto sol_perpetual = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Perpetual, 100);
  assert(sol_perpetual);
  const StreamRequest sol_perpetual_request{
      "SOLUSDT", "SOL_USDT", "futures.book_ticker",
      "futures.order_book_update", true, true};
  metadata.clear();
  assert(sol_perpetual->parse_metadata(
      R"([{"name":"SOL_USDT","order_price_round":"0.001","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true}])",
      {&sol_perpetual_request, 1}, metadata, error));
  constexpr std::string_view valid_sol_perpetual =
      R"({"channel":"futures.book_ticker","event":"update","time_ms":12,"result":{"s":"SOL_USDT","b":"150.000","B":"0.1","a":"150.001","A":"2","u":9,"t":12}})";
  assert(sol_perpetual->parse_ws(valid_sol_perpetual, event, error));
  assert(event.bid.quantity == 1);
  assert(event.ask.quantity == 20);
  expect_no_allocation(*sol_perpetual, valid_sol_perpetual, event, error);

  failure.clear();
  expect_no_allocation(*sol_perpetual, valid_sol_perpetual, event, failure);
  assert(!sol_perpetual->parse_ws(
      R"({"channel":"futures.book_ticker","event":"update","time_ms":13,"result":{"s":"SOL_USDT","b":"150.000","B":"0.01","a":"150.001","A":"2","u":10,"t":13}})",
      event, failure));
  assert(failure.category == ParseFailureCategory::DirtyData);
  assert(failure.scope == ParseFailureScope::Symbol);
  assert(failure.code == ParseFailureCode::InvalidQuantity);
  assert(failure.symbol_view() == "SOL_USDT");

  metadata.clear();
  assert(perpetual->parse_metadata(
      R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.0001","order_size_min":"1"}])",
      {&perpetual_request, 1}, metadata, error));
  StreamRequest decimal_request{
      "ETHUSDT", "ETH_USDT", "futures.book_ticker",
      "futures.order_book_update", true, true};
  decimal_request.update_interval_ms = 20;
  metadata.clear();
  assert(perpetual->parse_metadata(
      R"([{"name":"ETH_USDT","order_price_round":"0.01","quanto_multiplier":"0.01","order_size_min":0,"enable_decimal":true}])",
      {&decimal_request, 1}, metadata, error));
  assert(metadata.size() == 1);
  assert(metadata[0].price_scale == 2);
  assert(metadata[0].quantity_scale == 3);
  assert(metadata[0].tick_size == 1);
  assert(metadata[0].lot_size == 1);
  assert(metadata[0].contract_multiplier == 1);
  assert(metadata[0].contract_multiplier_scale == 2);

  constexpr std::string_view decimal_bbo =
      R"({"channel":"futures.book_ticker","event":"update","time_ms":12,"result":{"s":"ETH_USDT","b":"100.00","B":"0.1","a":"100.01","A":"2","u":9,"t":12}})";
  assert(perpetual->parse_ws(decimal_bbo, event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.bid.quantity == 1);
  assert(event.ask.quantity == 20);
  expect_no_allocation(*perpetual, decimal_bbo, event, error);

  constexpr std::string_view decimal_delta =
      R"({"channel":"futures.order_book_update","event":"update","time_ms":13,"result":{"s":"ETH_USDT","U":9,"u":10,"b":[{"p":"100.00","s":"0.1"}],"a":[{"p":"100.01","s":"-0.2"}]}})";
  assert(perpetual->parse_ws(decimal_delta, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids[0].quantity == 1);
  assert(event.asks[0].quantity == 2);
  expect_no_allocation(*perpetual, decimal_delta, event, error);

  assert(perpetual->parse_snapshot(
      R"({"id":10,"current":13,"bids":[{"p":"100.00","s":"0.1"}],"asks":[{"p":"100.01","s":"0.2"}]})",
      "ETH_USDT", event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.bids[0].quantity == 1);
  assert(event.asks[0].quantity == 2);

  assert(!perpetual->parse_ws(
      R"({"channel":"futures.order_book_update","event":"update","time_ms":14,"result":{"s":"ETH_USDT","U":10,"u":11,"b":[{"p":"100.00","s":"0.01"}],"a":[]}})",
      event, error));
  assert(error.find("symbol=ETH_USDT") != std::string::npos);
  assert(error.find("side=bid") != std::string::npos);
  assert(error.find("reason=quantity") != std::string::npos);

  assert(!perpetual->parse_ws(
      R"({"channel":"futures.order_book_update","event":"update","time_ms":15,"result":{"s":"ETH_USDT","U":11,"u":12,"b":[{"p":"100.00"}],"a":[]}})",
      event, error));
  assert(error.find("reason=shape") != std::string::npos);

  metadata.clear();
  assert(perpetual->parse_metadata(
      R"([{"name":"ETH_USDT","order_price_round":"0.01","quanto_multiplier":"0.01","order_size_min":"0.1","enable_decimal":true}])",
      {&decimal_request, 1}, metadata, error));
  assert(metadata[0].quantity_scale == 3);
  assert(metadata[0].lot_size == 1);
  assert(!perpetual->parse_ws(
      R"({"channel":"futures.order_book_update","event":"update","time_ms":16,"result":{"s":"ETH_USDT","U":12,"u":13,"b":[{"p":"100.00","s":"922337203685477580.8"}],"a":[]}})",
      event, error));
  assert(error.find("reason=quantity") != std::string::npos);

  const auto rejects_minimum = [&](std::string_view minimum) {
    const std::string json =
        "[{\"name\":\"BTC_USDT\",\"order_price_round\":\"0.1\","
        "\"quanto_multiplier\":\"0.0001\",\"order_size_min\":" +
        std::string(minimum) + "}]";
    metadata.clear();
    return !perpetual->parse_metadata(
        json, {&perpetual_request, 1}, metadata, error);
  };
  assert(rejects_minimum("0"));
  metadata.clear();
  assert(!perpetual->parse_metadata(
      R"([{"name":"BTC_USDT","order_price_round":"0.1","quanto_multiplier":"0.0001","order_size_min":0,"enable_decimal":false}])",
      {&perpetual_request, 1}, metadata, error));
  assert(rejects_minimum("-1"));
  assert(rejects_minimum("1.5"));
  assert(rejects_minimum("9223372036854775808"));

  auto burst = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Perpetual, 1024);
  assert(burst);
  const StreamRequest beat_request{
      "BEATUSDT", "BEAT_USDT", "futures.book_ticker",
      "futures.order_book_update", true, true};
  metadata.clear();
  assert(burst->parse_metadata(
      R"([{"name":"BEAT_USDT","order_price_round":"0.0001","quanto_multiplier":"1","order_size_min":1}])",
      {&beat_request, 1}, metadata, error));
  NormalizedEvent burst_event(1024);
  const auto burst_101 = gate_book_message(101);
  assert(burst->parse_ws(burst_101, burst_event, error));
  assert(burst_event.bids.size() == 101);
  const auto burst_1024 = gate_book_message(1024);
  assert(burst->parse_ws(burst_1024, burst_event, error));
  assert(burst_event.bids.size() == 1024);
  expect_no_allocation(*burst, burst_1024, burst_event, error);
  const auto burst_1025 = gate_book_message(1025);
  assert(!burst->parse_ws(burst_1025, burst_event, error));
  assert(error.find("reason=capacity") != std::string::npos);
  assert(error.find("configured=1024") != std::string::npos);
  assert(error.find("observed_at_least=1025") != std::string::npos);
  assert(error.find("symbol=BEAT_USDT") != std::string::npos);
  assert(error.find("side=bid") != std::string::npos);
  assert(error.find("channel=futures.order_book_update") !=
         std::string::npos);
  assert(error.find("action=update") != std::string::npos);
  assert(burst->snapshot_request("BEAT_USDT", 0).target.find(
             "limit=100") != std::string::npos);
  assert(legacy_spot->snapshot_request("BICO_USDT", 0).target.find(
             "limit=100") != std::string::npos);

  auto limited = mds::exchange::make_venue_adapter(
      Venue::Gate, ProductType::Perpetual, 1);
  assert(limited);
  metadata.clear();
  assert(limited->parse_metadata(
      perpetual_metadata, {&perpetual_request, 1}, metadata, error));
  assert(!limited->parse_ws(
      R"({"channel":"futures.order_book_update","event":"update","time_ms":16,"result":{"s":"BTC_USDT","U":12,"u":13,"b":[{"p":"100.0","s":1},{"p":"99.9","s":2}],"a":[]}})",
      event, error));
  assert(error.find("side=bid") != std::string::npos);
  assert(error.find("reason=capacity") != std::string::npos);
  assert(error.find("configured=1") != std::string::npos);
  assert(error.find("observed_at_least=2") != std::string::npos);
}

void test_hyperliquid() {
  auto adapter = mds::exchange::make_venue_adapter(
      Venue::Hyperliquid, ProductType::Perpetual, 20);
  assert(adapter);
  const StreamRequest request{"BTCUSDT", "BTC", "bbo", "l2Book", true,
                              true};
  std::vector<std::string> batches;
  std::string error;
  assert(adapter->build_subscription_batches({&request, 1}, batches, error));
  assert(batches.size() == 2);
  const auto metadata_request = adapter->metadata_request();
  assert(metadata_request.method ==
         mds::exchange::HttpRequestSpec::Method::Post);

  constexpr std::string_view metadata_json =
      R"({"universe":[{"name":"BTC","szDecimals":5}]})";
  std::vector<InstrumentMetadata> metadata;
  assert(adapter->parse_metadata(metadata_json, {&request, 1}, metadata,
                                 error));
  assert(metadata.size() == 1);
  assert(metadata[0].tick_size == 1);

  NormalizedEvent event(20);
  assert(adapter->parse_ws(
      R"({"channel":"subscriptionResponse","data":{"method":"unsubscribe","subscription":{"type":"l2Book","coin":"BTC"}}})",
      event, error));
  assert(event.type == AdapterEventType::SubscribeAck);
  assert(error.empty());
  assert(adapter->parse_ws(
      R"({"channel":"l2Book","data":{"coin":"BTC","time":10,"levels":[[{"px":"100.0","sz":"1.00000","n":1}],[{"px":"100.1","sz":"2.00000","n":1}]]}})",
      event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.final_sequence == 1);
  expect_no_allocation(
      *adapter,
      R"({"channel":"l2Book","data":{"coin":"BTC","time":11,"levels":[[{"px":"100.1","sz":"1.00000","n":1}],[{"px":"100.2","sz":"2.00000","n":1}]]}})",
      event, error);

  auto spot = mds::exchange::make_venue_adapter(
      Venue::Hyperliquid, ProductType::Spot, 20);
  assert(spot);
  const StreamRequest spot_request{"BTCUSDC", "BTC_USDC", "bbo",
                                   "l2Book", true, true};
  constexpr std::string_view spot_metadata =
      R"({"tokens":[{"name":"BTC","szDecimals":5,"index":0},{"name":"USDC","szDecimals":8,"index":1}],"universe":[{"name":"@1","tokens":[0,1],"index":1}]})";
  metadata.clear();
  assert(spot->parse_metadata(spot_metadata, {&spot_request, 1}, metadata,
                              error));
  assert(metadata.size() == 1);
  assert(metadata[0].venue_symbol == "@1");
}

void test_unsubscription_regression() {
  struct Case {
    Venue venue;
    ProductType product;
    std::string_view venue_symbol;
    std::string_view ticker;
    std::string_view orderbook;
  };
  constexpr Case cases[] = {
      {Venue::Okx, ProductType::Perpetual, "BTC-USDT-SWAP", "bbo-tbt",
       "books-l2-tbt"},
      {Venue::Bybit, ProductType::Perpetual, "BTCUSDT", "orderbook.1",
       "orderbook.50"},
      {Venue::Bitget, ProductType::Perpetual, "BTCUSDT", "books1",
       "books"},
      {Venue::Gate, ProductType::Perpetual, "BTC_USDT",
       "futures.book_ticker", "futures.order_book_update"},
      {Venue::Hyperliquid, ProductType::Perpetual, "BTC", "bbo",
       "l2Book"},
  };
  for (const auto &item : cases) {
    auto adapter = mds::exchange::make_venue_adapter(
        item.venue, item.product, 1000);
    assert(adapter);
    const StreamRequest request{"BTCUSDT", item.venue_symbol,
                                item.ticker, item.orderbook,
                                false, true, 100};
    std::vector<std::string> batches;
    std::string error;
    assert(adapter->build_unsubscription_batches(
        {&request, 1}, batches, error));
    assert(!batches.empty());
    for (const auto &batch : batches) {
      assert(batch.find("unsubscribe") != std::string::npos);
      assert(batch.find("\"subscribe\"") == std::string::npos);
    }
  }

  auto binance = mds::exchange::make_venue_adapter(
      Venue::Binance, ProductType::Perpetual, 1000);
  assert(binance);
  const StreamRequest request{"BTCUSDT", "BTCUSDT", "bookTicker",
                              "depth", false, true, 100};
  std::vector<std::string> batches;
  std::string error;
  assert(!binance->build_unsubscription_batches(
      {&request, 1}, batches, error));
  assert(error == "venue subscription cannot be converted to unsubscribe");
}

}  // namespace

int main() {
  test_binance();
  test_binance_shared_parser_scales();
  test_binance_metadata_request_capacity();
  test_binance_large_metadata();
  test_okx();
  test_bybit();
  test_bitget();
  test_gate();
  test_hyperliquid();
  test_unsubscription_regression();
  return 0;
}
