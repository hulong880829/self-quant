#include "tradefimm/engine.h"
#include "tradefimm/normalize.h"
#include "tradefimm/recovery.h"

#include <cassert>
#include <iostream>

namespace {

tradefimm::PairSpec spec_hl_maker() {
  tradefimm::PairSpec spec;
  spec.maker.venue = tradefimm::Venue::Hyperliquid;
  spec.hedge.venue = tradefimm::Venue::Ibkr;
  spec.maker.multiplier = 1;
  spec.hedge.multiplier = 10;
  spec.maker.min_native = 1;
  spec.hedge.min_native = 1;
  spec.maker.lot = 1;
  spec.hedge.lot = 1;
  spec.maker.max_position = 1000;
  spec.hedge.max_position = 1000;
  spec.delta_tolerance = 0;
  spec.quote_size_base = 10;
  spec.tick = 1;
  spec.slippage_per_base = 1;
  spec.basis_offset = 5;
  spec.basis_buffer = 1;
  spec.target_profit_per_base = 1;
  spec.max_net_delta = 1000;
  spec.max_notional = 100000000;
  spec.max_unhedged_notional = 100000000;
  spec.fees.hl_maker_bps = 0;
  spec.fees.ib_sell_bps = 0;
  spec.fees.ib_buy_bps = 0;
  spec.ibkr_short_policy = tradefimm::IbkrShortPolicy::Allow;
  return spec;
}

tradefimm::Books fresh_books() {
  tradefimm::Books books;
  books.maker_bid = 100;
  books.maker_ask = 101;
  books.hedge_bid = 100;
  books.hedge_ask = 102;
  books.maker_recv_ns = 1'000;
  books.hedge_recv_ns = 1'000;
  return books;
}

void arm(tradefimm::Engine& engine, const tradefimm::PairSpec& spec) {
  assert(engine.start(spec));
  engine.note_catalog();
  engine.note_oms(true, true);
  engine.note_reconciled(true);
  engine.set_books(fresh_books(), 1'500, 10'000);
  assert(engine.baseline_ready());
}

}  // namespace

