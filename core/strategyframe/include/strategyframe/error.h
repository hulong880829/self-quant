#pragma once

#include <cstdint>
#include <utility>

namespace strategyframe {

enum class Error : std::uint16_t {
  Ok = 0,
  InvalidArgument = 1,
  InvalidConfig = 2,
  InvalidThread = 3,
  InvalidState = 4,
  NotReady = 5,
  WouldBlock = 6,
  Unsupported = 7,
  CapacityExceeded = 8,
  NotFound = 9,
  ShuttingDown = 10,
  CallbackFailed = 11,
  MdsFailure = 12,
  OmsFailure = 13,
  Internal = 14,
};

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

}  // namespace strategyframe
