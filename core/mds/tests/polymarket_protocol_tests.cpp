#include "mds/exchange/polymarket/polymarket_adapter.h"

#include <array>
#include <cassert>
#include <iostream>
#include <string>
#include <string_view>
#include <vector>

using mds::exchange::AdapterEventType;
using mds::exchange::NormalizedEvent;
using mds::exchange::StreamRequest;
using mds::exchange::polymarket::MarketLifecycle;
using mds::exchange::polymarket::Outcome;
using mds::exchange::polymarket::PolymarketAdapter;
using mds::exchange::polymarket::ResolveSlot;

namespace {

constexpr std::int64_t kStart = 1'723'093'500;
constexpr std::string_view kUp =
    "111111111111111111111111111111111111111111111111111111111111111111";
constexpr std::string_view kDown =
    "222222222222222222222222222222222222222222222222222222222222222222";

const std::string &gamma_response() {
  static const std::string response =
      R"([{"id":"event","slug":"btc-updown-5m-1723093500","markets":[)"
      R"({"id":"market","question":"Bitcoin Up or Down","conditionId":"0x)"
      R"(abcdef","slug":"btc-updown-5m-1723093500","outcomes":"[\"Up\",)"
      R"( \"Down\"]","clobTokenIds":"[\")" +
      std::string(kUp) + R"(\", \")" + std::string(kDown) +
      R"(\"]","orderMinSize":"5","signatureType":3,"negRisk":true,)"
      R"("active":true,"closed":false}]}])";
  return response;
}

void test_slug_and_gamma() {
  const auto window =
      mds::exchange::polymarket::btc_five_minute_window(kStart + 299);
  assert(window.start_unix == kStart);
  assert(window.end_unix == kStart + 300);

  mds::exchange::polymarket::FixedText<64> slug;
  assert(mds::exchange::polymarket::btc_five_minute_slug(window, slug));
  assert(slug.view() == "btc-updown-5m-1723093500");

  mds::exchange::polymarket::GammaMarket market;
  std::string error;
  const auto parsed = mds::exchange::polymarket::parse_gamma_market_response(
      gamma_response(), slug.view(), market, error);
  if (!parsed)
    std::cerr << "gamma parse error: " << error
              << " response=" << gamma_response() << '\n';
  assert(parsed);
  assert(market.window.start_unix == kStart);
  assert(market.token(Outcome::Up) == kUp);
  assert(market.token(Outcome::Down) == kDown);
  assert(market.active && !market.closed);
  assert(market.condition_id.view() == "0xabcdef");
  assert(market.minimum_order_size == 5'000'000);
  assert(market.signature_type == 3);
  assert(market.negative_risk);
}