int main() {
  using tradefimm::Engine;
  using tradefimm::HedgeAction;
  using tradefimm::Phase;
  using tradefimm::Recovery;
  using tradefimm::SlotId;
  using tradefimm::SlotState;

  assert(Engine::hedge_native_for(100, 10, 1, 1) == -10);
  assert(100 + (-10 * 10) == 0);

  Engine crossed;
  auto bad = spec_hl_maker();
  bad.same_base = false;
  assert(!crossed.start(bad));
  bad = spec_hl_maker();
  bad.delta_tolerance = 1;
  assert(!crossed.start(bad));
  bad.delta_tolerance = 0;
  assert(crossed.start(bad));

  Engine maker;
  arm(maker, spec_hl_maker());
  assert(maker.phase() == Phase::Quoting);
  maker.set_positions(100, 0);
  assert(maker.net_delta() == 100);
  const auto hedge = maker.plan();
  assert(hedge.action == HedgeAction::Hedge);
  assert(hedge.native_quantity == -10);
  maker.on_hedge_fill(-10, 100);
  maker.note_hedge_terminal();
  assert(maker.net_delta() == 0);
  assert(maker.phase() == Phase::Quoting);

  Engine dust;
  arm(dust, spec_hl_maker());
  dust.set_positions(15, 0);
  dust.on_hedge_fill(-1, 100);
  const auto leftover = dust.plan();
  assert(leftover.action == HedgeAction::Unwind);
  assert(leftover.native_quantity == -5);
  assert(dust.net_delta() == 5);

  Engine stuck;
  auto both_coarse = spec_hl_maker();
  both_coarse.maker.min_native = 10;
  both_coarse.maker.multiplier = 1;
  both_coarse.hedge.multiplier = 10;
  both_coarse.quote_size_base = 10;
  assert(stuck.start(both_coarse));
  stuck.note_catalog();
  stuck.note_oms(true, true);
  stuck.note_reconciled(true);
  stuck.set_positions(5, 0);
  stuck.set_books(fresh_books(), 1'500, 10'000);
  assert(stuck.plan().action == HedgeAction::Halt);
  assert(stuck.net_delta() == 5);

  Engine rejected_hedge;
  auto no_retry = spec_hl_maker();
  no_retry.max_hedge_retries = 0;
  arm(rejected_hedge, no_retry);
  rejected_hedge.on_maker_fill(no_retry.quote_size_base, 100);
  assert(rejected_hedge.phase() == Phase::HedgePending);
  rejected_hedge.note_hedge_terminal();
  assert(rejected_hedge.phase() == Phase::EmergencyUnwind);
  assert(rejected_hedge.plan().action == HedgeAction::Unwind);

  Engine priced;
  auto fees = spec_hl_maker();
  fees.fees.hl_maker_bps = 100;
  fees.fees.ib_sell_bps = 100;
  fees.basis_offset = 0;
  fees.basis_buffer = 0;
  fees.slippage_per_base = 0;
  fees.target_profit_per_base = 0;
  arm(priced, fees);
  const auto quote = priced.quote();
  assert(quote.bid);
  assert(quote.cost_per_base >= 2);

  auto inventory_only = spec_hl_maker();
  inventory_only.ibkr_short_policy =
      tradefimm::IbkrShortPolicy::InventoryOnly;
  Engine hl_inventory;
  arm(hl_inventory, inventory_only);
  hl_inventory.set_ib_inventory(0);
  const auto hl_zero = hl_inventory.quote();
  assert(!hl_zero.bid);
  assert(hl_zero.ask);
  hl_inventory.set_ib_inventory(inventory_only.quote_size_base);
  assert(hl_inventory.quote().bid);
  hl_inventory.set_ib_inventory(inventory_only.quote_size_base,
                                inventory_only.quote_size_base);
  assert(!hl_inventory.quote().bid);

  Engine hl_short_allowed;
  arm(hl_short_allowed, spec_hl_maker());
  hl_short_allowed.set_ib_inventory(0);
  assert(hl_short_allowed.quote().bid);

  Engine ib_maker;
  auto reversed = spec_hl_maker();
  reversed.maker.venue = tradefimm::Venue::Ibkr;
  reversed.hedge.venue = tradefimm::Venue::Hyperliquid;
  reversed.maker.multiplier = 10;
  reversed.hedge.multiplier = 1;
  reversed.ibkr_short_policy = tradefimm::IbkrShortPolicy::InventoryOnly;
  reversed.fees.ib_taker_bps = 0;
  reversed.fees.hl_taker_bps = 0;
  reversed.basis_offset = 0;
  reversed.basis_buffer = 0;
  reversed.slippage_per_base = 0;
  reversed.target_profit_per_base = 0;
  arm(ib_maker, reversed);
  ib_maker.set_ib_inventory(0);
  const auto ib_zero = ib_maker.quote();
  assert(ib_zero.bid);
  assert(!ib_zero.ask);
  ib_maker.set_ib_inventory(reversed.quote_size_base);
  assert(ib_maker.quote().ask);
  ib_maker.set_positions(10, 0);
  assert(ib_maker.plan().native_quantity == -100);

  auto stale = fresh_books();
  stale.maker_recv_ns = 1;
  ib_maker.set_books(stale, 100'000, 1'000);
  assert(ib_maker.makers_must_cancel());

  Recovery recovery;
  recovery.note_stable(true);
  recovery.barrier().maker_orders = true;
  recovery.barrier().maker_positions = true;
  recovery.barrier().hedge_orders = true;
  recovery.barrier().hedge_positions = true;
  recovery.barrier().fills_caught_up = true;
  recovery.barrier().orders_determined = true;
  recovery.barrier().delta_within_tolerance = true;
  recovery.barrier().maker_oms_ready = true;
  recovery.barrier().hedge_oms_ready = true;
  recovery.barrier().maker_bbo_fresh = true;
  recovery.barrier().hedge_bbo_fresh = true;
  assert(!recovery.can_resume_quotes());
  recovery.note_stable(false);
  assert(recovery.can_resume_quotes());
  assert(recovery.submit(SlotId::Hedge, 1, 7));
  recovery.timeout(SlotId::Hedge);
  assert(recovery.slot(SlotId::Hedge).state == SlotState::CancelPending);
  assert(!recovery.submit(SlotId::Hedge, 1, 8));
  recovery.cancel_ack(SlotId::Hedge, 1);
  assert(recovery.slot(SlotId::Hedge).state == SlotState::Terminal);
  assert(recovery.submit(SlotId::MakerBid, recovery.maker_generation(), 9));
  recovery.ack(SlotId::MakerBid, recovery.maker_generation() + 1);
  assert(recovery.slot(SlotId::MakerBid).state == SlotState::Submitted);
  assert(recovery.fill(SlotId::MakerBid, 42, 1, 1));
  assert(!recovery.fill(SlotId::MakerBid, 42, 1, 0));
  recovery.hedge_reject();
  assert(!recovery.quoting_allowed());
  recovery.unwind_reject();
  assert(recovery.halted());

  Recovery flap;
  flap.note_reconnect(true);
  flap.note_reconnect(true);
  flap.note_reconnect(true);
  assert(flap.circuit_open());
  flap.maker_venue_down();
  assert(flap.submit(SlotId::MakerBid, 1, 1) == false);

  for (int direction = 0; direction < 2; ++direction) {
    for (int loop = 0; loop < 1000; ++loop) {
      Engine engine;
      auto cycle = spec_hl_maker();
      if (direction == 1) {
        cycle.maker.venue = tradefimm::Venue::Ibkr;
        cycle.hedge.venue = tradefimm::Venue::Hyperliquid;
        cycle.maker.multiplier = 1;
        cycle.hedge.multiplier = 1;
        cycle.quote_size_base = 1;
      }
      arm(engine, cycle);
      engine.set_positions(0, 0);
      assert(engine.plan().action == HedgeAction::Accept);
      const std::int64_t fill = cycle.quote_size_base;
      engine.on_maker_fill(fill, 100);
      const auto planned = engine.plan();
      assert(planned.action == HedgeAction::Hedge);
      engine.on_hedge_fill(planned.native_quantity / 2, 100);
      assert(engine.net_delta() != 0);
      engine.on_hedge_fill(planned.native_quantity - planned.native_quantity / 2, 100);
      engine.note_hedge_terminal();
      assert(engine.net_delta() == 0);
      assert(engine.phase() == Phase::Quoting);
      assert(engine.session_pnl() == 0 || engine.baseline_ready());
    }
  }

  Engine marked;
  auto inventory = spec_hl_maker();
  assert(marked.start(inventory));
  marked.note_catalog();
  marked.note_oms(true, true);
  marked.note_reconciled(true);
  marked.set_positions(100, -10);
  assert(marked.session_pnl() == 0);
  marked.set_books(fresh_books(), 1'500, 10'000);
  assert(marked.baseline_ready());
  assert(marked.net_delta() == 0);
  assert(marked.session_pnl() == 0);
  marked.on_maker_fill(10, 100);
  assert(marked.session_pnl() == -1000);

  Engine one_sided;
  assert(one_sided.start(inventory));
  one_sided.note_catalog();
  one_sided.note_oms(true, true);
  one_sided.note_reconciled(true);
  auto missing = fresh_books();
  missing.hedge_recv_ns = 0;
  one_sided.set_books(missing, 1'500, 10'000);
  assert(!one_sided.baseline_ready());
  assert(one_sided.session_pnl() == 0);

  assert(Engine::hedge_native_for(15, 15, 1, 1, 1) == -10);
  Engine fractional;
  auto fractional_spec = spec_hl_maker();
  fractional_spec.hedge.multiplier = 15;
  fractional_spec.hedge.multiplier_scale = 1;
  fractional_spec.quote_size_base = 15;
  assert(fractional.start(fractional_spec));
  fractional.on_maker_fill(15, 100);
  assert(fractional.net_delta() == 15);
  const auto fractional_plan = fractional.plan();
  assert(fractional_plan.action == HedgeAction::Hedge);
  assert(fractional_plan.native_quantity == -10);
  assert(fractional_plan.residual == 0);

  std::int64_t scale_two = 0;
  std::int64_t scale_four = 0;
  assert(tradefimm::parse_decimal("0.01", 2, scale_two) && scale_two == 1);
  assert(tradefimm::parse_decimal("0.01", 4, scale_four) && scale_four == 100);
  auto quote_at = [](std::int64_t offset, std::int64_t bid, std::int64_t ask,
                     std::int64_t tick) {
    tradefimm::Engine engine;
    auto spec = spec_hl_maker();
    spec.basis_offset = offset;
    spec.basis_buffer = offset;
    spec.slippage_per_base = offset;
    spec.target_profit_per_base = offset;
    spec.tick = tick;
    spec.fees = {};
    spec.max_notional = 1000000000000LL;
    assert(engine.start(spec));
    engine.note_catalog();
    engine.note_oms(true, true);
    engine.note_reconciled(true);
    tradefimm::Books books;
    books.maker_bid = bid;
    books.maker_ask = ask;
    books.hedge_bid = bid;
    books.hedge_ask = ask;
    books.maker_recv_ns = 1000;
    books.hedge_recv_ns = 1000;
    engine.set_books(books, 1500, 10000);
    return engine.quote();
  };
  const auto coarse = quote_at(1, 10000, 10100, 1);
  const auto fine = quote_at(100, 1000000, 1010000, 100);
  assert(coarse.bid && fine.bid);
  assert(fine.bid_price == coarse.bid_price * 100);
  assert(fine.ask_price == coarse.ask_price * 100);

  std::cout << "tradefi engine tests passed\n";
  return 0;
}
