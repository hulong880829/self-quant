#pragma once

#include <cstdint>
#include <type_traits>

namespace oms::api {

enum class Error : std::uint8_t {
  Ok = 0,
  InvalidArgument = 1,
  InvalidScale = 2,
  InvalidTransition = 3,
  NotFound = 4,
  CapacityExceeded = 5,
  Duplicate = 6,
  Conflict = 7,
  StaleHandle = 8,
  Frozen = 9,
  Untradeable = 10,
  ProtocolConflict = 11,
  Overfill = 12,
  ArithmeticOverflow = 13,
  QueueFull = 14,
  InvalidMode = 15,
  NotReady = 16,
  ShuttingDown = 17,
  DeadlineCapacityExceeded = 18,
  Unsupported = 19
};

using ErrorCode = Error;

template <typename T>
struct Result {
  T value{};
  Error error{Error::Ok};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return error == Error::Ok;
  }
};

template <>
struct Result<void> {
  Error error{Error::Ok};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return error == Error::Ok;
  }
};

static_assert(std::is_trivially_copyable_v<Error>);
static_assert(std::is_trivially_copyable_v<Result<std::uint64_t>>);
static_assert(std::is_trivially_copyable_v<Result<void>>);
static_assert(static_cast<std::uint8_t>(Error::ArithmeticOverflow) == 13);
static_assert(static_cast<std::uint8_t>(Error::QueueFull) == 14);
static_assert(static_cast<std::uint8_t>(Error::InvalidMode) == 15);
static_assert(static_cast<std::uint8_t>(Error::NotReady) == 16);
static_assert(static_cast<std::uint8_t>(Error::ShuttingDown) == 17);
static_assert(
    static_cast<std::uint8_t>(Error::DeadlineCapacityExceeded) == 18);
static_assert(static_cast<std::uint8_t>(Error::Unsupported) == 19);

}  // namespace oms::api
