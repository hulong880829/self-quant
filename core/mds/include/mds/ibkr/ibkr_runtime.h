#pragma once

#include "mds/ibkr/ibkr_client.h"
#include "utils/md/types.h"

#include <atomic>
#include <cstdint>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <vector>

namespace mds::ibkr {

enum class SessionState : std::uint8_t {
  Disconnected = 0,
  Connecting,
  Handshaking,
  ResolvingContracts,
  ProbingMetrics,
  Subscribing,
  Live,
  Reconnecting,
};

inline constexpr std::size_t kDefaultMaxSubscriptions = 512;
inline constexpr std::size_t kMetricProbeConcurrency = 5;
inline constexpr int kMetricProbeTimeoutMs = 5000;
inline constexpr std::size_t kMarketCapCanaryCount = 5;

struct SubscriptionFilterOptions {
  std::optional<double> min_average_volume_90d;
  std::optional<double> min_market_cap_usd;
  std::size_t max_subscriptions{kDefaultMaxSubscriptions};
};

struct InstrumentSpec {
  std::string canonical_symbol;
  // Symbol sent to IB. For CASH this is the first three letters of the pair
  // (USDJPY -> USD). canonical_symbol stays the full pair.
  std::string request_symbol;
  std::int64_t con_id{};
  std::string sec_type{"STK"};
  std::string exchange{"SMART"};
  std::string primary_exchange;
  std::string currency{"USD"};
  // nullopt means derive from ContractDetails minTick (CMDTY/CASH).
  std::optional<std::uint8_t> price_scale{4};
  std::uint8_t quantity_scale{4};
};

struct InstrumentTurnoverMetric {
  std::int64_t con_id{};
  std::string canonical_symbol;
  double turnover_usd_90d{};
};

using PersistTurnoverMetrics =
    std::function<bool(const std::vector<InstrumentTurnoverMetric> &,
                       std::size_t &, std::string &)>;

struct RuntimeOptions {
  std::string host{"127.0.0.1"};
  int port{4001};
  int client_id{1};
  int connect_timeout_ms{10000};
  int reconnect_initial_ms{1000};
  int reconnect_max_ms{30000};
  int heartbeat_interval_ms{1000};
  int subscribe_pacing_per_second{20};
  // Test seam. Production starts at 2_000_000 and never wraps.
  int contract_req_id_origin{2'000'000};
  std::string shm_prefix{"/selfquant.mds.ibkr"};
  std::size_t ring_bytes{1U << 20U};
  std::size_t max_record_bytes{65536};
  std::size_t max_readers{8};
  bool unlink_on_shutdown{false};
  SubscriptionFilterOptions filter;
  std::vector<InstrumentSpec> subscriptions;
  PersistTurnoverMetrics persist_turnover_metrics;
};

struct RuntimeMetrics {
  std::uint64_t publish_count{};
  std::uint64_t mailbox_overwrite_count{};
  std::uint64_t wake_syscall_count{};
  std::uint64_t snapshot_retry_count{};
  std::uint64_t catalog_failures{};
  std::uint64_t invalid_tick_count{};
  std::uint64_t non_live_count{};
  std::uint64_t informational_errors{};
  std::uint64_t metric_success_average_volume{};
  std::uint64_t metric_success_market_cap{};
  std::uint64_t metric_success_reference_price{};
  std::uint64_t metric_missing_reference_price{};
  std::uint64_t metric_persisted_turnover{};
  std::uint64_t filtered_below_threshold{};
  std::uint64_t missing_metric_rejected{};
  std::uint64_t non_stock_exempt{};
  std::uint64_t truncated{};
  std::uint64_t selected_non_stock{};
  std::uint64_t selected_stock{};
  std::uint64_t selected_subscriptions{};
  std::uint64_t active_subscriptions{};
  std::uint64_t catalog_publish_count{};
  std::uint64_t bbo_publish_count{};
  std::uint64_t session_generation{};
  std::uint64_t stale_contract_callbacks{};
  bool market_cap_filter_degraded{false};
};

class IbkrRuntime : public IbkrClientListener {
 public:
  IbkrRuntime(RuntimeOptions options, IbkrClient &client);
  ~IbkrRuntime() override;

  IbkrRuntime(const IbkrRuntime &) = delete;
  IbkrRuntime &operator=(const IbkrRuntime &) = delete;

  [[nodiscard]] bool start(std::string &error);
  void stop() noexcept;

  [[nodiscard]] std::string segment_name() const { return segment_name_; }
  [[nodiscard]] utils::md::InstrumentId instrument_id(std::size_t index) const;
  [[nodiscard]] std::uint32_t generation() const noexcept;
  [[nodiscard]] SessionState state() const noexcept;
  [[nodiscard]] bool live() const noexcept;
  [[nodiscard]] bool failed() const noexcept;
  [[nodiscard]] int exit_code() const noexcept;
  [[nodiscard]] std::string failure_message() const;
  [[nodiscard]] RuntimeMetrics metrics() const noexcept;

  void on_connected() noexcept override;
  void on_disconnected() noexcept override;
  void on_contract_details(int req_id,
                           const IbkrContractDetails &details) noexcept override;
  void on_contract_details_end(int req_id) noexcept override;
  void on_tick_price(int ticker_id, bool bid, double price,
                     std::uint64_t receive_tsc) noexcept override;
  void on_reference_price(int ticker_id, ReferencePriceSource source,
                          double price) noexcept override;
  void on_tick_size(int ticker_id, bool bid, std::string_view size_text,
                    std::uint64_t receive_tsc) noexcept override;
  void on_average_volume(int ticker_id,
                         std::string_view size_text) noexcept override;
  void on_fundamental_ratios(int ticker_id,
                             std::string_view ratios) noexcept override;
  void on_market_data_type(int ticker_id, MarketDataKind kind) noexcept override;
  void on_error(int id, int code, std::string_view message) noexcept override;

 private:
  struct Impl;
  RuntimeOptions options_;
  IbkrClient &client_;
  std::string segment_name_;
  std::unique_ptr<Impl> impl_;
};

}  // namespace mds::ibkr
