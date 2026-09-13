#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <span>
#include <vector>

#include "strategyframe/config.h"
#include "strategyframe/error.h"
#include "strategyframe/strategy_context.h"
#include "strategyframe/types.h"
#include "utils/md/types.h"

namespace strategyframe {

class MarketDataSource {
 public:
  struct Sink {
    void* context{};
    bool (*instrument)(void*, const utils::md::Instrument&) noexcept{};
    bool (*catalog)(void*, const utils::md::InstrumentCatalog&,
                    std::uint32_t generation) noexcept{};
    bool (*bbo)(void*, const BboUpdate&) noexcept{};
    bool (*book)(void*, const OrderBookUpdate&) noexcept{};
    bool (*agg_bbo)(void*, const AggBboUpdate&) noexcept{};
    bool (*agg_book)(void*, const AggOrderBookUpdate&) noexcept{};
    void (*gap)(void*, std::size_t) noexcept{};
  };

  explicit MarketDataSource(MdsConfig config, Sink sink);
  ~MarketDataSource();
  MarketDataSource(const MarketDataSource&) = delete;
  MarketDataSource& operator=(const MarketDataSource&) = delete;

  [[nodiscard]] Error start() noexcept;
  [[nodiscard]] Error poll(std::size_t budget, std::size_t& dispatched) noexcept;
  void stop() noexcept;
  [[nodiscard]] bool ready() const noexcept;
  [[nodiscard]] const RuntimeMetrics& metrics() const noexcept;
  void retire_instrument(InstrumentId instrument_id) noexcept;

  // Deterministic in-memory source used by runtime/MDS tests. Each entry is one
  // complete utils::md::wire record payload.
  [[nodiscard]] Error enqueue_replay(
      std::span<const std::byte> record) noexcept;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace strategyframe
