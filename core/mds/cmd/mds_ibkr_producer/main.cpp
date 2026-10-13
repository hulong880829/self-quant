#include "ibkr_config.h"
#include "ibkr_candidates.h"

#include "mds/ibkr/ibkr_tws_client.h"

#include <atomic>
#include <chrono>
#include <cstddef>
#include <csignal>
#include <iostream>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

namespace {

std::atomic<bool> running{true};

extern "C" void request_stop(int) noexcept {
  running.store(false, std::memory_order_relaxed);
}

struct Cli {
  std::string config_path;
  bool validate_only{};
};

void usage(std::ostream &output) {
  output << "Usage: mds_ibkr_producer --config PATH [--validate-only]\n";
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

}  // namespace

int main(int argc, char **argv) {
  Cli cli;
  if (!parse_cli(argc, argv, cli)) {
    usage(std::cerr);
    return 2;
  }
  auto loaded = mds::ibkr::load_config(cli.config_path);
  if (!loaded) {
    std::cerr << loaded.message << '\n';
    return 2;
  }
  if (!loaded.value.postgres_url_env.empty()) {
    mds::ibkr::CandidateLoad candidates;
    std::string candidate_error;
    if (!mds::ibkr::load_postgres_candidates(
            loaded.value.postgres_url_env, candidates, candidate_error)) {
      std::cerr << candidate_error << '\n';
      return 2;
    }
    loaded.value.runtime.subscriptions = std::move(candidates.subscriptions);
    if (candidates.skipped != 0) {
      std::cerr << "ibkr postgres skipped invalid candidates="
                << candidates.skipped << '\n';
    }
    const auto postgres_url_env = loaded.value.postgres_url_env;
    loaded.value.runtime.persist_turnover_metrics =
        [postgres_url_env](
            const std::vector<mds::ibkr::InstrumentTurnoverMetric> &metrics,
            std::size_t &updated, std::string &persist_error) {
          const bool persisted = mds::ibkr::persist_postgres_turnover_metrics(
              postgres_url_env, metrics, updated, persist_error);
          if (persisted && updated != metrics.size()) {
            std::cerr << "ibkr turnover persistence warning requested="
                      << metrics.size() << " updated=" << updated << '\n';
          }
          return persisted;
        };
  }
  if (cli.validate_only) {
    std::cout << "ibkr config ok subscriptions="
              << loaded.value.runtime.subscriptions.size()
              << " max_subscriptions=" << loaded.value.max_subscriptions
              << '\n';
    return 0;
  }

  mds::ibkr::TwsIbkrClient client;
  mds::ibkr::IbkrRuntime runtime(loaded.value.runtime, client);
  std::string error;
  if (!runtime.start(error)) {
    std::cerr << error << '\n';
    return 1;
  }
  std::signal(SIGINT, request_stop);
  std::signal(SIGTERM, request_stop);
  while (running.load(std::memory_order_relaxed) && !runtime.failed()) {
    std::this_thread::sleep_for(std::chrono::milliseconds(200));
  }
  const auto failed = runtime.failed();
  const auto exit_code = failed ? runtime.exit_code() : 0;
  if (failed) {
    std::cerr << runtime.failure_message() << '\n';
  }
  runtime.stop();
  return exit_code;
}
