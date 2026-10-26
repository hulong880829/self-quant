#include "oms/exchange/ibkr/tws_exec_client.h"

#if !defined(OMS_HAS_IBKR)
#error "tws_exec_client.cpp requires OMS_HAS_IBKR"
#endif

#include "Contract.h"
#include "Decimal.h"
#include "DefaultEWrapper.h"
#include "EClientSocket.h"
#include "EReader.h"
#include "EReaderOSSignal.h"
#include "Execution.h"
#include "CommissionAndFeesReport.h"
#include "Order.h"
#include "OrderCancel.h"

#include <atomic>
#include <cerrno>
#include <cstring>
#include <exception>
#include <iostream>
#include <limits>
#include <sys/eventfd.h>
#include <thread>
#include <unistd.h>

namespace oms::exchange::ibkr {
namespace {

void copy_text(char* destination, std::size_t capacity, std::string_view value) {
  const std::size_t count =
      value.size() < capacity - 1 ? value.size() : capacity - 1;
  if (count != 0) std::memcpy(destination, value.data(), count);
  destination[count] = '\0';
}

class Queue {
 public:
  Queue() {
    event_fd_ = ::eventfd(0, EFD_CLOEXEC | EFD_NONBLOCK);
  }
  ~Queue() {
    if (event_fd_ >= 0) ::close(event_fd_);
  }

  [[nodiscard]] int event_fd() const noexcept { return event_fd_; }
  [[nodiscard]] bool overflowed() const noexcept {
    return overflow_.load(std::memory_order_acquire);
  }
  void set_generation(std::uint64_t generation) noexcept {
    generation_.store(generation, std::memory_order_release);
  }

  bool push(ExecEvent event) noexcept {
    event.generation = generation_.load(std::memory_order_acquire);
    const std::size_t write = write_.load(std::memory_order_relaxed);
    const std::size_t read = read_.load(std::memory_order_acquire);
    if (write - read >= kExecEventCapacity) {
      overflow_.store(true, std::memory_order_release);
      return false;
    }
    slots_[write % kExecEventCapacity] = event;
    write_.store(write + 1, std::memory_order_release);
    const std::uint64_t one = 1;
    if (event_fd_ >= 0) {
      const ssize_t written = ::write(event_fd_, &one, sizeof(one));
      if (written < 0 && errno != EAGAIN) overflow_.store(true, std::memory_order_release);
    }
    return true;
  }

  bool pop(ExecEvent& event) noexcept {
    const std::size_t write = write_.load(std::memory_order_acquire);
    const std::size_t read = read_.load(std::memory_order_relaxed);
    if (read == write) return false;
    event = slots_[read % kExecEventCapacity];
    read_.store(read + 1, std::memory_order_release);
    return true;
  }

 private:
  ExecEvent slots_[kExecEventCapacity]{};
  std::atomic<std::size_t> write_{0};
  std::atomic<std::size_t> read_{0};
  std::atomic<bool> overflow_{false};
  std::atomic<std::uint64_t> generation_{1};
  int event_fd_{-1};
};

class Wrapper final : public DefaultEWrapper {
 public:
  explicit Wrapper(Queue& queue) : queue_(queue) {}

  void nextValidId(int order_id) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::NextValidId;
    event.order_id = order_id;
    (void)queue_.push(event);
  }

  void commissionAndFeesReport(const CommissionAndFeesReport& report) override {
    static std::atomic<int> seen{0};
    const int index = seen.fetch_add(1, std::memory_order_relaxed);
    if (index < 8 || index % 64 == 0) {
      std::cerr << "ibkr commission exec=" << report.execId
                << " fee=" << report.commissionAndFees
                << " currency=" << report.currency << '\n';
    }
  }

  void connectionClosed() override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::ConnectionClosed;
    (void)queue_.push(event);
  }

