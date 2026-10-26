#include "oms/exchange/hyperliquid/protocol.h"
#include "oms/exchange/hyperliquid/session.h"
#include "oms/exchange/hyperliquid/trade_adapter.h"
#include "oms/exchange/ibkr/session.h"
#include "oms/exchange/ibkr/trade_adapter.h"

#include <cstdint>
#include <iostream>
#include <stdexcept>
#include <string>
#include <vector>

namespace {

void require(bool condition, const char* message) {
  if (!condition) throw std::runtime_error(message);
}

}  // namespace

int main() {
  try {
    oms::exchange::hyperliquid::AssetTable assets;
    require(assets.add_perp("BTC", 0), "btc metadata");
    require(assets.add_perp("GME", 1, 1, "xyz"), "gme metadata");
    assets.add_spot("PURR/USDC", 5);
    oms::exchange::hyperliquid::AssetId btc;
    oms::exchange::hyperliquid::AssetId gme;
    oms::exchange::hyperliquid::AssetId purr;
    require(assets.find("BTC", btc) && btc.value == 0, "btc id");
    require(assets.find("xyz:GME", gme) && gme.value == 110001, "hip3 id");
    require(assets.find("PURR/USDC", purr) && purr.value == 10005, "spot id");

    oms::exchange::hyperliquid::LimitOrder order;
    order.asset = 0;
    order.buy = true;
    order.price = "20000";
    order.size = "0.1";
    order.tif = "Alo";
    const auto encoded =
        oms::exchange::hyperliquid::encode_order_action(order);
    const std::vector<std::uint8_t> expected = {
        0x83, 0xa4, 't', 'y', 'p', 'e', 0xa5, 'o', 'r', 'd', 'e', 'r',
        0xa6, 'o', 'r', 'd', 'e', 'r', 's', 0x91, 0x86, 0xa1, 'a', 0x00,
        0xa1, 'b', 0xc3, 0xa1, 'p', 0xa5, '2', '0', '0', '0', '0', 0xa1,
        's', 0xa3, '0', '.', '1', 0xa1, 'r', 0xc2, 0xa1, 't', 0x81, 0xa5,
        'l', 'i', 'm', 'i', 't', 0x81, 0xa3, 't', 'i', 'f', 0xa3, 'A', 'l',
        'o', 0xa8, 'g', 'r', 'o', 'u', 'p', 'i', 'n', 'g', 0xa2, 'n', 'a'};
    require(encoded == expected, "order action msgpack");

    constexpr char kKey[] =
        "0000000000000000000000000000000000000000000000000000000000000001";
    oms::exchange::hyperliquid::Session session;
    require(session.arm_signer(kKey), "signer");
    require(session.place_market() == oms::exchange::AdapterResult::Unsupported,
            "market stays unsupported");
    require(session.add_perp("BTC", 0), "session metadata");
    session.add_spot("PURR/USDC", 5);
    session.seal_metadata();
    session.note_private_open();
    require(session.complete_reconcile(true, true), "reconcile");
    require(session.ready(), "ready");
    const auto first =
        session.place_limit(7, "BTC", true, "20000", "0.1", false, "Alo", 1000,
                            true);
    const auto rolled =
        session.place_limit(8, "BTC", false, "20001", "0.1", false, "Gtc", 1000,
                            true);
    require(first.accepted && rolled.accepted, "signed limits");
    require(rolled.nonce == first.nonce + 1, "nonce rollback");
    require(first.signature[64] == 27 || first.signature[64] == 28, "recovery");
    require(session.apply_fill(11, 7, 1) ==
                oms::exchange::hyperliquid::Session::FillResult::Applied,
            "fill");
    require(session.apply_fill(11, 7, 1) ==
                oms::exchange::hyperliquid::Session::FillResult::Duplicate,
            "dedup");
    require(session.filled(7) == 1, "partial");
    require(session.cancel(7), "cancel");
    session.note_private_lost();
    require(!session.ready(), "reconnect drops ready");
    require(!session.complete_reconcile(false, true), "unmapped fails closed");

    oms::exchange::ibkr::Session broker(32, "DU123");
    require(!oms::exchange::ibkr::client_id_conflicts(32), "32 is legal");
    require(oms::exchange::ibkr::client_id_conflicts(30) &&
                oms::exchange::ibkr::client_id_conflicts(31),
            "reserved client ids");
    broker.note_connected(true);
    oms::exchange::ibkr::Contract stock;
    stock.con_id = 265598;
    stock.security = utils::md::IbkrSecurityType::Stock;
    stock.ioc = true;
    stock.multiplier_present = false;
    stock.min_size = 1;
    stock.size_increment = 1;
    stock.units_per_native = 1;
    require(broker.resolve(stock), "stock");
    require(!broker.execution_ready(265598), "stock waits for session");
    oms::exchange::ibkr::Contract commodity;
    commodity.con_id = 69067924;
    commodity.security = utils::md::IbkrSecurityType::Commodity;
    commodity.ioc = true;
    commodity.multiplier_present = false;
    commodity.min_size = 1;
    commodity.size_increment = 1;
    require(!broker.resolve(commodity), "cmdty multiplier required");
    commodity.multiplier_present = true;
    commodity.multiplier = 100;
    commodity.units_per_native = 100;
    commodity.ioc = false;
    require(broker.resolve(commodity), "cmdty stored");
    require(broker.instrument_ready(69067924), "cmdty instrument ready");
    require(broker.ioc_capability(69067924) ==
                oms::exchange::ibkr::IocCapability::Unsupported,
            "ioc unsupported recorded");
    require(!broker.execution_ready(69067924), "session still closed");
    const auto hedge = broker.hedge(69067924, 250);
    require(hedge.tradable && hedge.native_quantity == 2 &&
                hedge.residual_base_units == 50,
            "cmdty step");

    oms::exchange::ibkr::Callback next_id;
    next_id.kind = oms::exchange::ibkr::Callback::Kind::NextValidId;
    next_id.order_id = 50;
    require(broker.push(next_id), "next id");
    broker.service();
    broker.note_reconciled();
    require(broker.execution_ready(265598), "stock ready");
    require(broker.execution_ready(69067924), "ioc unsupported stays tradable");
    require(broker.allocate_order_id() == 50, "order id");
    oms::exchange::ibkr::Callback lost;
    lost.kind = oms::exchange::ibkr::Callback::Kind::Error;
    lost.code = 1100;
    require(broker.push(lost), "1100");
    broker.service();
    require(broker.cancel_makers() && !broker.new_orders_allowed(), "halt");
    const auto generation_before_1102 = broker.generation();
    const auto connection_before_1102 = broker.connection_generation();
    oms::exchange::ibkr::Callback maintained;
    maintained.kind = oms::exchange::ibkr::Callback::Kind::Error;
    maintained.code = 1102;
    require(broker.push(maintained), "1102");
    broker.service();
    require(broker.status() == oms::exchange::AdapterStatus::Reconciling &&
                !broker.new_orders_allowed(),
            "1102 waits for reconcile");
    require(broker.confirm_reconnect(), "1102 confirm");
    require(broker.generation() == generation_before_1102 &&
                broker.connection_generation() == connection_before_1102,
            "1102 keeps generations");
    require(broker.status() == oms::exchange::AdapterStatus::Ready &&
                broker.new_orders_allowed(),
            "1102");
    require(broker.allocate_order_id() == 51, "order id monotonic");
    const auto generation = broker.generation();
    oms::exchange::ibkr::Callback restored;
    restored.kind = oms::exchange::ibkr::Callback::Kind::Error;
    restored.code = 1101;
    require(broker.push(restored), "1101");
    broker.service();
    require(broker.generation() == generation + 1, "session generation");
    require(broker.allocate_order_id() < 0, "1101 stops new orders");
    require(broker.next_order_id() == 52, "order id is not reused");

    oms::exchange::ibkr::Session overflow(32, "DU123");
    oms::exchange::ibkr::Callback beat;
    beat.kind = oms::exchange::ibkr::Callback::Kind::Exec;
    beat.exec_key = 1;
    for (std::size_t index = 0; index < oms::exchange::ibkr::kCallbackCapacity;
         ++index) {
      require(overflow.push(beat), "queue accepts");
    }
    require(!overflow.push(beat), "queue overflow fails closed");
    require(overflow.status() == oms::exchange::AdapterStatus::Failed,
            "overflow status");

    oms::exchange::ibkr::Session duplicate(7, "DU123");
    oms::exchange::ibkr::Callback used;
    used.kind = oms::exchange::ibkr::Callback::Kind::Error;
    used.code = 326;
    require(duplicate.push(used), "326 queued");
    duplicate.service();
    require(duplicate.status() == oms::exchange::AdapterStatus::Failed,
            "duplicate client id");
    require(duplicate.allocate_order_id() < 0, "stopped");

    oms::exchange::hyperliquid::HyperliquidTradeAdapter hyperliquid(kKey);
    oms::exchange::AdapterReservation reserved;
    require(hyperliquid.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                        reserved) ==
                oms::exchange::AdapterResult::Ok,
            "hl reserve");
    oms::exchange::AdapterPlaceCommand market;
    market.request.type = oms::api::OrderType::Market;
    require(hyperliquid.commit_place(reserved, market) ==
                oms::exchange::AdapterResult::Ok,
            "unsupported place is queued");
    oms::exchange::AdapterEvent rejected;
    oms::exchange::AdapterEventSink sink{
        &rejected,
        [](void* context, const oms::exchange::AdapterEvent& event) noexcept {
          *static_cast<oms::exchange::AdapterEvent*>(context) = event;
          return oms::exchange::AdapterResult::Ok;
        }};
    const auto serviced = hyperliquid.service_io(0, 4, sink);
    require(serviced.result == oms::exchange::AdapterResult::Ok &&
                rejected.venue.type == oms::api::VenueEventType::NewReject &&
                rejected.venue.reconciled_status ==
                    oms::api::OrderStatus::Rejected,
            "hl NewReject");

    oms::exchange::ibkr::IbkrTradeAdapter ibkr(32, "DU123");
    require(ibkr.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                 reserved) == oms::exchange::AdapterResult::Ok,
            "ib reserve");
    oms::exchange::AdapterPlaceCommand post_only;
    post_only.request.type = oms::api::OrderType::Limit;
    post_only.request.time_in_force = oms::api::TimeInForce::GTC;
    post_only.request.flags = oms::api::PostOnly;
    require(ibkr.commit_place(reserved, post_only) ==
                oms::exchange::AdapterResult::Ok,
            "post-only queued");
    rejected = {};
    require(ibkr.service_io(0, 4, sink).result ==
                    oms::exchange::AdapterResult::Ok &&
                rejected.venue.type == oms::api::VenueEventType::NewReject,
            "ib PostOnly NewReject");
    require(ibkr.reserve_command(oms::exchange::AdapterCommandKind::Place,
                                 reserved) == oms::exchange::AdapterResult::Ok,
            "ib reserve tif");
    oms::exchange::AdapterPlaceCommand fok;
    fok.request.type = oms::api::OrderType::Limit;
    fok.request.time_in_force = oms::api::TimeInForce::FOK;
    require(ibkr.commit_place(reserved, fok) ==
                oms::exchange::AdapterResult::Ok,
            "fok queued");
    rejected = {};
    require(ibkr.service_io(0, 4, sink).result ==
                    oms::exchange::AdapterResult::Ok &&
                rejected.venue.type == oms::api::VenueEventType::NewReject,
            "ib FOK NewReject");
  } catch (const std::exception& error) {
    std::cerr << error.what() << '\n';
    return 1;
  }
  std::cout << "tradefi oms tests passed\n";
  return 0;
}
