#pragma once

#include "mds/api/mds_api.h"
#include "mds/record/clickhouse_bbo_recorder.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace mds::consumer {

enum class SelectorKind : std::uint8_t { Symbol, Product, Venue, All };

struct Selector {
  SelectorKind kind{SelectorKind::All};
  std::string symbol;
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  utils::md::Venue venue{utils::md::Venue::Unknown};

  [[nodiscard]] bool matches(const utils::md::Instrument &instrument) const
      noexcept;
  [[nodiscard]] bool matches(
      const utils::md::InstrumentCatalog &catalog) const noexcept;
};

struct SegmentConfig {
  std::string name;
  Selector selector;
};

struct GatewayConfig {
  std::string listen_address{"127.0.0.1"};
  std::uint16_t port{9443};
  std::string auth_token_env;
  std::size_t max_clients{128};
  std::size_t max_subscriptions_per_client{32};
  std::size_t send_queue_slots{1};
  std::uint64_t publish_interval_ms{200};
  std::uint64_t ping_interval_ms{15'000};
  std::uint64_t pong_timeout_ms{5'000};
  std::uint64_t slow_client_timeout_ms{5'000};
  std::size_t depth{50};
  bool reuse_port{};
};

struct RecordingConfig {
  std::string output_directory;
  std::uint64_t sample_interval_ms{200};
  std::uint32_t retention_hours{24};
  std::size_t depth{50};
  std::uint32_t shard_hours{1};
  int zstd_level{1};
  std::size_t writer_queue_slots{1024};
  std::size_t topic_slots{64};
  std::uint64_t min_free_disk_bytes{1ULL << 30U};
};

struct ClickHouseBboConfig {
  record::ClickHouseBboOptions options;
  std::string password_env;
  std::vector<std::string> segments;
};

struct IngestionConfig {
  std::uint64_t stale_after_ms{5'000};
  std::uint64_t hard_reset_after_ms{10'000};
  std::size_t max_drain_records{512};
};

struct ConsumerConfig {
  std::vector<SegmentConfig> segments;
  IngestionConfig ingestion;
  GatewayConfig gateway;
  RecordingConfig recording;
  ClickHouseBboConfig clickhouse_bbo;
};

[[nodiscard]] api::Result<ConsumerConfig> load_config(
    const std::string &path, bool gateway_requested = false,
    bool recording_requested = false,
    bool clickhouse_bbo_requested = false) noexcept;

[[nodiscard]] bool recording_available() noexcept;

}  // namespace mds::consumer
