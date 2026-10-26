#pragma once

#include "mds/ibkr/ibkr_bbo_assembler.h"

#include <chrono>
#include <cstdint>
#include <string>
#include <string_view>

namespace mds::ibkr {

enum class IbkrErrorClass : std::uint8_t {
  Ignored = 0,
  ConnectionLost,
  ConnectionRestoredDataLost,
  ConnectionRestoredDataMaintained,
  Contract,
  MarketDataDenied,
  Informational,
};

[[nodiscard]] IbkrErrorClass classify_ibkr_error(int code) noexcept;

struct IbkrConnectParams {
  std::string host{"127.0.0.1"};
  int port{4001};
  int client_id{1};
  int timeout_ms{10000};
};

struct IbkrContractRequest {
  std::int64_t con_id{};
  std::string symbol;
  std::string sec_type{"STK"};
  std::string exchange{"SMART"};
  std::string primary_exchange;
  std::string currency{"USD"};
};

struct IbkrContractDetails {
  std::int64_t con_id{};
  std::string symbol;
  std::string local_symbol;
  std::string exchange;
  std::string primary_exchange;
  std::string currency;
  std::string sec_type;
  double min_tick{};
  // Empty text means the venue did not return the field.
  std::string min_size_text;
  std::string size_increment_text;
  std::string multiplier_text;
};

enum class ReferencePriceSource : std::uint8_t {
  None = 0,
  DelayedLast,
  Last,
  DelayedClose,
  Close,
};

class IbkrClientListener {
 public:
  virtual ~IbkrClientListener() = default;
  virtual void on_connected() noexcept = 0;
  virtual void on_disconnected() noexcept = 0;
  virtual void on_contract_details(int req_id,
                                   const IbkrContractDetails &details) noexcept = 0;
  virtual void on_contract_details_end(int req_id) noexcept = 0;
  virtual void on_tick_price(int ticker_id, bool bid, double price,
                             std::uint64_t receive_tsc) noexcept = 0;
  virtual void on_reference_price(int ticker_id, ReferencePriceSource source,
                                  double price) noexcept = 0;
  virtual void on_tick_size(int ticker_id, bool bid, std::string_view size_text,
                            std::uint64_t receive_tsc) noexcept = 0;
  virtual void on_average_volume(int ticker_id,
                                 std::string_view size_text) noexcept = 0;
  virtual void on_fundamental_ratios(int ticker_id,
                                     std::string_view ratios) noexcept = 0;
  virtual void on_market_data_type(int ticker_id,
                                   MarketDataKind kind) noexcept = 0;
  virtual void on_error(int id, int code, std::string_view message) noexcept = 0;
};

class IbkrClient {
 public:
  virtual ~IbkrClient() = default;
  virtual void set_listener(IbkrClientListener *listener) noexcept = 0;
  virtual bool connect(const IbkrConnectParams &params, std::string &error) = 0;
  virtual void disconnect() noexcept = 0;
  virtual void select_live_market_data() = 0;
  virtual bool request_contract_details(int req_id,
                                        const IbkrContractRequest &request,
                                        std::string &error) = 0;
  virtual bool request_market_data(int ticker_id,
                                   const IbkrContractRequest &request,
                                   std::string_view generic_ticks,
                                   std::string &error) = 0;
  virtual void cancel_market_data(int ticker_id) noexcept = 0;
  virtual void pump(std::chrono::milliseconds timeout) = 0;
};

}  // namespace mds::ibkr
