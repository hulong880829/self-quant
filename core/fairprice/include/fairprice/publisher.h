#pragma once

#include "fairprice/engine.h"
#include "mds/publish/wire_publisher.h"

#include <cstdint>
#include <memory>
#include <string>
#include <unordered_map>

namespace fairprice {

class FairPublisher {
 public:
  explicit FairPublisher(std::string prefix, mds::transport::RingOptions options);

  void publish(const std::string &key, std::string_view profile,
               std::string_view symbol, const FairPricePod &pod);
  void close_missing(const std::unordered_map<std::string, bool> &active);
  [[nodiscard]] std::uint64_t dropped() const noexcept { return dropped_; }

 private:
  mds::publish::WirePublisher *ensure(const std::string &key,
                                      std::string_view profile,
                                      std::string_view symbol);

  std::string prefix_;
  mds::transport::RingOptions options_;
  std::unordered_map<std::string, std::unique_ptr<mds::publish::WirePublisher>>
      publishers_;
  std::uint64_t dropped_{};
};

}  // namespace fairprice