  void error(int id, time_t, int error_code, const std::string& message,
             const std::string&) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::Error;
    event.order_id = id;
    event.error_code = error_code;
    copy_text(event.message, sizeof(event.message), message);
    (void)queue_.push(event);
  }

  void contractDetails(int request_id, const ContractDetails& details) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::ContractDetails;
    event.request_id = request_id;
    event.con_id = details.contract.conId;
    event.min_tick = details.minTick;
    copy_text(event.sec_type, sizeof(event.sec_type), details.contract.secType);
    copy_text(event.currency, sizeof(event.currency), details.contract.currency);
    copy_text(event.multiplier, sizeof(event.multiplier),
              details.contract.multiplier);
    copy_text(event.min_size, sizeof(event.min_size),
              DecimalFunctions::decimalToString(details.minSize));
    copy_text(event.size_increment, sizeof(event.size_increment),
              DecimalFunctions::decimalToString(details.sizeIncrement));
    (void)queue_.push(event);
  }

  void contractDetailsEnd(int request_id) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::ContractDetailsEnd;
    event.request_id = request_id;
    (void)queue_.push(event);
  }

  void openOrder(int order_id, const Contract& contract, const Order& order,
                 const OrderState&) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::OpenOrder;
    event.order_id = order_id;
    event.client_id = order.clientId;
    event.perm_id = static_cast<long>(order.permId);
    event.con_id = contract.conId;
    event.limit_price = order.lmtPrice;
    copy_text(event.account, sizeof(event.account), order.account);
    copy_text(event.order_ref, sizeof(event.order_ref), order.orderRef);
    copy_text(event.action, sizeof(event.action), order.action);
    copy_text(event.quantity, sizeof(event.quantity),
              DecimalFunctions::decimalToString(order.totalQuantity));
    (void)queue_.push(event);
  }

  void openOrderEnd() override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::OpenOrderEnd;
    (void)queue_.push(event);
  }

  void orderStatus(int order_id, const std::string& status, Decimal filled,
                   Decimal remaining, double avg_price, long long perm_id,
                   int, double, int client_id, const std::string&,
                   double) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::OrderStatus;
    event.order_id = order_id;
    event.client_id = client_id;
    event.perm_id = static_cast<long>(perm_id);
    event.limit_price = avg_price;
    copy_text(event.status, sizeof(event.status), status);
    copy_text(event.filled, sizeof(event.filled),
              DecimalFunctions::decimalToString(filled));
    copy_text(event.remaining, sizeof(event.remaining),
              DecimalFunctions::decimalToString(remaining));
    (void)queue_.push(event);
  }

  void execDetails(int request_id, const Contract& contract,
                   const Execution& execution) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::ExecDetails;
    event.request_id = request_id;
    event.order_id = execution.orderId;
    event.client_id = execution.clientId;
    event.perm_id = static_cast<long>(execution.permId);
    event.con_id = contract.conId;
    event.limit_price = execution.price;
    copy_text(event.exec_id, sizeof(event.exec_id), execution.execId);
    copy_text(event.quantity, sizeof(event.quantity),
              DecimalFunctions::decimalToString(execution.shares));
    copy_text(event.account, sizeof(event.account), execution.acctNumber);
    (void)queue_.push(event);
  }

  void execDetailsEnd(int request_id) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::ExecutionEnd;
    event.request_id = request_id;
    (void)queue_.push(event);
  }

  void position(const std::string& account, const Contract& contract,
                Decimal position, double) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::Position;
    event.con_id = contract.conId;
    event.position = 0;
    copy_text(event.account, sizeof(event.account), account);
    copy_text(event.quantity, sizeof(event.quantity),
              DecimalFunctions::decimalToString(position));
    (void)queue_.push(event);
  }

  void positionEnd() override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::PositionEnd;
    (void)queue_.push(event);
  }

  void updateAccountValue(const std::string& tag, const std::string& value,
                          const std::string& currency,
                          const std::string& account) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::AccountValue;
    copy_text(event.account, sizeof(event.account), account);
    copy_text(event.tag, sizeof(event.tag), tag);
    copy_text(event.value, sizeof(event.value), value);
    copy_text(event.currency, sizeof(event.currency), currency);
    (void)queue_.push(event);
  }

  void accountDownloadEnd(const std::string& account) override {
    ExecEvent event;
    event.kind = ExecEvent::Kind::AccountDownloadEnd;
    copy_text(event.account, sizeof(event.account), account);
    (void)queue_.push(event);
  }

 private:
  Queue& queue_;
};

}  // namespace

struct TwsExecClient::Impl {
  Queue queue;
  Wrapper wrapper;
  EReaderOSSignal signal{2000};
  EClientSocket socket;
  std::unique_ptr<EReader> reader;
  std::thread reader_thread;
  std::atomic<bool> stop_reader{false};

