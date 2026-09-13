#pragma once

#include <algorithm>
#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>

namespace mds::service {

[[nodiscard]] constexpr bool RecoveryStreamReady(
    bool ticker_required, bool ticker_live,
    bool ticker_requires_first_data,
    bool orderbook_required, bool metadata_ready,
    bool image_ready, bool book_live,
    bool awaiting_snapshot_bridge) noexcept {
  // Ticker-only subscriptions have no image/bridge phase; the subscribe ACK
  // is their readiness boundary. Combined ticker+book streams require a real
  // ticker event as well as a fully bridged book.
  const bool ticker_ready =
      !ticker_required || ticker_live ||
      (!ticker_requires_first_data && !orderbook_required);
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

class SubscriptionBudget {
 public:
  using Clock = std::chrono::steady_clock;

  void prune(Clock::time_point now) noexcept {
    const auto window = std::chrono::hours(1);
    while (size_ > 0 && now - requests_[begin_] >= window) {
      begin_ = (begin_ + 1) % requests_.size();
      --size_;
    }
  }

  [[nodiscard]] bool allow(Clock::time_point now,
                           std::size_t limit) noexcept {
    prune(now);
    return size_ < std::min(limit, requests_.size());
  }

  [[nodiscard]] Clock::time_point retry_at(Clock::time_point now,
                                            std::size_t limit) noexcept {
    prune(now);
    if (size_ < std::min(limit, requests_.size())) {
      return now;
    }
    return requests_[begin_] + std::chrono::hours(1);
  }

  void record(Clock::time_point now) noexcept {
    if (size_ == requests_.size()) {
      begin_ = (begin_ + 1) % requests_.size();
      --size_;
    }
    requests_[(begin_ + size_) % requests_.size()] = now;
    ++size_;
  }

  [[nodiscard]] std::size_t size() const noexcept { return size_; }

 private:
  std::array<Clock::time_point, 480> requests_{};
  std::size_t begin_{};
  std::size_t size_{};
};

// Shared by every connection/shard for a venue whose documented limit is
// process-wide (Lighter's client-message limit is IP-wide). The deployment
// still guarantees that only one such producer uses an egress IP.
class ClientMessageBudget {
 public:
  using Clock = std::chrono::steady_clock;
  static constexpr std::size_t kMaximumLimit = 199;

  void prune(Clock::time_point now) noexcept {
    constexpr auto window = std::chrono::minutes(1);
    while (size_ > 0 && now - requests_[begin_] >= window) {
      begin_ = (begin_ + 1) % requests_.size();
      --size_;
    }
  }

  [[nodiscard]] bool allow(Clock::time_point now,
                           std::size_t limit) noexcept {
    prune(now);
    return limit > 0 && size_ < std::min(limit, requests_.size());
  }

  [[nodiscard]] Clock::time_point retry_at(
      Clock::time_point now, std::size_t limit) noexcept {
    prune(now);
    if (limit > 0 && size_ < std::min(limit, requests_.size())) {
      return now;
    }
    return size_ == 0 ? now + std::chrono::minutes(1)
                      : requests_[begin_] + std::chrono::minutes(1);
  }

  void record(Clock::time_point now) noexcept {
    if (size_ == requests_.size()) {
      begin_ = (begin_ + 1) % requests_.size();
      --size_;
    }
    requests_[(begin_ + size_) % requests_.size()] = now;
    ++size_;
  }

  [[nodiscard]] std::size_t size() const noexcept { return size_; }

 private:
  std::array<Clock::time_point, kMaximumLimit> requests_{};
  std::size_t begin_{};
  std::size_t size_{};
};

class ConnectionAttemptBudget {
 public:
  using Clock = std::chrono::steady_clock;
  static constexpr std::size_t kLimitPerMinute = 255;

  void prune(Clock::time_point now) noexcept {
    constexpr auto window = std::chrono::minutes(1);
    while (size_ > 0 && now - requests_[begin_] >= window) {
      begin_ = (begin_ + 1) % requests_.size();
      --size_;
    }
  }

  [[nodiscard]] bool allow(Clock::time_point now) noexcept {
    prune(now);
    return size_ < requests_.size();
  }

  [[nodiscard]] Clock::time_point retry_at(
      Clock::time_point now) noexcept {
    prune(now);
    return size_ < requests_.size()
               ? now
               : requests_[begin_] + std::chrono::minutes(1);
  }

  void record(Clock::time_point now) noexcept {
    if (size_ == requests_.size()) {
      begin_ = (begin_ + 1) % requests_.size();
      --size_;
    }
    requests_[(begin_ + size_) % requests_.size()] = now;
    ++size_;
  }

  [[nodiscard]] std::size_t size() const noexcept { return size_; }

 private:
  std::array<Clock::time_point, kLimitPerMinute> requests_{};
  std::size_t begin_{};
  std::size_t size_{};
};

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

  [[nodiscard]] bool continue_recovery(Clock::time_point now,
                                       Clock::duration timeout) noexcept {
    if (reached_live_once_ && recovery_ != Clock::time_point{}) {
      recovery_ = now + timeout;
      return true;
    }
    return false;
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
