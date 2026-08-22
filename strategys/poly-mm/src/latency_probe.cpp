#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string_view>
#include <thread>
#include <vector>

#include "polymm/fairprice_client.h"

int main(int argc, char** argv) {
  polymm::SidecarConfig config;
  std::uint32_t seconds = 30;
  if (argc > 1) config.fairprice_host = argv[1];
  if (argc > 2) config.fairprice_service = argv[2];
  if (argc > 3) config.fairprice_profile = argv[3];
  if (argc > 4) config.fairprice_symbol = argv[4];
  if (argc > 5)
    seconds = static_cast<std::uint32_t>(std::strtoul(argv[5], nullptr, 10));
  if (const char* token = std::getenv("AGGDATA_BROWSER_TOKEN"))
    config.fairprice_token = token;
  if (const char* origin = std::getenv("FAIRPRICE_ORIGIN"))
    config.fairprice_origin = origin;
  if (const char* secure = std::getenv("FAIRPRICE_SECURE"))
    config.fairprice_secure = std::string_view(secure) != "false";
  config.connect_timeout_ms = 500;

  polymm::FairPriceClient client(std::move(config));
  if (!client.start()) return 1;
  std::vector<std::uint64_t> lags;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(seconds);
  while (std::chrono::steady_clock::now() < deadline) {
    polymm::SidecarEvent event;
    while (client.try_pop(event)) {
      if (event.kind == polymm::SidecarEvent::Kind::FairPrice &&
          event.fair_price.received_ns >= event.fair_price.wall_ns) {
        lags.push_back(event.fair_price.received_ns -
                       event.fair_price.wall_ns);
      }
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  client.stop();
  if (lags.empty()) {
    std::cerr << "no fairprice samples received\n";
    return 2;
  }
  std::sort(lags.begin(), lags.end());
  const auto percentile = [&lags](double value) {
    const auto index = static_cast<std::size_t>(
        value * static_cast<double>(lags.size() - 1));
    return static_cast<double>(lags[index]) / 1e6;
  };
  const auto metrics = client.metrics();
  std::cout << "samples=" << lags.size() << " p50_ms=" << percentile(0.50)
            << " p95_ms=" << percentile(0.95)
            << " p99_ms=" << percentile(0.99)
            << " max_ms=" << static_cast<double>(lags.back()) / 1e6
            << " parse_errors=" << metrics.fairprice_parse_errors
            << " reconnects=" << metrics.fairprice_reconnects
            << " drops=" << metrics.queue_drops
            << '\n';
  return 0;
}
