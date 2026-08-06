#pragma once

#include <chrono>
#include <cstdint>

namespace utils::runtime {

struct TscSample {
  std::uint64_t cycles{};
  std::uint32_t aux{};
};

enum class TscCalibrationState : std::uint8_t {
  Unsupported,
  Detected,
  InvalidInterval,
  ClockUnavailable,
  CpuMigration,
  NonMonotonic,
  Calibrated
};

struct TscCapability {
  bool rdtscp{};
  bool invariant_tsc{};
  bool calibrated{};
  double cycles_per_ns{};
  std::uint64_t calibration_tsc{};
  std::uint64_t calibration_mono_ns{};
  TscCalibrationState state{TscCalibrationState::Unsupported};
};

class Timestamp {
 public:
  [[nodiscard]] static TscSample NowTSC() noexcept;
  [[nodiscard]] static std::uint64_t NowMono() noexcept;
  [[nodiscard]] static TscCapability Detect() noexcept;
  [[nodiscard]] static TscCapability Calibrate(
      std::chrono::milliseconds interval = std::chrono::milliseconds(20)) noexcept;
  [[nodiscard]] static std::uint64_t CyclesToNs(
      std::uint64_t cycles, const TscCapability& capability) noexcept;
  [[nodiscard]] static std::uint64_t TscToMonoNs(
      std::uint64_t cycles, const TscCapability& capability) noexcept;
};

}  // namespace utils::runtime
