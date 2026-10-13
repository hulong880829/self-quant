#include "ibkr_config.h"
#include "ibkr_candidates.h"

#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <unistd.h>

namespace {

void check(bool condition, std::string_view message) {
  if (!condition) {
    throw std::runtime_error(std::string(message));
  }
}

class TempYaml {
 public:
  TempYaml(std::string_view content, std::string_view suffix) {
    path_ = std::filesystem::temp_directory_path() /
            ("mds-ibkr-config-" + std::to_string(::getpid()) + "-" +
             std::string(suffix) + ".yaml");
    std::ofstream output(path_);
    output << content;
  }

  ~TempYaml() {
    std::error_code error;
    std::filesystem::remove(path_, error);
  }

  [[nodiscard]] std::string string() const { return path_.string(); }

 private:
  std::filesystem::path path_;
};

constexpr std::string_view kLegal = R"(ibkr:
  host: 127.0.0.1
  port: 4002
  client_id: 7
shared_memory:
  prefix: /selfquant.mds.ibkr.test
subscriptions:
  - canonical_symbol: AAPL
    con_id: 265598
    primary_exchange: nasdaq
)";

std::string with_subscriptions(std::string_view subscriptions) {
  return "ibkr:\n  host: 127.0.0.1\n  port: 4002\n  client_id: 1\n"
         "shared_memory:\n  prefix: /selfquant.mds.ibkr.test\n"
         "subscriptions:\n" +
         std::string(subscriptions);
}

void expect_reject(std::string_view yaml, std::string_view suffix,
                   std::string_view needle) {
  const TempYaml file(yaml, suffix);
  const auto loaded = mds::ibkr::load_config(file.string());
  check(!loaded, std::string(suffix) + " was accepted");
  check(loaded.message.find("failed to load ibkr producer config") !=
            std::string::npos,
        "rejection was not prefixed");
  check(loaded.message.find(needle) != std::string::npos,
        std::string(suffix) + " message missing " + std::string(needle) +
            ": " + loaded.message);
}

}  // namespace

