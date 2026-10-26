#pragma once

#include <array>
#include <atomic>
#include <cstdint>
#include <string>
#include <string_view>
#include <unordered_map>
#include <unordered_set>

#include "oms/api/order_types.h"
#include "oms/exchange/adapter_types.h"
#include "oms/exchange/ibkr/order_rules.h"
#include "utils/md/types.h"

namespace oms::exchange::ibkr {

inline constexpr int kDiscoveryClientId = 30;
inline constexpr int kMdsProducerClientId = 31;
inline constexpr std::size_t kCallbackCapacity = 64;
inline constexpr std::size_t kOrderCapacity = 64;
inline constexpr std::size_t kExecCapacity = 128;

[[nodiscard]] bool client_id_conflicts(int client_id) noexcept;

enum class InstrumentExecutionState : std::uint8_t {
  Resolving = 1,
  RetryPending,
  Ready,
  Rejected,
  Retiring,
};

enum class IocCapability : std::uint8_t {
  Unknown = 0,
  Supported,
  Unsupported,
};

struct Callback {
  enum class Kind : std::uint8_t {
    NextValidId,
    Error,
    Exec,
    Closed,
    OrderStatus,
  };
  Kind kind{Kind::Error};
  int code{};
  int order_id{};
  std::uint64_t generation{};
  std::uint64_t exec_key{};
};

struct Contract {
  std::int64_t con_id{};
  utils::md::IbkrSecurityType security{utils::md::IbkrSecurityType::Unknown};
  bool ioc{};
  bool multiplier_present{};
  std::int64_t multiplier{1};
  std::uint8_t multiplier_scale{};
  std::int64_t min_size{};
  std::int64_t size_increment{};
  std::int64_t min_tick{};
  std::int64_t units_per_native{1};
  bool unique{true};
  InstrumentExecutionState state{InstrumentExecutionState::Resolving};
  IocCapability ioc_capability{IocCapability::Unknown};
  api::InstrumentId instrument_id{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::int64_t catalog_tick{};
  std::int64_t catalog_lot{};
  std::int64_t catalog_multiplier{};
  std::uint8_t catalog_multiplier_scale{};
  std::uint64_t request_generation{};
  std::uint64_t deadline_ns{};
  int details_request_id{};
  int detail_count{};
  bool details_sent{};
  bool multiple_details{};
};

struct TrackedOrder {
  int client_id{};
  int order_id{};
  long perm_id{};
  api::OrderHandle handle{};
  api::RequestToken token{};
  api::ClientOrderId client_order_id{};
  api::ClientOrderId order_ref{};
  api::InstrumentId instrument_id{};
  std::int64_t con_id{};
  std::int64_t quantity{};
  std::int64_t filled{};
  std::uint8_t quantity_scale{};
  std::uint8_t price_scale{};
  api::TimeInForce tif{api::TimeInForce::GTC};
  bool occupied{};
  bool ours{};
  bool active{};
  bool terminal_sent{};
};

class Session {
 public:
  Session(int client_id, std::string account);

