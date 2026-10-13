#pragma once

#include "mds/ibkr/ibkr_client.h"

#include <memory>

namespace mds::ibkr {

// Official TWS API Stable 10.45 wrapper. Compiled only when
// SELF_QUANT_ENABLE_IBKR_MDS=ON.
class TwsIbkrClient final : public IbkrClient {
 public:
  TwsIbkrClient();
  ~TwsIbkrClient() override;

  void set_listener(IbkrClientListener *listener) noexcept override;
  bool connect(const IbkrConnectParams &params, std::string &error) override;
  void disconnect() noexcept override;
  void select_live_market_data() override;
  bool request_contract_details(int req_id, const IbkrContractRequest &request,
                                std::string &error) override;
  bool request_market_data(int ticker_id, const IbkrContractRequest &request,
                           std::string_view generic_ticks,
                           std::string &error) override;
  void cancel_market_data(int ticker_id) noexcept override;
  void pump(std::chrono::milliseconds timeout) override;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace mds::ibkr