void resolve_and_subscribe(PolymarketAdapter &adapter) {
  adapter.prepare_resolution(kStart + 1);
  auto request = adapter.next_gamma_request();
  assert(request && request->slot == ResolveSlot::Current);
  assert(request->target.view() ==
         "/events?slug=btc-updown-5m-1723093500");
  assert(adapter.mark_gamma_requested(request->slot));

  std::string error;
  assert(adapter.apply_gamma_response(request->slot, gamma_response(), error));
  const auto *market = adapter.resolved_market(ResolveSlot::Current);
  assert(market && market->token(Outcome::Up) == kUp);

  std::array<StreamRequest, 2> requests{{
      {"BTC5MUP", "BTC5MUP", "best_bid_ask", "market", true, true, 0},
      {"BTC5MDOWN", "BTC5MDOWN", "best_bid_ask", "market", true, true, 0},
  }};
  std::vector<mds::exchange::InstrumentMetadata> metadata;
  assert(adapter.build_resolved_metadata(ResolveSlot::Current, requests,
                                         metadata, error));
  assert(metadata.size() == 2);
  assert(metadata[0].venue_symbol ==
         "btc-updown-5m-1723093500:UP");
  assert(metadata[1].venue_symbol ==
         "btc-updown-5m-1723093500:DOWN");
  assert(metadata[0].lot_size == 5'000'000);
  assert(metadata[0].signature_type == 3);
  assert(metadata[0].negative_risk);
  std::vector<std::string> batches;
  assert(adapter.build_subscription_batches(requests, batches, error));
  assert(batches.size() == 1);
  assert(batches[0] ==
         "{\"assets_ids\":[\"" + std::string(kUp) + "\",\"" +
             std::string(kDown) +
             "\"],\"type\":\"market\",\"custom_feature_enabled\":true}");

  std::array<std::string_view, 2> assets{kUp, kDown};
  std::string operation;
  assert(adapter.build_dynamic_operation(assets, true, operation, error));
  assert(operation.find("\"operation\":\"subscribe\"") !=
         std::string::npos);
  assert(adapter.build_dynamic_operation(assets, false, operation, error));
  assert(operation.find("\"operation\":\"unsubscribe\"") !=
         std::string::npos);

  const auto heartbeat = adapter.heartbeat();
  assert(heartbeat.payload == "PING");
  assert(heartbeat.interval_ms == 10'000);
}

void test_wss_protocol() {
  PolymarketAdapter adapter(8);
  resolve_and_subscribe(adapter);

  NormalizedEvent event(8);
  std::string error;
  const std::string books =
      R"([{"event_type":"book","asset_id":")" + std::string(kUp) +
      R"(","timestamp":"1000","bids":[{"price":"0.45","size":"12.5"}],)"
      R"("asks":[{"price":"0.55","size":"7"}]},)"
      R"({"type":"book","assetId":")" + std::string(kDown) +
      R"(","timestamp":1001,"bids":[{"price":0.44,"size":3}],)"
      R"("asks":[{"price":0.56,"size":4}]}])";
  assert(adapter.parse_ws(books, event, error));
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.symbol_view() == "BTC5MUP");
  assert(event.bids[0].price == 450'000);
  assert(event.bids[0].quantity == 12'500'000);

  assert(adapter.parse_ws({}, event, error));
  if (event.type != AdapterEventType::BookSnapshot)
    std::cerr << "unexpected queued event type="
              << static_cast<int>(event.type)
              << " symbol=" << event.symbol_view() << '\n';
  assert(event.type == AdapterEventType::BookSnapshot);
  assert(event.symbol_view() == "BTC5MDOWN");

  const std::string delta =
      R"({"event_type":"price_change","timestamp":"1002","price_changes":[)"
      R"({"asset_id":")" + std::string(kUp) +
      R"(","price":"0.46","size":"2.25","side":"BUY",)"
      R"("best_bid":"0.46","best_ask":"0.54"}]})";
  assert(adapter.parse_ws(delta, event, error));
  assert(event.type == AdapterEventType::BookDelta);
  assert(event.bids.size() == 1);
  assert(event.bids[0].price == 460'000);

  assert(adapter.parse_ws({}, event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.bid.price == 460'000);
  assert(event.ask.price == 540'000);

  const std::string sdk_bbo =
      R"({"type":"best_bid_ask","data":{"assetId":")" +
      std::string(kDown) +
      R"(","bestBid":0.47,"bestAsk":0.53,"timestamp":1003}})";
  assert(adapter.parse_ws(sdk_bbo, event, error));
  assert(event.type == AdapterEventType::Bbo);
  assert(event.symbol_view() == "BTC5MDOWN");

  const std::string tick =
      R"({"event_type":"tick_size_change","asset_id":")" +
      std::string(kUp) + R"(","new_tick_size":"0.001","timestamp":"1004"})";
  assert(adapter.parse_ws(tick, event, error));
  assert(event.type == AdapterEventType::InstrumentUpdate);
  assert(event.tick_size == 1'000);

  const std::string opened =
      R"({"event_type":"new_market","asset_id":")" + std::string(kUp) +
      R"(","timestamp":"1005"})";
  assert(adapter.parse_ws(opened, event, error));
  assert(adapter.lifecycle(kUp) == MarketLifecycle::Active);

  const std::string resolved =
      R"({"event_type":"market_resolved","asset_id":")" +
      std::string(kUp) +
      R"(","winning_outcome":"Up","timestamp":"1006"})";
  assert(adapter.parse_ws(resolved, event, error));
  assert(adapter.lifecycle(kUp) == MarketLifecycle::ResolvedUp);
}

void test_capacity_becomes_gap() {
  PolymarketAdapter adapter(1);
  resolve_and_subscribe(adapter);
  NormalizedEvent event(1);
  std::string error;
  const std::string oversized =
      R"({"event_type":"book","asset_id":")" + std::string(kUp) +
      R"(","bids":[{"price":"0.4","size":"1"},{"price":"0.3","size":"2"}],)"
      R"("asks":[]})";
  assert(adapter.parse_ws(oversized, event, error));
  assert(event.type == AdapterEventType::BookGap);
}

}  // namespace

int main() {
  test_slug_and_gamma();
  test_wss_protocol();
  test_capacity_becomes_gap();
}
