#pragma once

#include "oms/exchange/ibkr/exec_client.h"

#include <memory>

namespace oms::exchange::ibkr {

// Official TWS API Stable 10.45 execution client. Compiled only when
// SELF_QUANT_ENABLE_IBKR_OMS=ON. Callbacks are copied into a bounded queue.
class TwsExecClient final : public ExecClient {
 public:
  TwsExecClient();
  ~TwsExecClient() override;

  bool connect(std::string_view host, int port, int client_id) override;
  void disconnect() noexcept override;
  [[nodiscard]] bool connected() const noexcept override;
  [[nodiscard]] int event_fd() const noexcept override;
  bool request_contract_details(int request_id, std::int64_t con_id) override;
  bool place_limit(int order_id, std::int64_t con_id, std::string_view action,
                   std::string_view quantity, std::string_view limit_price,
                   std::string_view tif, std::string_view account,
                   std::string_view order_ref,
                   std::string_view good_till_utc) override;
  bool cancel_order(int order_id) override;
  bool request_all_open_orders() override;
  bool request_positions() override;
  bool request_account_updates(int request_id, bool subscribe,
                               std::string_view account) override;
  bool request_executions(std::string_view account) override;
  bool request_ids() override;
  [[nodiscard]] bool pop(ExecEvent& event) noexcept override;
  void set_generation(std::uint64_t generation) noexcept override;
  [[nodiscard]] bool overflowed() const noexcept override;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace oms::exchange::ibkr
