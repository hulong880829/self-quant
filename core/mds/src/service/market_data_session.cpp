#include "mds/service/market_data_session.h"

namespace mds::service {

std::string_view to_string(MarketDataState state) noexcept {
  switch (state) {
  case MarketDataState::Stopped:
    return "Stopped";
  case MarketDataState::Connecting:
    return "Connecting";
  case MarketDataState::Authenticating:
    return "Authenticating";
  case MarketDataState::Subscribing:
    return "Subscribing";
  case MarketDataState::Metadata:
    return "Metadata";
  case MarketDataState::Buffering:
    return "Buffering";
  case MarketDataState::Live:
    return "Live";
  case MarketDataState::ReconnectWait:
    return "ReconnectWait";
  case MarketDataState::Failed:
    return "Failed";
  }
  return "Unknown";
}

std::string_view to_string(ResyncReason reason) noexcept {
  switch (reason) {
  case ResyncReason::None:
    return "none";
  case ResyncReason::SnapshotBridgeGap:
    return "snapshot_bridge_gap";
  case ResyncReason::LiveSequenceGap:
    return "live_sequence_gap";
  case ResyncReason::InvalidImage:
    return "invalid_image";
  case ResyncReason::PipelineResync:
    return "pipeline_resync";
  case ResyncReason::InputCapacity:
    return "input_capacity";
  case ResyncReason::DirtyData:
    return "dirty_data";
  }
  return "unknown";
}

}  // namespace mds::service
