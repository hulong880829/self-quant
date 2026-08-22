#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <vector>

namespace oms::exchange {

struct RestRequestHandle {
  std::uint32_t slot{};
  std::uint32_t reserved{};
  std::uint64_t generation{};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return generation != 0;
  }

  friend constexpr bool operator==(const RestRequestHandle&,
                                   const RestRequestHandle&) = default;
};

struct RestRequestCorrelation {
  std::uint64_t request_id{};
  std::uint64_t user_data{};

  friend constexpr bool operator==(const RestRequestCorrelation&,
                                   const RestRequestCorrelation&) = default;
};

struct RestRequest {
  RestRequestHandle handle{};
  RestRequestCorrelation correlation{};
  std::uint64_t deadline_ns{};
};

class RestRequestPool {
 public:
  class Lease {
   public:
    Lease() noexcept = default;
    Lease(const Lease&) = delete;
    Lease& operator=(const Lease&) = delete;
    Lease(Lease&& other) noexcept;
    Lease& operator=(Lease&& other) noexcept;
    ~Lease();

    [[nodiscard]] RestRequestHandle handle() const noexcept { return handle_; }
    [[nodiscard]] explicit operator bool() const noexcept {
      return pool_ != nullptr;
    }

    // Commit transfers ownership of the slot to the pool until complete(),
    // cancel(), or pop_expired() releases it.
    [[nodiscard]] bool commit() noexcept;
    void cancel() noexcept;

   private:
    friend class RestRequestPool;
    Lease(RestRequestPool* pool, RestRequestHandle handle) noexcept
        : pool_(pool), handle_(handle) {}
    void move_from(Lease& other) noexcept;

    RestRequestPool* pool_{};
    RestRequestHandle handle_{};
  };

  explicit RestRequestPool(std::size_t capacity);
  ~RestRequestPool() = default;

  RestRequestPool(const RestRequestPool&) = delete;
  RestRequestPool& operator=(const RestRequestPool&) = delete;
  RestRequestPool(RestRequestPool&&) = delete;
  RestRequestPool& operator=(RestRequestPool&&) = delete;

  [[nodiscard]] std::optional<Lease> reserve(
      RestRequestCorrelation correlation, std::uint64_t deadline_ns) noexcept;
  [[nodiscard]] bool cancel(RestRequestHandle handle) noexcept;
  [[nodiscard]] bool complete(RestRequestHandle handle) noexcept;
  [[nodiscard]] const RestRequest* lookup(RestRequestHandle handle) const
      noexcept;
  [[nodiscard]] std::optional<RestRequest> pop_expired(
      std::uint64_t now_ns) noexcept;

  [[nodiscard]] std::size_t capacity() const noexcept { return slots_.size(); }
  [[nodiscard]] std::size_t size() const noexcept { return active_count_; }
  [[nodiscard]] bool empty() const noexcept { return active_count_ == 0; }

 private:
  enum class SlotState : std::uint8_t { Free, Reserved, Active };

  struct Slot {
    RestRequest request{};
    std::uint64_t generation{};
    SlotState state{SlotState::Free};
  };

  [[nodiscard]] bool commit_reservation(RestRequestHandle handle) noexcept;
  [[nodiscard]] bool release(RestRequestHandle handle) noexcept;
  [[nodiscard]] bool matches(RestRequestHandle handle) const noexcept;

  std::vector<Slot> slots_;
  std::vector<std::uint32_t> free_slots_;
  std::size_t free_count_{};
  std::size_t active_count_{};
};

}  // namespace oms::exchange
