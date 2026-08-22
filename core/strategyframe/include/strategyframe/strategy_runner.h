#pragma once

#include <atomic>
#include <memory>
#include <utility>

#include "strategyframe/detail/runtime_bridge.h"
#include "strategyframe/strategy.h"

namespace strategyframe {

template <Strategy T>
class StrategyRunner final {
 public:
  template <typename U>
    requires std::constructible_from<T, U&&>
  explicit StrategyRunner(StrategyFrameConfig config, U&& strategy)
      : config_(std::move(config)), strategy_(std::forward<U>(strategy)) {}

  StrategyRunner(const StrategyRunner&) = delete;
  StrategyRunner& operator=(const StrategyRunner&) = delete;

  [[nodiscard]] Error run() noexcept {
    bool expected = false;
    if (!running_.compare_exchange_strong(expected, true,
                                          std::memory_order_acq_rel)) {
      return Error::InvalidState;
    }
    if (has_run_.exchange(true, std::memory_order_acq_rel)) {
      running_.store(false, std::memory_order_release);
      return Error::InvalidState;
    }

    auto created =
        detail::make_runtime(std::move(config_), callbacks(), &strategy_);
    if (!created) {
      running_.store(false, std::memory_order_release);
      return created.error;
    }
    std::unique_ptr<detail::Runtime> runtime = std::move(created.value);
    runtime_.store(runtime.get(), std::memory_order_release);
    if (stop_requested_.load(std::memory_order_acquire)) {
      runtime->request_stop();
    }
    const Error result = runtime->run();
    runtime_.store(nullptr, std::memory_order_release);
    last_metrics_ = runtime->metrics();
    running_.store(false, std::memory_order_release);
    return result;
  }

  void request_stop() noexcept {
    stop_requested_.store(true, std::memory_order_release);
    if (auto* runtime = runtime_.load(std::memory_order_acquire)) {
      runtime->request_stop();
    }
  }

  [[nodiscard]] bool running() const noexcept {
    return running_.load(std::memory_order_acquire);
  }
  [[nodiscard]] const RuntimeMetrics& metrics() const noexcept {
    return last_metrics_;
  }

 private:
  template <typename Callback>
  static bool Invoke(Callback&& callback) noexcept {
    try {
      std::forward<Callback>(callback)();
      return true;
    } catch (...) {
      return false;
    }
  }

  static const detail::CallbackTable& callbacks() noexcept {
    static const detail::CallbackTable table{
        [](void* value, StrategyContext& context) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->init(context); });
        },
        [](void* value, const BboUpdate& update) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->on_bbo_update(update); });
        },
        [](void* value, const OrderBookUpdate& update) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->on_orderbook_update(update); });
        },
        [](void* value, const AggBboUpdate& update) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->on_agg_bbo_update(update); });
        },
        [](void* value, const AggOrderBookUpdate& update) noexcept {
          return Invoke([&] {
            static_cast<T*>(value)->on_agg_orderbook_update(update);
          });
        },
        [](void* value, const ExecutionUpdate& update) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->on_order_update(update); });
        },
        [](void* value, const OmsStatusUpdate& update) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->on_oms_status(update); });
        },
        [](void* value, const TimerEvent& update) noexcept {
          return Invoke(
              [&] { static_cast<T*>(value)->on_timer(update); });
        },
        [](void* value, const InstrumentCatalogInfo& update) noexcept {
          if constexpr (requires(T& strategy,
                                 const InstrumentCatalogInfo& catalog) {
                          strategy.on_instrument_catalog(catalog);
                        }) {
            return Invoke([&] {
              static_cast<T*>(value)->on_instrument_catalog(update);
            });
          }
          return true;
        },
        [](void* value, const OpenOrdersSnapshot& update) noexcept {
          if constexpr (requires(T& strategy,
                                 const OpenOrdersSnapshot& snapshot) {
                          strategy.on_open_orders_snapshot(snapshot);
                        }) {
            return Invoke([&] {
              static_cast<T*>(value)->on_open_orders_snapshot(update);
            });
          }
          return true;
        },
        [](void* value, const PositionsSnapshot& update) noexcept {
          if constexpr (requires(T& strategy,
                                 const PositionsSnapshot& snapshot) {
                          strategy.on_positions_snapshot(snapshot);
                        }) {
            return Invoke([&] {
              static_cast<T*>(value)->on_positions_snapshot(update);
            });
          }
          return true;
        },
        [](void* value, const QueryComplete& update) noexcept {
          if constexpr (requires(T& strategy, const QueryComplete& complete) {
                          strategy.on_query_complete(complete);
                        }) {
            return Invoke([&] {
              static_cast<T*>(value)->on_query_complete(update);
            });
          }
          return true;
        }};
    return table;
  }

  StrategyFrameConfig config_;
  T strategy_;
  std::atomic<detail::Runtime*> runtime_{nullptr};
  std::atomic<bool> running_{false};
  std::atomic<bool> has_run_{false};
  std::atomic<bool> stop_requested_{false};
  RuntimeMetrics last_metrics_{};
};

}  // namespace strategyframe
