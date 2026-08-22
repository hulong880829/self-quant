#pragma once

#include "mds/agg/aggregation_engine.h"
#include "mds/agg/venue_ingest.h"
#include "mds/api/mds_api.h"
#include "mds/transport/shared_ring.h"

#include <array>
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace mds::service {

enum class AggregateKind : std::uint8_t { Bbo, OrderBook };

class InProcessAggregation {
 public:
  InProcessAggregation(api::SubscriptionHandle handle, AggregateKind kind,
                       api::AggregateSubscription subscription,
                       const api::MdsConfig &config);
  ~InProcessAggregation();

  InProcessAggregation(const InProcessAggregation &) = delete;
  InProcessAggregation &operator=(const InProcessAggregation &) = delete;

  api::Result<void> start();
  void stop() noexcept;
  // Polls all inputs and timer-driven expiry. No allocation occurs after all
  // member instruments have initialized the engine.
  [[nodiscard]] bool poll(std::uint64_t now_mono_ns) noexcept;

  [[nodiscard]] api::SubscriptionHandle handle() const noexcept {
    return handle_;
  }
  [[nodiscard]] AggregateKind kind() const noexcept { return kind_; }
  [[nodiscard]] api::SubscriptionState state() const noexcept {
    return state_;
  }
  [[nodiscard]] api::ErrorCode
  read(api::AggBboRecord &record) const noexcept;
  [[nodiscard]] api::ErrorCode
  read(api::AggOrderBookRecord &record) const noexcept;

 private:
  struct Input {
    Input(utils::md::Venue venue_value, std::size_t member_index,
          transport::SharedRing &&ring_value,
          transport::ReaderHandle reader_value);

    utils::md::Venue venue{utils::md::Venue::Unknown};
    std::size_t member_index{};
    bool fx{};
    bool compatible{true};
    transport::SharedRing ring{};
    transport::ReaderHandle reader{};
    agg::VenueIngest ingest{};
  };

  [[nodiscard]] bool ensure_engine() noexcept;
  void reset_engine() noexcept;

  api::SubscriptionHandle handle_{};
  AggregateKind kind_{AggregateKind::Bbo};
  api::AggregateSubscription subscription_{};
  const api::MdsConfig *config_{};
  std::vector<Input> inputs_{};
  std::optional<agg::AggregationEngine> engine_{};
  api::AggBboRecord latest_bbo_{};
  api::AggOrderBookRecord latest_book_{};
  std::array<char, 16> base_asset_{};
  std::array<char, 16> quote_asset_{};
  std::uint8_t price_scale_{};
  std::uint8_t quantity_scale_{};
  bool metadata_frozen_{};
  api::SubscriptionState state_{api::SubscriptionState::Pending};
  bool have_latest_{};
};

}  // namespace mds::service