  Impl() : wrapper(queue), socket(&wrapper, &signal) {}
  ~Impl() { stop(); }

  void stop() noexcept {
    stop_reader.store(true, std::memory_order_release);
    signal.issueSignal();
    if (socket.isConnected()) socket.eDisconnect();
    if (reader_thread.joinable()) reader_thread.join();
    reader.reset();
  }
};

TwsExecClient::TwsExecClient() : impl_(std::make_unique<Impl>()) {}
TwsExecClient::~TwsExecClient() { disconnect(); }

bool TwsExecClient::connect(std::string_view host, int port, int client_id) {
  disconnect();
  impl_->stop_reader.store(false, std::memory_order_release);
  if (!impl_->socket.eConnect(std::string(host).c_str(), port, client_id))
    return false;
  impl_->reader = std::make_unique<EReader>(&impl_->socket, &impl_->signal);
  impl_->reader->start();
  impl_->reader_thread = std::thread([this] {
    while (!impl_->stop_reader.load(std::memory_order_acquire)) {
      impl_->signal.waitForSignal();
      if (impl_->reader != nullptr) impl_->reader->processMsgs();
    }
  });
  return true;
}

void TwsExecClient::disconnect() noexcept {
  if (impl_ != nullptr) impl_->stop();
}

bool TwsExecClient::connected() const noexcept {
  return impl_->socket.isConnected();
}

int TwsExecClient::event_fd() const noexcept { return impl_->queue.event_fd(); }

bool TwsExecClient::request_contract_details(int request_id, std::int64_t con_id) {
  if (!connected()) return false;
  if (con_id <= 0 || con_id > std::numeric_limits<int>::max()) return false;
  Contract contract;
  contract.conId = static_cast<int>(con_id);
  impl_->socket.reqContractDetails(request_id, contract);
  return true;
}

bool TwsExecClient::place_limit(int order_id, std::int64_t con_id,
                                std::string_view action, std::string_view quantity,
                                std::string_view limit_price, std::string_view tif,
                                std::string_view account, std::string_view order_ref,
                                std::string_view good_till_utc) {
  if (!connected()) return false;
  if (con_id <= 0 || con_id > std::numeric_limits<int>::max()) return false;
  Contract contract;
  contract.conId = static_cast<int>(con_id);
  Order order;
  order.action = std::string(action);
  order.orderType = "LMT";
  order.totalQuantity = DecimalFunctions::stringToDecimal(std::string(quantity));
  try {
    order.lmtPrice = std::stod(std::string(limit_price));
  } catch (const std::exception&) {
    return false;
  }
  order.tif = std::string(tif);
  order.account = std::string(account);
  order.orderRef = std::string(order_ref);
  order.transmit = true;
  if (!good_till_utc.empty()) order.goodTillDate = std::string(good_till_utc);
  impl_->socket.placeOrder(order_id, contract, order);
  return true;
}

bool TwsExecClient::cancel_order(int order_id) {
  if (!connected()) return false;
  impl_->socket.cancelOrder(order_id, OrderCancel{});
  return true;
}

bool TwsExecClient::request_all_open_orders() {
  if (!connected()) return false;
  impl_->socket.reqAllOpenOrders();
  return true;
}

bool TwsExecClient::request_positions() {
  if (!connected()) return false;
  impl_->socket.reqPositions();
  return true;
}

bool TwsExecClient::request_account_updates(int, bool subscribe,
                                            std::string_view account) {
  if (!connected()) return false;
  impl_->socket.reqAccountUpdates(subscribe, std::string(account));
  return true;
}

bool TwsExecClient::request_executions(std::string_view account) {
  if (!connected()) return false;
  ExecutionFilter filter;
  filter.m_clientId = 0;
  filter.m_acctCode = std::string(account);
  impl_->socket.reqExecutions(1, filter);
  return true;
}

bool TwsExecClient::request_ids() {
  if (!connected()) return false;
  impl_->socket.reqIds(-1);
  return true;
}

bool TwsExecClient::pop(ExecEvent& event) noexcept { return impl_->queue.pop(event); }

void TwsExecClient::set_generation(std::uint64_t generation) noexcept {
  impl_->queue.set_generation(generation);
}

bool TwsExecClient::overflowed() const noexcept { return impl_->queue.overflowed(); }

}  // namespace oms::exchange::ibkr
