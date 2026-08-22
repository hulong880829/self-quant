#pragma once

#include <array>
#include <cstddef>

#include "oms/exchange/trade_adapter.h"

namespace oms::exchange {

// Control-path registration followed by allocation-free/noexcept lookup.
// Registration is bounded to one instance of each frozen AdapterKind.
class AdapterRouter {
 public:
  [[nodiscard]] AdapterResult add(TradeAdapter& adapter) noexcept;

  [[nodiscard]] TradeAdapter* find(AdapterKind kind) noexcept;
  [[nodiscard]] const TradeAdapter* find(AdapterKind kind) const noexcept;
  [[nodiscard]] TradeAdapter* route(
      const utils::md::Instrument& instrument) noexcept;
  [[nodiscard]] const TradeAdapter* route(
      const utils::md::Instrument& instrument) const noexcept;

  [[nodiscard]] std::size_t size() const noexcept { return size_; }

 private:
  [[nodiscard]] static AdapterKind route_kind(
      const utils::md::Instrument& instrument) noexcept;

  std::array<TradeAdapter*, kMaxTradeAdapters> adapters_{};
  std::size_t size_{};
};

static_assert(sizeof(AdapterRouter) == 40);

}  // namespace oms::exchange
