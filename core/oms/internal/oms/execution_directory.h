#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

#include "oms/api/error.h"
#include "oms/api/order_types.h"

namespace oms {

// Fixed-capacity owner-thread directory. Construction performs every
// allocation; Register/Retire/lookups never resize and require no lock.
class ExecutionDirectory {
 public:
  enum class Lifecycle : std::uint8_t { Active = 1, Retiring = 2 };

  struct Entry {
    api::InstrumentId instrument_id{};
    api::ResolvedInstrument routing{};
    Lifecycle lifecycle{Lifecycle::Active};
    bool occupied{};
  };

  explicit ExecutionDirectory(std::size_t capacity);

  [[nodiscard]] std::size_t capacity() const noexcept {
    return entries_.size();
  }
  [[nodiscard]] std::size_t size() const noexcept { return size_; }

  api::Error Register(api::InstrumentId instrument_id,
                      const api::ResolvedInstrument& routing) noexcept;
  // The owner marks an entry Retiring before checking references. Deferred
  // entries remain addressable for control-plane reconciliation but are no
  // longer Active.
  api::Error Retire(api::InstrumentId instrument_id,
                    bool has_active_or_inflight) noexcept;

  [[nodiscard]] const Entry* Find(api::InstrumentId instrument_id) const
      noexcept;
  [[nodiscard]] api::InstrumentId Find(
      const api::ResolvedInstrument& routing) const noexcept;
  [[nodiscard]] api::InstrumentId Find(
      const api::VenueInstrumentRef& reference) const noexcept;
  [[nodiscard]] api::InstrumentId FindPolymarketToken(
      const std::array<std::uint8_t, 32>& token_id) const noexcept;

  // Test/diagnostic counter. Submit and cancel must not change this value.
  [[nodiscard]] std::uint64_t access_count() const noexcept {
    return access_count_;
  }

 private:
  struct IdSlot {
    api::InstrumentId key{};
    std::uint32_t entry{};
    std::uint8_t state{};
  };
  struct RouteSlot {
    api::ResolvedInstrument key{};
    std::uint32_t entry{};
    std::uint8_t state{};
  };

  [[nodiscard]] const Entry* FindEntry(api::InstrumentId instrument_id) const
      noexcept;
  [[nodiscard]] std::size_t FindIdSlot(api::InstrumentId instrument_id) const
      noexcept;
  [[nodiscard]] std::size_t FindRouteSlot(
      const api::ResolvedInstrument& routing) const noexcept;
  api::Error InsertId(api::InstrumentId instrument_id,
                      std::uint32_t entry) noexcept;
  api::Error InsertRoute(const api::ResolvedInstrument& routing,
                         std::uint32_t entry) noexcept;
  void EraseId(api::InstrumentId instrument_id) noexcept;
  void EraseRoute(const api::ResolvedInstrument& routing) noexcept;

  std::vector<Entry> entries_;
  std::vector<std::uint32_t> free_slots_;
  std::vector<IdSlot> by_id_;
  std::vector<RouteSlot> by_route_;
  std::size_t free_count_{};
  std::size_t size_{};
  mutable std::uint64_t access_count_{};
};

}  // namespace oms
