#pragma once

#include <chrono>
#include <cstdint>

namespace mds::service {

[[nodiscard]] constexpr bool RecoveryStreamReady(
    bool ticker_required, bool ticker_live,
    bool orderbook_required, bool metadata_ready,
    bool image_ready, bool book_live,
    bool awaiting_snapshot_bridge) noexcept {
  // Ticker-only subscriptions have no image/bridge phase; the subscribe ACK
  // is their readiness boundary. Combined ticker+book streams require a real
  // ticker event as well as a fully bridged book.
  const bool ticker_ready =
      !ticker_required || ticker_live || !orderbook_required;
  const bool book_ready =
      !orderbook_required ||
      (metadata_ready && image_ready && book_live &&
       !awaiting_snapshot_bridge);
  return ticker_ready && book_ready;
}

[[nodiscard]] constexpr bool ContinuousRecoveryExpired(
    std::chrono::steady_clock::time_point started,
    std::chrono::steady_clock::time_point now,
    std::chrono::steady_clock::duration maximum) noexcept {
  return started != std::chrono::steady_clock::time_point{} &&
         now - started >= maximum;
}

class ConnectionDeadlines {
 public:
  using Clock = std::chrono::steady_clock;

  enum class Expiration : std::uint8_t { None, Startup, Recovery };

  void reset_connection() noexcept {
    startup_ = {};
    recovery_ = {};
  }

  void subscriptions_ready(Clock::time_point now,
                           Clock::duration timeout) noexcept {
    if (reached_live_once_) {
      startup_ = {};
      recovery_ = now + timeout;
    } else {
      startup_ = now + timeout;
      recovery_ = {};
    }
  }

  void begin_recovery(Clock::time_point now,
                      Clock::duration timeout) noexcept {
    if (reached_live_once_ && recovery_ == Clock::time_point{}) {
      recovery_ = now + timeout;
    }
  }

  void continue_recovery(Clock::time_point now,
                         Clock::duration timeout) noexcept {
    if (reached_live_once_ && recovery_ != Clock::time_point{}) {
      recovery_ = now + timeout;
    }
  }

  void mark_live() noexcept {
    reached_live_once_ = true;
    startup_ = {};
    recovery_ = {};
  }

  [[nodiscard]] bool reached_live_once() const noexcept {
    return reached_live_once_;
  }

  [[nodiscard]] Expiration expiration(Clock::time_point now,
                                      bool all_subscribed,
                                      bool live) const noexcept {
    if (!all_subscribed || live) {
      return Expiration::None;
    }
    if (!reached_live_once_ && startup_ != Clock::time_point{} &&
        now >= startup_) {
      return Expiration::Startup;
    }
    if (reached_live_once_ && recovery_ != Clock::time_point{} &&
        now >= recovery_) {
      return Expiration::Recovery;
    }
    return Expiration::None;
  }

 private:
  Clock::time_point startup_{};
  Clock::time_point recovery_{};
  bool reached_live_once_{};
};

}  // namespace mds::service
