#include "oms/exchange/adapter_router.h"

#include <cstdint>

namespace oms::exchange {
namespace {

constexpr std::size_t Index(AdapterKind kind) noexcept {
  const auto value = static_cast<std::uint8_t>(kind);
  return value == 0 ? kMaxTradeAdapters
                    : static_cast<std::size_t>(value - 1U);
}

bool ValidIdentity(const AdapterIdentity& identity) noexcept {
  switch (identity.kind) {
    case AdapterKind::BinanceSpot:
      return identity.venue == utils::md::Venue::Binance &&
             identity.product_type == utils::md::ProductType::Spot;
    case AdapterKind::BinanceUsdm:
      return identity.venue == utils::md::Venue::Binance &&
             (identity.product_type == utils::md::ProductType::Perpetual ||
              identity.product_type == utils::md::ProductType::Future);
    case AdapterKind::Polymarket:
      return identity.venue == utils::md::Venue::Polymarket &&
             identity.product_type == utils::md::ProductType::BinaryOption;
    case AdapterKind::Fake:
      return identity.venue == utils::md::Venue::Unknown &&
             identity.product_type == utils::md::ProductType::Unknown;
  }
  return false;
}

}  // namespace

AdapterResult AdapterRouter::add(TradeAdapter& adapter) noexcept {
  const AdapterIdentity identity = adapter.identity();
  if (!ValidIdentity(identity)) return AdapterResult::InvalidArgument;
  const std::size_t index = Index(identity.kind);
  if (index >= adapters_.size()) return AdapterResult::InvalidArgument;
  if (adapters_[index] != nullptr) return AdapterResult::InvalidArgument;
  adapters_[index] = &adapter;
  ++size_;
  return AdapterResult::Ok;
}

TradeAdapter* AdapterRouter::find(AdapterKind kind) noexcept {
  const std::size_t index = Index(kind);
  return index < adapters_.size() ? adapters_[index] : nullptr;
}

const TradeAdapter* AdapterRouter::find(AdapterKind kind) const noexcept {
  return const_cast<AdapterRouter*>(this)->find(kind);
}

AdapterKind AdapterRouter::route_kind(
    const utils::md::Instrument& instrument) noexcept {
  if (instrument.venue == utils::md::Venue::Binance) {
    if (instrument.product_type == utils::md::ProductType::Spot)
      return AdapterKind::BinanceSpot;
    if (instrument.product_type == utils::md::ProductType::Perpetual ||
        instrument.product_type == utils::md::ProductType::Future)
      return AdapterKind::BinanceUsdm;
  }
  if (instrument.venue == utils::md::Venue::Polymarket &&
      instrument.product_type == utils::md::ProductType::BinaryOption)
    return AdapterKind::Polymarket;
  return AdapterKind::Fake;
}

TradeAdapter* AdapterRouter::route(
    const utils::md::Instrument& instrument) noexcept {
  const AdapterKind kind = route_kind(instrument);
  if (kind == AdapterKind::Fake &&
      (instrument.venue == utils::md::Venue::Unknown ||
       instrument.product_type == utils::md::ProductType::Unknown))
    return nullptr;
  if (TradeAdapter* adapter = find(kind); adapter != nullptr) return adapter;
  return find(AdapterKind::Fake);
}

const TradeAdapter* AdapterRouter::route(
    const utils::md::Instrument& instrument) const noexcept {
  return const_cast<AdapterRouter*>(this)->route(instrument);
}

}  // namespace oms::exchange
