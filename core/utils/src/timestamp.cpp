#include "utils/runtime/timestamp.h"

#include <cmath>
#include <ctime>
#include <limits>
#include <thread>

#if defined(__x86_64__) || defined(__i386__)
#include <cpuid.h>
#endif

namespace utils::runtime {
namespace {

bool HasUsableTsc() noexcept {
  static const bool supported = [] {
    const auto capability = Timestamp::Detect();
    return capability.rdtscp && capability.invariant_tsc;
  }();
  return supported;
}

}  // namespace

TscSample Timestamp::NowTSC() noexcept {
#if defined(__x86_64__) || defined(__i386__)
  if (!HasUsableTsc()) return {};
  std::uint32_t low;
  std::uint32_t high;
  std::uint32_t aux;
  asm volatile("lfence\n\trdtscp\n\tlfence"
               : "=a"(low), "=d"(high), "=c"(aux) :: "memory");
  return {(static_cast<std::uint64_t>(high) << 32) | low, aux};
#else
  return {};
#endif
}

std::uint64_t Timestamp::NowMono() noexcept {
  timespec value{};
  if (clock_gettime(CLOCK_MONOTONIC_RAW, &value) != 0) return 0;
  return static_cast<std::uint64_t>(value.tv_sec) * 1'000'000'000ULL +
         static_cast<std::uint64_t>(value.tv_nsec);
}

TscCapability Timestamp::Detect() noexcept {
  TscCapability result{};
#if defined(__x86_64__) || defined(__i386__)
  const unsigned max_extended = __get_cpuid_max(0x80000000, nullptr);
  unsigned eax, ebx, ecx, edx;
  if (max_extended >= 0x80000001 &&
      __get_cpuid(0x80000001, &eax, &ebx, &ecx, &edx))
    result.rdtscp = (edx & (1U << 27)) != 0;
  if (max_extended >= 0x80000007 &&
      __get_cpuid(0x80000007, &eax, &ebx, &ecx, &edx))
    result.invariant_tsc = (edx & (1U << 8)) != 0;
#endif
  if (result.rdtscp && result.invariant_tsc) result.state = TscCalibrationState::Detected;
  return result;
}

TscCapability Timestamp::Calibrate(std::chrono::milliseconds interval) noexcept {
  auto result = Detect();
  if (!result.rdtscp || !result.invariant_tsc) return result;
  if (interval.count() <= 0) {
    result.state = TscCalibrationState::InvalidInterval;
    return result;
  }
  const auto mono_begin = NowMono();
  const auto tsc_begin = NowTSC();
  std::this_thread::sleep_for(interval);
  const auto tsc_end = NowTSC();
  const auto mono_end = NowMono();
  if (mono_begin == 0 || mono_end == 0 || tsc_begin.cycles == 0 || tsc_end.cycles == 0) {
    result.state = TscCalibrationState::ClockUnavailable;
    return result;
  }
  if (tsc_begin.aux != tsc_end.aux) {
    result.state = TscCalibrationState::CpuMigration;
    return result;
  }
  if (mono_end <= mono_begin || tsc_end.cycles <= tsc_begin.cycles) {
    result.state = TscCalibrationState::NonMonotonic;
    return result;
  }
  result.cycles_per_ns = static_cast<double>(tsc_end.cycles - tsc_begin.cycles) /
                         static_cast<double>(mono_end - mono_begin);
  if (!std::isfinite(result.cycles_per_ns) || result.cycles_per_ns <= 0.0) {
    result.cycles_per_ns = 0.0;
    result.state = TscCalibrationState::NonMonotonic;
    return result;
  }
  result.calibrated = true;
  result.calibration_tsc = tsc_end.cycles;
  result.calibration_mono_ns = mono_end;
  result.state = TscCalibrationState::Calibrated;
  return result;
}

std::uint64_t Timestamp::CyclesToNs(
    std::uint64_t cycles, const TscCapability& capability) noexcept {
  if (!capability.calibrated || capability.cycles_per_ns <= 0.0) return 0;
  const long double nanoseconds =
      static_cast<long double>(cycles) / capability.cycles_per_ns;
  if (nanoseconds >= static_cast<long double>(std::numeric_limits<std::uint64_t>::max()))
    return std::numeric_limits<std::uint64_t>::max();
  return static_cast<std::uint64_t>(nanoseconds);
}

std::uint64_t Timestamp::TscToMonoNs(
    std::uint64_t cycles, const TscCapability& capability) noexcept {
  if (!capability.calibrated || capability.calibration_tsc == 0 ||
      capability.calibration_mono_ns == 0)
    return 0;
  if (cycles >= capability.calibration_tsc) {
    const auto delta = CyclesToNs(cycles - capability.calibration_tsc, capability);
    if (delta > std::numeric_limits<std::uint64_t>::max() -
                    capability.calibration_mono_ns)
      return std::numeric_limits<std::uint64_t>::max();
    return capability.calibration_mono_ns + delta;
  }
  const auto delta = CyclesToNs(capability.calibration_tsc - cycles, capability);
  return delta > capability.calibration_mono_ns
             ? 0
             : capability.calibration_mono_ns - delta;
}

}  // namespace utils::runtime
