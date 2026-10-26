#include "oms/exchange/ibkr/session.h"

#include <cstring>

namespace oms::exchange::ibkr {
namespace {

std::uint64_t hash_text(std::string_view text) noexcept {
  std::uint64_t hash = 1469598103934665603ULL;
  for (const char value : text) {
    hash ^= static_cast<unsigned char>(value);
    hash *= 1099511628211ULL;
  }
  return hash;
}

}  // namespace

bool client_id_conflicts(int client_id) noexcept {
  return client_id < 0 || client_id == kDiscoveryClientId ||
         client_id == kMdsProducerClientId;
}

Session::Session(int client_id, std::string account)
    : client_id_(client_id), account_(std::move(account)) {
  if (client_id_conflicts(client_id_) || account_.empty()) {
    status_ = AdapterStatus::Failed;
    new_orders_ = false;
    failed_ = true;
  }
}

bool Session::push(const Callback& callback) noexcept {
  if (status_ == AdapterStatus::Failed) return false;
  const std::size_t write = write_.load(std::memory_order_relaxed);
  const std::size_t read = read_.load(std::memory_order_acquire);
  if (write - read >= kCallbackCapacity) {
    fail();
    return false;
  }
  Callback stamped = callback;
  stamped.generation = generation_;
  slots_[write % kCallbackCapacity] = stamped;
  write_.store(write + 1, std::memory_order_release);
  return true;
}

void Session::service() noexcept {
  const std::size_t write = write_.load(std::memory_order_acquire);
  std::size_t read = read_.load(std::memory_order_relaxed);
  while (read != write) {
    apply(slots_[read % kCallbackCapacity]);
    ++read;
  }
  read_.store(read, std::memory_order_release);
}

void Session::apply(const Callback& callback) noexcept {
  switch (callback.kind) {
    case Callback::Kind::NextValidId:
      note_next_valid_id(callback.order_id);
      break;
    case Callback::Kind::Error:
      if (classify_client_id_error(callback.code) == ClientIdFault::Duplicate) {
        fail();
        break;
      }
      if (callback.code == 1100) {
        new_orders_ = false;
        cancel_makers_ = true;
        farm_down_ = true;
        status_ = AdapterStatus::Reconnecting;
        reconciled_ = false;
        for (auto& [con_id, contract] : contracts_) {
          (void)con_id;
          if (contract.state == InstrumentExecutionState::Resolving)
            contract.state = InstrumentExecutionState::RetryPending;
        }
      } else if (callback.code == 1101) {
        farm_down_ = false;
        status_ = AdapterStatus::Reconciling;
        reconciled_ = false;
        new_orders_ = false;
        ++generation_;
        needs_ids_ = true;
      } else if (callback.code == 1102) {
        farm_down_ = false;
        if (status_ == AdapterStatus::Reconnecting ||
            status_ == AdapterStatus::Ready ||
            status_ == AdapterStatus::Connecting) {
          status_ = AdapterStatus::Reconciling;
          reconciled_ = false;
          new_orders_ = false;
          cancel_makers_ = true;
        }
      }
      break;
    case Callback::Kind::Exec:
      (void)remember_exec(std::to_string(callback.exec_key));
      break;
    case Callback::Kind::Closed:
      new_orders_ = false;
      cancel_makers_ = true;
      connected_ = false;
      status_ = AdapterStatus::Reconnecting;
      for (auto& [con_id, contract] : contracts_) {
        (void)con_id;
        if (contract.state == InstrumentExecutionState::Resolving)
          contract.state = InstrumentExecutionState::RetryPending;
      }
      break;
    case Callback::Kind::OrderStatus:
      if (callback.order_id > 0) observe_order_id(callback.order_id);
      break;
  }
  if (listener_ != nullptr) listener_(callback, listener_context_);
}

void Session::raise_next_order_id(int candidate) noexcept {
  if (candidate > next_order_id_) next_order_id_ = candidate;
}

void Session::observe_order_id(int order_id) noexcept {
  if (order_id > max_observed_order_id_) max_observed_order_id_ = order_id;
  raise_next_order_id(max_observed_order_id_ + 1);
}

void Session::note_next_valid_id(int order_id) noexcept {
  if (order_id <= 0) return;
  next_valid_id_ = order_id;
  next_id_seen_ = true;
  raise_next_order_id(order_id);
  needs_ids_ = false;
}

void Session::note_connected(bool connected) noexcept {
  connected_ = connected;
  if (!connected && status_ != AdapterStatus::Failed) {
    new_orders_ = false;
    next_id_seen_ = false;
    reconciled_ = false;
    status_ = AdapterStatus::Reconnecting;
  }
}

void Session::note_reconciled() noexcept {
  if (status_ == AdapterStatus::Failed || unknown_order_ || unmapped_position_ ||
      !connected_ || !next_id_seen_)
    return;
  reconciled_ = true;
  status_ = AdapterStatus::Ready;
  new_orders_ = true;
  cancel_makers_ = false;
}

void Session::note_query_timeout() noexcept {
  if (status_ == AdapterStatus::Failed) return;
  reconciled_ = false;
  new_orders_ = false;
  if (status_ == AdapterStatus::Ready) status_ = AdapterStatus::Reconciling;
}

void Session::set_capacities(std::size_t order_capacity,
                             std::size_t exec_capacity) noexcept {
  order_capacity_ = order_capacity == 0 || order_capacity > orders_.size()
                        ? orders_.size()
                        : order_capacity;
  exec_capacity_ = exec_capacity == 0 || exec_capacity > execs_.size()
                       ? execs_.size()
                       : exec_capacity;
}

bool Session::session_ready() const noexcept {
  return status_ == AdapterStatus::Ready && connected_ && next_id_seen_ &&
         reconciled_ && !unknown_order_ && !unmapped_position_ && !failed_;
}

bool Session::resolve(const Contract& contract) noexcept {
  if (contract.con_id <= 0 || !contract.unique ||
      contract.security == utils::md::IbkrSecurityType::Unknown ||
      contract.min_size <= 0 || contract.size_increment <= 0)
    return false;
  if (contract.security == utils::md::IbkrSecurityType::Commodity &&
      !contract.multiplier_present)
    return false;
  Contract stored = contract;
  stored.state = InstrumentExecutionState::Ready;
  if (!stored.ioc) stored.ioc_capability = IocCapability::Unsupported;
  const auto existing = contracts_.find(contract.con_id);
  if (existing != contracts_.end()) stored.instrument_id = existing->second.instrument_id;
  contracts_[contract.con_id] = stored;
  return true;
}

Contract* Session::contract_by_instrument(
    api::InstrumentId instrument_id) noexcept {
  for (auto& [con_id, contract] : contracts_) {
    (void)con_id;
    if (contract.instrument_id == instrument_id) return &contract;
  }
  return nullptr;
}

AdapterResult Session::register_instrument(
    api::InstrumentId instrument_id,
    const api::ResolvedInstrument& routing) noexcept {
  if (instrument_id == 0 || routing.kind != api::ExecutionRouteKind::Ibkr ||
      routing.ibkr.con_id <= 0 ||
      routing.ibkr.security_type == utils::md::IbkrSecurityType::Unknown)
    return AdapterResult::InvalidArgument;
  if (undo_valid_ && undo_.instrument_id == instrument_id &&
      undo_.con_id == routing.ibkr.con_id) {
    contracts_[undo_.con_id] = undo_;
    undo_valid_ = false;
    return AdapterResult::Ok;
  }
  if (Contract* existing = contract_by_instrument(instrument_id);
      existing != nullptr) {
    const bool same = existing->con_id == routing.ibkr.con_id &&
                      existing->security == routing.ibkr.security_type;
    if (!same || existing->state == InstrumentExecutionState::Rejected ||
        existing->state == InstrumentExecutionState::Retiring)
      return AdapterResult::InvalidArgument;
    return AdapterResult::Ok;
  }
  Contract contract;
  contract.con_id = routing.ibkr.con_id;
  contract.security = routing.ibkr.security_type;
  contract.instrument_id = instrument_id;
  contract.price_scale = routing.price_scale;
  contract.quantity_scale = routing.quantity_scale;
  contract.catalog_tick = routing.tick_size;
  contract.catalog_lot = routing.lot_size;
  contract.catalog_multiplier = routing.ibkr.contract_multiplier;
  contract.catalog_multiplier_scale = routing.ibkr.contract_multiplier_scale;
  contract.state = InstrumentExecutionState::Resolving;
  contract.request_generation = connection_generation_;
  contract.deadline_ns = 0;
  contracts_[contract.con_id] = contract;
  return AdapterResult::Ok;
}

AdapterResult Session::retire_instrument(
    api::InstrumentId instrument_id,
    const api::ResolvedInstrument& routing) noexcept {
  Contract* contract = contract_by_instrument(instrument_id);
  if (contract == nullptr) return AdapterResult::InvalidArgument;
  if (contract->con_id != routing.ibkr.con_id) return AdapterResult::InvalidArgument;
  const InstrumentExecutionState previous = contract->state;
  contract->state = InstrumentExecutionState::Retiring;
  for (const TrackedOrder& order : orders_) {
    if (order.occupied && order.active && order.instrument_id == instrument_id) {
      contract->state = previous;
      return AdapterResult::Failed;
    }
  }
  const auto position = positions_.find(instrument_id);
  if (position != positions_.end() && position->second != 0) {
    contract->state = previous;
    return AdapterResult::Failed;
  }
  undo_ = *contract;
  undo_.state = previous;
  undo_valid_ = true;
  contracts_.erase(contract->con_id);
  return AdapterResult::Ok;
}

bool Session::instrument_ready(std::int64_t con_id) const noexcept {
  const auto found = contracts_.find(con_id);
  return found != contracts_.end() &&
         found->second.state == InstrumentExecutionState::Ready;
}

bool Session::execution_ready(std::int64_t con_id) const noexcept {
  return session_ready() && instrument_ready(con_id);
}

InstrumentExecutionState Session::instrument_state(
    std::int64_t con_id) const noexcept {
  const auto found = contracts_.find(con_id);
  return found == contracts_.end() ? InstrumentExecutionState::Rejected
                                   : found->second.state;
}

IocCapability Session::ioc_capability(std::int64_t con_id) const noexcept {
  const auto found = contracts_.find(con_id);
  return found == contracts_.end() ? IocCapability::Unknown
                                   : found->second.ioc_capability;
}

void Session::note_ioc_unsupported(std::int64_t con_id) noexcept {
  const auto found = contracts_.find(con_id);
  if (found == contracts_.end()) return;
  found->second.ioc_capability = IocCapability::Unsupported;
  if (found->second.state == InstrumentExecutionState::Ready)
    found->second.state = InstrumentExecutionState::Ready;
}

QuantityPlan Session::hedge(std::int64_t con_id,
                            std::int64_t target_base_units) const noexcept {
  const auto found = contracts_.find(con_id);
  if (found == contracts_.end()) return {};
  const Contract& contract = found->second;
  return plan_hedge_quantity(target_base_units, contract.units_per_native,
                             contract.size_increment, contract.min_size);
}

bool Session::begin_session_reconcile() noexcept {
  if (failed_ || farm_down_ || !connected_ || !next_id_seen_ || reconciled_)
    return false;
  if (status_ != AdapterStatus::Connecting &&
      status_ != AdapterStatus::Reconnecting)
    return false;
  status_ = AdapterStatus::Reconciling;
  new_orders_ = false;
  return true;
}

void Session::abandon_resolving(api::InstrumentId instrument_id,
                                std::int64_t con_id) noexcept {
  Contract* contract = contract_by_instrument(instrument_id);
  if (contract == nullptr || contract->con_id != con_id || contract->details_sent)
    return;
  if (contract->state != InstrumentExecutionState::Resolving &&
      contract->state != InstrumentExecutionState::RetryPending)
    return;
  contracts_.erase(con_id);
}

bool Session::confirm_reconnect() noexcept {
  if (status_ != AdapterStatus::Reconciling) return false;
  if (unknown_order_ || unmapped_position_ || !next_id_seen_) return false;
  reconciled_ = true;
  connected_ = true;
  status_ = AdapterStatus::Ready;
  new_orders_ = true;
  cancel_makers_ = false;
  return true;
}

int Session::allocate_order_id() noexcept {
  if (!new_orders_ || status_ == AdapterStatus::Failed) return -1;
  raise_next_order_id(next_valid_id_);
  raise_next_order_id(max_observed_order_id_ + 1);
  const int order_id = next_order_id_;
  observe_order_id(order_id);
  return order_id;
}

bool Session::saw_exec(std::uint64_t exec_key) const noexcept {
  for (std::size_t index = 0; index < exec_count_; ++index) {
    if (execs_[index] == exec_key) return true;
  }
  return false;
}

bool Session::remember_exec(std::string_view exec_id) noexcept {
  if (exec_id.empty()) return false;
  const std::uint64_t key = hash_text(exec_id);
  if (saw_exec(key)) return false;
  if (exec_count_ >= exec_capacity_) {
    fail();
    return false;
  }
  execs_[exec_count_++] = key;
  return true;
}

TrackedOrder* Session::find_order(int client_id, int order_id) noexcept {
  for (TrackedOrder& order : orders_) {
    if (order.occupied && order.client_id == client_id &&
        order.order_id == order_id)
      return &order;
  }
  return nullptr;
}

TrackedOrder* Session::insert_order(const TrackedOrder& order) noexcept {
  std::size_t used = 0;
  TrackedOrder* free_slot = nullptr;
  for (TrackedOrder& slot : orders_) {
    if (slot.occupied) {
      ++used;
    } else if (free_slot == nullptr) {
      free_slot = &slot;
    }
  }
  if (used >= order_capacity_ || free_slot == nullptr) {
    fail();
    return nullptr;
  }
  *free_slot = order;
  free_slot->occupied = true;
  return free_slot;
}

const Contract* Session::find_contract(std::int64_t con_id) const noexcept {
  const auto found = contracts_.find(con_id);
  return found == contracts_.end() ? nullptr : &found->second;
}

Contract* Session::find_contract(std::int64_t con_id) noexcept {
  const auto found = contracts_.find(con_id);
  return found == contracts_.end() ? nullptr : &found->second;
}

bool Session::has_position(api::InstrumentId instrument_id) const noexcept {
  const auto found = positions_.find(instrument_id);
  return found != positions_.end() && found->second != 0;
}

void Session::set_position(api::InstrumentId instrument_id,
                           std::int64_t quantity) noexcept {
  positions_[instrument_id] = quantity;
}

void Session::fail() noexcept {
  failed_ = true;
  status_ = AdapterStatus::Failed;
  new_orders_ = false;
}

}  // namespace oms::exchange::ibkr
