#include "tradefimm/strategy.h"

#include <cstdio>
#include <cstring>
#include <iostream>
#include <limits>

namespace tradefimm {
namespace {

std::string text_of(const strategyframe::StrategyParams& params,
                    std::string_view key) {
  const auto value = params.require_string(key);
  return value ? std::string(value.value) : std::string{};
}

Venue parse_venue(std::string_view text) {
  if (text == "hyperliquid") return Venue::Hyperliquid;
  if (text == "ibkr") return Venue::Ibkr;
  return Venue::Hyperliquid;
}

strategyframe::ProductType parse_product(std::string_view text) {
  using strategyframe::ProductType;
  if (text == "perpetual") return ProductType::Perpetual;
  if (text == "equity" || text == "stock") return ProductType::Equity;
  if (text == "commodity") return ProductType::Future;
  if (text == "cash" || text == "fx") return ProductType::Spot;
  return ProductType::Unknown;
}

bool stable_quote(std::string_view quote) {
  return quote == "USD" || quote == "USDT" || quote == "USDC";
}

template <std::size_t Capacity>
std::string bounded_text(
    const std::array<char, Capacity>& value) {
  std::size_t length = 0;
  while (length < value.size() && value[length] != '\0') ++length;
  return {value.data(), length};
}

bool read_decimal(const strategyframe::StrategyParams& params,
                  std::string_view key, std::string& output,
                  std::string_view fallback) {
  const auto value = params.require_string(key);
  if (value) {
    output.assign(value.value);
    return true;
  }
  if (value.error == strategyframe::Error::NotFound) {
    output.assign(fallback);
    return true;
  }
  return false;
}

bool valid_strategy_instance(std::string_view value) noexcept {
  if (value.empty() || value.size() > 24) return false;
  for (const char character : value) {
    const bool alpha =
        (character >= 'a' && character <= 'z') ||
        (character >= 'A' && character <= 'Z');
    const bool digit = character >= '0' && character <= '9';
    if (!alpha && !digit && character != '-' && character != '_')
      return false;
  }
  return true;
}

std::string_view client_id_view(
    const strategyframe::ClientOrderId& value) noexcept {
  if (value.length > sizeof(value.value)) return {};
  return {value.value, value.length};
}

std::uint32_t stable_strategy_hash(std::string_view text) noexcept {
  std::uint32_t hash = 2166136261U;
  for (const char raw : text) {
    hash ^= static_cast<unsigned char>(raw);
    hash *= 16777619U;
  }
  return hash;
}

bool hyperliquid_cloid_owned(std::string_view text,
                             std::string_view strategy_instance) noexcept {
  if (text.size() != 34 || text[0] != '0' ||
      (text[1] != 'x' && text[1] != 'X'))
    return false;
  const auto nibble = [](char value, std::uint8_t& output) noexcept {
    if (value >= '0' && value <= '9')
      output = static_cast<std::uint8_t>(value - '0');
    else if (value >= 'a' && value <= 'f')
      output = static_cast<std::uint8_t>(value - 'a' + 10);
    else if (value >= 'A' && value <= 'F')
      output = static_cast<std::uint8_t>(value - 'A' + 10);
    else
      return false;
    return true;
  };
  std::array<std::uint8_t, 7> bytes{};
  for (std::size_t index = 0; index < bytes.size(); ++index) {
    std::uint8_t high = 0;
    std::uint8_t low = 0;
    if (!nibble(text[2 + index * 2], high) ||
        !nibble(text[3 + index * 2], low))
      return false;
    bytes[index] = static_cast<std::uint8_t>((high << 4U) | low);
  }
  if (bytes[0] != 0x53 || bytes[1] != 0x51 || bytes[2] != 1) return false;
  std::uint32_t owner = 0;
  for (std::size_t index = 3; index < 7; ++index)
    owner = (owner << 8U) | bytes[index];
  return owner == stable_strategy_hash(strategy_instance);
}

}  // namespace

TradefiPerpsMm::TradefiPerpsMm(int* exit_code, bool shadow) noexcept
    : exit_code_(exit_code), shadow_(shadow) {}

void TradefiPerpsMm::fail(const char* reason) noexcept {
  std::cerr << "tradefi-perps-mm " << reason << '\n';
  if (exit_code_ != nullptr) *exit_code_ = 1;
  if (context_ != nullptr) context_->request_stop();
}

bool TradefiPerpsMm::load(const strategyframe::StrategyParams& params) {
  strategy_instance_ = text_of(params, "strategy_instance");
  if (!valid_strategy_instance(strategy_instance_) ||
      params.contains("allow_short")) {
    invalid_config_ = true;
    return false;
  }
  const std::string short_policy = text_of(params, "ibkr_short_policy");
  if (short_policy == "inventory_only") {
    spec_.ibkr_short_policy = IbkrShortPolicy::InventoryOnly;
  } else if (short_policy == "allow") {
    spec_.ibkr_short_policy = IbkrShortPolicy::Allow;
  } else {
    invalid_config_ = true;
    return false;
  }
  maker_.venue = parse_venue(text_of(params, "maker.venue"));
  hedge_.venue = parse_venue(text_of(params, "hedge.venue"));
  const auto maker_account = params.require_int("maker.account_id");
  const auto hedge_account = params.require_int("hedge.account_id");
  if (!maker_account || !hedge_account) return false;
  maker_.account = static_cast<strategyframe::AccountId>(maker_account.value);
  hedge_.account = static_cast<strategyframe::AccountId>(hedge_account.value);
  maker_.symbol = text_of(params, "maker.instrument.symbol");
  hedge_.symbol = text_of(params, "hedge.instrument.symbol");
  const std::string maker_product = text_of(params, "maker.instrument.product");
  const std::string hedge_product = text_of(params, "hedge.instrument.product");
  maker_.product = parse_product(maker_product);
  hedge_.product = parse_product(hedge_product);
  maker_.security = text_of(params, "maker.instrument.security");
  hedge_.security = text_of(params, "hedge.instrument.security");
  if (maker_.venue == Venue::Ibkr && maker_.security.empty())
    maker_.security = "STK";
  if (hedge_.venue == Venue::Ibkr && hedge_.security.empty())
    hedge_.security = "STK";
  const auto valid_security = [](const ConfiguredLeg& leg) {
    return leg.venue != Venue::Ibkr || leg.security == "STK" ||
           leg.security == "CMDTY" || leg.security == "CASH";
  };
  if (!valid_security(maker_) || !valid_security(hedge_)) return false;
  maker_.strict = text_of(params, "maker.passive_mode") == "strict";
  if (maker_.symbol.empty() || hedge_.symbol.empty()) return false;
  if (maker_.venue == hedge_.venue) return false;
  const bool one_hl = (maker_.venue == Venue::Hyperliquid) !=
                      (hedge_.venue == Venue::Hyperliquid);
  const bool one_ib =
      (maker_.venue == Venue::Ibkr) != (hedge_.venue == Venue::Ibkr);
  if (!one_hl || !one_ib) return false;
  if (params.contains("maker.multiplier") || params.contains("maker.min_native") ||
      params.contains("maker.lot") || params.contains("hedge.multiplier") ||
      params.contains("hedge.min_native") || params.contains("hedge.lot") ||
      params.contains("tick") || params.contains("max_notional")) {
    invalid_config_ = true;
    return false;
  }
  spec_.maker.venue = maker_.venue;
  spec_.hedge.venue = hedge_.venue;
  if (!read_decimal(params, "basis_offset", decimals_.basis_offset, "0.00") ||
      !read_decimal(params, "basis_buffer", decimals_.basis_buffer, "0.01") ||
      !read_decimal(params, "slippage_per_base", decimals_.slippage_per_base,
                    "0.01") ||
      !read_decimal(params, "target_profit_per_base",
                    decimals_.target_profit_per_base, "0.01") ||
      !read_decimal(params, "quote_size_base", decimals_.quote_size_base, "1") ||
      !read_decimal(params, "max_maker_position", decimals_.max_maker_position,
                    "100") ||
      !read_decimal(params, "max_hedge_position", decimals_.max_hedge_position,
                    "100") ||
      !read_decimal(params, "max_net_delta", decimals_.max_net_delta, "100") ||
      !read_decimal(params, "delta_tolerance", decimals_.delta_tolerance, "0") ||
      !read_decimal(params, "max_notional_usd", decimals_.max_notional_usd,
                    "10000.00") ||
      !read_decimal(params, "max_unhedged_notional",
                    decimals_.max_unhedged_notional, "100.00")) {
    invalid_config_ = true;
    return false;
  }
  if (params.contains("maker_tick_multiplier")) {
    const auto ticks = params.require_int("maker_tick_multiplier");
    if (!ticks || ticks.value <= 0) {
      invalid_config_ = true;
      return false;
    }
    decimals_.maker_tick_multiplier = static_cast<int>(ticks.value);
  }
  spec_.max_hedge_retries = static_cast<int>(params.optional_int("max_hedge_retries", 1));
  spec_.max_hedge_slippage_ticks = static_cast<int>(
      params.optional_int("max_hedge_slippage_ticks", 5));
  stop_after_hedge_cycles_ =
      static_cast<int>(params.optional_int("stop_after_hedge_cycles", 0));
  max_active_maker_orders_ =
      static_cast<int>(params.optional_int("max_active_maker_orders", 1));
  if (spec_.max_hedge_retries < 0 || spec_.max_hedge_slippage_ticks < 0 ||
      stop_after_hedge_cycles_ < 0 ||
      (max_active_maker_orders_ != 1 && max_active_maker_orders_ != 2)) {
    invalid_config_ = true;
    return false;
  }
  spec_.same_base = true;
  spec_.same_quote = true;
  spec_.fees.hl_maker_bps = params.optional_int("hl_maker_bps", 1);
  spec_.fees.hl_taker_bps = params.optional_int("hl_taker_bps", 3);
  spec_.fees.ib_buy_bps = params.optional_int("ib_buy_bps", 1);
  spec_.fees.ib_sell_bps = params.optional_int("ib_sell_bps", 1);
  spec_.fees.ib_passive_bps = params.optional_int("ib_passive_bps", 1);
  spec_.fees.ib_taker_bps = params.optional_int("ib_taker_bps", 2);
  max_bbo_age_ns_ = static_cast<std::uint64_t>(
      params.optional_int("max_bbo_age_ms", 1000)) * 1000000ULL;
  max_hedge_latency_ns_ = static_cast<std::uint64_t>(
      params.optional_int("hedge_timeout_ms", 2000)) * 1000000ULL;
  return true;
}

bool TradefiPerpsMm::price_at(const strategyframe::FixedPoint& price,
                              std::int64_t& out) const noexcept {
  return rescale_exact(price.value, price.scale, price_calc_, out);
}

bool TradefiPerpsMm::native_order(const ConfiguredLeg& leg, std::int64_t base_qty,
                                  std::int64_t calc_price, std::int64_t& quantity,
                                  std::int64_t& order_price) const noexcept {
  if (leg.multiplier <= 0 || leg.multiplier_scale > 18) return false;
  std::int64_t factor = 1;
  for (std::uint8_t index = 0; index < leg.multiplier_scale; ++index) {
    if (factor > std::numeric_limits<std::int64_t>::max() / 10) return false;
    factor *= 10;
  }
  if (base_qty != 0 &&
      factor > std::numeric_limits<std::int64_t>::max() / (base_qty < 0 ? -base_qty : base_qty))
    return false;
  const std::int64_t numerator = base_qty * factor;
  if (numerator % leg.multiplier != 0) return false;
  const std::int64_t at_calc = numerator / leg.multiplier;
  if (!rescale_exact(at_calc, quantity_calc_, leg.quantity_scale, quantity))
    return false;
  if (!rescale_exact(calc_price, price_calc_, leg.price_scale, order_price))
    return false;
  if (quantity <= 0 || order_price <= 0) return false;
  if (leg.lot > 0 && quantity % leg.lot != 0) return false;
  if (leg.tick > 0 && order_price % leg.tick != 0) return false;
  return true;
}

bool TradefiPerpsMm::apply_catalogs() noexcept {
  price_calc_ = maker_.price_scale > hedge_.price_scale ? maker_.price_scale
                                                        : hedge_.price_scale;
  quantity_calc_ = maker_.quantity_scale > hedge_.quantity_scale
                       ? maker_.quantity_scale
                       : hedge_.quantity_scale;
  if (price_calc_ > 18 || quantity_calc_ > 18) return false;
  const std::uint8_t notional_scale = static_cast<std::uint8_t>(
      price_calc_ + quantity_calc_);
  if (notional_scale > 18) return false;
  auto parse_at = [](const std::string& text, std::uint8_t scale,
                     std::int64_t& output) {
    return parse_decimal(text, scale, output);
  };
  if (!parse_at(decimals_.basis_offset, price_calc_, spec_.basis_offset) ||
      !parse_at(decimals_.basis_buffer, price_calc_, spec_.basis_buffer) ||
      !parse_at(decimals_.slippage_per_base, price_calc_,
                spec_.slippage_per_base) ||
      !parse_at(decimals_.target_profit_per_base, price_calc_,
                spec_.target_profit_per_base) ||
      !parse_at(decimals_.quote_size_base, quantity_calc_,
                spec_.quote_size_base) ||
      !parse_at(decimals_.max_maker_position, quantity_calc_,
                spec_.maker.max_position) ||
      !parse_at(decimals_.max_hedge_position, quantity_calc_,
                spec_.hedge.max_position) ||
      !parse_at(decimals_.max_net_delta, quantity_calc_, spec_.max_net_delta) ||
      !parse_at(decimals_.delta_tolerance, quantity_calc_,
                spec_.delta_tolerance) ||
      !parse_at(decimals_.max_notional_usd, notional_scale,
                spec_.max_notional) ||
      !parse_at(decimals_.max_unhedged_notional, notional_scale,
                spec_.max_unhedged_notional))
    return false;
  std::int64_t tick = 0;
  if (!rescale_exact(maker_.tick, maker_.price_scale, price_calc_, tick))
    return false;
  if (decimals_.maker_tick_multiplier > 1 &&
      tick > std::numeric_limits<std::int64_t>::max() /
                 decimals_.maker_tick_multiplier)
    return false;
  spec_.tick = tick * decimals_.maker_tick_multiplier;
  auto lift = [&](ConfiguredLeg& leg, LegSpec& spec) {
    spec.multiplier = leg.multiplier;
    spec.multiplier_scale = leg.multiplier_scale;
    if (!rescale_exact(leg.lot, leg.quantity_scale, quantity_calc_, spec.lot) ||
        !rescale_exact(leg.min_native, leg.quantity_scale, quantity_calc_,
                       spec.min_native))
      return false;
    return spec.lot > 0 && spec.min_native > 0;
  };
  spec_.maker.venue = maker_.venue;
  spec_.hedge.venue = hedge_.venue;
  return lift(maker_, spec_.maker) && lift(hedge_, spec_.hedge);
}

void TradefiPerpsMm::init(strategyframe::StrategyContext& context) {
  context_ = &context;
  process_epoch_ = context.now_ns();
  if (!load(context.params())) {
    fail(invalid_config_ ? "invalid config" : "invalid maker/hedge configuration");
    return;
  }
  const auto timer = context.schedule_timer(context.now_ns() + 1000000000ULL, 1000000000ULL);
  if (!timer) fail("timer schedule failed");
}

std::string_view TradefiPerpsMm::next_client_order_id(
    std::string_view role) noexcept {
  (void)role;
  const int written = std::snprintf(
      cloid_.data(), cloid_.size(), "tf-%s-%llx-%llu",
      strategy_instance_.c_str(),
      static_cast<unsigned long long>(process_epoch_),
      static_cast<unsigned long long>(cloid_sequence_++));
  if (written <= 0 || static_cast<std::size_t>(written) >= cloid_.size())
    return {};
  return {cloid_.data(), static_cast<std::size_t>(written)};
}

bool TradefiPerpsMm::submit_order_or_shadow(
    strategyframe::OrderRequest& request, SlotId slot,
    std::uint64_t generation, bool buy) noexcept {
  if (context_ == nullptr ||
      !recovery_.reserve(slot, generation, buy, context_->now_ns()))
    return false;
  if (shadow_) {
    std::cerr << "tradefi shadow would_place account=" << request.account_id
              << " instrument=" << request.instrument_id
              << " side=" << (buy ? "buy" : "sell")
              << " quantity=" << request.quantity.value
              << " price=" << request.price.value << '\n';
    recovery_.rollback_reservation(slot, generation);
    return true;
  }
  const auto token = context_->place_order(request);
  if (!token ||
      !recovery_.bind_submission(slot, generation, token.value.sequence)) {
    recovery_.rollback_reservation(slot, generation);
    return false;
  }
  return true;
}

void TradefiPerpsMm::cancel_order_or_shadow(
    const strategyframe::OrderView& order) noexcept {
  if (context_ == nullptr) return;
  if (shadow_) {
    std::cerr << "tradefi shadow would_cancel token="
              << order.token.sequence << '\n';
    return;
  }
  (void)context_->cancel(order.token);
}

bool TradefiPerpsMm::cancel_venue_order_or_shadow(
    ConfiguredLeg& value,
    const strategyframe::QueriedOrderView& order) noexcept {
  if (context_ == nullptr) return false;
  const std::string_view client_id = client_id_view(order.client_order_id);
  if (shadow_) {
    std::cerr << "tradefi shadow would_cancel legacy client_order_id="
              << client_id << '\n';
    return false;
  }
  if (value.cleanup_token_count >= value.cleanup_tokens.size() ||
      value.cleanup_attempts >= 3)
    return false;
  const auto canceled = context_->cancel_venue_order(
      value.account, value.instrument, order.client_order_id,
      order.venue_order_id);
  if (!canceled) return false;
  value.cleanup_tokens[value.cleanup_token_count++] = canceled.value;
  ++value.cleanup_attempts;
  value.cleanup_needed = true;
  return true;
}

void TradefiPerpsMm::submit_deferred_cleanup(
    ConfiguredLeg& value) noexcept {
  if (!value.adapter_reconciled || value.cleanup_order_count == 0) return;
  const std::size_t count = value.cleanup_order_count;
  value.cleanup_order_count = 0;
  for (std::size_t index = 0; index < count; ++index) {
    if (!cancel_venue_order_or_shadow(
            value, value.cleanup_orders[index])) {
      value.unknown_order = true;
      unknown_order_ = true;
      recovery_.halt();
      engine_.note_halted();
      return;
    }
  }
}

TradefiPerpsMm::ConfiguredLeg& TradefiPerpsMm::leg(LegId id) noexcept {
  return id == LegId::Maker ? maker_ : hedge_;
}

const TradefiPerpsMm::ConfiguredLeg& TradefiPerpsMm::leg(
    LegId id) const noexcept {
  return id == LegId::Maker ? maker_ : hedge_;
}

std::uint8_t TradefiPerpsMm::adapter_kind(Venue venue) noexcept {
  return venue == Venue::Hyperliquid ? 5U : 6U;
}

TradefiPerpsMm::PendingQuery* TradefiPerpsMm::find_pending_query(
    strategyframe::QueryToken token) noexcept {
  for (auto& pending : pending_queries_) {
    if (pending.occupied && pending.token == token) return &pending;
  }
  return nullptr;
}

const TradefiPerpsMm::PendingQuery* TradefiPerpsMm::find_pending_query(
    strategyframe::QueryToken token) const noexcept {
  for (const auto& pending : pending_queries_) {
    if (pending.occupied && pending.token == token) return &pending;
  }
  return nullptr;
}

bool TradefiPerpsMm::remember_query(
    strategyframe::QueryToken token, LegId leg_id,
    strategyframe::QueryComplete::Kind kind,
    std::uint64_t generation) noexcept {
  for (auto& pending : pending_queries_) {
    if (!pending.occupied) {
      pending = PendingQuery{token, leg_id, kind, generation, true, true};
      return true;
    }
  }
  return false;
}

bool TradefiPerpsMm::has_active_query(
    LegId leg_id, std::uint64_t generation) const noexcept {
  for (const auto& pending : pending_queries_) {
    if (pending.occupied && pending.active && pending.leg == leg_id &&
        pending.generation == generation)
      return true;
  }
  return false;
}

void TradefiPerpsMm::begin_snapshot_queries(LegId leg_id) noexcept {
  if (context_ == nullptr) return;
  ConfiguredLeg& value = leg(leg_id);
  if (!value.catalog_seen) return;
  value.orders_done = false;
  value.positions_done = value.cash;
  value.balances_done = !value.cash;
  value.balance_seen = false;
  value.unknown_order = false;
  value.query_seen_token_count = 0;
  if (value.cleanup_token_count == 0) {
    value.cleanup_needed = false;
    value.cleanup_requery_ns = 0;
  }
  const auto orders =
      context_->query_open_orders(value.account, value.instrument);
  if (!orders ||
      !remember_query(orders.value, leg_id,
                      strategyframe::QueryComplete::Kind::OpenOrders,
                      value.reconcile_generation)) {
    recovery_.halt();
    engine_.note_halted();
  }
}

void TradefiPerpsMm::update_reconcile_barrier() noexcept {
  auto& barrier = recovery_.barrier();
  barrier.maker_orders = maker_.orders_done;
  barrier.hedge_orders = hedge_.orders_done;
  barrier.maker_positions = maker_.positions_done && maker_.balances_done;
  barrier.hedge_positions = hedge_.positions_done && hedge_.balances_done;
  barrier.fills_caught_up =
      maker_.adapter_reconciled && hedge_.adapter_reconciled;
  unknown_order_ = maker_.unknown_order || hedge_.unknown_order;
  barrier.orders_determined = !unknown_order_;
  if (maker_.orders_done && hedge_.orders_done &&
      maker_.positions_done && hedge_.positions_done &&
      maker_.balances_done && hedge_.balances_done &&
      maker_.adapter_reconciled && hedge_.adapter_reconciled) {
    engine_.set_positions(maker_.position, hedge_.position);
    if (maker_.venue == Venue::Ibkr && maker_.cash)
      engine_.set_ib_inventory(maker_.available_balance);
    else if (hedge_.venue == Venue::Ibkr && hedge_.cash)
      engine_.set_ib_inventory(hedge_.available_balance);
    const std::int64_t delta = engine_.net_delta();
    const std::int64_t magnitude = delta < 0 ? -delta : delta;
    if (!cold_start_reconciled_ && magnitude > spec_.delta_tolerance) {
      std::cerr << "tradefi cold-start exposure requires manual recovery "
                << "net_delta=" << delta << '\n';
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    cold_start_reconciled_ = true;
    engine_.note_reconciled(!unknown_order_);
    if (!unknown_order_ && magnitude > spec_.delta_tolerance)
      drive_recovery();
  }
}

TradefiPerpsMm::ConfiguredLeg* TradefiPerpsMm::leg_for(
    strategyframe::InstrumentId id) noexcept {
  if (maker_.instrument == id) return &maker_;
  if (hedge_.instrument == id) return &hedge_;
  return nullptr;
}

bool TradefiPerpsMm::slot_for_token(std::uint64_t token,
                                    SlotId& slot) const noexcept {
  for (SlotId candidate : {SlotId::MakerBid, SlotId::MakerAsk, SlotId::Hedge,
                           SlotId::Unwind}) {
    const Slot& item = recovery_.slot(candidate);
    if (item.token == token && item.state != SlotState::Empty) {
      slot = candidate;
      return true;
    }
  }
  return false;
}

void TradefiPerpsMm::on_instrument_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog) {
  if (context_ == nullptr) return;
  const std::string symbol(catalog.canonical_symbol.value,
                           catalog.canonical_symbol.length);
  ConfiguredLeg* matched_leg = nullptr;
  if (!maker_.catalog_seen && symbol == maker_.symbol &&
      catalog.venue == (maker_.venue == Venue::Hyperliquid
                            ? strategyframe::Venue::Hyperliquid
                            : strategyframe::Venue::Ibkr))
    matched_leg = &maker_;
  else if (!hedge_.catalog_seen && symbol == hedge_.symbol &&
           catalog.venue == (hedge_.venue == Venue::Hyperliquid
                                 ? strategyframe::Venue::Hyperliquid
                                 : strategyframe::Venue::Ibkr))
    matched_leg = &hedge_;
  if (matched_leg == nullptr) return;
  if (matched_leg->venue == Venue::Ibkr) {
    using utils::md::IbkrSecurityType;
    const IbkrSecurityType expected =
        matched_leg->security == "CASH"
            ? IbkrSecurityType::Cash
            : (matched_leg->security == "CMDTY"
                   ? IbkrSecurityType::Commodity
                   : IbkrSecurityType::Stock);
    if (catalog.ibkr_security_type != expected) {
      fail("IB security type does not match strategy configuration");
      return;
    }
    matched_leg->cash = expected == IbkrSecurityType::Cash;
  }
  matched_leg->catalog_seen = true;
  matched_leg->instrument = catalog.instrument_id;
  matched_leg->base = bounded_text(catalog.base_asset);
  matched_leg->quote = bounded_text(catalog.quote_asset);
  matched_leg->price_scale = catalog.price_scale;
  matched_leg->quantity_scale = catalog.quantity_scale;
  matched_leg->tick = catalog.tick_size > 0 ? catalog.tick_size : 1;
  matched_leg->lot = catalog.lot_size > 0 ? catalog.lot_size : 1;
  matched_leg->min_native = matched_leg->lot;
  matched_leg->multiplier =
      catalog.contract_multiplier > 0 ? catalog.contract_multiplier : 1;
  matched_leg->multiplier_scale = catalog.contract_multiplier_scale;
  if (!maker_.catalog_seen || !hedge_.catalog_seen) return;
  if (maker_.base != hedge_.base || !stable_quote(maker_.quote) ||
      !stable_quote(hedge_.quote)) {
    fail("legs are not the same underlying and quote");
    return;
  }
  if (!apply_catalogs() || !engine_.start(spec_)) {
    fail("engine rejected catalog market rules");
    return;
  }
  engine_.note_catalog();
  engine_.note_oms(true, true);
  for (LegId leg_id : {LegId::Maker, LegId::Hedge}) {
    ConfiguredLeg& value = leg(leg_id);
    const auto status = context_->oms_status(adapter_kind(value.venue));
    if (status) {
      value.reconcile_generation = status.value.generation;
      if (status.value.kind ==
          strategyframe::OmsStatusUpdate::Kind::ReconcileComplete) {
        on_oms_status(status.value);
        continue;
      }
    }
    begin_snapshot_queries(leg_id);
  }
}

void TradefiPerpsMm::on_open_orders_snapshot(
    const strategyframe::OpenOrdersSnapshot& snapshot) {
  PendingQuery* pending = find_pending_query(snapshot.token);
  if (pending == nullptr || !pending->active ||
      pending->kind != strategyframe::QueryComplete::Kind::OpenOrders)
    return;
  ConfiguredLeg& value = leg(pending->leg);
  if (pending->generation != value.reconcile_generation) return;
  for (const auto& order : snapshot.orders) {
    if (order.instrument_id != value.instrument) continue;
    if ((order.snapshot_flags &
         static_cast<std::uint32_t>(strategyframe::SnapshotTracked)) != 0) {
      SlotId slot = SlotId::MakerBid;
      if (!slot_for_token(order.tracked_token.sequence, slot) ||
          value.query_seen_token_count >= value.query_seen_tokens.size()) {
        value.unknown_order = true;
        unknown_order_ = true;
        recovery_.halt();
        engine_.note_halted();
        continue;
      }
      value.query_seen_tokens[value.query_seen_token_count++] =
          order.tracked_token.sequence;
      if (recovery_.slot(slot).state == SlotState::Uncertain) {
        recovery_.query_open(slot);
        for (const auto& live : context_->open_orders()) {
          if (live.token == order.tracked_token)
            cancel_order_or_shadow(live);
        }
      }
      continue;
    }
    const std::string_view client_id = client_id_view(order.client_order_id);
    const bool ours =
        value.venue == Venue::Hyperliquid
            ? hyperliquid_cloid_owned(client_id, strategy_instance_)
            : client_id.starts_with("tf-" + strategy_instance_ + "-");
    const bool adapter_owned =
        (order.snapshot_flags &
         static_cast<std::uint32_t>(
             strategyframe::SnapshotOwnedExternal)) != 0;
    if (ours && adapter_owned) {
      value.cleanup_needed = true;
      if (!value.adapter_reconciled) {
        if (value.cleanup_order_count < value.cleanup_orders.size()) {
          value.cleanup_orders[value.cleanup_order_count++] = order;
          continue;
        }
      } else if (cancel_venue_order_or_shadow(value, order)) {
        continue;
      }
    }
    value.unknown_order = true;
    unknown_order_ = true;
    if (!ours || !adapter_owned) {
      std::cerr << "tradefi orphan order left untouched client_order_id="
                << client_id << '\n';
    } else if (shadow_) {
      std::cerr << "tradefi shadow keeps legacy order unresolved\n";
    } else {
      std::cerr << "tradefi legacy cancel submission failed client_order_id="
                << client_id << '\n';
    }
  }
  if (unknown_order_) {
    engine_.note_suspended();
    std::cerr << "tradefi unknown resting order; positions stay in the risk view\n";
  }
}

void TradefiPerpsMm::on_positions_snapshot(
    const strategyframe::PositionsSnapshot& snapshot) {
  PendingQuery* pending = find_pending_query(snapshot.token);
  if (pending == nullptr || !pending->active ||
      pending->kind != strategyframe::QueryComplete::Kind::Positions)
    return;
  ConfiguredLeg& value = leg(pending->leg);
  if (pending->generation != value.reconcile_generation) return;
  value.position = 0;
  for (const auto& position : snapshot.positions) {
    if (position.account_id == value.account &&
        position.instrument_id == value.instrument) {
      std::int64_t quantity = 0;
      if (!rescale_exact(position.quantity.value, position.quantity.scale,
                         quantity_calc_, quantity)) {
        fail("position scale");
        return;
      }
      value.position = quantity;
    }
  }
}

void TradefiPerpsMm::on_balances_snapshot(
    const strategyframe::BalanceSnapshot& snapshot) {
  PendingQuery* pending = find_pending_query(snapshot.query_token);
  if (pending == nullptr || !pending->active ||
      pending->kind != strategyframe::QueryComplete::Kind::Balances)
    return;
  ConfiguredLeg& value = leg(pending->leg);
  if (!value.cash ||
      pending->generation != value.reconcile_generation ||
      snapshot.account_id != value.account)
    return;
  std::size_t length = 0;
  while (length < snapshot.asset_or_currency.size() &&
         snapshot.asset_or_currency[length] != '\0')
    ++length;
  const std::string_view asset(snapshot.asset_or_currency.data(), length);
  if (asset != value.base) return;
  std::int64_t available = 0;
  if (!rescale_exact(snapshot.available_quantity, snapshot.scale,
                     quantity_calc_, available)) {
    fail("balance scale");
    return;
  }
  value.available_balance = available;
  value.balance_seen = true;
}

void TradefiPerpsMm::on_query_complete(const strategyframe::QueryComplete& complete) {
  PendingQuery* pending = find_pending_query(complete.token);
  if (pending == nullptr) {
    std::cerr << "tradefi unknown query completion token="
              << complete.token.sequence << '\n';
    return;
  }
  ConfiguredLeg& value = leg(pending->leg);
  if (!pending->active ||
      pending->generation != value.reconcile_generation ||
      pending->kind != complete.kind) {
    pending->active = false;
    return;
  }
  pending->active = false;
  if (complete.error != 0) {
    recovery_.halt();
    engine_.note_halted();
    return;
  }
  if (complete.kind == strategyframe::QueryComplete::Kind::OpenOrders) {
    const auto seen = [&](std::uint64_t token) {
      for (std::size_t index = 0;
           index < value.query_seen_token_count; ++index) {
        if (value.query_seen_tokens[index] == token) return true;
      }
      return false;
    };
    const auto resolve_absent = [&](SlotId slot) {
      const Slot& item = recovery_.slot(slot);
      if (item.state != SlotState::Uncertain || seen(item.token)) return;
      if (item.seen_fill)
        recovery_.query_filled(slot);
      else
        (void)recovery_.query_absent(slot);
    };
    if (pending->leg == LegId::Maker) {
      resolve_absent(SlotId::MakerBid);
      resolve_absent(SlotId::MakerAsk);
      resolve_absent(SlotId::Unwind);
    } else {
      resolve_absent(SlotId::Hedge);
    }
    const bool cleanup_waiting = value.cleanup_needed;
    value.orders_done = !cleanup_waiting;
    if (cleanup_waiting) {
      if (value.adapter_reconciled &&
          value.cleanup_token_count == 0 &&
          value.cleanup_order_count == 0 &&
          context_ != nullptr)
        value.cleanup_requery_ns =
            context_->now_ns() + 100'000'000ULL;
    } else {
      value.cleanup_attempts = 0;
    }
    if (value.cash) {
      const auto balances = context_->query_balances(value.account);
      if (!balances ||
          !remember_query(balances.value, pending->leg,
                          strategyframe::QueryComplete::Kind::Balances,
                          value.reconcile_generation)) {
        recovery_.halt();
        engine_.note_halted();
        return;
      }
    } else {
      const auto positions =
          context_->query_positions(value.account, value.instrument);
      if (!positions ||
          !remember_query(positions.value, pending->leg,
                          strategyframe::QueryComplete::Kind::Positions,
                          value.reconcile_generation)) {
        recovery_.halt();
        engine_.note_halted();
        return;
      }
    }
  } else if (complete.kind ==
             strategyframe::QueryComplete::Kind::Positions) {
    value.positions_done = true;
  } else if (complete.kind ==
             strategyframe::QueryComplete::Kind::Balances) {
    if (!value.cash || !value.balance_seen) {
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    value.balances_done = true;
  }
  update_reconcile_barrier();
}

void TradefiPerpsMm::refresh_books() noexcept {
  if (context_ == nullptr) return;
  Books books;
  if (!price_at(maker_.bid, books.maker_bid) ||
      !price_at(maker_.ask, books.maker_ask) ||
      !price_at(hedge_.bid, books.hedge_bid) ||
      !price_at(hedge_.ask, books.hedge_ask))
    return;
  books.maker_recv_ns = maker_.recv_ns;
  books.hedge_recv_ns = hedge_.recv_ns;
  books.maker_mds = maker_.recv_ns != 0;
  books.hedge_mds = hedge_.recv_ns != 0;
  books.maker_execution_ready =
      maker_.instrument != 0 && context_->execution_ready(maker_.instrument);
  books.hedge_execution_ready =
      hedge_.instrument != 0 && context_->execution_ready(hedge_.instrument);
  engine_.set_books(books, context_->now_ns(), max_bbo_age_ns_);
  if (engine_.fresh()) engine_.note_baseline();
  recovery_.barrier().maker_bbo_fresh = maker_.recv_ns != 0;
  recovery_.barrier().hedge_bbo_fresh = hedge_.recv_ns != 0;
  recovery_.barrier().maker_oms_ready = books.maker_execution_ready;
  recovery_.barrier().hedge_oms_ready = books.hedge_execution_ready;
  const auto magnitude = engine_.net_delta() < 0 ? -engine_.net_delta() : engine_.net_delta();
  recovery_.barrier().delta_within_tolerance = magnitude <= spec_.delta_tolerance;
}

void TradefiPerpsMm::on_bbo_update(const strategyframe::BboUpdate& update) {
  ConfiguredLeg* leg = leg_for(update.header.instrument_id);
  if (leg == nullptr || context_ == nullptr) return;
  leg->bid = update.bid.price;
  leg->ask = update.ask.price;
  leg->recv_ns = context_->now_ns();
  refresh_books();
  maybe_quote();
}

void TradefiPerpsMm::maybe_quote() noexcept {
  if (context_ == nullptr || unknown_order_ || engine_.phase() != Phase::Quoting)
    return;
  if (!recovery_.can_resume_quotes()) return;
  const auto active = [](SlotState state) {
    return state == SlotState::Reserved || state == SlotState::Submitted ||
           state == SlotState::Acked ||
           state == SlotState::PartiallyFilled ||
           state == SlotState::CancelPending ||
           state == SlotState::Uncertain;
  };
  std::int64_t reserved_ib_sell = 0;
  const SlotId ib_sell_maker_slot =
      maker_.venue == Venue::Ibkr ? SlotId::MakerAsk : SlotId::MakerBid;
  if (active(recovery_.slot(ib_sell_maker_slot).state))
    reserved_ib_sell += spec_.quote_size_base;
  for (SlotId recovery_slot : {SlotId::Hedge, SlotId::Unwind}) {
    const Slot& item = recovery_.slot(recovery_slot);
    const Venue order_venue =
        recovery_slot == SlotId::Hedge ? hedge_.venue : maker_.venue;
    if (order_venue == Venue::Ibkr && active(item.state) && !item.buy) {
      const std::int64_t delta = engine_.net_delta();
      reserved_ib_sell += delta < 0 ? -delta : delta;
    }
  }
  engine_.set_ib_sell_reservation(reserved_ib_sell);
  const Quote quote = engine_.quote();
  if (!quote.bid && !quote.ask) return;
  auto place = [&](bool buy, std::int64_t price, SlotId slot) {
    std::int64_t quantity = 0;
    std::int64_t order_price = 0;
    if (!native_order(maker_, spec_.quote_size_base, price, quantity, order_price)) {
      recovery_.maker_reject();
      return;
    }
    if (maker_.venue == Venue::Ibkr) {
      std::int64_t current_bid = 0;
      std::int64_t current_ask = 0;
      if (!price_at(maker_.bid, current_bid) ||
          !price_at(maker_.ask, current_ask) ||
          (buy ? price >= current_ask : price <= current_bid))
        return;
    }
    strategyframe::OrderRequest request;
    request.account_id = maker_.account;
    request.instrument_id = maker_.instrument;
    request.side = buy ? strategyframe::Side::Buy : strategyframe::Side::Sell;
    request.type = strategyframe::OrderType::Limit;
    request.time_in_force = strategyframe::TimeInForce::GTC;
    if (maker_.strict && maker_.venue == Venue::Hyperliquid)
      request.flags = static_cast<std::uint16_t>(strategyframe::OrderFlag::PostOnly);
    request.quantity =
        strategyframe::FixedPoint{quantity, maker_.quantity_scale, {}};
    request.price =
        strategyframe::FixedPoint{order_price, maker_.price_scale, {}};
    request.client_order_id = next_client_order_id("maker");
    if (request.client_order_id.empty() ||
        !submit_order_or_shadow(request, slot, recovery_.maker_generation(),
                                buy)) {
      recovery_.maker_reject();
      return;
    }
    quoted_ = true;
  };
  if (max_active_maker_orders_ == 1) {
    // Prefer the side whose fill buys inventory at IB and therefore cannot
    // violate inventory_only.
    if (maker_.venue == Venue::Hyperliquid) {
      if (quote.ask) place(false, quote.ask_price, SlotId::MakerAsk);
      else if (quote.bid) place(true, quote.bid_price, SlotId::MakerBid);
    } else {
      if (quote.bid) place(true, quote.bid_price, SlotId::MakerBid);
      else if (quote.ask) place(false, quote.ask_price, SlotId::MakerAsk);
    }
  } else {
    if (quote.bid) place(true, quote.bid_price, SlotId::MakerBid);
    if (quote.ask) place(false, quote.ask_price, SlotId::MakerAsk);
  }
}

void TradefiPerpsMm::cancel_maker_orders() noexcept {
  if (context_ == nullptr) return;
  for (const auto& order : context_->open_orders()) {
    if (order.instrument_id == maker_.instrument) {
      SlotId slot = SlotId::MakerBid;
      if (slot_for_token(order.token.sequence, slot))
        recovery_.timeout(slot);
      cancel_order_or_shadow(order);
    }
  }
}

void TradefiPerpsMm::drive_recovery() noexcept {
  if (context_ == nullptr || recovery_.halted()) return;
  const HedgePlan plan = engine_.plan();
  if (plan.action == HedgeAction::Accept) {
    const bool completed = recovery_.hedge_recovery();
    recovery_.complete_hedge();
    if (completed && stop_after_hedge_cycles_ > 0 &&
        ++completed_cycles_ >= stop_after_hedge_cycles_) {
      cancel_maker_orders();
      recovery_.halt();
      engine_.note_halted();
      context_->request_stop();
    }
    return;
  }
  if (plan.action != HedgeAction::Hedge &&
      plan.action != HedgeAction::Unwind) {
    recovery_.halt();
    engine_.note_halted();
    return;
  }
  recovery_.begin_hedge();
  cancel_maker_orders();
  const SlotId slot = plan.action == HedgeAction::Hedge ? SlotId::Hedge
                                                        : SlotId::Unwind;
  const SlotState state = recovery_.slot(slot).state;
  if (state == SlotState::Reserved || state == SlotState::Submitted ||
      state == SlotState::Acked || state == SlotState::PartiallyFilled ||
      state == SlotState::CancelPending || state == SlotState::Uncertain)
    return;

  strategyframe::OrderRequest request;
  request.account_id =
      plan.action == HedgeAction::Hedge ? hedge_.account : maker_.account;
  request.instrument_id =
      plan.action == HedgeAction::Hedge ? hedge_.instrument
                                        : maker_.instrument;
  request.side = plan.native_quantity < 0 ? strategyframe::Side::Sell
                                          : strategyframe::Side::Buy;
  request.type = strategyframe::OrderType::Limit;
  request.time_in_force = strategyframe::TimeInForce::IOC;
  if (plan.action == HedgeAction::Unwind &&
      maker_.venue == Venue::Hyperliquid)
    request.flags =
        static_cast<std::uint16_t>(strategyframe::OrderFlag::ReduceOnly);

  const std::int64_t native_calc =
      plan.native_quantity < 0 ? -plan.native_quantity : plan.native_quantity;
  const ConfiguredLeg& order_leg =
      plan.action == HedgeAction::Hedge ? hedge_ : maker_;
  const strategyframe::FixedPoint& market =
      plan.action == HedgeAction::Hedge
          ? (request.side == strategyframe::Side::Buy ? hedge_.ask
                                                       : hedge_.bid)
          : (request.side == strategyframe::Side::Buy ? maker_.ask
                                                       : maker_.bid);
  std::int64_t native = 0;
  std::int64_t calc_price = 0;
  std::int64_t order_price = 0;
  std::int64_t leg_tick = 0;
  bool valid =
      rescale_exact(native_calc, quantity_calc_, order_leg.quantity_scale,
                    native) &&
      price_at(market, calc_price) &&
      rescale_exact(order_leg.tick, order_leg.price_scale, price_calc_,
                    leg_tick) &&
      native > 0 && calc_price > 0 && leg_tick > 0 &&
      (order_leg.lot <= 0 || native % order_leg.lot == 0);
  if (valid && spec_.max_hedge_slippage_ticks > 0 &&
      leg_tick > std::numeric_limits<std::int64_t>::max() /
                     spec_.max_hedge_slippage_ticks)
    valid = false;
  const std::int64_t slippage =
      valid ? leg_tick * spec_.max_hedge_slippage_ticks : 0;
  if (valid && request.side == strategyframe::Side::Buy) {
    if (calc_price > std::numeric_limits<std::int64_t>::max() - slippage)
      valid = false;
    else
      calc_price += slippage;
  } else if (valid) {
    if (calc_price <= slippage)
      valid = false;
    else
      calc_price -= slippage;
  }
  valid = valid &&
          rescale_exact(calc_price, price_calc_, order_leg.price_scale,
                        order_price) &&
          order_price > 0 &&
          (order_leg.tick <= 0 || order_price % order_leg.tick == 0);
  if (!valid) {
    if (plan.action == HedgeAction::Hedge) recovery_.hedge_reject();
    else recovery_.unwind_reject();
    return;
  }
  request.quantity =
      strategyframe::FixedPoint{native, order_leg.quantity_scale, {}};
  request.price =
      strategyframe::FixedPoint{order_price, order_leg.price_scale, {}};
  request.client_order_id = next_client_order_id(
      plan.action == HedgeAction::Hedge ? "hedge" : "unwind");
  const std::uint64_t generation =
      plan.action == HedgeAction::Hedge ? recovery_.hedge_generation()
                                        : recovery_.maker_generation();
  if (request.client_order_id.empty() ||
      !submit_order_or_shadow(
          request, slot, generation,
          request.side == strategyframe::Side::Buy)) {
    if (plan.action == HedgeAction::Hedge) recovery_.hedge_reject();
    else recovery_.unwind_reject();
  }
}

void TradefiPerpsMm::on_order_update(const strategyframe::ExecutionUpdate& update) {
  if (update.kind ==
      strategyframe::ExecutionUpdate::Kind::CommandResult) {
    for (LegId leg_id : {LegId::Maker, LegId::Hedge}) {
      ConfiguredLeg& value = leg(leg_id);
      for (std::size_t index = 0; index < value.cleanup_token_count; ++index) {
        if (!(value.cleanup_tokens[index] == update.token)) continue;
        value.cleanup_tokens[index] =
            value.cleanup_tokens[--value.cleanup_token_count];
        if (update.error != 0) {
          recovery_.halt();
          engine_.note_halted();
        } else if (value.cleanup_token_count == 0 && context_ != nullptr) {
          value.cleanup_requery_ns =
              context_->now_ns() + 100'000'000ULL;
        }
        return;
      }
    }
  }
  SlotId slot = SlotId::MakerBid;
  const bool known_slot = slot_for_token(update.token.sequence, slot);
  if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill) {
    if (!known_slot) {
      std::cerr << "tradefi unmatched fill token=" << update.token.sequence
                << '\n';
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    if (update.trade_id.length == 0 ||
        update.trade_id.length > sizeof(update.trade_id.value)) {
      std::cerr << "tradefi fill without venue trade identity\n";
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    const bool maker = update.instrument_id == maker_.instrument;
    std::uint64_t trade_key = 1469598103934665603ULL;
    trade_key ^=
        static_cast<std::uint8_t>(maker ? maker_.venue : hedge_.venue);
    trade_key *= 1099511628211ULL;
    for (std::uint16_t index = 0; index < update.trade_id.length; ++index) {
      trade_key ^= static_cast<unsigned char>(update.trade_id.value[index]);
      trade_key *= 1099511628211ULL;
    }
    const Slot& matched = recovery_.slot(slot);
    const bool buy = matched.buy;
    std::int64_t qty = 0;
    std::int64_t fill_price = 0;
    if (!rescale_exact(update.fill_quantity.value, update.fill_quantity.scale,
                       quantity_calc_, qty) ||
        !rescale_exact(update.fill_price.value, update.fill_price.scale,
                       price_calc_, fill_price)) {
      fail("fill scale");
      return;
    }
    if (qty <= 0) {
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    const std::int64_t signed_qty = buy ? qty : -qty;
    if (maker) engine_.on_maker_fill(signed_qty, fill_price);
    else engine_.on_hedge_fill(signed_qty, fill_price);
    if (!recovery_.fill(slot, trade_key, qty, matched.generation)) {
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    if (maker) {
      drive_recovery();
    } else if (engine_.plan().action == HedgeAction::Accept) {
      engine_.note_hedge_terminal();
      recovery_.complete_hedge();
    }
    return;
  }
  if (!known_slot) {
    if (update.kind == strategyframe::ExecutionUpdate::Kind::Order) {
      std::cerr << "tradefi unmatched order update token="
                << update.token.sequence << '\n';
      recovery_.halt();
      engine_.note_halted();
    }
    return;
  }
  const std::uint64_t generation = recovery_.slot(slot).generation;
  if (update.status == strategyframe::OrderStatus::Open) {
    recovery_.ack(slot, generation);
    return;
  }
  if (update.status == strategyframe::OrderStatus::Rejected) {
    recovery_.terminal(slot, generation);
    if (slot == SlotId::Hedge) {
      recovery_.hedge_reject();
      engine_.note_hedge_terminal();
    } else if (slot == SlotId::Unwind) {
      recovery_.unwind_reject();
      engine_.note_unwind_terminal(false);
    } else {
      recovery_.maker_reject();
    }
    drive_recovery();
  }
  if (update.status == strategyframe::OrderStatus::Canceled) {
    recovery_.cancel_ack(slot, generation);
  } else if (update.status == strategyframe::OrderStatus::Filled ||
             update.status == strategyframe::OrderStatus::Rejected ||
             update.status == strategyframe::OrderStatus::Expired) {
    recovery_.terminal(slot, generation);
  }
  if (update.status == strategyframe::OrderStatus::Canceled ||
      update.status == strategyframe::OrderStatus::Filled ||
      update.status == strategyframe::OrderStatus::Expired) {
    if (update.instrument_id == hedge_.instrument) engine_.note_hedge_terminal();
    if (recovery_.slot(SlotId::Unwind).token == update.token.sequence)
      engine_.note_unwind_terminal(update.status == strategyframe::OrderStatus::Filled);
    drive_recovery();
  }
}

void TradefiPerpsMm::on_oms_status(const strategyframe::OmsStatusUpdate& update) {
  LegId leg_id;
  if (update.adapter_kind == adapter_kind(maker_.venue))
    leg_id = LegId::Maker;
  else if (update.adapter_kind == adapter_kind(hedge_.venue))
    leg_id = LegId::Hedge;
  else
    return;
  ConfiguredLeg& value = leg(leg_id);
  const bool maker_leg = leg_id == LegId::Maker;
  if (update.kind == strategyframe::OmsStatusUpdate::Kind::ReconcileComplete) {
    if (update.error != 0) {
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    const bool generation_changed =
        value.reconcile_generation != update.generation;
    value.reconcile_generation = update.generation;
    value.adapter_reconciled = true;
    if (generation_changed) {
      value.orders_done = false;
      value.positions_done = value.cash;
      value.balances_done = !value.cash;
      value.balance_seen = false;
      value.unknown_order = false;
    }
    recovery_.note_stable(maker_leg);
    submit_deferred_cleanup(value);
    if (value.cleanup_token_count == 0 &&
        value.cleanup_order_count == 0 &&
        !has_active_query(leg_id, value.reconcile_generation) &&
        (generation_changed ||
         !value.orders_done ||
         (!value.cash && !value.positions_done) ||
         (value.cash && !value.balances_done)))
      begin_snapshot_queries(leg_id);
    update_reconcile_barrier();
    return;
  }
  if (update.adapter_status == 7 || update.error != 0) {
    recovery_.halt();
    engine_.note_halted();
    return;
  }
  if (update.adapter_status != 3 &&
      (value.adapter_reconciled ||
       update.generation != value.reconcile_generation)) {
    if (value.cleanup_token_count != 0) {
      recovery_.halt();
      engine_.note_halted();
      return;
    }
    engine_.note_reconnecting();
    if (maker_leg)
      recovery_.maker_venue_down();
    else
      recovery_.hedge_venue_down();
    recovery_.note_reconnect(maker_leg);
    value.reconcile_generation = update.generation;
    value.adapter_reconciled = false;
    value.orders_done = false;
    value.positions_done = value.cash;
    value.balances_done = !value.cash;
    value.balance_seen = false;
    value.unknown_order = false;
    update_reconcile_barrier();
  }
}

void TradefiPerpsMm::on_timer(const strategyframe::TimerEvent&) {
  refresh_books();
  const std::uint64_t now = context_->now_ns();
  for (LegId leg_id : {LegId::Maker, LegId::Hedge}) {
    ConfiguredLeg& value = leg(leg_id);
    if (value.cleanup_needed && value.cleanup_token_count == 0 &&
        value.cleanup_requery_ns != 0 &&
        now >= value.cleanup_requery_ns &&
        !has_active_query(leg_id, value.reconcile_generation)) {
      begin_snapshot_queries(leg_id);
    }
  }
  for (SlotId slot : {SlotId::Hedge, SlotId::Unwind}) {
    const Slot& item = recovery_.slot(slot);
    if (item.submitted_ns == 0 ||
        (item.state != SlotState::Submitted &&
         item.state != SlotState::Acked &&
         item.state != SlotState::PartiallyFilled))
      continue;
    if (now < item.submitted_ns + max_hedge_latency_ns_) continue;
    recovery_.timeout(slot);
    for (const auto& order : context_->open_orders()) {
      if (order.token.sequence == item.token) cancel_order_or_shadow(order);
    }
  }
  if (engine_.makers_must_cancel()) {
    for (const auto& order : context_->open_orders()) {
      if (order.instrument_id == maker_.instrument)
        cancel_order_or_shadow(order);
    }
  }
  const std::int64_t pnl = engine_.session_pnl();
  std::cerr << "tradefi session_pnl=" << pnl << " net_delta=" << engine_.net_delta()
            << " phase=" << static_cast<unsigned>(engine_.phase()) << '\n';
}

}  // namespace tradefimm
