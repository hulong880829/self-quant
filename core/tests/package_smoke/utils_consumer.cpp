#include "utils/md/wire_codec.h"
#include "utils/queue/spsc_ring.h"
#include "utils/runtime/timestamp.h"

#if __has_include("utils/runtime/hardware.h")
#error "internal utils/runtime/hardware.h leaked into the installed SDK"
#endif

int main() {
  utils::queue::SpscRing<int, 4> ring;
  auto producer = ring.try_reserve();
  if (!producer) {
    return 1;
  }
  producer->emplace(42);
  if (!producer->commit()) {
    return 2;
  }
  auto consumer = ring.try_peek();
  return consumer && **consumer == 42 ? 0 : 3;
}
