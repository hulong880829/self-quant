#include <string_view>
#include <type_traits>

#include "strategyframe/strategyframe.h"

#if __has_include("strategyframe/internal/strategyframe/runtime_core.h")
#error "StrategyFrame internal headers leaked into installed SDK"
#endif

static_assert(
    std::is_trivially_copyable_v<strategyframe::ExecutionUpdate>);
static_assert(sizeof(strategyframe::InstrumentId) == 8);

int main() {
  strategyframe::StrategyFrameConfig config{};
  using CanonicalLookup =
      strategyframe::Result<strategyframe::InstrumentCatalogInfo>
      (strategyframe::StrategyContext::*)(std::string_view) const noexcept;
  const auto lookup = static_cast<CanonicalLookup>(
      &strategyframe::StrategyContext::find_instrument);
  (void)lookup;
  return config.threading == strategyframe::ThreadingMode::SingleThread ? 0
                                                                        : 1;
}
