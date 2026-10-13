#include "fairprice/config.h"
#include "fairprice/log.h"
#include "fairprice/runtime.h"

#include <atomic>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <iostream>
#include <string>
#include <string_view>
#include <thread>
#include <unistd.h>

#include <signal.h>

namespace {

std::atomic<bool> running{true};
std::atomic<int> stop_signum{0};

extern "C" void request_stop(int signum) noexcept {
  stop_signum.store(signum, std::memory_order_relaxed);
  running.store(false, std::memory_order_relaxed);
}

struct Cli {
  std::string config_path;
  bool validate_only{};
};

void usage(std::ostream &output) {
  output << "Usage: fairprice --config PATH [--validate-only]\n";
}

bool parse_cli(int argc, char **argv, Cli &cli) {
  for (int index = 1; index < argc; ++index) {
    const auto argument = std::string_view(argv[index]);
    if (argument == "--validate-only") {
      cli.validate_only = true;
      continue;
    }
    if (argument == "--help" || argument == "-h") {
      return false;
    }
    if (argument != "--config") {
      return false;
    }
    if (++index == argc) {
      return false;
    }
    cli.config_path = argv[index];
  }
  return !cli.config_path.empty();
}

bool ignore_sigpipe() noexcept {
  struct sigaction action {};
  action.sa_handler = SIG_IGN;
  return ::sigaction(SIGPIPE, &action, nullptr) == 0;
}

const char *stop_reason_name(int signum) noexcept {
  if (signum == SIGINT) {
    return "SIGINT";
  }
  if (signum == SIGTERM) {
    return "SIGTERM";
  }
  return "unknown";
}

}  // namespace

int main(int argc, char **argv) {
  Cli cli;
  if (!parse_cli(argc, argv, cli)) {
    usage(std::cerr);
    return 2;
  }
  auto loaded = fairprice::load_config(cli.config_path);
  if (!loaded) {
    std::cerr << "fairprice: " << loaded.message << '\n';
    return 1;
  }
  auto validated = fairprice::validate_loaded_config(loaded.value);
  if (!validated) {
    std::cerr << "fairprice: " << validated.message << '\n';
    return 1;
  }
  if (cli.validate_only) {
    return 0;
  }
  fairprice::LogLevel log_level{};
  if (!fairprice::parse_log_level(loaded.value.log_level, log_level)) {
    std::cerr << "fairprice: runtime.log_level must be debug, info, warn, or error\n";
    return 1;
  }
  const auto start_wall_ns = static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
  const auto instance =
      std::to_string(::getpid()) + "-" + std::to_string(start_wall_ns);
  fairprice::init_log(log_level, instance);
  fairprice::log_info("starting config=%s log_level=%s development=%s",
                      cli.config_path.c_str(), loaded.value.log_level.c_str(),
                      loaded.value.development ? "true" : "false");
  if (loaded.value.fair_price.enabled) {
    std::string error;
    if (!fairprice::Engine::create(loaded.value.fair_price, error)) {
      fairprice::log_error("%s", error.c_str());
      return 1;
    }
  }
  if (!ignore_sigpipe()) {
    fairprice::log_error("failed to ignore SIGPIPE");
    return 1;
  }
  std::signal(SIGINT, request_stop);
  std::signal(SIGTERM, request_stop);
  fairprice::Runtime runtime(std::move(loaded.value));
  auto started = runtime.start();
  if (!started) {
    fairprice::log_error("%s", started.message.c_str());
    return 1;
  }
  while (running.load(std::memory_order_relaxed)) {
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
  }
  const auto signum = stop_signum.load(std::memory_order_relaxed);
  const auto *reason = stop_reason_name(signum);
  fairprice::log_info("stopping reason=%s", reason);
  runtime.stop();
  fairprice::log_info("stopped reason=%s", reason);
  return 0;
}
