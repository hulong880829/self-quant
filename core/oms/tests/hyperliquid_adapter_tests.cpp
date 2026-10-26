#include "oms/exchange/hyperliquid/metadata.h"
#include "oms/exchange/hyperliquid/protocol.h"
#include "oms/exchange/hyperliquid/session.h"

#include <cstdint>
#include <iostream>
#include <stdexcept>
#include <string>
#include <vector>

namespace {

using namespace oms::exchange::hyperliquid;

void require(bool condition, const char* message) {
  if (!condition) throw std::runtime_error(message);
}

constexpr char kKey[] =
    "0000000000000000000000000000000000000000000000000000000000000001";

void make_ready(Session& session) {
  require(session.arm_signer(kKey), "signer");
  session.set_strategy_instance("tradefi-perps-mm:maker");
  require(session.add_perp("xyz:GME", 7, 2, "xyz", 3), "metadata insert");
  session.seal_metadata();
  session.note_private_open();
  require(session.complete_reconcile(true, true), "reconcile");
}

void test_cloid_namespace() {
  const auto owner = stable_strategy_hash("tradefi-perps-mm:maker");
  std::string cloid;
  require(encode_cloid({owner, 0x123456, 0x010203040506ULL}, cloid),
          "encode cloid");
  require(cloid.size() == 34 && cloid.starts_with("0x"), "128-bit cloid");
  CloidFields decoded;
  require(decode_cloid(cloid, decoded), "decode cloid");
  require(decoded.strategy_instance_hash == owner &&
              decoded.process_epoch == 0x123456 &&
              decoded.sequence == 0x010203040506ULL,
          "cloid fields");
  require(cloid_owned_by(cloid, owner), "owner survives epoch");
  require(!cloid_owned_by(cloid, stable_strategy_hash("other")),
          "foreign owner");
  require(!encode_cloid({owner, 1, kMaximumCloidSequence + 1}, cloid),
          "sequence overflow");
  cloid[2] = '0';
  cloid[3] = '0';
  require(!decode_cloid(cloid, decoded), "magic required");
}

void test_nonce_and_fill_identity() {
  Session session(11);
  make_ready(session);
  const auto first = session.place_limit(41, "xyz:GME", true, "10", "0.125",
                                         false, "Gtc", 1000, true, "maker-41");
  const auto second = session.place_limit(42, "xyz:GME", false, "11", "0.125",
                                          false, "Gtc", 999, true, "maker-42");
  require(first.accepted && second.accepted, "orders accepted");
  require(second.nonce == first.nonce + 1, "monotonic place nonce");
  const auto cancel = session.cancel_by_cloid(41, 500, true);
  require(cancel.accepted && cancel.nonce == second.nonce + 1,
          "monotonic cancel nonce");
  const auto legacy_cancel =
      session.cancel_by_cloid("xyz:GME", second.cloid, 501, true);
  require(legacy_cancel.accepted &&
              legacy_cancel.cloid == second.cloid &&
              legacy_cancel.asset == second.asset &&
              legacy_cancel.nonce == cancel.nonce + 1,
          "native cloid cancel");
  CloidFields fields;
  require(decode_cloid(first.cloid, fields) && fields.sequence == 41,
          "session cloid");
  Session restarted(12);
  restarted.set_strategy_instance("tradefi-perps-mm:maker");
  std::uint64_t recovered_sequence{};
  require(restarted.sequence_from_cloid(first.cloid, recovered_sequence) &&
              recovered_sequence == 41,
          "owner recognized after restart");
  require(session.apply_fill(700, "xyz:GME", "9", 41, {125, 3, {}}) ==
              Session::FillResult::Applied,
          "fractional fill");
  require(session.apply_fill(700, "xyz:GME", "9", 41, {125, 3, {}}) ==
              Session::FillResult::Duplicate,
          "triple duplicate");
  require(session.apply_fill(701, "xyz:GME", "9", 41, {25, 3, {}}) ==
              Session::FillResult::Applied,
          "same tid different time");
  require(session.filled(41) == 150, "fractional cumulative units");
}

void test_metadata() {
  std::vector<PerpDex> dexs;
  require(parse_perp_dexs(
              R"([null,{"name":"xyz"},{"name":"Flx"}])", dexs) ==
              JsonParseResult::Ok,
          "perp dex parse");
  require(dexs.size() == 3 && dexs[1].index == 1 &&
              dexs[1].name == "xyz",
          "real dex index");
  std::vector<PerpAsset> assets;
  require(parse_perp_meta(
              R"([{"universe":[{"name":"GME","szDecimals":3},{"name":"ABC","szDecimals":1}]},[{},{}]])",
              dexs[1].index, dexs[1].name, assets) == JsonParseResult::Ok,
          "metaAndAssetCtxs parse");
  require(assets[0].symbol == "xyz:GME" && assets[0].asset_index == 0 &&
              assets[0].size_decimals == 3,
          "HIP-3 metadata");
  AssetTable table;
  require(table.add_perp(assets[0].symbol, assets[0].asset_index,
                         assets[0].dex_index, dexs[1].name,
                         assets[0].size_decimals),
          "asset table");
  AssetId id;
  require(table.find("xyz:GME", id) && id.value == 110000 &&
              id.size_decimals == 3,
          "HIP-3 asset id");
  require(!table.find("xyz:gme", id), "symbols are case-sensitive");
}

void test_structured_events() {
  std::string cloid;
  require(encode_cloid({stable_strategy_hash("strategy"), 7, 99}, cloid),
          "event cloid");
  const std::string fills =
      std::string(
          R"({"channel":"userFills","data":{"fills":[{"coin":"xyz:GME","px":"12.3400","sz":"0.125","time":1700,"tid":42,"oid":9,"cloid":")") +
      cloid + R"("}]}})";
  std::vector<UserEvent> events;
  require(parse_user_events(fills, events) == JsonParseResult::Ok &&
              events.size() == 1,
          "fill event parse");
  require(events[0].size.value == 125 && events[0].size.scale == 3 &&
              events[0].price.value == 123400 &&
              events[0].price.scale == 4 && events[0].tid == "42",
          "fractional event values");
  const std::string orders =
      std::string(
          R"({"channel":"orderUpdates","data":[{"order":{"coin":"xyz:GME","oid":9,"sz":"0.125","cloid":")") +
      cloid + R"(","timestamp":1700},"status":"canceled","statusTimestamp":1701}]})";
  require(parse_user_events(orders, events) == JsonParseResult::Ok &&
              events[0].kind == UserEventKind::Order &&
              events[0].status == "canceled",
          "order event parse");
  require(parse_user_events(
              R"({"channel":"userFills","data":{"fills":[{"coin":"BTC","tid":1}]}})",
              events) == JsonParseResult::InvalidValue,
          "malformed event rejected");

  ExchangeResponse response;
  require(parse_exchange_response(
              R"({"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":17,"cloid":"0x1234"}}]}}})",
              response) == JsonParseResult::Ok &&
              response.accepted && response.oid == "17",
          "structured exchange response");

  std::vector<OpenOrder> open;
  require(parse_open_orders(
              R"([{"coin":"xyz:GME","limitPx":"12.34","oid":17,"side":"B","sz":"0.125","timestamp":1700,"cloid":"0x1234"}])",
              open) == JsonParseResult::Ok &&
              open[0].size.value == 125 && open[0].side == oms::api::Side::Buy,
          "open-order snapshot parse");
  std::vector<Position> positions;
  require(parse_positions(
              R"({"assetPositions":[{"position":{"coin":"xyz:GME","szi":"-1.250"}}]})",
              positions) == JsonParseResult::Ok &&
              positions[0].size.value == -1250 &&
              positions[0].size.scale == 3,
          "position snapshot parse");
}

}  // namespace

int main() {
  try {
    test_cloid_namespace();
    test_nonce_and_fill_identity();
    test_metadata();
    test_structured_events();
  } catch (const std::exception& error) {
    std::cerr << error.what() << '\n';
    return 1;
  }
  std::cout << "hyperliquid adapter tests passed\n";
  return 0;
}
