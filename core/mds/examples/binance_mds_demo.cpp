#include "mds/service/binance_session.h"

#include <atomic>
#include <charconv>
#include <chrono>
#include <csignal>
#include <cstddef>
#include <iostream>
#include <limits>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

namespace {

std::atomic<bool> running{true};

extern "C" void on_signal(int) { running.store(false, std::memory_order_relaxed); }

struct Cli {
  std::string profile{"both"};
  std::string symbol{"BTCUSDT"};
  std::string shm_prefix{"/selfquant.mds"};
  std::string websocket_endpoint;
  std::string rest_endpoint;
  std::size_t ladder{8192};
  unsigned duration_seconds{60};
};

void usage(std::ostream &output) {
  output << "Usage: binance_mds_demo [options]\n"
            "  --profile spot|usdm|both  (default both)\n"
            "  --symbol SYMBOL           (default BTCUSDT)\n"
            "  --duration SECONDS        (0 runs until SIGINT)\n"
            "  --shm-prefix PREFIX       (default /selfquant.mds)\n"
            "  --ladder LEVELS           (default 8192, max 16384)\n"
            "  --ws-endpoint URL         override selected WebSocket endpoint(s)\n"
            "  --rest-endpoint URL       override selected REST endpoint(s)\n";
}

template <typename T> bool parse_integer(std::string_view text, T &value) {
  const auto parsed =
      std::from_chars(text.data(), text.data() + text.size(), value);
  return parsed.ec == std::errc{} && parsed.ptr == text.data() + text.size();
}

bool parse_cli(int argc, char **argv, Cli &cli) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--help" || argument == "-h") {
      usage(std::cout);
      return false;
    }
    if (index + 1 >= argc) {
      std::cerr << "missing value for " << argument << '\n';
      return false;
    }
    const std::string_view value(argv[++index]);
    if (argument == "--profile") {
      cli.profile = value;
    } else if (argument == "--symbol") {
      cli.symbol = value;
    } else if (argument == "--duration") {
      if (!parse_integer(value, cli.duration_seconds)) {
        std::cerr << "invalid duration\n";
        return false;
      }
    } else if (argument == "--shm-prefix") {
      cli.shm_prefix = value;
    } else if (argument == "--ladder") {
      if (!parse_integer(value, cli.ladder)) {
        std::cerr << "invalid ladder size\n";
        return false;
      }
    } else if (argument == "--ws-endpoint") {
      cli.websocket_endpoint = value;
    } else if (argument == "--rest-endpoint") {
      cli.rest_endpoint = value;
    } else {
      std::cerr << "unknown option: " << argument << '\n';
      return false;
    }
  }
  if ((cli.profile != "spot" && cli.profile != "usdm" &&
       cli.profile != "both") ||
      cli.symbol.empty() || cli.ladder == 0 ||
      cli.ladder > utils::md::kMaxLadderLevels) {
    std::cerr << "invalid profile, symbol, or ladder\n";
    return false;
  }
  return true;
}

} // namespace

int main(int argc, char **argv) {
  Cli cli;
  if (!parse_cli(argc, argv, cli)) {
    return 2;
  }
  std::signal(SIGINT, on_signal);
  std::signal(SIGTERM, on_signal);

  mds::service::SessionManager manager;
  std::vector<mds::service::BinanceSession *> sessions;
  const auto add = [&](mds::exchange::binance::Profile profile) {
    mds::service::BinanceSessionOptions options;
    options.profile = profile;
    options.symbol = cli.symbol;
    options.shm_prefix = cli.shm_prefix;
    options.websocket_endpoint = cli.websocket_endpoint;
    options.rest_endpoint = cli.rest_endpoint;
    options.ladder_ticks_per_side = cli.ladder;
    options.max_ladder_ticks_per_side = utils::md::kMaxLadderLevels;
    auto created = manager.create(std::move(options));
    if (!created) {
      std::cerr << "session start failed: " << created.message << '\n';
      return false;
    }
    sessions.push_back(created.value);
    return true;
  };

  if ((cli.profile == "spot" || cli.profile == "both") &&
      !add(mds::exchange::binance::Profile::Spot)) {
    return 1;
  }
  if ((cli.profile == "usdm" || cli.profile == "both") &&
      !add(mds::exchange::binance::Profile::UsdM)) {
    return 1;
  }

  std::vector<mds::service::BinanceSessionState> previous;
  previous.reserve(sessions.size());
  for (const auto *session : sessions) {
    previous.push_back(session->state());
    std::cout << (session->profile() == mds::exchange::binance::Profile::Spot
                      ? "spot"
                      : "usdm")
              << " ticker=" << session->ticker_segment()
              << " orderbook=" << session->orderbook_segment()
              << " state=" << mds::service::to_string(session->state()) << '\n';
  }

  const auto started = std::chrono::steady_clock::now();
  while (running.load(std::memory_order_relaxed) &&
         (cli.duration_seconds == 0 ||
          std::chrono::steady_clock::now() - started <
              std::chrono::seconds(
                  static_cast<std::chrono::seconds::rep>(
                      cli.duration_seconds)))) {
    if (manager.run_once(100) < 0) {
      std::cerr << "epoll wait failed\n";
      break;
    }
    for (std::size_t index = 0; index < sessions.size(); ++index) {
      if (sessions[index]->state() != previous[index]) {
        previous[index] = sessions[index]->state();
        std::cout << (sessions[index]->profile() ==
                              mds::exchange::binance::Profile::Spot
                          ? "spot"
                          : "usdm")
                  << " state="
                  << mds::service::to_string(sessions[index]->state())
                  << " generation=" << sessions[index]->generation();
        if (!sessions[index]->error_message().empty()) {
          std::cout << " reason=" << sessions[index]->error_message();
        }
        std::cout << '\n';
      }
    }
  }

  manager.stop();
  for (const auto *session : sessions) {
    const auto &metrics = session->metrics();
    std::cout << (session->profile() == mds::exchange::binance::Profile::Spot
                      ? "spot"
                      : "usdm")
              << " stopped reconnects=" << metrics.reconnects
              << " resyncs=" << metrics.resyncs
              << " rotations=" << metrics.rotations
              << " rotation_mode=safe-reconnect"
              << " depth=" << metrics.depth_updates
              << " ticker=" << metrics.ticker_updates << '\n';
  }
  return 0;
}
