#include "oms/exchange/ibkr/exec_client.h"

namespace oms::exchange::ibkr {

bool FakeExecClient::connect(std::string_view, int, int client_id) {
  if (fail_next_) {
    fail_next_ = false;
    return false;
  }
  connected_ = true;
  disconnected_ = false;
  client_id_ = client_id;
  ++connect_count_;
  return true;
}

void FakeExecClient::disconnect() noexcept {
  connected_ = false;
  disconnected_ = true;
}

bool FakeExecClient::connected() const noexcept { return connected_; }

bool FakeExecClient::request_contract_details(int request_id,
                                              std::int64_t con_id) {
  if (!connected_) return false;
  if (fail_contract_details_) {
    fail_contract_details_ = false;
    return false;
  }
  ++contract_requests_;
  last_contract_request_ = request_id;
  last_con_id_ = con_id;
  return true;
}

bool FakeExecClient::place_limit(int order_id, std::int64_t con_id,
                                 std::string_view, std::string_view quantity,
                                 std::string_view limit_price,
                                 std::string_view tif, std::string_view,
                                 std::string_view order_ref,
                                 std::string_view good_till_utc) {
  if (!connected_) return false;
  last_order_id_ = order_id;
  last_con_id_ = con_id;
  last_quantity_.assign(quantity);
  last_price_.assign(limit_price);
  last_tif_.assign(tif);
  last_order_ref_.assign(order_ref);
  last_good_till_.assign(good_till_utc);
  return true;
}

bool FakeExecClient::cancel_order(int order_id) {
  if (!connected_) return false;
  last_order_id_ = order_id;
  return true;
}

bool FakeExecClient::request_all_open_orders() {
  if (!connected_) return false;
  ++open_order_requests_;
  return true;
}

bool FakeExecClient::request_positions() { return connected_; }

bool FakeExecClient::request_account_updates(int request_id, bool,
                                             std::string_view) {
  if (!connected_) return false;
  ++account_summary_requests_;
  last_account_summary_request_ = request_id;
  return true;
}

bool FakeExecClient::request_executions(std::string_view) { return connected_; }

bool FakeExecClient::request_ids() {
  if (!connected_) return false;
  ++id_requests_;
  return true;
}

bool FakeExecClient::pop(ExecEvent& event) noexcept {
  if (read_ == write_) return false;
  event = queue_[read_ % kExecEventCapacity];
  ++read_;
  return true;
}

bool FakeExecClient::push(const ExecEvent& event) noexcept {
  if (write_ - read_ >= kExecEventCapacity) return false;
  queue_[write_ % kExecEventCapacity] = event;
  ++write_;
  return true;
}

}  // namespace oms::exchange::ibkr
