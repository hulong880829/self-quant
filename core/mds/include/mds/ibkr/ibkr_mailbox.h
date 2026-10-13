#pragma once

#include <atomic>
#include <cstdint>
#include <thread>

namespace mds::ibkr {

struct BboSnapshot {
  std::int64_t bid_price{};
  std::int64_t ask_price{};
  std::int64_t bid_size{};
  std::int64_t ask_size{};
  std::uint32_t book_generation{};
  std::uint64_t receive_tsc{};
  bool publishable{};
};

// Seqlock snapshot. dirty is notification only and lives outside this type.
// A writer flag serializes the callback thread and the disconnect thread;
// readers never take it and retry while version is odd or unstable.
class SeqlockMailbox {
 public:
  void write(const BboSnapshot &snapshot) noexcept {
    while (writer_.test_and_set(std::memory_order_acquire)) {
      std::this_thread::yield();
    }
    const auto version = version_.load(std::memory_order_relaxed);
    version_.store(version + 1, std::memory_order_release);
    bid_price_.store(snapshot.bid_price, std::memory_order_relaxed);
    ask_price_.store(snapshot.ask_price, std::memory_order_relaxed);
    bid_size_.store(snapshot.bid_size, std::memory_order_relaxed);
    ask_size_.store(snapshot.ask_size, std::memory_order_relaxed);
    book_generation_.store(snapshot.book_generation, std::memory_order_relaxed);
    receive_tsc_.store(snapshot.receive_tsc, std::memory_order_relaxed);
    publishable_.store(snapshot.publishable ? 1 : 0, std::memory_order_relaxed);
    version_.store(version + 2, std::memory_order_release);
    writer_.clear(std::memory_order_release);
  }

  [[nodiscard]] bool read(BboSnapshot &out) noexcept {
    for (int attempt = 0; attempt < 64; ++attempt) {
      const auto first = version_.load(std::memory_order_acquire);
      if ((first & 1ULL) != 0) {
        retries_.fetch_add(1, std::memory_order_relaxed);
        continue;
      }
      out.bid_price = bid_price_.load(std::memory_order_relaxed);
      out.ask_price = ask_price_.load(std::memory_order_relaxed);
      out.bid_size = bid_size_.load(std::memory_order_relaxed);
      out.ask_size = ask_size_.load(std::memory_order_relaxed);
      out.book_generation = book_generation_.load(std::memory_order_relaxed);
      out.receive_tsc = receive_tsc_.load(std::memory_order_relaxed);
      out.publishable = publishable_.load(std::memory_order_relaxed) != 0;
      const auto second = version_.load(std::memory_order_acquire);
      if (first == second) {
        return true;
      }
      retries_.fetch_add(1, std::memory_order_relaxed);
    }
    return false;
  }

  [[nodiscard]] std::uint64_t retries() const noexcept {
    return retries_.load(std::memory_order_relaxed);
  }

 private:
  std::atomic_flag writer_ = ATOMIC_FLAG_INIT;
  std::atomic<std::uint64_t> version_{0};
  std::atomic<std::int64_t> bid_price_{0};
  std::atomic<std::int64_t> ask_price_{0};
  std::atomic<std::int64_t> bid_size_{0};
  std::atomic<std::int64_t> ask_size_{0};
  std::atomic<std::uint32_t> book_generation_{0};
  std::atomic<std::uint64_t> receive_tsc_{0};
  std::atomic<std::uint8_t> publishable_{0};
  std::atomic<std::uint64_t> retries_{0};
};

inline void mark_dirty(std::atomic<bool> &dirty,
                       std::atomic<std::uint64_t> &overwrites) noexcept {
  if (dirty.exchange(true, std::memory_order_acq_rel)) {
    overwrites.fetch_add(1, std::memory_order_relaxed);
  }
}

}  // namespace mds::ibkr
