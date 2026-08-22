#pragma once

#include "mds/consume/aggregate_dispatch.h"

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

namespace mds::gateway {

struct Topic {
  std::string segment;
  consume::AggregateTopic kind{consume::AggregateTopic::Unsupported};
  consume::AggregateLatestState *latest{};
};

struct GatewayOptions {
  std::string listen_address{"127.0.0.1"};
  std::uint16_t port{9443};
  std::string bearer_token;
  std::size_t max_clients{128};
  std::size_t max_subscriptions_per_client{32};
  std::uint64_t publish_interval_ms{200};
  std::uint64_t ping_interval_ms{15'000};
  std::uint64_t pong_timeout_ms{5'000};
  std::uint64_t slow_client_timeout_ms{5'000};
  std::size_t depth{50};
  std::size_t max_control_frame_bytes{4096};
  bool reuse_port{};
};

class Gateway {
public:
  Gateway(GatewayOptions options, std::vector<Topic> topics);
  ~Gateway();
  Gateway(const Gateway &) = delete;
  Gateway &operator=(const Gateway &) = delete;

  [[nodiscard]] bool start();
  void stop() noexcept;
  [[nodiscard]] bool failed() const noexcept;
  [[nodiscard]] std::string error() const;
  [[nodiscard]] std::uint16_t port() const noexcept;

private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

} // namespace mds::gateway
