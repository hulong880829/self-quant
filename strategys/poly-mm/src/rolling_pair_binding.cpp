#include "polymm/rolling_pair_binding.h"

namespace polymm {
namespace {

bool SameSymbol(const strategyframe::InstrumentSymbol& lhs,
                const strategyframe::InstrumentSymbol& rhs) noexcept {
  return lhs == rhs;
}

bool SameCatalog(const strategyframe::InstrumentCatalogInfo& lhs,
                 const strategyframe::InstrumentCatalogInfo& rhs) noexcept {
  return lhs.instrument_id == rhs.instrument_id &&
         lhs.generation == rhs.generation;
}

}  // namespace

RollingPairBinding::RollingPairBinding(MarketBindingConfig config) noexcept
    : config_(config) {}

int RollingPairBinding::leg_index(
    const strategyframe::InstrumentCatalogInfo& catalog) const noexcept {
  if (catalog.venue != config_.venue || catalog.product != config_.product)
    return -1;
  if (SameSymbol(catalog.canonical_symbol, config_.up_symbol)) return 0;
  if (SameSymbol(catalog.canonical_symbol, config_.down_symbol)) return 1;
  return -1;
}

WindowCandidate* RollingPairBinding::find_expiry(
    std::uint64_t expiry_ns) noexcept {
  for (WindowCandidate& candidate : candidates_) {
    if (candidate.used && candidate.expiry_ns == expiry_ns) return &candidate;
  }
  return nullptr;
}

void RollingPairBinding::drop_expired(std::uint64_t now_wall_ns) noexcept {
  for (WindowCandidate& candidate : candidates_) {
    if (!candidate.used) continue;
    if (active_.valid && candidate.expiry_ns == active_.expiry_ns) continue;
    if (candidate.expiry_ns <= now_wall_ns) candidate = {};
  }
}

WindowCandidate* RollingPairBinding::farthest_future(
    std::uint64_t now_wall_ns) noexcept {
  WindowCandidate* farthest = nullptr;
  for (WindowCandidate& candidate : candidates_) {
    if (!candidate.used) continue;
    if (active_.valid && candidate.expiry_ns == active_.expiry_ns) continue;
    if (candidate.begin_ns <= now_wall_ns) continue;
    if (farthest == nullptr || candidate.begin_ns > farthest->begin_ns)
      farthest = &candidate;
  }
  return farthest;
}

bool RollingPairBinding::insert_candidate(WindowCandidate value,
                                          std::uint64_t now_wall_ns,
                                          CatalogAdmit& admit) noexcept {
  drop_expired(now_wall_ns);
  for (WindowCandidate& candidate : candidates_) {
    if (!candidate.used) {
      candidate = value;
      admit = CatalogAdmit::Cached;
      return true;
    }
  }
  WindowCandidate* farthest = farthest_future(now_wall_ns);
  if (farthest != nullptr && value.begin_ns < farthest->begin_ns) {
    *farthest = value;
    admit = CatalogAdmit::Cached;
    return true;
  }
  admit = CatalogAdmit::Dropped;
  return false;
}

CatalogAdmit RollingPairBinding::on_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog,
    std::uint64_t now_wall_ns) noexcept {
  const int leg = leg_index(catalog);
  if (leg < 0) return CatalogAdmit::Ignored;
  if (config_.window_ns == 0 || catalog.expiry_time_ns < config_.window_ns ||
      catalog.lot_size <= 0 || catalog.tick_size <= 0 ||
      catalog.instrument_id == 0) {
    return CatalogAdmit::Rejected;
  }
  const std::uint64_t begin_ns = catalog.expiry_time_ns - config_.window_ns;
  WindowCandidate* existing = find_expiry(catalog.expiry_time_ns);
  if (existing != nullptr) {
    strategyframe::InstrumentCatalogInfo& dest =
        leg == 0 ? existing->up : existing->down;
    bool& has = leg == 0 ? existing->has_up : existing->has_down;
    if (has && SameCatalog(dest, catalog)) return CatalogAdmit::Duplicate;
    dest = catalog;
    has = true;
    existing->begin_ns = begin_ns;
    existing->expiry_ns = catalog.expiry_time_ns;
    dirty_ = true;
    return CatalogAdmit::Cached;
  }
  WindowCandidate value{};
  value.used = true;
  value.begin_ns = begin_ns;
  value.expiry_ns = catalog.expiry_time_ns;
  if (leg == 0) {
    value.up = catalog;
    value.has_up = true;
  } else {
    value.down = catalog;
    value.has_down = true;
  }
  CatalogAdmit admit = CatalogAdmit::Dropped;
  if (!insert_candidate(value, now_wall_ns, admit)) return admit;
  dirty_ = true;
  return admit;
}

std::uint64_t RollingPairBinding::next_deadline_ns() const noexcept {
  std::uint64_t deadline = 0;
  const auto consider = [&](std::uint64_t time) {
    if (time == 0) return;
    if (deadline == 0 || time < deadline) deadline = time;
  };
  if (active_.valid) consider(active_.expiry_ns);
  for (const WindowCandidate& candidate : candidates_) {
    if (!candidate.used) continue;
    consider(candidate.begin_ns);
    consider(candidate.expiry_ns);
  }
  return deadline;
}

bool RollingPairBinding::is_candidate_instrument(
    strategyframe::InstrumentId instrument_id) const noexcept {
  if (instrument_id == 0) return false;
  if (active_.valid &&
      (active_.up.instrument_id == instrument_id ||
       active_.down.instrument_id == instrument_id)) {
    return true;
  }
  for (const WindowCandidate& candidate : candidates_) {
    if (!candidate.used) continue;
    if ((candidate.has_up && candidate.up.instrument_id == instrument_id) ||
        (candidate.has_down &&
         candidate.down.instrument_id == instrument_id)) {
      return true;
    }
  }
  return false;
}

bool RollingPairBinding::propose_expiry(
    std::uint64_t now_wall_ns) const noexcept {
  return active_.valid && now_wall_ns >= active_.expiry_ns;
}

void RollingPairBinding::commit_activation(
    const ActiveWindow& window) noexcept {
  active_ = window;
  active_.valid = true;
  active_.generation = ++generation_;
  last_activated_expiry_ns_ = window.expiry_ns;
  dirty_ = false;
}

void RollingPairBinding::commit_deactivation() noexcept {
  active_.valid = false;
  dirty_ = true;
}

}  // namespace polymm
