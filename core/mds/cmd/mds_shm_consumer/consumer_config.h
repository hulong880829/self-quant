#pragma once

#include "mds/api/mds_api.h"
#include "mds/record/clickhouse_aggregate_recorder.h"
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

struct ClickHouseBboConfig {
  record::ClickHouseBboOptions options;
  std::string password_env;
  std::vector<std::string> segments;
};

struct ClickHouseAggregateConfig {
  record::ClickHouseAggregateOptions options;
  std::string password_env;
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
  ClickHouseBboConfig clickhouse_bbo;
  ClickHouseAggregateConfig clickhouse_aggregate;
};

[[nodiscard]] api::Result<ConsumerConfig> load_config(
    const std::string &path, bool gateway_requested = false,
    bool clickhouse_bbo_requested = false,
    bool clickhouse_aggregate_requested = false) noexcept;

}  // namespace mds::consumer
