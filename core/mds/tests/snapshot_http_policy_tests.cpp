#include "mds/service/snapshot_http_policy.h"

#include <cassert>
#include <chrono>

int main() {
  using mds::service::SnapshotHttpAction;
  using mds::service::classify_snapshot_http;
  using utils::md::Venue;

  assert(classify_snapshot_http(Venue::Binance, 200, "{}") ==
         SnapshotHttpAction::Parse);
  assert(classify_snapshot_http(Venue::Binance, 429, "{}") ==
         SnapshotHttpAction::CooldownVenue);
  assert(classify_snapshot_http(Venue::Binance, 418, "{}") ==
         SnapshotHttpAction::BanCooldownVenue);
  assert(classify_snapshot_http(
             Venue::Binance, 400,
             R"({"code":-1121,"msg":"Invalid symbol."})") ==
         SnapshotHttpAction::QuarantineSymbol);
  assert(classify_snapshot_http(
             Venue::Aster, 400,
             R"({"code":-1121,"msg":"Invalid symbol."})") ==
         SnapshotHttpAction::QuarantineSymbol);
  assert(classify_snapshot_http(Venue::Binance, 503, "") ==
         SnapshotHttpAction::RetrySymbol);
  assert(classify_snapshot_http(Venue::Binance, 200, "{") ==
         SnapshotHttpAction::Parse);
  assert(mds::service::parse_retry_after("12") ==
         std::chrono::seconds(12));
  assert(mds::service::parse_retry_after("invalid").count() == 0);
  assert(mds::service::parse_retry_after("999999") ==
         std::chrono::hours(1));
  return 0;
}
