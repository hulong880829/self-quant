#pragma once

#include <array>
#include <cstddef>
#include <cstdint>

#include "strategyframe/types.h"

namespace polymm {

inline constexpr std::size_t kMaxCandidates = 4;
inline constexpr std::size_t kMaxRetiredWindows = 16;
inline constexpr std::size_t kPnlRetiredRows = 32;
static_assert(kMaxRetiredWindows * 2U == kPnlRetiredRows);

struct MarketBindingConfig {
  strategyframe::Venue venue{strategyframe::Venue::Unknown};
  strategyframe::ProductType product{strategyframe::ProductType::Unknown};
  strategyframe::InstrumentSymbol up_symbol{};
  strategyframe::InstrumentSymbol down_symbol{};
  std::uint64_t window_ns{};
};

struct WindowCandidate {
  bool used{};
  std::uint64_t begin_ns{};
  std::uint64_t expiry_ns{};
  strategyframe::InstrumentCatalogInfo up{};
  strategyframe::InstrumentCatalogInfo down{};
  bool has_up{};
  bool has_down{};
};

struct ActiveWindow {
  bool valid{};
  std::uint64_t generation{};
  std::uint64_t begin_ns{};
  std::uint64_t expiry_ns{};
  strategyframe::InstrumentCatalogInfo up{};
  strategyframe::InstrumentCatalogInfo down{};
};

[[nodiscard]] inline bool same_market_condition(
    const strategyframe::ConditionId& up,
    const strategyframe::ConditionId& down) noexcept {
  return up.length != 0 && up == down;
}

enum class CatalogAdmit : std::uint8_t {
  Ignored = 0,
  Cached = 1,
  Duplicate = 2,
  Rejected = 3,
  Dropped = 4,
};

class RollingPairBinding {
 public:
  explicit RollingPairBinding(MarketBindingConfig config) noexcept;

  [[nodiscard]] CatalogAdmit on_catalog(
      const strategyframe::InstrumentCatalogInfo& catalog,
      std::uint64_t now_wall_ns) noexcept;
  void set_dirty() noexcept { dirty_ = true; }
  [[nodiscard]] bool dirty() const noexcept { return dirty_; }
  [[nodiscard]] std::uint64_t next_deadline_ns() const noexcept;
  [[nodiscard]] const ActiveWindow& active() const noexcept { return active_; }
  [[nodiscard]] const MarketBindingConfig& config() const noexcept {
    return config_;
  }
  [[nodiscard]] bool is_candidate_instrument(
      strategyframe::InstrumentId instrument_id) const noexcept;
  [[nodiscard]] const std::array<WindowCandidate, kMaxCandidates>&
  candidates() const noexcept {
    return candidates_;
  }

  template <typename Ready>
  [[nodiscard]] ActiveWindow propose_live_window(
      std::uint64_t now_wall_ns, Ready ready) const noexcept {
    ActiveWindow best{};
    for (const WindowCandidate& candidate : candidates_) {
      if (!candidate.used || !candidate.has_up || !candidate.has_down)
        continue;
      if (now_wall_ns < candidate.begin_ns ||
          now_wall_ns >= candidate.expiry_ns) {
        continue;
      }
      if (last_activated_expiry_ns_ == candidate.expiry_ns) continue;
      if (!ready(candidate.up.instrument_id) ||
          !ready(candidate.down.instrument_id)) {
        continue;
      }
      if (!best.valid || candidate.expiry_ns < best.expiry_ns) {
        best.valid = true;
        best.begin_ns = candidate.begin_ns;
        best.expiry_ns = candidate.expiry_ns;
        best.up = candidate.up;
        best.down = candidate.down;
      }
    }
    return best;
  }

  [[nodiscard]] bool propose_expiry(std::uint64_t now_wall_ns) const noexcept;
  void commit_activation(const ActiveWindow& window) noexcept;
  void commit_deactivation() noexcept;

 private:
  [[nodiscard]] int leg_index(
      const strategyframe::InstrumentCatalogInfo& catalog) const noexcept;
  WindowCandidate* find_expiry(std::uint64_t expiry_ns) noexcept;
  bool insert_candidate(WindowCandidate value, std::uint64_t now_wall_ns,
                        CatalogAdmit& admit) noexcept;
  void drop_expired(std::uint64_t now_wall_ns) noexcept;
  [[nodiscard]] WindowCandidate* farthest_future(
      std::uint64_t now_wall_ns) noexcept;

  MarketBindingConfig config_{};
  std::array<WindowCandidate, kMaxCandidates> candidates_{};
  ActiveWindow active_{};
  std::uint64_t generation_{};
  std::uint64_t last_activated_expiry_ns_{};
  bool dirty_{};
};

}  // namespace polymm
