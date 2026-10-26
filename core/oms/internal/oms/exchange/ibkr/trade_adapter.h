#pragma once

#include <array>
#include <cstdint>
#include <string>

#include "oms/exchange/ibkr/exec_client.h"
#include "oms/exchange/ibkr/session.h"
#include "oms/exchange/trade_adapter.h"

namespace oms::exchange::ibkr {

class IbkrWakeDriver;

class IbkrTradeAdapter final : public TradeAdapter {
 public:
  IbkrTradeAdapter(int client_id, std::string account,
                   std::string_view strategy_instance = {});
  void attach_client(ExecClient* client) noexcept;
  void configure_gateway(std::string host, int port, std::size_t order_capacity = 0,
                         std::size_t exec_capacity = 0,
                         std::size_t query_waiter_capacity = 0) noexcept;

  [[nodiscard]] AdapterIdentity identity() const noexcept override;
  [[nodiscard]] AdapterStatus status() const noexcept override;
  [[nodiscard]] AdapterCapabilities capabilities() const noexcept override;
  [[nodiscard]] AdapterResult reserve_command(
      AdapterCommandKind kind, AdapterReservation& reservation) noexcept override;
  void cancel_reservation(AdapterReservation reservation) noexcept override;
  [[nodiscard]] AdapterResult commit_place(
      AdapterReservation reservation,
      const AdapterPlaceCommand& command) noexcept override;
  [[nodiscard]] AdapterResult commit_cancel(
      AdapterReservation reservation,
      const AdapterCancelCommand& command) noexcept override;
  [[nodiscard]] AdapterResult commit_cancel_venue(
      AdapterReservation reservation,
      const AdapterCancelVenueCommand& command) noexcept override;
  [[nodiscard]] AdapterResult query_open_orders(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult query_positions(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult query_balances(
      const AdapterQueryRequest& request,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult register_instrument(
      api::InstrumentId instrument_id,
      const api::ResolvedInstrument& routing) noexcept override;
  [[nodiscard]] AdapterResult retire_instrument(
      api::InstrumentId instrument_id,
      const api::ResolvedInstrument& routing) noexcept override;
  [[nodiscard]] bool instrument_ready(
      const api::ResolvedInstrument& routing) const noexcept override;
  [[nodiscard]] bool tracks_instrument_readiness() const noexcept override {
    return true;
  }
  [[nodiscard]] AdapterServiceResult service_io(
      std::uint64_t now_ns, std::uint32_t event_budget,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult on_deadline(
      const AdapterDeadline& deadline, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult begin_reconcile(
      std::uint64_t generation, std::uint64_t now_ns,
      const AdapterEventSink& sink) noexcept override;
  [[nodiscard]] AdapterResult shutdown(
      std::uint64_t now_ns, const AdapterEventSink& sink) noexcept override;

  [[nodiscard]] Session& session() noexcept { return session_; }
  [[nodiscard]] const Session& session() const noexcept { return session_; }
  [[nodiscard]] int contract_request_count() const noexcept {
    return contract_requests_;
  }

 private:
  struct QueryWaiter {
    api::QueryToken token{};
    api::AccountId account_id{};
    api::QueryKind kind{api::QueryKind::OpenOrders};
    api::InstrumentId instrument_id{};
    std::int64_t con_id{};
  };

  struct BalanceValue {
    std::array<char, 16> currency{};
    std::array<char, 32> total{};
    std::array<char, 32> available{};
    bool has_total{};
    bool has_available{};
  };

  [[nodiscard]] bool queue_reject(const AdapterPlaceCommand& command) noexcept;
  [[nodiscard]] AdapterResult emit(const AdapterEventSink& sink) noexcept;
  void push_event(const AdapterEvent& event) noexcept;
  void request_registered_contracts() noexcept;
  void start_reconcile() noexcept;
  void complete_reconcile_if_ready() noexcept;
  void handle_event(const ExecEvent& event) noexcept;
  void finish_queries(api::QueryKind kind) noexcept;
  void finish_balance_queries() noexcept;
  [[nodiscard]] bool enqueue_query(const QueryWaiter& waiter) noexcept;
  static void on_callback(const Callback& callback, void* context) noexcept;

  Session session_;
  ExecClient* client_{};
  AdapterEventSink sink_{};
  std::array<AdapterEvent, 160> pending_{};
  std::size_t pending_count_{};
  std::size_t query_waiter_capacity_{8};
  std::uint64_t query_deadline_ns_{};
  std::array<QueryWaiter, 8> order_waiters_{};
  std::array<QueryWaiter, 8> position_waiters_{};
  std::array<QueryWaiter, 8> balance_waiters_{};
  std::size_t order_waiter_count_{};
  std::size_t position_waiter_count_{};
  std::size_t balance_waiter_count_{};
  std::array<BalanceValue, 16> balance_values_{};
  std::size_t balance_value_count_{};
  bool orders_in_flight_{};
  bool positions_in_flight_{};
  bool orders_requery_needed_{};
  bool positions_requery_needed_{};
  bool balances_in_flight_{};
  int balance_request_id_{};
  bool executions_in_flight_{};
  bool orders_done_{};
  bool positions_done_{};
  bool executions_done_{};
  bool reconcile_emitted_{};
  int next_request_id_{1};
  int contract_requests_{};
  std::int64_t detail_con_id_{};
  Contract pending_details_{};
  bool reserved_{};
  bool connect_started_{};
  std::uint64_t generation_{1};
  std::uint64_t now_ns_{};
  int port_{4002};
  std::string host_;
  std::string strategy_order_ref_prefix_;
};

class IbkrWakeDriver final : public AsyncIoDriver {
 public:
  explicit IbkrWakeDriver(ExecClient& client) noexcept;

  [[nodiscard]] std::size_t descriptor_capacity() const noexcept override;
  [[nodiscard]] std::size_t snapshot_descriptors(
      std::span<AsyncIoDescriptor> output) const noexcept override;
  void service_io(int fd, std::uint64_t generation, std::uint32_t events,
                  std::uint64_t now_ns) noexcept override;
  void check_timeouts(std::uint64_t now_ns) noexcept override;
  [[nodiscard]] std::uint64_t next_deadline_ns() const noexcept override;

 private:
  ExecClient& client_;
  std::uint64_t generation_{1};
};

}  // namespace oms::exchange::ibkr
