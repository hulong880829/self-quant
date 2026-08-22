#include "mds/producer/producer_runtime.h"

#include <atomic>
#include <charconv>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <iostream>
#include <string>
#include <string_view>

namespace {

std::atomic<bool> running{true};

extern "C" void request_stop(int) noexcept {
  running.store(false, std::memory_order_relaxed);
}

struct Cli {
  std::string config_path;
  std::uint64_t duration_seconds{};
  bool validate_only{};
  bool discover_only{};
};

void usage(std::ostream &output) {
  output << "Usage: mds_producer --config PATH "
            "[--validate-only|--discover-only] "
            "[--duration SECONDS]\n";
}

bool parse_cli(int argc, char **argv, Cli &cli) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--help" || argument == "-h") {
      usage(std::cout);
      return false;
    }
    if (argument == "--validate-only") {
      cli.validate_only = true;
      continue;
    }
    if (argument == "--discover-only") {
      cli.discover_only = true;
      continue;
    }
    if (argument != "--config" && argument != "--duration") return false;
    if (++index == argc) return false;
    const std::string_view value(argv[index]);
    if (argument == "--config") {
      cli.config_path = value;
    } else {
      const auto parsed = std::from_chars(
          value.data(), value.data() + value.size(), cli.duration_seconds);
      if (parsed.ec != std::errc{} ||
          parsed.ptr != value.data() + value.size())
        return false;
    }
  }
  return !cli.config_path.empty() &&
         !(cli.validate_only && cli.discover_only) &&
         !(cli.discover_only && cli.duration_seconds != 0);
}

}  // namespace

int main(int argc, char **argv) {
  Cli cli;
  if (!parse_cli(argc, argv, cli)) {
    usage(std::cerr);
    return 2;
  }
  mds::producer::ProducerRuntime runtime(
      {cli.config_path, cli.validate_only, cli.discover_only,
       &std::cout, &std::cerr});
  const auto started = runtime.start();
  if (!started) {
    std::cerr << started.message << '\n';
    return cli.validate_only ? 2 : 1;
  }
  if (cli.validate_only || cli.discover_only) return 0;

  std::signal(SIGINT, request_stop);
  std::signal(SIGTERM, request_stop);
  std::signal(SIGPIPE, SIG_IGN);
  const auto began = std::chrono::steady_clock::now();
  while (running.load(std::memory_order_relaxed) &&
         (cli.duration_seconds == 0 ||
          std::chrono::steady_clock::now() - began <
              std::chrono::seconds(cli.duration_seconds))) {
    if (runtime.run_once(100) < 0) break;
  }
  runtime.stop();
  if (runtime.failed()) {
    std::cerr << runtime.error() << '\n';
    return 1;
  }
  return 0;
}
