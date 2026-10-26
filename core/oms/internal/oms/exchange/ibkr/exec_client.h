#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

namespace oms::exchange::ibkr {

inline constexpr std::size_t kExecEventCapacity = 64;
inline constexpr std::size_t kOrderRefCapacity = 32;

struct ExecEvent {
  enum class Kind : std::uint8_t {
    NextValidId = 1,
    Error,
    ConnectionClosed,
    ContractDetails,
    ContractDetailsEnd,
    OpenOrder,
    OpenOrderEnd,
    OrderStatus,
    ExecDetails,
    Position,
    PositionEnd,
    ExecutionEnd,
    AccountValue,
    AccountDownloadEnd,
  };

  Kind kind{Kind::Error};
  int request_id{};
  int error_code{};
  int client_id{};
  int order_id{};
  long perm_id{};
  std::int64_t con_id{};
  std::int64_t position{};
  double min_tick{};
  double limit_price{};
  char account[24]{};
  char sec_type[8]{};
  char currency[8]{};
  char tag[32]{};
  char value[32]{};
  char multiplier[24]{};
  char min_size[32]{};
  char size_increment[32]{};
  char quantity[32]{};
  char filled[32]{};
  char remaining[32]{};
  char price[32]{};
  char exec_id[48]{};
  char order_ref[40]{};
  char status[24]{};
  char action[8]{};
  char message[96]{};
  std::uint64_t generation{};
};

class ExecClient {
 public:
  virtual ~ExecClient() = default;

  virtual bool connect(std::string_view host, int port, int client_id) = 0;
  virtual void disconnect() noexcept = 0;
  [[nodiscard]] virtual bool connected() const noexcept = 0;
  [[nodiscard]] virtual int event_fd() const noexcept { return -1; }
  virtual bool request_contract_details(int request_id,
                                        std::int64_t con_id) = 0;
  virtual bool place_limit(int order_id, std::int64_t con_id,
                           std::string_view action, std::string_view quantity,
                           std::string_view limit_price, std::string_view tif,
                           std::string_view account, std::string_view order_ref,
                           std::string_view good_till_utc) = 0;
  virtual bool cancel_order(int order_id) = 0;
  virtual bool request_all_open_orders() = 0;
  virtual bool request_positions() = 0;
  virtual bool request_account_updates(int request_id, bool subscribe,
                                       std::string_view account) = 0;
  virtual bool request_executions(std::string_view account) = 0;
  virtual bool request_ids() = 0;
  [[nodiscard]] virtual bool pop(ExecEvent& event) noexcept = 0;
  virtual void set_generation(std::uint64_t) noexcept {}
  [[nodiscard]] virtual bool overflowed() const noexcept { return false; }
};

class FakeExecClient final : public ExecClient {
 public:
  bool connect(std::string_view host, int port, int client_id) override;
  void disconnect() noexcept override;
  [[nodiscard]] bool connected() const noexcept override;
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

  [[nodiscard]] bool push(const ExecEvent& event) noexcept;
  void set_fail_next(bool fail) noexcept { fail_next_ = fail; }
  void set_fail_contract_details(bool fail) noexcept {
    fail_contract_details_ = fail;
  }
  [[nodiscard]] int contract_requests() const noexcept {
    return contract_requests_;
  }
  [[nodiscard]] int last_contract_request() const noexcept {
    return last_contract_request_;
  }
  [[nodiscard]] std::int64_t last_con_id() const noexcept { return last_con_id_; }
  [[nodiscard]] int last_order_id() const noexcept { return last_order_id_; }
  [[nodiscard]] const std::string& last_quantity() const noexcept {
    return last_quantity_;
  }
  [[nodiscard]] const std::string& last_price() const noexcept {
    return last_price_;
  }
  [[nodiscard]] const std::string& last_tif() const noexcept { return last_tif_; }
  [[nodiscard]] const std::string& last_order_ref() const noexcept {
    return last_order_ref_;
  }
  [[nodiscard]] const std::string& last_good_till() const noexcept {
    return last_good_till_;
  }
  [[nodiscard]] int open_order_requests() const noexcept {
    return open_order_requests_;
  }
  [[nodiscard]] int account_summary_requests() const noexcept {
    return account_summary_requests_;
  }
  [[nodiscard]] int last_account_summary_request() const noexcept {
    return last_account_summary_request_;
  }
  [[nodiscard]] int id_requests() const noexcept { return id_requests_; }
  [[nodiscard]] int connect_count() const noexcept { return connect_count_; }
  [[nodiscard]] bool saw_disconnect() const noexcept { return disconnected_; }

 private:
  bool connected_{};
  bool fail_next_{};
  bool fail_contract_details_{};
  bool disconnected_{};
  int client_id_{};
  int contract_requests_{};
  int last_contract_request_{};
  std::int64_t last_con_id_{};
  int last_order_id_{};
  int open_order_requests_{};
  int account_summary_requests_{};
  int last_account_summary_request_{};
  int id_requests_{};
  int connect_count_{};
  std::string last_quantity_;
  std::string last_price_;
  std::string last_tif_;
  std::string last_order_ref_;
  std::string last_good_till_;
  ExecEvent queue_[kExecEventCapacity]{};
  std::size_t read_{};
  std::size_t write_{};
};

}  // namespace oms::exchange::ibkr