int main() {
  {
    const TempYaml file(kLegal, "legal");
    const auto loaded = mds::ibkr::load_config(file.string());
    check(static_cast<bool>(loaded), loaded.message);
    const auto &config = loaded.value;
    check(config.runtime.host == "127.0.0.1" && config.runtime.port == 4002 &&
              config.runtime.client_id == 7,
          "connection defaults were not parsed");
    check(config.market_data_mode == "LIVE_ONLY" &&
              config.runtime.subscribe_pacing_per_second == 20 &&
              config.max_subscriptions == mds::ibkr::kHardMaxSubscriptions &&
              config.runtime.filter.max_subscriptions ==
                  mds::ibkr::kHardMaxSubscriptions,
          "ibkr defaults were not applied");
    check(!config.runtime.filter.min_average_volume_90d.has_value() &&
              !config.runtime.filter.min_market_cap_usd.has_value(),
          "selection thresholds were enabled by default");
    check(config.backend == "POSIX_SHM" && config.mode == "OVERWRITE_OLDEST" &&
              config.ring_layout == "multiplex" && config.shard_count == 1 &&
              !config.runtime.unlink_on_shutdown,
          "shared-memory defaults were not applied");
    check(config.runtime.subscriptions.size() == 1 &&
              config.runtime.subscriptions[0].canonical_symbol == "AAPL" &&
              config.runtime.subscriptions[0].request_symbol == "AAPL" &&
              config.runtime.subscriptions[0].con_id == 265598 &&
              config.runtime.subscriptions[0].sec_type == "STK" &&
              config.runtime.subscriptions[0].exchange == "SMART" &&
              config.runtime.subscriptions[0].currency == "USD" &&
              config.runtime.subscriptions[0].primary_exchange == "NASDAQ" &&
              config.runtime.subscriptions[0].price_scale == 4,
          "subscription identity was not normalized");
  }

  const auto example = mds::ibkr::load_config(MDS_IBKR_EXAMPLE_CONFIG);
  check(static_cast<bool>(example), example.message);
  check(example.value.runtime.shm_prefix == "/selfquant.mds.bbo" &&
            example.value.runtime.subscriptions.size() == 1 &&
            example.value.runtime.subscriptions[0].canonical_symbol == "AAPL" &&
            example.value.runtime.subscriptions[0].con_id == 0 &&
            example.value.runtime.subscriptions[0].primary_exchange == "NASDAQ" &&
            example.value.runtime.subscriptions[0].price_scale == 4 &&
            example.value.max_subscriptions == 90 &&
            example.value.runtime.filter.max_subscriptions == 90 &&
            example.value.runtime.unlink_on_shutdown,
        "example IBKR config did not load the AAPL subscription");

  expect_reject("ibkr: {}\nshared_memory: {}\nsubscriptions: []\nextra: 1\n",
                "unknown", "unknown key");
  {
    const TempYaml file(with_subscriptions(
                            "  - canonical_symbol: AAPL\n    con_id: 1\n"
                            "    primary_exchange: NASDAQ\n"
                            "  - canonical_symbol: MSFT\n    con_id: 1\n"
                            "    primary_exchange: NASDAQ\n"),
                        "same-conid");
    const auto loaded = mds::ibkr::load_config(file.string());
    check(static_cast<bool>(loaded), loaded.message);
    check(loaded.value.runtime.subscriptions.size() == 2,
          "different symbols sharing an explicit con_id were rejected at load");
  }
  {
    const TempYaml file(with_subscriptions(
                            "  - canonical_symbol: AAPL\n    primary_exchange: NASDAQ\n"
                            "  - canonical_symbol: XAUUSD\n    sec_type: CMDTY\n"
                            "  - canonical_symbol: USDJPY\n    sec_type: CASH\n"
                            "  - canonical_symbol: EURUSD\n    sec_type: CASH\n"
                            "  - canonical_symbol: AAPL\n    con_id: 1\n"
                            "    primary_exchange: NYSE\n"),
                        "multi");
    const auto loaded = mds::ibkr::load_config(file.string());
    check(static_cast<bool>(loaded), loaded.message);
    const auto &subs = loaded.value.runtime.subscriptions;
    check(subs.size() == 5, "mixed subscriptions were not all accepted");
    check(subs[0].con_id == 0 && subs[0].price_scale == 4 &&
              subs[0].request_symbol == "AAPL",
          "omitted AAPL con_id was not auto");
    check(!subs[1].price_scale.has_value() && subs[1].sec_type == "CMDTY" &&
              subs[1].exchange == "SMART" && subs[1].currency == "USD" &&
              subs[1].primary_exchange.empty(),
          "CMDTY defaults were not applied");
    check(!subs[2].price_scale.has_value() && subs[2].canonical_symbol == "USDJPY" &&
              subs[2].request_symbol == "USD" && subs[2].currency == "JPY" &&
              subs[2].exchange == "IDEALPRO",
          "CASH pair was rewritten into the canonical symbol");
    check(subs[3].request_symbol == "EUR" && subs[3].currency == "USD",
          "EURUSD was not split");
    check(subs[4].canonical_symbol == "AAPL" && subs[4].con_id == 1 &&
              subs[4].primary_exchange == "NYSE",
          "same ticker on another listing was rejected");
  }
  expect_reject(with_subscriptions(
                    "  - canonical_symbol: AAPL\n    con_id: 1\n"
                    "    primary_exchange: NASDAQ\n"
                    "  - canonical_symbol: AAPL\n    con_id: 1\n"
                    "    primary_exchange: NASDAQ\n"),
                "dup-request", "duplicate");
  expect_reject(with_subscriptions(
                    "  - canonical_symbol: AAPL\n"),
                "stk-primary", "invalid");
  expect_reject(with_subscriptions(
                    "  - canonical_symbol: USD\n    sec_type: CASH\n"),
                "cash-short", "invalid");
  expect_reject(with_subscriptions(
                    "  - canonical_symbol: AAPL\n    sec_type: FUT\n"
                    "    primary_exchange: NASDAQ\n"),
                "future", "invalid");
  expect_reject("ibkr:\n  client_id: 32\nshared_memory:\n"
                "  prefix: /selfquant.mds.ibkr.test\nsubscriptions:\n"
                "  - canonical_symbol: AAPL\n    con_id: 1\n"
                "    primary_exchange: NASDAQ\n",
                "client-id", "invalid");
  expect_reject("ibkr:\n  market_data_mode: DELAYED\nshared_memory:\n"
                "  prefix: /selfquant.mds.ibkr.test\nsubscriptions:\n"
                "  - canonical_symbol: AAPL\n    con_id: 1\n"
                "    primary_exchange: NASDAQ\n",
                "delayed", "invalid");
  expect_reject("ibkr: {}\nshared_memory:\n  prefix: selfquant.mds.ibkr\n"
                "subscriptions:\n  - canonical_symbol: AAPL\n    con_id: 1\n"
                "    primary_exchange: NASDAQ\n",
                "prefix", "invalid");
  expect_reject(with_subscriptions(
                    "  - canonical_symbol: AAPL\n    con_id: 1\n"
                    "    sec_type: OPT\n    primary_exchange: NASDAQ\n"),
                "option", "invalid");
  expect_reject("ibkr: {}\nshared_memory:\n  prefix: /selfquant.mds.ibkr.test\n"
                "subscriptions: []\n",
                "empty", "requires postgres");
  ::unsetenv("MDS_IBKR_CONFIG_TEST_DB");
  {
    const TempYaml file(
        "ibkr: {}\npostgres:\n  url_env: MDS_IBKR_CONFIG_TEST_DB\n"
        "shared_memory:\n  prefix: /selfquant.mds.ibkr.test\n"
        "subscriptions: []\n",
        "pg-source");
    const auto loaded = mds::ibkr::load_config(file.string());
    check(static_cast<bool>(loaded), loaded.message);
    check(loaded.value.postgres_url_env == "MDS_IBKR_CONFIG_TEST_DB" &&
              loaded.value.runtime.subscriptions.empty(),
          "Postgres source was resolved during YAML parsing");
  }
  expect_reject(
      "ibkr: {}\npostgres:\n  url_env: DATABASE_URL\n"
      "shared_memory:\n  prefix: /selfquant.mds.ibkr.test\n"
      "subscriptions:\n  - canonical_symbol: AAPL\n    con_id: 1\n"
      "    primary_exchange: NASDAQ\n",
      "source-conflict", "mutually exclusive");
  expect_reject(
      "ibkr: {}\nselection:\n  rank_by: price\n"
      "shared_memory:\n  prefix: /selfquant.mds.ibkr.test\n"
      "subscriptions:\n  - canonical_symbol: AAPL\n    con_id: 1\n"
      "    primary_exchange: NASDAQ\n",
      "rank", "unknown key");
  expect_reject(
      "ibkr: {}\nselection:\n  min_average_volume_90d: -1\n"
      "shared_memory:\n  prefix: /selfquant.mds.ibkr.test\n"
      "subscriptions:\n  - canonical_symbol: AAPL\n    con_id: 1\n"
      "    primary_exchange: NASDAQ\n",
      "negative-threshold", "selection settings");
  {
    const TempYaml file(
        "ibkr:\n  max_subscriptions: 1\nselection:\n"
        "  min_average_volume_90d: 1000000\n  min_market_cap_usd: 1000000000\n"
        "shared_memory:\n  prefix: /selfquant.mds.ibkr.test\nsubscriptions:\n"
        "  - canonical_symbol: AAPL\n    con_id: 1\n"
        "    primary_exchange: NASDAQ\n"
        "  - canonical_symbol: MSFT\n    con_id: 2\n"
        "    primary_exchange: NASDAQ\n",
        "cap");
    const auto loaded = mds::ibkr::load_config(file.string());
    check(static_cast<bool>(loaded), loaded.message);
    check(loaded.value.runtime.subscriptions.size() == 2 &&
              loaded.value.max_subscriptions == 1 &&
              loaded.value.runtime.filter.max_subscriptions == 1 &&
              loaded.value.runtime.filter.min_average_volume_90d == 1000000 &&
              loaded.value.runtime.filter.min_market_cap_usd == 1000000000,
          "candidate count above max_subscriptions was rejected at load");
  }
  const std::string query(mds::ibkr::postgres_candidate_query());
  check(query.starts_with("SELECT ") &&
            query.find("INSERT ") == std::string::npos &&
            query.find("UPDATE ") == std::string::npos &&
            query.find("DELETE ") == std::string::npos,
        "Postgres candidate query is not read-only");
  const std::string update(mds::ibkr::postgres_turnover_update_query());
  check(update.starts_with("UPDATE instruments ") &&
            update.find("turnover_usd_90d = $2::double precision") !=
                std::string::npos &&
            update.find("ib_metrics_observed_at = now()") !=
                std::string::npos &&
            update.find("exchange = 'ibkr'") != std::string::npos &&
            update.find("ib_con_id = $1::bigint") != std::string::npos &&
            update.find("INSERT ") == std::string::npos &&
            update.find("DELETE ") == std::string::npos,
        "Postgres turnover update is not scoped and parameterized");
  std::size_t updated = 1;
  std::string persist_error;
  check(mds::ibkr::persist_postgres_turnover_metrics(
            "MDS_IBKR_CONFIG_TEST_DB", {}, updated, persist_error) &&
            updated == 0 && persist_error.empty(),
        "empty turnover persistence unexpectedly required Postgres");
  return 0;
}
