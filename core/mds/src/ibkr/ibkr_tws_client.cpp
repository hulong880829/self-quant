#include "mds/ibkr/ibkr_tws_client.h"

#if !defined(MDS_HAS_IBKR)
#error "ibkr_tws_client.cpp requires MDS_HAS_IBKR and the official TWS API Stable 10.45"
#endif

#include "Contract.h"
#include "Decimal.h"
#include "DefaultEWrapper.h"
#include "EClientSocket.h"
#include "EReader.h"
#include "EReaderOSSignal.h"

#include <atomic>
#include <ctime>
#include <thread>
#include <utility>

namespace mds::ibkr {
namespace {

MarketDataKind market_data_kind(int type) noexcept {
  switch (type) {
    case 1:
      return MarketDataKind::Live;
    case 2:
      return MarketDataKind::Frozen;
    case 3:
      return MarketDataKind::Delayed;
    case 4:
      return MarketDataKind::DelayedFrozen;
    default:
      return MarketDataKind::Unset;
  }
}

bool is_bid_price(TickType field) noexcept { return field == BID; }
bool is_ask_price(TickType field) noexcept { return field == ASK; }
bool is_bid_size(TickType field) noexcept { return field == BID_SIZE; }
bool is_ask_size(TickType field) noexcept { return field == ASK_SIZE; }

ReferencePriceSource reference_price_source(TickType field) noexcept {
  switch (field) {
    case LAST:
      return ReferencePriceSource::Last;
    case CLOSE:
      return ReferencePriceSource::Close;
    case DELAYED_LAST:
      return ReferencePriceSource::DelayedLast;
    case DELAYED_CLOSE:
      return ReferencePriceSource::DelayedClose;
    default:
      return ReferencePriceSource::None;
  }
}

class Wrapper final : public DefaultEWrapper {
 public:
  std::atomic<IbkrClientListener *> listener{nullptr};

  void nextValidId(int) override {
    if (auto *target = listener.load(std::memory_order_acquire)) {
      target->on_connected();
    }
  }

  void connectionClosed() override {
    if (auto *target = listener.load(std::memory_order_acquire)) {
      target->on_disconnected();
    }
  }

  void error(int id, time_t, int error_code, const std::string &error_string,
             const std::string &) override {
    if (auto *target = listener.load(std::memory_order_acquire)) {
      target->on_error(id, error_code, error_string);
    }
  }

  void contractDetails(int req_id, const ContractDetails &details) override {
    auto *target = listener.load(std::memory_order_acquire);
    if (target == nullptr) {
      return;
    }
    IbkrContractDetails parsed;
    parsed.con_id = details.contract.conId;
    parsed.symbol = details.contract.symbol;
    parsed.local_symbol = details.contract.localSymbol;
    parsed.exchange = details.contract.exchange;
    parsed.primary_exchange = details.contract.primaryExchange;
    parsed.currency = details.contract.currency;
    parsed.sec_type = details.contract.secType;
    parsed.min_tick = details.minTick;
    parsed.min_size_text = DecimalFunctions::decimalToString(details.minSize);
    parsed.size_increment_text =
        DecimalFunctions::decimalToString(details.sizeIncrement);
    parsed.multiplier_text = details.contract.multiplier;
    target->on_contract_details(req_id, parsed);
  }

  void contractDetailsEnd(int req_id) override {
    if (auto *target = listener.load(std::memory_order_acquire)) {
      target->on_contract_details_end(req_id);
    }
  }

  void tickPrice(int ticker_id, TickType field, double price,
                 const TickAttrib &) override {
    auto *target = listener.load(std::memory_order_acquire);
    if (target == nullptr) {
      return;
    }
    if (is_bid_price(field) || is_ask_price(field)) {
      target->on_tick_price(static_cast<int>(ticker_id), is_bid_price(field),
                            price, 0);
      return;
    }
    const auto source = reference_price_source(field);
    if (source != ReferencePriceSource::None) {
      target->on_reference_price(static_cast<int>(ticker_id), source, price);
    }
  }

