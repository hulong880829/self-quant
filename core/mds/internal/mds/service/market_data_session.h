#pragma once

#include "mds/api/mds_api.h"
#include "mds/book/book_bridge.h"

#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <string_view>

namespace mds::service {

enum class MarketDataState : std::uint8_t {
  Stopped,
  Connecting,
  Authenticating,
  Subscribing,
  Metadata,
  Buffering,
  Live,
  ReconnectWait,
  Failed,
};

[[nodiscard]] std::string_view
to_string(MarketDataState state) noexcept;

enum class ResyncReason : std::uint8_t {
  None,
  SnapshotBridgeGap,
  LiveSequenceGap,
  InvalidImage,
  PipelineResync,
  InputCapacity,
  DirtyData,
  ScaleMismatch,
};

[[nodiscard]] std::string_view
to_string(ResyncReason reason) noexcept;

struct MarketDataMetrics {
  std::uint64_t ws_shards{};
  std::uint64_t ws_shards_live{};
  std::uint64_t ws_shards_reconnecting{};
  std::uint64_t last_reconnect_shard{};
  std::uint64_t reconnects{};
  std::uint64_t server_expirations{};
  std::uint64_t connection_rebuild_attempts{};
  std::uint64_t connection_rebuild_successes{};
  std::uint64_t connection_rebuild_failures{};
  std::uint64_t connection_degraded_duration_ms{};
  std::uint64_t resyncs{};
  std::uint64_t snapshot_bridge_gaps{};
  std::uint64_t live_sequence_gaps{};
  std::uint64_t invalid_images{};
  std::uint64_t pipeline_resyncs{};
  std::array<
      std::uint64_t,
      static_cast<std::size_t>(book::BridgeResyncReason::Count)>
      pipeline_resync_causes{};
  std::uint64_t input_capacity_resyncs{};
  std::uint64_t dirty_data_resyncs{};
  std::uint64_t parse_errors{};
  std::uint64_t one_sided_book_frames{};
  std::uint64_t numeric_tail_normalizations{};
  std::uint64_t scale_mismatch_frames{};
  std::uint64_t scale_mismatch_symbols{};
  std::uint64_t metadata_refresh_requests{};
  std::uint64_t metadata_refresh_successes{};
  std::uint64_t metadata_refresh_failures{};
  std::uint64_t metadata_refresh_coalesced{};
  std::uint64_t metadata_refresh_rate_limits{};
  std::uint64_t publish_errors{};
  std::uint64_t ticker_updates{};
  std::uint64_t depth_updates{};
  std::uint64_t subscription_requests{};
  std::uint64_t subscription_rejections{};
  std::uint64_t subscription_rate_limit_deferrals{};
  std::uint64_t config_symbol_quarantines{};
  std::uint64_t metadata_symbol_quarantines{};
  std::uint64_t subscription_symbol_quarantines{};
  std::uint64_t budget_reconnects{};
  std::uint64_t budget_deferrals{};
  std::uint64_t global_budget_deferrals{};
  std::uint64_t cooldown_deferrals{};
  std::uint64_t recovery_deadline_extensions{};
  std::uint64_t discovery_requests{};
  std::uint64_t discovery_failures{};
  std::uint64_t market_rollovers{};
  std::uint64_t stale_market_messages{};
  std::uint64_t snapshot_failures{};
  std::uint64_t snapshot_rate_limits{};
  std::uint64_t snapshot_quarantines{};
  std::uint64_t heartbeat_timeouts{};
  std::uint64_t last_discovery_latency_us{};
  std::uint64_t last_rollover_latency_us{};
  std::array<char, 33> last_resync_symbol{};
  ResyncReason last_resync_reason{ResyncReason::None};
  book::BridgeResyncReason last_pipeline_resync_cause{
      book::BridgeResyncReason::None};
  std::uint64_t last_resync_expected{};
  std::uint64_t last_resync_previous{};
  std::uint64_t last_resync_first{};
  std::uint64_t last_resync_final{};
  std::uint64_t last_input_capacity{};
  char last_input_side{};
};

class MarketDataSession {
 public:
  using Clock = std::chrono::steady_clock;
  virtual ~MarketDataSession() = default;

  virtual api::Result<void> start(Clock::time_point now = Clock::now()) = 0;
  virtual void tick(Clock::time_point now = Clock::now()) noexcept = 0;
  virtual void stop() noexcept = 0;
  [[nodiscard]] virtual MarketDataState state() const noexcept = 0;
  [[nodiscard]] virtual const MarketDataMetrics &metrics() const noexcept = 0;
  [[nodiscard]] virtual std::string_view error_message() const noexcept = 0;
};

}  // namespace mds::service
