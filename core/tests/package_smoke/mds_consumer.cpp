#include "mds/api/mds_api.h"
#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"

#if __has_include("mds/network/epoll_loop.h")
#error "internal mds/network/epoll_loop.h leaked into the installed SDK"
#endif

int main() {
  static_assert(mds::transport::kRingSchemaMajor == 4);
  return mds::publish::make_publisher_segment_name(
             "spot", "BTCUSDT", "ticker") ==
                 "/selfquant.mds.spot.btcusdt.ticker.1"
             ? 0
             : 1;
}
