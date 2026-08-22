#pragma once

#include "utils/md/wire.h"

#include <cstdint>
#include <unordered_map>

namespace mds::consume {

enum class SequenceError : std::uint8_t {
  None,
  Ring,
  Bus,
  Source,
};

class SequenceTracker {
 public:
  [[nodiscard]] SequenceError
  check(std::uint64_t ring_sequence,
        const utils::md::wire::RecordHeader &header) const noexcept;
  [[nodiscard]] SequenceError
  check_transport(std::uint64_t ring_sequence,
                  const utils::md::wire::RecordHeader &header) const noexcept;
  void accept(std::uint64_t ring_sequence,
              const utils::md::wire::RecordHeader &header) noexcept;
  void accept_transport(
      std::uint64_t ring_sequence,
      const utils::md::wire::RecordHeader &header) noexcept;
  void reset() noexcept;

 private:
  static bool
  is_instrument(const utils::md::wire::RecordHeader &header) noexcept;

  std::uint64_t last_ring_sequence_{};
  std::uint64_t last_bus_sequence_{};
  struct SourceState {
    std::uint64_t sequence{};
    std::uint32_t generation{};
    bool baseline{};
  };
  std::unordered_map<utils::md::InstrumentId, SourceState> sources_;
  bool sequence_baseline_{};
};

}  // namespace mds::consume
