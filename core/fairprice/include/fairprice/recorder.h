#pragma once

#include "fairprice/config.h"
#include "fairprice/store.h"

#include <atomic>
#include <cstdint>
#include <string>
#include <string_view>
#include <thread>

namespace fairprice {

class Recorder {
 public:
  Recorder(Store *store, PersistenceConfig config, std::string database_url);
  ~Recorder();
  void start();
  void stop();
  std::string query_history_json(std::string_view profile,
                                 std::string_view symbol,
                                 std::string_view model_id,
                                 std::string_view start_rfc3339,
                                 std::string_view end_rfc3339,
                                 std::string_view resolution, int &status);

 private:
  void sample_loop();
  void write_loop();
  bool upsert(const std::vector<Market *> &markets, std::uint64_t observed_ns);

  Store *store_{};
  PersistenceConfig config_{};
  std::string database_url_;
  std::atomic<bool> stop_{false};
  std::thread sample_thread_{};
  std::thread write_thread_{};
};

std::string fixed_decimal_string(std::int64_t mantissa, std::uint8_t scale);
std::string postgres_text_array_from_mask(std::uint32_t mask);
std::string history_degraded_reasons_json_field(
    std::string_view array_to_json_text);

struct SampleTickPlan {
  std::uint64_t sleep_until_mono_ns{};
  std::uint64_t missed{};
};

SampleTickPlan next_sample_tick(std::uint64_t now_mono_ns,
                                std::uint64_t interval_ns,
                                std::uint64_t next_tick_ns);
int validate_fair_price_history(std::string_view start_rfc3339,
                                std::string_view end_rfc3339,
                                std::string_view resolution, std::string &error);
bool should_retry_history(int status) noexcept;

}  // namespace fairprice
