#include <array>
#include <cassert>
#include <cstddef>
#include <cstdint>
#include <iostream>
#include <span>
#include <string_view>
#include <vector>

#include "strategyframe/config.h"
#include "strategyframe/managers.h"

// StrategyContext intentionally has no public test constructor. Keep this
// access shim local to the test so production APIs remain unchanged.
#define private public
#include "strategyframe/strategy_context.h"
#undef private

#include "strategyframe/strategyframe.h"

#define private public
#include "tradefimm/strategy.h"
#undef private

namespace {

using strategyframe::Error;
using strategyframe::OrderToken;
using strategyframe::QueryComplete;
using strategyframe::QueryToken;
using strategyframe::Result;

struct FakeContextState {
  std::uint64_t now_ns{1'000'000};
  std::uint64_t next_sequence{100};
  std::size_t places{};
  std::size_t cancels{};
  std::size_t venue_cancels{};
  std::size_t open_order_queries{};
  std::size_t position_queries{};
  std::size_t balance_queries{};
  std::vector<strategyframe::OrderView> open_orders;
  strategyframe::StrategyParams params;
};

Result<OrderToken> place(void* raw,
                         const strategyframe::OrderRequest&) noexcept {
  auto& state = *static_cast<FakeContextState*>(raw);
  ++state.places;
  return {{0, 1, state.next_sequence++}, Error::Ok};
}

Result<OrderToken> cancel(void* raw, OrderToken) noexcept {
  auto& state = *static_cast<FakeContextState*>(raw);
  ++state.cancels;
  return {{0, 1, state.next_sequence++}, Error::Ok};
}

Result<OrderToken> cancel_venue_order(
    void* raw, strategyframe::AccountId, strategyframe::InstrumentId,
    const strategyframe::ClientOrderId&,
    const strategyframe::VenueOrderId&) noexcept {
  auto& state = *static_cast<FakeContextState*>(raw);
  ++state.venue_cancels;
  return {{0, 1, state.next_sequence++}, Error::Ok};
}

Result<QueryToken> query_open_orders(void* raw,
                                     strategyframe::AccountId) noexcept {
  auto& state = *static_cast<FakeContextState*>(raw);
  ++state.open_order_queries;
  return {{0, 1, state.next_sequence++}, Error::Ok};
}

Result<QueryToken> query_open_orders_for(
    void* raw, strategyframe::AccountId,
    strategyframe::InstrumentId) noexcept {
  return query_open_orders(raw, 0);
}

Result<QueryToken> query_positions(void* raw,
                                   strategyframe::AccountId) noexcept {
  auto& state = *static_cast<FakeContextState*>(raw);
  ++state.position_queries;
  return {{0, 1, state.next_sequence++}, Error::Ok};
}

Result<QueryToken> query_positions_for(
    void* raw, strategyframe::AccountId,
    strategyframe::InstrumentId) noexcept {
  return query_positions(raw, 0);
}

Result<QueryToken> query_balances(void* raw,
                                  strategyframe::AccountId) noexcept {
  auto& state = *static_cast<FakeContextState*>(raw);
  ++state.balance_queries;
  return {{0, 1, state.next_sequence++}, Error::Ok};
}

std::span<const strategyframe::OrderView> open_orders(
    const void* raw) noexcept {
  const auto& state = *static_cast<const FakeContextState*>(raw);
  return state.open_orders;
}

bool execution_ready(const void*, strategyframe::InstrumentId) noexcept {
  return true;
}

const strategyframe::StrategyParams& params(const void* raw) noexcept {
  return static_cast<const FakeContextState*>(raw)->params;
}

std::uint64_t now_ns(const void* raw) noexcept {
  return static_cast<const FakeContextState*>(raw)->now_ns;
}

strategyframe::StrategyContext make_context(FakeContextState& state) {
  static const strategyframe::StrategyContext::Ops ops = [] {
    strategyframe::StrategyContext::Ops value{};
    value.place = place;
    value.cancel = cancel;
    value.cancel_venue_order = cancel_venue_order;
    value.query_open_orders = query_open_orders;
    value.query_open_orders_for = query_open_orders_for;
    value.query_positions = query_positions;
    value.query_positions_for = query_positions_for;
    value.query_balances = query_balances;
    value.open_orders = open_orders;
    value.execution_ready = execution_ready;
    value.params = params;
    value.now_ns = now_ns;
    return value;
  }();
  return strategyframe::StrategyContext(&state, &ops);
}

tradefimm::PairSpec pair_spec(tradefimm::Venue maker) {
  tradefimm::PairSpec spec;
  spec.maker.venue = maker;
  spec.hedge.venue = maker == tradefimm::Venue::Hyperliquid
                         ? tradefimm::Venue::Ibkr
                         : tradefimm::Venue::Hyperliquid;
  spec.maker.multiplier = 1;
  spec.hedge.multiplier = 1;
  spec.maker.min_native = 1;
  spec.hedge.min_native = 1;
  spec.maker.lot = 1;
  spec.hedge.lot = 1;
  spec.maker.max_position = 100;
  spec.hedge.max_position = 100;
  spec.quote_size_base = 10;
  spec.tick = 1;
  spec.basis_buffer = 1;
  spec.target_profit_per_base = 1;
  spec.max_net_delta = 100;
  spec.max_notional = 1'000'000;
  spec.max_unhedged_notional = 1'000'000;
  spec.max_hedge_retries = 1;
  spec.ibkr_short_policy = tradefimm::IbkrShortPolicy::InventoryOnly;
  return spec;
}

tradefimm::Books books() {
  tradefimm::Books value;
  value.maker_bid = 100;
  value.maker_ask = 110;
  value.hedge_bid = 100;
  value.hedge_ask = 102;
  value.maker_recv_ns = 1'000;
  value.hedge_recv_ns = 1'000;
  return value;
}

void arm(tradefimm::Engine& engine, tradefimm::Venue maker) {
  assert(engine.start(pair_spec(maker)));
  engine.note_catalog();
  engine.note_oms(true, true);
  engine.note_reconciled(true);
  engine.set_books(books(), 1'500, 10'000);
  assert(engine.phase() == tradefimm::Phase::Quoting);
}

void test_role_inventory_matrix() {
  tradefimm::Engine hl_maker_empty;
  arm(hl_maker_empty, tradefimm::Venue::Hyperliquid);
  hl_maker_empty.set_ib_inventory(0);
  assert(!hl_maker_empty.quote().bid);
  assert(hl_maker_empty.quote().ask);

  tradefimm::Engine hl_maker_stocked;
  arm(hl_maker_stocked, tradefimm::Venue::Hyperliquid);
  hl_maker_stocked.set_ib_inventory(10);
  assert(hl_maker_stocked.quote().bid);
  assert(hl_maker_stocked.quote().ask);

  tradefimm::Engine ib_maker_empty;
  arm(ib_maker_empty, tradefimm::Venue::Ibkr);
  ib_maker_empty.set_ib_inventory(0);
  assert(ib_maker_empty.quote().bid);
  assert(!ib_maker_empty.quote().ask);

  tradefimm::Engine ib_maker_stocked;
  arm(ib_maker_stocked, tradefimm::Venue::Ibkr);
  ib_maker_stocked.set_ib_inventory(10);
  assert(ib_maker_stocked.quote().bid);
  assert(ib_maker_stocked.quote().ask);
}

void test_bid_ask_partial_fill_cycles() {
  for (const auto maker :
       {tradefimm::Venue::Hyperliquid, tradefimm::Venue::Ibkr}) {
    for (const std::int64_t maker_fill : {10, -10}) {
      tradefimm::Engine engine;
      arm(engine, maker);
      engine.set_ib_inventory(100);
      engine.on_maker_fill(maker_fill, 105);
      assert(engine.phase() == tradefimm::Phase::HedgePending);
      const auto plan = engine.plan();
      assert(plan.action == tradefimm::HedgeAction::Hedge);
      assert(plan.native_quantity == -maker_fill);

      const std::int64_t first = plan.native_quantity / 2;
      engine.on_hedge_fill(first, 105);
      assert(engine.net_delta() == maker_fill + first);
      assert(engine.plan().action == tradefimm::HedgeAction::Hedge);
      engine.on_hedge_fill(plan.native_quantity - first, 105);
      engine.note_hedge_terminal();
      assert(engine.net_delta() == 0);
      assert(engine.phase() == tradefimm::Phase::Quoting);
    }
  }
}

void complete_barrier(tradefimm::Recovery& recovery) {
  auto& barrier = recovery.barrier();
  barrier.maker_orders = true;
  barrier.maker_positions = true;
  barrier.hedge_orders = true;
  barrier.hedge_positions = true;
  barrier.fills_caught_up = true;
  barrier.orders_determined = true;
  barrier.delta_within_tolerance = true;
  barrier.maker_oms_ready = true;
  barrier.hedge_oms_ready = true;
  barrier.maker_bbo_fresh = true;
  barrier.hedge_bbo_fresh = true;
}

void test_recovery_fill_reject_timeout_and_disconnect() {
  using tradefimm::SlotId;
  using tradefimm::SlotState;

  tradefimm::Recovery fills;
  assert(fills.submit(SlotId::MakerBid, 1, 11, true, 100));
  fills.ack(SlotId::MakerBid, 1);
  assert(fills.fill(SlotId::MakerBid, 501, 4, 1));
  assert(fills.fill(SlotId::MakerBid, 502, 6, 1));
  assert(fills.slot(SlotId::MakerBid).filled == 10);
  assert(!fills.fill(SlotId::MakerBid, 502, 6, 1));
  assert(fills.slot(SlotId::MakerBid).filled == 10);

  assert(fills.submit(SlotId::Hedge, 1, 12, false, 100));
  assert(fills.fill(SlotId::Hedge, 601, 3, 1));
  assert(fills.fill(SlotId::Hedge, 602, 7, 1));
  assert(fills.slot(SlotId::Hedge).filled == 10);
  fills.timeout(SlotId::Hedge);
  assert(fills.slot(SlotId::Hedge).state == SlotState::CancelPending);
  fills.cancel_ack(SlotId::Hedge, 1);
  assert(fills.slot(SlotId::Hedge).state == SlotState::Terminal);

  tradefimm::Recovery rejects;
  rejects.maker_reject();
  rejects.maker_reject();
  assert(rejects.quoting_allowed());
  rejects.maker_reject();
  assert(rejects.makers_stopped());
  rejects.hedge_reject();
  assert(rejects.hedge_recovery());
  rejects.unwind_reject();
  assert(rejects.halted());

  tradefimm::Recovery maker_down;
  assert(maker_down.submit(SlotId::MakerAsk, 1, 21));
  maker_down.maker_venue_down();
  assert(maker_down.slot(SlotId::MakerAsk).state == SlotState::Uncertain);
  assert(!maker_down.quoting_allowed());

  tradefimm::Recovery hedge_down;
  assert(hedge_down.submit(SlotId::MakerBid, 1, 31));
  hedge_down.hedge_venue_down();
  assert(hedge_down.slot(SlotId::MakerBid).state ==
         SlotState::CancelPending);
  assert(hedge_down.makers_stopped());

  tradefimm::Engine disconnected;
  arm(disconnected, tradefimm::Venue::Hyperliquid);
  disconnected.note_disconnected();
  assert(disconnected.phase() == tradefimm::Phase::Reconnecting);
  assert(disconnected.makers_must_cancel());

  tradefimm::Recovery resume;
  complete_barrier(resume);
  resume.note_stable(true);
  assert(!resume.can_resume_quotes());
  resume.note_stable(false);
  assert(resume.can_resume_quotes());
}

void configure_leg(tradefimm::TradefiPerpsMm::ConfiguredLeg& leg,
                   tradefimm::Venue venue, strategyframe::AccountId account,
                   strategyframe::InstrumentId instrument,
                   std::uint64_t generation) {
  leg.venue = venue;
  leg.account = account;
  leg.instrument = instrument;
  leg.catalog_seen = true;
  leg.reconcile_generation = generation;
}

void test_reconcile_and_query_barriers_emit_no_quote() {
  FakeContextState state;
  auto context = make_context(state);
  tradefimm::TradefiPerpsMm reconcile_only;
  reconcile_only.context_ = &context;
  configure_leg(reconcile_only.maker_, tradefimm::Venue::Hyperliquid, 1, 101,
                0);
  configure_leg(reconcile_only.hedge_, tradefimm::Venue::Ibkr, 2, 202, 0);

  strategyframe::OmsStatusUpdate status;
  status.kind = strategyframe::OmsStatusUpdate::Kind::ReconcileComplete;
  status.adapter_kind = 5;
  status.generation = 7;
  reconcile_only.on_oms_status(status);
  assert(reconcile_only.maker_.adapter_reconciled);
  assert(!reconcile_only.maker_.orders_done);
  assert(!reconcile_only.maker_.positions_done);
  assert(state.open_order_queries == 1);
  assert(state.position_queries == 0);
  reconcile_only.maybe_quote();
  assert(state.places == 0);

  FakeContextState query_state;
  auto query_context = make_context(query_state);
  tradefimm::TradefiPerpsMm query_only;
  query_only.context_ = &query_context;
  configure_leg(query_only.maker_, tradefimm::Venue::Hyperliquid, 1, 101, 3);
  configure_leg(query_only.hedge_, tradefimm::Venue::Ibkr, 2, 202, 3);
  const std::array<QueryComplete::Kind, 2> kinds{
      QueryComplete::Kind::OpenOrders, QueryComplete::Kind::Positions};
  std::size_t pending_index = 0;
  for (const auto leg : {tradefimm::TradefiPerpsMm::LegId::Maker,
                         tradefimm::TradefiPerpsMm::LegId::Hedge}) {
    for (const auto kind : kinds) {
      const QueryToken token{0, 1, 300 + pending_index};
      query_only.pending_queries_[pending_index] = {
          token, leg, kind, 3, true, true};
      assert(query_only.find_pending_query(token) != nullptr);
      query_only.on_query_complete(
          QueryComplete{token, leg == tradefimm::TradefiPerpsMm::LegId::Maker
                                   ? 1U
                                   : 2U,
                        kind, 0, 0});
      ++pending_index;
    }
  }
  assert(query_only.maker_.orders_done &&
         query_only.maker_.positions_done);
  assert(query_only.hedge_.orders_done &&
         query_only.hedge_.positions_done);
  assert(!query_only.recovery_.barrier().fills_caught_up);
  query_only.maybe_quote();
  assert(query_state.places == 0);
}

void test_old_query_completion_is_ignored() {
  tradefimm::TradefiPerpsMm strategy;
  configure_leg(strategy.maker_, tradefimm::Venue::Hyperliquid, 1, 101, 9);
  const QueryToken token{0, 1, 401};
  strategy.pending_queries_[0] = {
      token, tradefimm::TradefiPerpsMm::LegId::Maker,
      QueryComplete::Kind::OpenOrders, 8, true, true};

  strategy.on_query_complete(
      QueryComplete{token, 1, QueryComplete::Kind::OpenOrders, 0, 0});
  assert(!strategy.maker_.orders_done);
  assert(!strategy.pending_queries_[0].active);
}

strategyframe::ClientOrderId client_order_id(std::string_view text) {
  strategyframe::ClientOrderId result;
  assert(text.size() <= sizeof(result.value));
  for (std::size_t index = 0; index < text.size(); ++index)
    result.value[index] = text[index];
  result.length = static_cast<std::uint16_t>(text.size());
  return result;
}

void test_cold_open_order_fails_closed() {
  tradefimm::TradefiPerpsMm strategy;
  strategy.strategy_instance_ = "alpha";
  configure_leg(strategy.maker_, tradefimm::Venue::Ibkr, 1, 101, 4);
  const QueryToken token{0, 1, 501};
  strategy.pending_queries_[0] = {
      token, tradefimm::TradefiPerpsMm::LegId::Maker,
      QueryComplete::Kind::OpenOrders, 4, true, true};
  assert(strategy.engine_.start(pair_spec(tradefimm::Venue::Ibkr)));
  strategy.engine_.note_catalog();
  strategy.engine_.note_oms(true, true);

  strategyframe::QueriedOrderView order;
  order.instrument_id = 101;
  order.client_order_id = client_order_id("tf-alpha-old-process-order");
  const std::array<strategyframe::QueriedOrderView, 1> orders{order};
  strategy.on_open_orders_snapshot({token, 1, orders});

  assert(strategy.unknown_order_);
  assert(strategy.engine_.phase() == tradefimm::Phase::Suspended);
  assert(!strategy.recovery_.barrier().orders_determined);
}

void test_owned_legacy_order_uses_venue_cancel() {
  FakeContextState state;
  auto context = make_context(state);
  tradefimm::TradefiPerpsMm strategy;
  strategy.context_ = &context;
  strategy.strategy_instance_ = "alpha";
  configure_leg(strategy.maker_, tradefimm::Venue::Ibkr, 1, 101, 4);
  strategy.maker_.adapter_reconciled = true;
  const QueryToken token{0, 1, 550};
  strategy.pending_queries_[0] = {
      token, tradefimm::TradefiPerpsMm::LegId::Maker,
      QueryComplete::Kind::OpenOrders, 4, true, true};

  strategyframe::QueriedOrderView order;
  order.instrument_id = 101;
  order.client_order_id = client_order_id("tf-alpha-old-process-order");
  order.snapshot_flags = strategyframe::SnapshotOwnedExternal;
  const std::array<strategyframe::QueriedOrderView, 1> orders{order};
  strategy.on_open_orders_snapshot({token, 1, orders});

  assert(state.venue_cancels == 1);
  assert(strategy.maker_.cleanup_token_count == 1);
  assert(!strategy.unknown_order_);
}

void test_shadow_emits_no_context_commands() {
  FakeContextState state;
  auto context = make_context(state);
  tradefimm::TradefiPerpsMm strategy(nullptr, true);
  strategy.context_ = &context;

  strategyframe::OrderRequest request;
  request.account_id = 1;
  request.instrument_id = 101;
  request.side = strategyframe::Side::Buy;
  request.quantity = {10, 0, {}};
  request.price = {100, 0, {}};
  assert(strategy.submit_order_or_shadow(
      request, tradefimm::SlotId::MakerBid,
      strategy.recovery_.maker_generation(), true));
  assert(state.places == 0);
  assert(strategy.recovery_.slot(tradefimm::SlotId::MakerBid).state ==
         tradefimm::SlotState::Empty);

  strategyframe::OrderView order;
  order.token = {0, 1, 77};
  strategy.cancel_order_or_shadow(order);
  assert(state.cancels == 0);

  tradefimm::TradefiPerpsMm::ConfiguredLeg leg;
  leg.account = 1;
  leg.instrument = 101;
  strategyframe::QueriedOrderView legacy;
  legacy.client_order_id = client_order_id("tf-alpha-old");
  legacy.snapshot_flags = strategyframe::SnapshotOwnedExternal;
  assert(!strategy.cancel_venue_order_or_shadow(leg, legacy));
  assert(state.venue_cancels == 0);
}

}  // namespace

int main() {
  test_role_inventory_matrix();
  test_bid_ask_partial_fill_cycles();
  test_recovery_fill_reject_timeout_and_disconnect();
  test_reconcile_and_query_barriers_emit_no_quote();
  test_old_query_completion_is_ignored();
  test_cold_open_order_fails_closed();
  test_owned_legacy_order_uses_venue_cancel();
  test_shadow_emits_no_context_commands();
  std::cout << "tradefi recovery strategy tests passed\n";
  return 0;
}
