#include <cstdlib>
#include <iostream>
#include <string_view>

namespace {

enum class Venue { Unknown, BinanceSpot, BinanceUsdm, Polymarket };

struct Options {
  Venue venue{Venue::Unknown};
  std::string_view rest_host;
  std::string_view websocket_host;
  bool execute_live{};
  bool confirm_live{};
};

bool Present(const char* name) noexcept {
  const char* value = std::getenv(name);
  return value != nullptr && value[0] != '\0';
}

std::string_view VenueName(Venue venue) noexcept {
  switch (venue) {
    case Venue::BinanceSpot:
      return "binance-spot";
    case Venue::BinanceUsdm:
      return "binance-usdm";
    case Venue::Polymarket:
      return "polymarket";
    case Venue::Unknown:
      break;
  }
  return "unknown";
}

bool Parse(int argc, char** argv, Options& output) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    const auto value = [&](std::string_view& target) {
      if (index + 1 >= argc) return false;
      target = argv[++index];
      return !target.empty();
    };
    if (argument == "--venue") {
      std::string_view venue;
      if (!value(venue)) return false;
      if (venue == "binance-spot")
        output.venue = Venue::BinanceSpot;
      else if (venue == "binance-usdm")
        output.venue = Venue::BinanceUsdm;
      else if (venue == "polymarket")
        output.venue = Venue::Polymarket;
      else
        return false;
    } else if (argument == "--rest-host") {
      if (!value(output.rest_host)) return false;
    } else if (argument == "--ws-host") {
      if (!value(output.websocket_host)) return false;
    } else if (argument == "--execute-live") {
      output.execute_live = true;
    } else if (argument == "--i-understand-real-orders") {
      output.confirm_live = true;
    } else if (argument == "--dry-run") {
      output.execute_live = false;
    } else {
      return false;
    }
  }
  return output.venue != Venue::Unknown;
}

bool CredentialsAvailable(Venue venue) noexcept {
  if (venue == Venue::BinanceSpot || venue == Venue::BinanceUsdm) {
    return (Present("BINANCE_OMS_API_KEY") &&
            Present("BINANCE_OMS_SECRET")) ||
           (Present("BINANCE_OMS_API_KEY_FD") &&
            Present("BINANCE_OMS_SECRET_FD"));
  }
  if (venue == Venue::Polymarket) {
    return Present("POLYMARKET_OMS_SIGNER") &&
           Present("POLYMARKET_OMS_FUNDER") &&
           (Present("POLYMARKET_OMS_PRIVATE_KEY") ||
            Present("POLYMARKET_OMS_PRIVATE_KEY_FD")) &&
           Present("POLYMARKET_OMS_API_KEY") &&
           Present("POLYMARKET_OMS_API_SECRET") &&
           Present("POLYMARKET_OMS_PASSPHRASE");
  }
  return false;
}

void Usage() {
  std::cerr
      << "usage: oms_acceptance --venue "
         "{binance-spot|binance-usdm|polymarket} "
         "[--rest-host HOST] [--ws-host HOST] [--dry-run]\n"
         "       live orders additionally require --execute-live "
         "--i-understand-real-orders\n";
}

}  // namespace

int main(int argc, char** argv) {
  Options options;
  if (!Parse(argc, argv, options)) {
    Usage();
    return 2;
  }
  const bool credentials = CredentialsAvailable(options.venue);
  std::cout << "{\"venue\":\"" << VenueName(options.venue)
            << "\",\"mode\":\""
            << (options.execute_live ? "live" : "dry-run")
            << "\",\"credentials_present\":"
            << (credentials ? "true" : "false")
            << ",\"rest_endpoint_override\":"
            << (!options.rest_host.empty() ? "true" : "false")
            << ",\"websocket_endpoint_override\":"
            << (!options.websocket_host.empty() ? "true" : "false")
            << "}\n";
  if (!options.execute_live) return 0;
  if (!options.confirm_live) {
    std::cerr << "live execution requires explicit confirmation\n";
    return 2;
  }
  if (!credentials) {
    std::cerr << "required credentials are unavailable\n";
    return 3;
  }
  std::cerr << "live venue acceptance is not started by this preflight-only "
               "build\n";
  return 4;
}