  void tickSize(int ticker_id, TickType field, Decimal size) override {
    auto *target = listener.load(std::memory_order_acquire);
    if (target == nullptr) {
      return;
    }
    const auto text = DecimalFunctions::decimalToString(size);
    if (field == AVG_VOLUME) {
      target->on_average_volume(static_cast<int>(ticker_id), text);
      return;
    }
    if (!is_bid_size(field) && !is_ask_size(field)) {
      return;
    }
    target->on_tick_size(static_cast<int>(ticker_id), is_bid_size(field), text, 0);
  }

  void tickString(int ticker_id, TickType field,
                  const std::string &value) override {
    if (field == FUNDAMENTAL_RATIOS) {
      if (auto *target = listener.load(std::memory_order_acquire)) {
        target->on_fundamental_ratios(static_cast<int>(ticker_id), value);
      }
    }
  }

  void marketDataType(int ticker_id, int type) override {
    if (auto *target = listener.load(std::memory_order_acquire)) {
      target->on_market_data_type(static_cast<int>(ticker_id),
                                  market_data_kind(type));
    }
  }
};

Contract make_contract(const IbkrContractRequest &request) {
  Contract contract;
  contract.conId = static_cast<long>(request.con_id);
  contract.symbol = request.symbol;
  contract.secType = request.sec_type;
  contract.exchange = request.exchange;
  contract.primaryExchange = request.primary_exchange;
  contract.currency = request.currency;
  return contract;
}

}  // namespace

struct TwsIbkrClient::Impl {
  Wrapper wrapper;
  EReaderOSSignal signal{2000};
  EClientSocket socket;
  std::unique_ptr<EReader> reader;
  std::thread reader_thread;
  std::atomic<bool> stop_reader{false};

  Impl() : socket(&wrapper, &signal) {}

  ~Impl() { stop(); }

  void stop() noexcept {
    stop_reader.store(true, std::memory_order_release);
    signal.issueSignal();
    if (socket.isConnected()) {
      socket.eDisconnect();
    }
    if (reader_thread.joinable()) {
      reader_thread.join();
    }
    reader.reset();
  }
};

TwsIbkrClient::TwsIbkrClient() : impl_(std::make_unique<Impl>()) {}

TwsIbkrClient::~TwsIbkrClient() { disconnect(); }

void TwsIbkrClient::set_listener(IbkrClientListener *listener) noexcept {
  impl_->wrapper.listener.store(listener, std::memory_order_release);
}

bool TwsIbkrClient::connect(const IbkrConnectParams &params, std::string &error) {
  disconnect();
  impl_->stop_reader.store(false, std::memory_order_release);
  if (!impl_->socket.eConnect(params.host.c_str(), params.port, params.client_id)) {
    error = "failed to connect to IB Gateway";
    return false;
  }
  impl_->reader = std::make_unique<EReader>(&impl_->socket, &impl_->signal);
  impl_->reader->start();
  impl_->reader_thread = std::thread([this] {
    while (!impl_->stop_reader.load(std::memory_order_acquire)) {
      impl_->signal.waitForSignal();
      if (impl_->reader != nullptr) {
        impl_->reader->processMsgs();
      }
    }
  });
  return true;
}

void TwsIbkrClient::disconnect() noexcept {
  if (impl_ != nullptr) {
    impl_->stop();
  }
}

void TwsIbkrClient::select_live_market_data() {
  impl_->socket.reqMarketDataType(1);
}

bool TwsIbkrClient::request_contract_details(int req_id,
                                             const IbkrContractRequest &request,
                                             std::string &error) {
  if (!impl_->socket.isConnected()) {
    error = "IB Gateway is not connected";
    return false;
  }
  impl_->socket.reqContractDetails(req_id, make_contract(request));
  return true;
}

bool TwsIbkrClient::request_market_data(int ticker_id,
                                        const IbkrContractRequest &request,
                                        std::string_view generic_ticks,
                                        std::string &error) {
  if (!impl_->socket.isConnected()) {
    error = "IB Gateway is not connected";
    return false;
  }
  impl_->socket.reqMktData(ticker_id, make_contract(request),
                           std::string(generic_ticks), false, false,
                           TagValueListSPtr());
  return true;
}

void TwsIbkrClient::cancel_market_data(int ticker_id) noexcept {
  if (impl_->socket.isConnected()) {
    impl_->socket.cancelMktData(ticker_id);
  }
}

void TwsIbkrClient::pump(std::chrono::milliseconds timeout) {
  std::this_thread::sleep_for(timeout);
}

}  // namespace mds::ibkr