  [[nodiscard]] AdapterStatus status() const noexcept { return status_; }
  [[nodiscard]] bool push(const Callback& callback) noexcept;
  void service() noexcept;
  [[nodiscard]] bool resolve(const Contract& contract) noexcept;
  [[nodiscard]] AdapterResult register_instrument(
      api::InstrumentId instrument_id,
      const api::ResolvedInstrument& routing) noexcept;
  [[nodiscard]] AdapterResult retire_instrument(
      api::InstrumentId instrument_id,
      const api::ResolvedInstrument& routing) noexcept;
  [[nodiscard]] bool instrument_ready(std::int64_t con_id) const noexcept;
  [[nodiscard]] bool execution_ready(std::int64_t con_id) const noexcept;
  [[nodiscard]] InstrumentExecutionState instrument_state(
      std::int64_t con_id) const noexcept;
  [[nodiscard]] IocCapability ioc_capability(std::int64_t con_id) const noexcept;
  void note_ioc_unsupported(std::int64_t con_id) noexcept;
  [[nodiscard]] QuantityPlan hedge(std::int64_t con_id,
                                   std::int64_t target_base_units) const noexcept;
  [[nodiscard]] int allocate_order_id() noexcept;
  void observe_order_id(int order_id) noexcept;
  void note_next_valid_id(int order_id) noexcept;
  void note_connected(bool connected) noexcept;
  void note_reconciled() noexcept;
  using CallbackListener = void (*)(const Callback&, void*) noexcept;
  void set_listener(CallbackListener listener, void* context) noexcept {
    listener_ = listener;
    listener_context_ = context;
  }
  [[nodiscard]] bool confirm_reconnect() noexcept;
  [[nodiscard]] bool begin_session_reconcile() noexcept;
  void abandon_resolving(api::InstrumentId instrument_id,
                         std::int64_t con_id) noexcept;
  [[nodiscard]] bool new_orders_allowed() const noexcept { return new_orders_; }
  [[nodiscard]] bool cancel_makers() const noexcept { return cancel_makers_; }
  [[nodiscard]] int next_order_id() const noexcept { return next_order_id_; }
  [[nodiscard]] int client_id() const noexcept { return client_id_; }
  [[nodiscard]] const std::string& account() const noexcept { return account_; }
  [[nodiscard]] std::uint64_t generation() const noexcept { return generation_; }
  [[nodiscard]] std::uint64_t connection_generation() const noexcept {
    return connection_generation_;
  }
  [[nodiscard]] bool needs_request_ids() const noexcept { return needs_ids_; }
  void clear_request_ids() noexcept { needs_ids_ = false; }
  [[nodiscard]] bool session_ready() const noexcept;
  [[nodiscard]] bool saw_exec(std::uint64_t exec_key) const noexcept;
  [[nodiscard]] bool remember_exec(std::string_view exec_id) noexcept;
  [[nodiscard]] TrackedOrder* find_order(int client_id, int order_id) noexcept;
  [[nodiscard]] TrackedOrder* insert_order(const TrackedOrder& order) noexcept;
  [[nodiscard]] const Contract* find_contract(std::int64_t con_id) const noexcept;
  Contract* find_contract(std::int64_t con_id) noexcept;
  template <class Fn>
  void for_each_contract(Fn&& fn) {
    for (auto& [con_id, contract] : contracts_) {
      (void)con_id;
      fn(contract);
    }
  }
  [[nodiscard]] std::array<TrackedOrder, kOrderCapacity>& orders() noexcept {
    return orders_;
  }
  void mark_unknown_order() noexcept { unknown_order_ = true; }
  void clear_unknown_order() noexcept { unknown_order_ = false; }
  [[nodiscard]] bool unknown_order() const noexcept { return unknown_order_; }
  void mark_unmapped_position() noexcept { unmapped_position_ = true; }
  [[nodiscard]] bool unmapped_position() const noexcept {
    return unmapped_position_;
  }
  [[nodiscard]] bool has_position(api::InstrumentId instrument_id) const noexcept;
  void set_position(api::InstrumentId instrument_id,
                    std::int64_t quantity) noexcept;
  void bump_connection_generation() noexcept { ++connection_generation_; }
  void fail() noexcept;
  void set_capacities(std::size_t order_capacity, std::size_t exec_capacity) noexcept;
  void note_query_timeout() noexcept;

 private:
  void apply(const Callback& callback) noexcept;
  void raise_next_order_id(int candidate) noexcept;
  Contract* contract_by_instrument(api::InstrumentId instrument_id) noexcept;

  int client_id_{};
  std::string account_;
  AdapterStatus status_{AdapterStatus::Connecting};
  bool new_orders_{};
  bool cancel_makers_{};
  bool reconciled_{};
  bool connected_{};
  bool next_id_seen_{};
  bool unknown_order_{};
  bool unmapped_position_{};
  bool needs_ids_{};
  bool failed_{};
  bool farm_down_{};
  int next_order_id_{1};
  int next_valid_id_{};
  int max_observed_order_id_{};
  std::uint64_t generation_{1};
  std::uint64_t connection_generation_{1};
  std::array<Callback, kCallbackCapacity> slots_{};
  std::atomic<std::size_t> write_{0};
  std::atomic<std::size_t> read_{0};
  std::unordered_map<std::int64_t, Contract> contracts_;
  std::unordered_map<api::InstrumentId, std::int64_t> positions_;
  std::array<TrackedOrder, kOrderCapacity> orders_{};
  std::array<std::uint64_t, kExecCapacity> execs_{};
  std::size_t exec_count_{};
  std::size_t order_capacity_{kOrderCapacity};
  std::size_t exec_capacity_{kExecCapacity};
  CallbackListener listener_{};
  void* listener_context_{};
  Contract undo_{};
  bool undo_valid_{};
};

}  // namespace oms::exchange::ibkr
