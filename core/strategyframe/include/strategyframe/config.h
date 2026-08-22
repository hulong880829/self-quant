#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

#include "strategyframe/error.h"
#include "strategyframe/types.h"

namespace strategyframe {

enum class MdsSourceMode : std::uint8_t {
  ExternalSharedMemory = 1,
  SelfHosted = 2,
  Replay = 3,
};
enum class ThreadingMode : std::uint8_t {
  SingleThread = 1,
  MultiIoThread = 2,
};
enum class IdlePolicy : std::uint8_t {
  BusySpin = 1,
  Adaptive = 2,
  LowCpu = 3,
};
enum class ConfigStrictness : std::uint8_t { Relaxed = 1, Strict = 2 };

struct EndpointConfig {
  std::string host{};
  std::string service{"443"};
};

struct SegmentConfig {
  std::string name{};
  std::size_t ring_bytes{8U << 20U};
  std::size_t max_record_bytes{64U << 10U};
  std::uint32_t expected_reader_budget{1};
  std::uint64_t heartbeat_interval_ns{500'000'000ULL};
};

struct MdsConfig {
  MdsSourceMode source{MdsSourceMode::ExternalSharedMemory};
  std::vector<SegmentConfig> segments{};
  std::string producer_config_path{};
  std::vector<InstrumentSelector> required_instruments{};
};

struct VenueExecutionConfig {
  enum class Kind : std::uint8_t {
    BinanceSpot = 1,
    BinanceUsdm = 2,
    Polymarket = 3,
  };

  Kind kind{Kind::BinanceSpot};
  AccountId account_id{};
  bool enabled{};
  EndpointConfig rest{};
  EndpointConfig trading_websocket{};
  EndpointConfig user_websocket{};
  std::string api_key_env{};
  std::string secret_env{};
  std::string passphrase_env{};
  std::string signer_address_env{};
  std::string funder_address_env{};
  std::string private_key_env{};
};

struct CapacityConfig {
  std::uint32_t command_queue{1024};
  std::uint32_t update_queue{4096};
  std::uint32_t market_data_queue{4096};
  std::uint32_t order_table{4096};
  std::uint32_t position_table{1024};
  std::uint32_t fill_dedup{8192};
  std::uint32_t timer_table{1024};
};

struct MemoryConfig {
  bool lock_pages{};
  bool prefault{true};
  bool strict{};
};

struct SocketConfig {
  bool tcp_nodelay{true};
  std::int32_t receive_buffer_bytes{};
  std::int32_t send_buffer_bytes{};
  std::int32_t busy_poll_us{};
};

struct InstrumentConfig {
  InstrumentId instrument_id{};
  Venue venue{Venue::Unknown};
  ProductType product{ProductType::Unknown};
  std::string symbol{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
  std::string polymarket_condition_id{};
  std::string polymarket_token_id{};
  std::uint8_t polymarket_outcome{};
  bool polymarket_negative_risk{};
  std::uint8_t polymarket_signature_type{};
  std::int64_t minimum_order_size{};
};

class StrategyParams {
 public:
  struct Impl;
  StrategyParams() noexcept;
  ~StrategyParams();
  StrategyParams(const StrategyParams&) noexcept;
  StrategyParams& operator=(const StrategyParams&) noexcept;
  StrategyParams(StrategyParams&&) noexcept;
  StrategyParams& operator=(StrategyParams&&) noexcept;

  [[nodiscard]] bool contains(std::string_view path) const noexcept;
  [[nodiscard]] Result<std::int64_t> require_int(
      std::string_view path) const noexcept;
  [[nodiscard]] Result<double> require_double(
      std::string_view path) const noexcept;
  [[nodiscard]] Result<std::string_view> require_string(
      std::string_view path) const noexcept;
  [[nodiscard]] Result<bool> require_bool(
      std::string_view path) const noexcept;
  [[nodiscard]] std::int64_t optional_int(
      std::string_view path, std::int64_t fallback) const noexcept;
  [[nodiscard]] double optional_double(
      std::string_view path, double fallback) const noexcept;
  [[nodiscard]] bool optional_bool(std::string_view path,
                                   bool fallback) const noexcept;
  [[nodiscard]] std::string_view optional_string(
      std::string_view path, std::string_view fallback = {}) const noexcept;
  [[nodiscard]] StrategyParams child(std::string_view path) const;
  [[nodiscard]] Result<std::size_t> sequence_size(
      std::string_view path) const noexcept;

 private:
  explicit StrategyParams(std::shared_ptr<const Impl> impl,
                          std::string prefix = {}) noexcept;
  [[nodiscard]] std::string key(std::string_view path) const;
  std::shared_ptr<const Impl> impl_;
  std::string prefix_;
  friend Result<struct StrategyFrameConfig> load_config(
      const std::string&) noexcept;
};

struct StrategyFrameConfig {
  MdsConfig mds{};
  ThreadingMode threading{ThreadingMode::SingleThread};
  IdlePolicy idle_policy{IdlePolicy::BusySpin};
  ConfigStrictness strictness{ConfigStrictness::Strict};
  std::int32_t strategy_cpu{-1};
  std::int32_t io_cpu{-1};
  std::int32_t numa_node{-1};
  MemoryConfig memory{};
  SocketConfig socket{};
  CapacityConfig capacities{};
  std::vector<InstrumentConfig> instruments{};
  std::vector<VenueExecutionConfig> venues{};
  StrategyParams strategy{};
  std::uint32_t session_epoch{1};
  std::uint32_t event_budget{64};
  std::uint32_t metric_sample_rate{100};
  std::uint64_t startup_timeout_ns{10'000'000'000ULL};
};

[[nodiscard]] Result<StrategyFrameConfig> load_config(
    const std::string& path) noexcept;

}  // namespace strategyframe
