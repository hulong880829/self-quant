#pragma once

#include <cstdint>
#include <optional>

#include "utils/md/types.h"

namespace mds::book {

enum class SequenceDomain : std::uint8_t { Shared, Independent };
enum class OverlayAction : std::uint8_t {
  Ignored,
  Updated,
  Pending,
  Cleared,
  Divergence,
  TickerOnly,
};

class alignas(64) BboOverlay {
 public:
  explicit BboOverlay(SequenceDomain domain = SequenceDomain::Shared) noexcept
      : domain_(domain) {}

  [[nodiscard]] OverlayAction
  OnTicker(const utils::md::BboEvent &ticker,
           const utils::md::BboEvent &canonical) noexcept;
  [[nodiscard]] OverlayAction
  OnCanonical(const utils::md::BboEvent &canonical) noexcept;
  [[nodiscard]] std::optional<utils::md::BboEvent>
  Effective(const utils::md::BboEvent &canonical) const noexcept;
  [[nodiscard]] std::optional<utils::md::BboEvent> ticker() const noexcept;

  void Clear() noexcept;
  [[nodiscard]] bool valid() const noexcept { return valid_ != 0; }
  [[nodiscard]] std::uint64_t sequence() const noexcept { return sequence_; }
  [[nodiscard]] std::uint64_t divergence_count() const noexcept {
    return divergence_count_;
  }

 private:
  utils::md::Level bid_{};
  utils::md::Level ask_{};
  std::uint64_t sequence_{};
  std::uint64_t receive_tsc_{};
  std::uint32_t divergence_count_{};
  std::uint32_t generation_{};
  utils::md::InstrumentId instrument_id_{};
  SequenceDomain domain_{SequenceDomain::Shared};
  std::uint8_t source_id_{};
  std::uint8_t valid_{};
};

static_assert(sizeof(BboOverlay) == 128);
static_assert(alignof(BboOverlay) == 64);

}  // namespace mds::book
