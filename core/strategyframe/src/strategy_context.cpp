#include "strategyframe/strategy_context.h"

namespace strategyframe {

Result<OrderToken> StrategyContext::place_order(
    const OrderRequest& request) noexcept {
  if (!state_ || !ops_ || !ops_->place)
    return {{}, Error::InvalidState};
  return ops_->place(state_, request);
}

Result<OrderToken> StrategyContext::cancel(OrderToken token) noexcept {
  if (!state_ || !ops_ || !ops_->cancel)
    return {{}, Error::InvalidState};
  return ops_->cancel(state_, token);
}

Result<QueryToken> StrategyContext::query_open_orders(
    AccountId account_id) noexcept {
  if (!state_ || !ops_ || !ops_->query_open_orders)
    return {{}, Error::NotReady};
  return ops_->query_open_orders(state_, account_id);
}

Result<QueryToken> StrategyContext::query_positions(
    AccountId account_id) noexcept {
  if (!state_ || !ops_ || !ops_->query_positions)
    return {{}, Error::NotReady};
  return ops_->query_positions(state_, account_id);
}

Result<TimerHandle> StrategyContext::schedule_timer(
    std::uint64_t first_deadline_ns, std::uint64_t interval_ns) noexcept {
  if (!state_ || !ops_ || !ops_->schedule_timer)
    return {{}, Error::InvalidState};
  return ops_->schedule_timer(state_, first_deadline_ns, interval_ns);
}

Error StrategyContext::cancel_timer(TimerHandle handle) noexcept {
  if (!state_ || !ops_ || !ops_->cancel_timer)
    return Error::InvalidState;
  return ops_->cancel_timer(state_, handle);
}

std::span<const OrderView> StrategyContext::open_orders() const noexcept {
  if (!state_ || !ops_ || !ops_->open_orders) return {};
  return ops_->open_orders(state_);
}

std::span<const PositionView> StrategyContext::positions() const noexcept {
  if (!state_ || !ops_ || !ops_->positions) return {};
  return ops_->positions(state_);
}

Result<OrderView> StrategyContext::find_order(OrderToken token) const noexcept {
  if (!state_ || !ops_ || !ops_->find_order)
    return {{}, Error::InvalidState};
  return ops_->find_order(state_, token);
}

Result<PositionView> StrategyContext::find_position(
    AccountId account_id, InstrumentId instrument_id,
    PositionSide side) const noexcept {
  if (!state_ || !ops_ || !ops_->find_position)
    return {{}, Error::InvalidState};
  return ops_->find_position(state_, account_id, instrument_id, side);
}

Result<InstrumentInfo> StrategyContext::find_instrument(
    InstrumentId instrument_id) const noexcept {
  if (!state_ || !ops_ || !ops_->find_instrument)
    return {{}, Error::InvalidState};
  return ops_->find_instrument(state_, instrument_id);
}

Result<InstrumentCatalogInfo> StrategyContext::find_instrument(
    const InstrumentSelector& selector) const noexcept {
  if (!state_ || !ops_ || !ops_->find_instrument_selector)
    return {{}, Error::InvalidState};
  return ops_->find_instrument_selector(state_, selector);
}

Result<InstrumentCatalogInfo> StrategyContext::find_instrument_catalog(
    InstrumentId instrument_id) const noexcept {
  if (!state_ || !ops_ || !ops_->find_instrument_catalog)
    return {{}, Error::InvalidState};
  return ops_->find_instrument_catalog(state_, instrument_id);
}

Result<OmsStatusUpdate> StrategyContext::oms_status(
    std::uint8_t adapter_kind) const noexcept {
  if (!state_ || !ops_ || !ops_->oms_status)
    return {{}, Error::InvalidState};
  return ops_->oms_status(state_, adapter_kind);
}

const StrategyParams& StrategyContext::params() const noexcept {
  static const StrategyParams empty;
  if (!state_ || !ops_ || !ops_->params) return empty;
  return ops_->params(state_);
}

RuntimeMetrics StrategyContext::metrics() const noexcept {
  if (!state_ || !ops_ || !ops_->metrics) return {};
  return ops_->metrics(state_);
}

std::uint64_t StrategyContext::now_ns() const noexcept {
  if (!state_ || !ops_ || !ops_->now_ns) return 0;
  return ops_->now_ns(state_);
}

void StrategyContext::request_stop() noexcept {
  if (state_ && ops_ && ops_->request_stop) ops_->request_stop(state_);
}

}  // namespace strategyframe
